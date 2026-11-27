package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	jade "github.com/luoxueyousheng/JadeViewGo/v3"
)

type command map[string]json.RawMessage
type launch struct {
	Title                                                                      string
	Width, Height, X, Y                                                        *int
	Theme, Style, Material                                                     string
	Frameless, Resizable, Topmost, Maximized, ContentProtection, AutoSaveState *bool
	TitlebarStyle                                                              *titlebar
	SplashLogo                                                                 *bool
	SplashLogoData, SplashBackground, SplashForeground                         string
	SplashDuration                                                             *uint32
}
type titlebar struct {
	Height int
	Colors map[string]string
}

func decode[T any](c command, name string, fallback T) T {
	b := c[name]
	if len(b) == 0 || string(b) == "null" {
		return fallback
	}
	var x T
	if json.Unmarshal(b, &x) != nil {
		return fallback
	}
	return x
}
func (c command) str(k string) string { return decode(c, k, "") }
func (c command) id(k string) uint32  { return decode(c, k, uint32(0)) }
func (c command) flag(k string) bool  { return decode(c, k, false) }
func defaultTo[T any](x *T, d T) T {
	if x != nil {
		return *x
	}
	return d
}

func (h *host) receive(source uint32, data string) string {
	if len(data) > 9<<20 {
		return wire(object{"ok": false, "error": "IPC 数据超过限制"})
	}
	var cmd command
	if err := json.Unmarshal([]byte(data), &cmd); err != nil || cmd.str("action") == "" {
		return wire(object{"ok": false, "error": "缺少有效的 action"})
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	parent, err := h.auth(source, cmd.str("action"))
	var result any
	if err == nil {
		result, err = h.dispatch(source, parent, cmd)
	}
	if err != nil {
		return wire(object{"ok": false, "error": err.Error()})
	}
	return wire(object{"ok": true, "result": result})
}

func (h *host) target(parent, source, id uint32) (*window, uint32, error) {
	if id == 0 {
		id = parent
	}
	w := h.windows[id]
	main := h.windows[h.main]
	if w == nil || (parent != id && (main == nil || source != main.controller)) {
		return nil, 0, errors.New("目标不属于当前 Playground 窗口")
	}
	return w, id, nil
}

func (h *host) dispatch(source, parent uint32, c command) (any, error) {
	action := c.str("action")
	switch action {
	case "hello":
		engine, _ := jade.GetWebViewVersion()
		goos := map[string]string{"windows": "Windows", "linux": "Linux"}[runtime.GOOS]
		windows11 := jade.IsWindows11()
		return object{"sourceId": source, "windowId": parent, "version": jade.Version(), "engine": engine,
			"platform": goos, "win11": windows11, "keepActiveBackdrop": windows11, "pointerBits": strconv.IntSize,
			"snapshot": h.snapshot(parent), "host": object{"language": "go", "name": hostDisplayName(),
				"launchCommand": "go -C hosts/go run . --dev",
				"repository":    h.info.Repository, "capabilities": h.info.Capabilities, "unsupportedExamples": h.info.UnsupportedExamples}}, nil
	case "snapshot":
		return h.snapshot(parent), nil
	case "frontend.ready":
		// 启动画面使用原生标题栏负责拖拽；等 WebView 就绪后再覆盖
		// 单窗菜单，避免提前设置被窗口的原生初始化覆盖。
		if w := h.windows[parent]; w != nil && w.data["kind"] == "splash" && w.controller == source {
			if jade.SetWindowMenu(parent, nil) {
				w.data["menuBarVisible"] = false
			}
		}
		fmt.Println("FRONTEND_READY", parent, source)
		return true, nil
	case "frontend.background":
		color := c.str("color")
		if parent != h.main || !colorHex.MatchString(color) || len(color) != 7 {
			return nil, errors.New("无效的主窗口底色")
		}
		return accepted(jade.SetBackgroundColor(parent, color+"ff"), action)
	case "example.open":
		if parent != h.main {
			return nil, errors.New("请从主窗口打开示例")
		}
		var opts launch
		if b := c["options"]; len(b) > 0 {
			if err := json.Unmarshal(b, &opts); err != nil {
				return nil, err
			}
		}
		return h.spawn(c.str("example"), opts)
	case "window.components", "window.style", "window.focus", "window.close", "window.attach":
		w, id, err := h.target(parent, source, c.id("id"))
		if err != nil {
			return nil, err
		}
		switch action {
		case "window.components":
			flags := map[string]bool{}
			for _, k := range []string{"back", "refresh", "icon", "minimize", "maximize"} {
				raw := c[k]
				if string(raw) != "true" && string(raw) != "false" {
					return nil, fmt.Errorf("无效开关：%s", k)
				}
				flags[k] = c.flag(k)
			}
			if !jade.SetTitlebarComponents(id, flags["back"], flags["refresh"], flags["icon"], flags["minimize"], flags["maximize"]) {
				return accepted(false, action)
			}
			w.data["components"] = flags
			return true, nil
		case "window.style":
			return h.windowStyle(w, id, c)
		case "window.focus":
			if !h.cfg.hidden && !jade.SetVisible(id, true) {
				return accepted(false, action)
			}
			return one(jade.SetFocus(id), action)
		case "window.close":
			return one(jade.Close(id), action)
		case "window.attach":
			if w.controller != 0 {
				return nil, errors.New("窗口已有关联 WebView")
			}
			opts := h.settings(false)
			view := jade.AddWebView(id, h.page("index.html", url.Values{"example": {"empty"}, "theme": {strings.ToLower(h.theme)}}), &opts, nil)
			if view == 0 || !jade.SetDefaultWebView(id, view) {
				if view != 0 {
					_, _ = resultOf(jade.DestroyWebView(view))
				}
				return accepted(false, action)
			}
			w.controller = view
			return h.snapshot(parent), nil
		}
	case "child.action":
		op := c.str("operation")
		if op == "back" {
			return one(jade.GoBack(source), op)
		}
		if parent == h.main {
			return nil, errors.New("主窗口不接受此动作")
		}
		switch op {
		case "minimize":
			return one(jade.Minimize(parent), op)
		case "maximize":
			return one(jade.ToggleMaximize(parent), op)
		case "close":
			return one(jade.Close(parent), op)
		}
	case "nav.state":
		return object{"canGoBack": jade.CanGoBack(source), "canGoForward": jade.CanGoForward(source)}, nil
	case "nav.go":
		switch c.str("direction") {
		case "back":
			return accepted(jade.GoBack(source), action)
		case "forward":
			return accepted(jade.GoForward(source), action)
		}
	case "pane.add", "layout", "layout.custom", "panes.visible", "pane.action":
		return h.paneCommand(parent, c)
	case "echo":
		raw := c["value"]
		if len(raw) > 8<<20 {
			return nil, errors.New("消息最多 8 MiB")
		}
		if len(raw) == 0 {
			raw = []byte(`""`)
		}
		return object{"value": raw, "utf8Bytes": len(raw), "webviewId": source, "receivedNs": strconv.FormatInt(time.Now().UnixNano(), 10)}, nil
	case "message":
		text := []rune(c.str("text"))
		if len(text) > 4096 {
			text = text[:4096]
		}
		target := c.id("target")
		targets := []uint32{}
		include := func(id uint32, path string) {
			if id == 0 || (target != 0 && target != id) {
				return
			}
			current, ok := jade.GetWebViewURL(id)
			if !ok || !h.viewPageAllowed(id, current, path) {
				return
			}
			if jade.SendIPCMessage(id, "playground.message", wire(object{"from": source, "target": id, "text": string(text)})) {
				targets = append(targets, id)
			}
		}
		for _, id := range sorted(h.windows) {
			include(h.windows[id].controller, "/index.html")
		}
		for _, id := range sorted(h.panes) {
			include(id, "/pane.html")
		}
		if target != 0 && len(targets) == 0 {
			return nil, errors.New("目标视图不可用")
		}
		return object{"targets": targets}, nil
	case "events":
		after := decode(c, "after", uint64(0))
		h.eventMu.Lock()
		defer h.eventMu.Unlock()
		records := []object{}
		for _, e := range h.events {
			if e["index"].(uint64) > after {
				records = append(records, e)
			}
		}
		return records, nil
	case "media.report":
		h.record("media", source, string(c["value"]))
		return true, nil
	case "clipboard.read":
		text, ok := jade.ClipboardReadText()
		if !ok {
			return "", nil
		}
		return text, nil
	case "clipboard.write":
		return one(jade.ClipboardWriteText(c.str("text")), action)
	case "notification":
		return one(jade.ShowNotification(jade.NotificationParams{Summary: title, Body: c.str("text"), Timeout: -1}), action)
	case "tray":
		return h.toggleTray(c.flag("enabled"))
	case "system":
		m := object{}
		raw, _ := jade.GetDisplaysInfo()
		var displays any
		if json.Unmarshal([]byte(raw), &displays) != nil {
			displays = []any{}
		}
		for _, n := range []string{"app", "userData", "temp"} {
			m[n], _ = jade.GetPath(n)
		}
		locale, _ := jade.GetLocale()
		var mount any
		if caps, code := jade.GetJAPKMountCapabilities(); code == 0 {
			mount = caps
		}
		return object{"displays": displays, "locale": locale, "paths": m, "mount": mount}, nil
	case "external.open":
		raw := strings.TrimSpace(c.str("url"))
		if !httpURL(raw) {
			return nil, errors.New("仅可在浏览器打开 HTTP/HTTPS URL")
		}
		var p *exec.Cmd
		if runtime.GOOS == "windows" {
			p = exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", raw)
		} else {
			p = exec.Command("xdg-open", raw)
		}
		if err := p.Start(); err != nil {
			return nil, err
		}
		go p.Wait()
		return true, nil
	case "storage.save":
		raw := c["value"]
		if len(raw) == 0 {
			return nil, errors.New("缺少 value")
		}
		return accepted(jade.YAMLSet("playground.yaml", "example.value", string(raw)) == 1, action)
	case "storage.read":
		s, ok := jade.YAMLGet("playground.yaml", "example.value")
		if !ok {
			return nil, nil
		}
		if !json.Valid([]byte(s)) {
			return nil, errors.New("持久化数据不是 JSON")
		}
		return json.RawMessage(s), nil
	case "menu.install":
		if h.windows[parent].data["kind"] != "menubar" {
			return nil, errors.New("请在菜单栏示例中调用")
		}
		return h.menuInstall(parent, h.windows[parent], true)
	case "menu.selection":
		return h.selections[parent], nil
	case "context.configure":
		return h.contextMenu(parent, source, c.str("mode"))
	case "resource.open":
		if h.windows[parent].data["kind"] != "resources" {
			return nil, errors.New("请在资源示例中选择要预览的文件")
		}
		return h.openResource(source, c.str("path"))
	case "resource.close":
		return h.closeResource(source, c.str("url"))
	case "permission":
		if h.windows[parent].data["kind"] != "permissions" {
			return nil, errors.New("请在权限示例中设置策略")
		}
		return h.setPermission(source, c["value"])
	}
	return nil, fmt.Errorf("不支持的操作：%s", action)
}

func one(ok bool, action string) (any, error) {
	if !ok {
		return accepted(false, action)
	}
	return 1, nil
}
func httpURL(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && u.Host != "" && u.User == nil && (strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https"))
}

func (h *host) spawn(kind string, l launch) (any, error) {
	if why := h.info.UnsupportedExamples[kind]; why != "" {
		return nil, errors.New(why)
	}
	name := modes[kind]
	if name == "" {
		return nil, fmt.Errorf("未知示例：%q", kind)
	}
	if len(h.windows) >= 30 {
		return nil, errors.New("窗口过多，请先关闭不再使用的示例")
	}
	o := jade.DefaultWindowOptions()
	name = first(strings.TrimSpace(l.Title), "NEXT · "+name)
	if len([]rune(name)) > 120 {
		return nil, errors.New("标题最多 120 字符")
	}
	o.Title = name
	w := 1060
	if kind == "layout" {
		w = 1280
	}
	o.Width, o.Height = defaultTo(l.Width, w), defaultTo(l.Height, 820)
	o.X, o.Y = defaultTo(l.X, -1), defaultTo(l.Y, -1)
	if o.Width < 480 || o.Width > 3840 || o.Height < 360 || o.Height > 2160 || o.X < -20000 || o.X > 20000 || o.Y < -20000 || o.Y > 20000 {
		return nil, errors.New("窗口尺寸或坐标超出范围")
	}
	o.MinWidth, o.MinHeight = min(o.Width, 800), min(o.Height, 600)
	o.TitleBarStyle = first(l.Style, "default")
	switch kind {
	case "dwm":
		o.TitleBarStyle = "default"
	case "overlay":
		o.TitleBarStyle = jade.TitleBarStyle.Overlay
	case "splash":
		// 启动画面期间 WebView 尚不可交互，拖拽须由原生标题栏处理。
		o.TitleBarStyle = jade.TitleBarStyle.Default
	case "hidden", "frameless":
		o.TitleBarStyle = "hidden"
	}
	switch o.TitleBarStyle {
	case "default", "hidden", "BarOverlay":
	default:
		return nil, errors.New("无效的标题栏模式")
	}
	o.NoFrame = defaultTo(l.Frameless, kind == "frameless")
	o.Resizable, o.AlwaysOnTop = defaultTo(l.Resizable, true), defaultTo(l.Topmost, false)
	o.Maximized, o.ContentProtection, o.AutoSaveState = defaultTo(l.Maximized, false), defaultTo(l.ContentProtection, false), defaultTo(l.AutoSaveState, false)
	o.Theme = first(l.Theme, h.theme)
	if !validTheme(o.Theme) {
		return nil, errors.New("无效的主题")
	}
	o.BackgroundMaterial = first(l.Material, "default")
	if !validMaterial(o.BackgroundMaterial) {
		return nil, errors.New("无效的背景材质或系统不支持")
	}
	o.Transparent = kind == "theme" && jade.IsWindows11() || o.BackgroundMaterial != "default"
	o.BackgroundColor = "#202020ff"
	if o.Theme == "Light" {
		o.BackgroundColor = "#f3f3f3ff"
	}
	if o.Transparent {
		o.BackgroundColor = "#00000000"
	}
	o.HideWindow = h.cfg.hidden
	if kind == "splash" {
		var err error
		o.Splash, err = splashOptions(l, o.Theme, h.asset())
		if err != nil {
			return nil, err
		}
	}
	var custom *jade.TitlebarStyleOptions
	if l.TitlebarStyle != nil && kind == "dwm" {
		style, err := l.TitlebarStyle.native()
		if err != nil {
			return nil, err
		}
		custom = &style
	}
	id := uint32(0)
	if kind == "empty" {
		id = jade.CreateEmptyWindow(0, &o)
	} else {
		s := h.settings(kind == "profiles" || kind == "permissions" || kind == "resources")
		s.BackgroundColor = o.BackgroundColor
		q := url.Values{"example": {kind}, "theme": {strings.ToLower(o.Theme)}}
		if o.Transparent {
			q.Set("surface", "material")
		}
		id = jade.CreateWindow(h.page("index.html", q), 0, &o, &s)
	}
	if id == 0 {
		return accepted(false, "example.open")
	}
	view := jade.DefaultWebView(id)
	if kind != "empty" && view == 0 {
		jade.Close(id)
		return nil, errors.New("示例没有默认 WebView")
	}
	record := h.track(id, view, kind, o, l.Theme == "")
	if o.Splash != nil {
		record.data["splashDuration"] = o.Splash.MinimumMS
	}
	if custom != nil {
		if !jade.SetTitlebarStyle(id, *custom) {
			jade.Close(id)
			return accepted(false, "titlebar-style")
		}
		record.data["titlebarHeight"], record.data["titlebarColors"] = custom.Height, l.TitlebarStyle.Colors
	} else if kind == "overlay" && runtime.GOOS == "windows" {
		jade.SetTitlebarStyle(id, jade.TitlebarStyleOptions{Height: 40})
	}
	return h.snapshot(h.main), nil
}

func validTheme(s string) bool { return s == "Light" || s == "Dark" || s == "System" }
func validMaterial(s string) bool {
	if s == "default" {
		return true
	}
	switch strings.TrimSuffix(s, "_keepActive") {
	case "mica", "micaAlt", "acrylic":
		return jade.IsWindows11()
	}
	return false
}

func (t titlebar) native() (jade.TitlebarStyleOptions, error) {
	b := jade.TitlebarStyleOptions{
		Height: t.Height, BackgroundColor: t.Colors["background_color"], ForegroundColor: t.Colors["foreground_color"],
		ButtonBackgroundColor: t.Colors["button_background_color"], ButtonHoverBackgroundColor: t.Colors["button_hover_background_color"],
		ButtonHoverForegroundColor: t.Colors["button_hover_foreground_color"],
	}
	if t.Height != 0 && (t.Height < 20 || t.Height > 100) {
		return b, errors.New("标题栏高度应为 0 或 20–100")
	}
	for _, v := range t.Colors {
		if v != "" && !colorHex.MatchString(v) {
			return b, errors.New("标题栏颜色格式无效")
		}
	}
	return b, nil
}

func (h *host) windowStyle(w *window, id uint32, c command) (any, error) {
	op := c.str("operation")
	val, flag := c.str("value"), c.flag("value")
	var ok bool
	switch op {
	case "theme":
		if !validTheme(val) {
			return nil, errors.New("无效主题")
		}
		ok = jade.SetTheme(id, val)
		if ok {
			w.data["theme"] = val
			if id == h.main {
				h.theme = val
				for wid, row := range h.windows {
					if wid != id && row.followTheme && jade.SetTheme(wid, val) {
						row.data["theme"] = val
					}
				}
			} else {
				w.followTheme = false
			}
		}
	case "material":
		ok = validMaterial(val) && jade.SetBackdrop(id, val)
		if ok {
			w.data["material"] = val
		}
	case "menubar":
		return h.menuInstall(id, w, flag)
	case "frame":
		style := c.str("style")
		if style != "default" && style != "hidden" && style != "BarOverlay" {
			return nil, errors.New("无效的边框模式")
		}
		ok = jade.SetWindowFrame(id, style)
		if ok {
			w.data["style"] = style
		}
	case "size":
		width, height := decode(c, "width", 0), decode(c, "height", 0)
		if width < 320 || width > 7680 || height < 240 || height > 4320 {
			return nil, errors.New("窗口尺寸超出范围")
		}
		ok = jade.SetSize(id, width, height)
		if ok {
			w.data["width"], w.data["height"] = width, height
		}
	case "titlebar-style", "overlay":
		t := titlebar{Height: decode(c, "height", -1), Colors: decode(c, "colors", map[string]string{})}
		if op == "overlay" {
			t.Colors = map[string]string{"foreground_color": c.str("color"), "button_background_color": c.str("button_bg"), "button_hover_background_color": c.str("hover")}
		}
		n, err := t.native()
		if err != nil {
			return nil, err
		}
		ok = jade.SetTitlebarStyle(id, n)
		if ok {
			w.data["titlebarHeight"], w.data["titlebarColors"] = n.Height, t.Colors
		}
	case "topmost", "resizable", "protected", "fullscreen":
		setters := map[string]func(uint32, bool) bool{"topmost": jade.SetAlwaysOnTop, "resizable": jade.SetResizable, "protected": jade.SetContentProtection, "fullscreen": jade.SetFullscreen}
		ok = setters[op](id, flag)
		if ok {
			w.data[op] = flag
		}
	default:
		return nil, fmt.Errorf("未知窗口操作：%s", op)
	}
	return accepted(ok, op)
}
