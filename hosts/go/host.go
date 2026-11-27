package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	jade "github.com/luoxueyousheng/JadeViewGo/v3"
)

//go:embed host.json
var manifestJSON []byte

type manifest struct {
	Repository          string
	Capabilities        []string
	UnsupportedExamples map[string]string
}

// hostDisplayName distinguishes the SDK dependency from the Playground app version.
func hostDisplayName() string {
	name := "Go " + strings.TrimPrefix(runtime.Version(), "go") + " · JadeViewGo"
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == "github.com/luoxueyousheng/JadeViewGo/v3" {
				if dep.Replace != nil {
					dep = dep.Replace
				}
				if strings.HasPrefix(dep.Version, "v3.") {
					return name + " " + dep.Version
				}
				break
			}
		}
	}
	return name + "（本地 SDK）"
}

type object = map[string]any
type window struct {
	data        object
	controller  uint32
	followTheme bool
}
type pane struct {
	data     object
	parent   uint32
	localURL string
	settings jade.WebViewSettings
}
type host struct {
	cfg         config
	mu          sync.Mutex // IPC 状态；原生通知回调不等待此锁。
	eventMu     sync.Mutex
	events      []object
	firstLoads  map[uint32]string
	sequence    uint64
	windows     map[uint32]*window
	panes       map[uint32]*pane
	selections  map[uint32]object
	entry       *url.URL
	main        uint32
	tray        uint32
	theme       string
	info        manifest
	resources   map[string]resourceRegistration
	permissions sync.Map
}

func newHost(c config) *host {
	h := &host{cfg: c, windows: map[uint32]*window{}, panes: map[uint32]*pane{}, selections: map[uint32]object{}, firstLoads: map[uint32]string{}, resources: map[string]resourceRegistration{}, theme: "System"}
	if err := json.Unmarshal(manifestJSON, &h.info); err != nil {
		panic(err)
	}
	if runtime.GOOS != "windows" {
		h.info.UnsupportedExamples["splash"] = "预览运行库只在 Windows 渲染启动画面。"
		h.info.UnsupportedExamples["webpopup"] = "预览运行库的自绘网页弹窗仅在 Windows 提供。"
		h.info.UnsupportedExamples["dwm"] = "原生标题栏组件需要 Windows。"
		h.info.UnsupportedExamples["menubar"] = "当前 Go 宿主的原生菜单栏仅启用 Windows 后端。"
	}
	return h
}

func wire(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `{"ok":false,"error":"响应编码失败"}`
	}
	return string(b)
}

