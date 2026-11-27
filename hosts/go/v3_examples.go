package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	jade "github.com/luoxueyousheng/JadeViewGo/v3"
)

func splashOptions(l launch, theme, logoPath string) (*jade.SplashOptions, error) {
	background, accent := uint32(0xff172622), uint32(0xff66d6ae)
	if theme == "Light" {
		background, accent = 0xfff5f7f6, 0xff237b61
	}
	toColor := func(color string, fallback uint32) (uint32, error) {
		if color == "" {
			return fallback, nil
		}
		if len(color) != 7 || !colorHex.MatchString(color) {
			return 0, errors.New("启动画面颜色必须是 #RRGGBB")
		}
		n, err := strconv.ParseUint(color[1:], 16, 24)
		return 0xff000000 | uint32(n), err
	}
	duration := defaultTo(l.SplashDuration, 1400)
	if duration > 5000 {
		return nil, errors.New("启动画面最短时长必须在 0–5000ms")
	}
	var err error
	if background, err = toColor(l.SplashBackground, background); err != nil {
		return nil, err
	}
	if accent, err = toColor(l.SplashForeground, accent); err != nil {
		return nil, err
	}
	s := &jade.SplashOptions{BackgroundColor: background, AccentColor: accent, MinimumMS: duration}
	if !defaultTo(l.SplashLogo, true) {
		return s, nil
	}
	if l.SplashLogoData == "" {
		s.LogoPath = logoPath
		return s, nil
	}
	if len(l.SplashLogoData) > 1500000 {
		return nil, errors.New("启动画面 LOGO 太大")
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(l.SplashLogoData)
	if err != nil {
		return nil, errors.New("启动画面 LOGO 编码无效")
	}
	config, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil || config.Width < 1 || config.Width > 512 || config.Height != config.Width {
		return nil, errors.New("启动画面 LOGO 必须是最大 512×512 的正方形 PNG")
	}
	if _, err = png.Decode(bytes.NewReader(raw)); err != nil {
		return nil, errors.New("启动画面 LOGO 内容无效")
	}
	s.LogoData = raw
	return s, nil
}

type permissionRule struct {
	origin   string
	decision jade.PermissionDecision
}

func webOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "jade") {
		return ""
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

func (h *host) setPermission(source uint32, raw json.RawMessage) (any, error) {
	var n int32
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &n) != nil || n < -1 || n > 1 {
		return nil, errors.New("权限策略必须是 -1、0 或 1")
	}
	page, ok := jade.GetWebViewURL(source)
	origin := webOrigin(page)
	if !ok || origin == "" || !h.viewPageAllowed(source, page, "/index.html") {
		return nil, errors.New("无法确定权限示例的来源")
	}
	if n == 0 {
		h.permissions.Delete(source)
	} else {
		h.permissions.Store(source, permissionRule{origin, jade.PermissionDecision(n)})
	}
	return true, nil
}

func (h *host) permissionPageMatches(source uint32, raw string) bool {
	rule, ok := h.permissions.Load(source)
	if !ok {
		return true
	}
	var e struct{ URL string }
	return json.Unmarshal([]byte(raw), &e) == nil && webOrigin(e.URL) == rule.(permissionRule).origin
}

// 同步权限回调不调用原生库，也不等待宿主的 IPC 状态锁。
func (h *host) decidePermission(source uint32, raw string) jade.PermissionDecision {
	decision := jade.PermissionDefault
	if v, exists := h.permissions.Load(source); exists {
		rule := v.(permissionRule)
		var event struct{ Origin, RequestingURL string }
		if json.Unmarshal([]byte(raw), &event) == nil {
			origin := webOrigin(first(event.Origin, event.RequestingURL))
			if origin == rule.origin || (origin == "" && rule.decision == jade.PermissionDeny) {
				decision = rule.decision
			}
		}
	}
	h.record("permission", source, wire(object{"decision": decision, "request": json.RawMessage(validRawJSON(raw))}))
	return decision
}

func validRawJSON(raw string) string {
	if json.Valid([]byte(raw)) {
		return raw
	}
	return "null"
}

type resourceRegistration struct {
	view uint32
	raw  string
}

func (h *host) openResource(view uint32, path string) (any, error) {
	if strings.IndexByte(path, 0) >= 0 || !filepath.IsAbs(path) {
		return nil, errors.New("请选择一个本地文件的绝对路径")
	}
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	f, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if !f.Mode().IsRegular() {
		return nil, errors.New("请选择普通文件")
	}
	if len(h.resources) >= 64 {
		return nil, errors.New("请先关闭已预览的文件")
	}
	caps, code := jade.GetResourceCapabilities(view)
	if code != 0 {
		return nil, fmt.Errorf("查询资源能力失败：%d", code)
	}
	raw, ok := jade.RegisterResource(p, view, ^uint32(0))
	if !ok {
		return nil, errors.New("注册文件资源失败")
	}
	info, code := jade.GetResourceDeliveryInfo(raw, view)
	if code != 0 {
		jade.UnregisterResource(raw)
		return nil, fmt.Errorf("读取资源交付诊断失败：%d", code)
	}
	public := raw
	if h.cfg.dev != "" && runtime.GOOS == "windows" && strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//") {
		public = "https://jade." + signature + ".v" + raw
	}
	if _, exists := h.resources[public]; exists {
		return nil, errors.New("资源地址已注册")
	}
	h.resources[public] = resourceRegistration{view, raw}
	mode := "controlled"
	if info.Mode == jade.ResourceNative {
		mode = "native"
	}
	if latest, rc := jade.GetResourceCapabilities(view); rc == 0 {
		caps = latest
	}
	return object{"url": public, "name": filepath.Base(p), "bytes": f.Size(), "capabilities": caps,
		"delivery": object{"mode": mode, "reason": info.Reason, "native_error_code": info.NativeErrorCode, "message": info.Message}}, nil
}

func (h *host) closeResource(view uint32, public string) (any, error) {
	r, ok := h.resources[public]
	if !ok || r.view != view {
		return nil, errors.New("资源不属于当前视图")
	}
	if !jade.UnregisterResource(r.raw) {
		return nil, errors.New("资源仍被浏览器占用，请先停止播放或关闭预览后重试")
	}
	delete(h.resources, public)
	return true, nil
}

func (h *host) dropResourcesLocked(view uint32) {
	for u, r := range h.resources {
		if r.view == view {
			delete(h.resources, u)
		}
	}
}
func (h *host) dropResourceRecords(view uint32) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dropResourcesLocked(view)
}