func (h *host) start(ctx context.Context) error {
	entry := h.cfg.dev
	if entry == "" {
		var ok bool
		entry, ok = jade.SetProtocolServicePath(h.cfg.frontend, false)
		if !ok {
			return errors.New("JadeView 未能注册前端目录或资源包")
		}
	}
	var err error
	h.entry, err = url.Parse(entry)
	if err != nil || h.entry.Host == "" {
		return fmt.Errorf("无效的前端入口：%q", entry)
	}
	if !jade.SetWebviewPermissionDecisionHandler(h.decidePermission) {
		return errors.New("注册网页权限处理器失败")
	}
	if !jade.RegisterIPCHandler("playground", h.receive) {
		return errors.New("注册 playground IPC 失败")
	}
	for _, name := range []string{
		jade.EventWindowClosed, jade.EventWindowDestroyed, jade.EventWindowStateChanged,
		jade.EventWebViewDidStartLoading, jade.EventWebViewDidFinishLoad, jade.EventMenuCommand,
		jade.EventMenuError, jade.EventContextMenuClick, jade.EventTrayEvent, jade.EventThemeChanged,
		"webview-destroyed", "webview-new-window",
	} {
		if _, ok := jade.On(name, func(source uint32, data string) string {
			h.record(name, source, data)
			if name == jade.EventWebViewDidStartLoading && !h.permissionPageMatches(source, data) {
				h.permissions.Delete(source)
			}
			if name == "webview-destroyed" {
				h.permissions.Delete(source)
				go h.dropResourceRecords(source)
			}
			if name == jade.EventWindowDestroyed || name == jade.EventWindowClosed {
				go h.closed(source)
			}
			if name == jade.EventMenuCommand || name == jade.EventContextMenuClick || name == jade.EventTrayEvent {
				go h.menuEvent(name, source, data)
			}
			return ""
		}); !ok {
			return fmt.Errorf("注册 %s 失败", name)
		}
	}
	h.mu.Lock()
	o := jade.DefaultWindowOptions()
	o.Title, o.Width, o.Height = title, 1380, 880
	o.MinWidth, o.MinHeight = 1000, 640
	o.TitleBarStyle = jade.TitleBarStyle.Overlay
	o.HideTitlebarBack, o.HideTitlebarRefresh = true, true
	o.HideWindow = h.cfg.hidden
	o.Theme = h.theme
	o.BackgroundColor = "#202020ff"
	s := h.settings(false)
	id := jade.CreateWindow(h.page("index.html", url.Values{}), 0, &o, &s)
	h.main = id
	view := jade.DefaultWebView(id)
	if id == 0 || view == 0 {
		h.mu.Unlock()
		return errors.New("主窗口创建未受理")
	}
	h.track(id, view, "main", o, true)
	if runtime.GOOS == "windows" {
		jade.SetTitlebarStyle(id, jade.TitlebarStyleOptions{Height: 48})
	}
	h.mu.Unlock()

	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		ws, wc := jade.GetWindowState(id)
		vs, vc := jade.GetWebViewState(view)
		if wc != 0 || vc != 0 || ws.ErrorCode != 0 || vs.ErrorCode != 0 {
			return fmt.Errorf("主窗口创建失败：window=%+v (%d), webview=%+v (%d)", ws, wc, vs, vc)
		}
		if ws.State >= 5 || vs.State >= 5 {
			return fmt.Errorf("主窗口已进入终止状态：%d/%d", ws.State, vs.State)
		}
		if ws.State == 4 && vs.State == 4 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
	fmt.Println("PLAYGROUND_READY", wire(object{"windowId": id, "webviewId": view, "version": jade.Version(), "pointerBits": strconv.IntSize}))
	return nil
}

func (h *host) page(file string, q url.Values) string {
	u := *h.entry
	u.Path = strings.TrimRight(u.Path, "/") + "/" + file
	u.RawPath = ""
	u.RawQuery = q.Encode()
	return u.String()
}

func (h *host) origins() string {
	scheme := strings.ToLower(h.entry.Scheme)
	if scheme == "jade" {
		return "jade://" + h.entry.Host + ",https://jade." + h.entry.Host
	}
	return h.entry.Scheme + "://" + h.entry.Host
}

func (h *host) settings(isolated bool) jade.WebViewSettings {
	s := jade.DefaultWebViewSettings()
	s.PostMessageAllowedOrigins = h.origins()
	if h.cfg.dev != "" {
		s.CORSAllowedOrigins = h.cfg.dev
	}
	if isolated {
		s.ProfileName = "playground-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return s
}

func pageAllowed(entry *url.URL, raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || (u.Path != "/index.html" && u.Path != "/pane.html") {
		return false
	}
	if strings.EqualFold(u.Scheme, entry.Scheme) && strings.EqualFold(u.Host, entry.Host) {
		return true
	}
	return strings.EqualFold(entry.Scheme, "jade") && strings.EqualFold(u.Scheme, "https") && strings.EqualFold(u.Host, "jade."+entry.Host)
}

// Linux JadeView 自定义协议为每个 WebView 分配独立的环回 HTTPS 来源。
// 只接受这个视图首次原生导航报告的确切地址，不能通用放行 127.0.0.1。
func (h *host) viewPageAllowed(source uint32, current, expectedPath string) bool {
	if pageAllowed(h.entry, current) {
		return true
	}
	if runtime.GOOS != "linux" || !strings.EqualFold(h.entry.Scheme, "jade") {
		return false
	}
	h.eventMu.Lock()
	first := h.firstLoads[source]
	h.eventMu.Unlock()
	return sameViewLoopback(first, current, expectedPath)
}

func sameViewLoopback(first, current, expectedPath string) bool {
	origin, e := url.Parse(first)
	if e != nil || origin.Scheme != "https" || origin.User != nil || origin.Hostname() != "127.0.0.1" || origin.Path != expectedPath {
		return false
	}
	port, err := strconv.ParseUint(origin.Port(), 10, 16)
	if err != nil || port == 0 {
		return false
	}
	page, e := url.Parse(current)
	return e == nil && page.User == nil && page.Scheme == origin.Scheme && page.Host == origin.Host && page.Path == expectedPath
}

func (h *host) auth(source uint32, action string) (uint32, error) {
	parent := jade.WebViewParentWindow(source)
	w := h.windows[parent]
	if source == 0 || w == nil || (w.controller != source && (h.panes[source] == nil || h.panes[source].parent != parent)) {
		return 0, errors.New("此 WebView 未获 Playground 权限")
	}
	current, ok := jade.GetWebViewURL(source)
	path := "/pane.html"
	if w.controller == source {
		path = "/index.html"
	}
	if !ok || !h.viewPageAllowed(source, current, path) {
		return 0, errors.New("只允许 Playground 自带页面调用宿主")
	}
	if source != w.controller {
		switch action {
		case "hello", "echo", "message", "child.action", "media.report":
		default:
			return 0, errors.New("此操作需要由控制视图发起")
		}
	}
	return parent, nil
}

func (h *host) track(id, view uint32, kind string, o jade.WindowOptions, follow bool) *window {
	w := &window{controller: view, followTheme: follow, data: object{
		"id": id, "kind": kind, "title": o.Title, "width": o.Width, "height": o.Height, "x": o.X, "y": o.Y,
		"theme": o.Theme, "material": first(o.BackgroundMaterial, "default"), "style": o.TitleBarStyle, "frameless": o.NoFrame,
		"topmost": o.AlwaysOnTop, "resizable": o.Resizable, "protected": o.ContentProtection, "fullscreen": o.Fullscreen,
		"maximized": o.Maximized, "components": map[string]bool{"back": !o.HideTitlebarBack, "refresh": !o.HideTitlebarRefresh,
			"icon": !o.HideTitlebarIcon, "minimize": o.Minimizable, "maximize": o.Maximizable},
	}}
	h.windows[id] = w
	return w
}

func first(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}
func copyMap(o object) object {
	out := object{}
	for k, v := range o {
		out[k] = v
	}
	return out
}

func sorted[V any](m map[uint32]V) []uint32 {
	ids := make([]uint32, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func (h *host) snapshot(parent uint32) object {
	ws, ps := []object{}, []object{}
	controller := uint32(0)
	w := h.windows[parent]
	if w != nil {
		controller = w.controller
	}
	all := parent == h.main || (w != nil && w.data["kind"] == "events")
	for _, id := range sorted(h.windows) {
		if !all && parent != id {
			continue
		}
		state, rc := jade.GetWindowState(id)
		if rc != 0 || state.State == 7 {
			continue
		}
		views, _ := jade.GetWindowWebViews(id)
		if views == nil {
			views = []uint32{}
		}
		record := copyMap(h.windows[id].data)
		record["state"], record["webviewIds"] = state.State, views
		ws = append(ws, record)
	}
	for _, id := range sorted(h.panes) {
		p := h.panes[id]
		if !all && p.parent != parent {
			continue
		}
		state, rc := jade.GetWebViewState(id)
		if rc != 0 || state.State == 7 {
			continue
		}
		record := copyMap(p.data)
		record["state"] = state.State
		ps = append(ps, record)
	}
	return object{"windows": ws, "panes": ps, "controllerId": controller, "windowId": parent, "appWindowId": h.main, "theme": h.theme}
}

func (h *host) record(name string, source uint32, data string) {
	var value any
	if json.Unmarshal([]byte(data), &value) != nil {
		value = data
	}
	h.eventMu.Lock()
	defer h.eventMu.Unlock()
	if name == jade.EventWebViewDidStartLoading && runtime.GOOS == "linux" && h.entry.Scheme == "jade" {
		if _, known := h.firstLoads[source]; !known {
			first := ""
			if page, ok := value.(map[string]any); ok {
				first, _ = page["url"].(string)
			}
			h.firstLoads[source] = first
		}
	}
	h.sequence++
	event := object{"index": h.sequence, "name": name, "sourceId": strconv.FormatUint(uint64(source), 10),
		"timeNs": strconv.FormatInt(time.Now().UnixNano(), 10), "data": value}
	if strings.HasPrefix(name, "window-") {
		event["windowId"] = source
	}
	if strings.HasPrefix(name, "webview-") {
		event["webviewId"] = source
	}
	if len(h.events) == 256 {
		copy(h.events, h.events[1:])
		h.events = h.events[:255]
	}
	h.events = append(h.events, event)
}

func (h *host) closed(id uint32) {
	h.mu.Lock()
	owned := h.windows[id] != nil
	if w := h.windows[id]; w != nil {
		h.permissions.Delete(w.controller)
		h.dropResourcesLocked(w.controller)
		h.eventMu.Lock()
		delete(h.firstLoads, w.controller)
		for view, p := range h.panes {
			if p.parent == id {
				h.permissions.Delete(view)
				h.dropResourcesLocked(view)
				delete(h.firstLoads, view)
			}
		}
		h.eventMu.Unlock()
	}
	delete(h.windows, id)
	delete(h.selections, id)
	for key, p := range h.panes {
		if p.parent == id {
			delete(h.panes, key)
		}
	}
	isMain := owned && id == h.main
	tray := uint32(0)
	if isMain {
		tray = h.tray
		h.tray = 0
	}
	h.mu.Unlock()
	if isMain {
		if tray != 0 {
			jade.TrayDestroy(tray)
		}
		jade.Exit()
	}
}

func accepted(ok bool, action string) (any, error) {
	if !ok {
		return nil, fmt.Errorf("%s：JadeView 拒绝操作", action)
	}
	return true, nil
}

func resultOf(id jade.RequestID) (string, error) {
	if id == 0 {
		return "", errors.New("JadeView 未接受请求")
	}
	defer jade.ReleaseRequest(id)
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		r, code := jade.GetRequestResult(id)
		if code != 0 {
			return "", fmt.Errorf("查询请求失败：%d", code)
		}
		switch r.State {
		case jade.RequestSucceeded:
			return r.Message, nil
		case jade.RequestFailed, jade.RequestCancelled:
			return "", fmt.Errorf("原生请求失败 (%d)：%s", r.ErrorCode, r.Message)
		}
		time.Sleep(15 * time.Millisecond)
	}
	return "", errors.New("等待原生请求超时")
}

var colorHex = regexp.MustCompile(`^#[0-9a-fA-F]{6}([0-9a-fA-F]{2})?$`)
var modes = map[string]string{
	"layout": "多 WebView 工作区", "bounds": "坐标表达式", "navscript": "导航与脚本",
	"dwm": "标题栏组件", "overlay": "覆盖层标题栏", "hidden": "无标题栏窗口", "frameless": "无边框窗口",
	"theme": "主题与背景材料", "flags": "窗口标志与尺寸", "empty": "独立 Window",
	"ipc": "IPC 请求与响应", "messages": "视图间消息", "events": "事件查看器", "dialogs": "原生对话框",
	"tray": "通知与托盘", "clipboard": "剪贴板", "storage": "YAML 持久化", "runtime": "运行环境",
	"profiles": "独立 Profile", "menubar": "原生菜单栏", "contextmenu": "网页右键菜单",
	"resources": "资源注册与预览", "permissions": "权限决策", "splash": "窗口启动画面", "webpopup": "网页弹窗",
}

func (h *host) asset() string { return filepath.Join(h.cfg.root, "assets", "playground.png") }
