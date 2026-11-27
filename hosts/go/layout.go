package main

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"

	jade "github.com/luoxueyousheng/JadeViewGo/v3"
)

type rect struct{ X, Y, Width, Height, ViewportWidth, ViewportHeight float64 }

func cells(mode string, count int, r rect, ratio float64) ([][]string, error) {
	out := [][]string{}
	if count == 0 {
		return out, nil
	}
	for _, f := range []float64{r.X, r.Y, r.Width, r.Height, r.ViewportWidth, r.ViewportHeight, ratio} {
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, errors.New("布局坐标必须是有限数值")
		}
	}
	if r.X < 0 || r.Y < 0 || r.Width < 64 || r.Height < 64 || count < 0 || count > 6 {
		return nil, errors.New("布局尺寸或视图数量无效")
	}
	if r.ViewportWidth == 0 {
		r.ViewportWidth = r.X + r.Width
	}
	if r.ViewportHeight == 0 {
		r.ViewportHeight = r.Y + r.Height
	}
	if r.ViewportWidth < 1 || r.ViewportHeight < 1 {
		return nil, errors.New("视口尺寸无效")
	}
	ratio = max(.15, min(.85, ratio))
	cols, rows := 1, 1
	switch mode {
	case "columns":
		cols = count
	case "rows":
		rows = count
	case "grid":
		cols = int(math.Ceil(math.Sqrt(float64(count))))
		rows = (count + cols - 1) / cols
	default:
		return nil, errors.New("未知布局模式")
	}
	const gap = 8.0
	for i := 0; i < count; i++ {
		x, y := float64(i%cols)/float64(cols), float64(i/cols)/float64(rows)
		w, h := 1/float64(cols), 1/float64(rows)
		left, top := x*(r.Width+gap), y*(r.Height+gap)
		width, height := (r.Width-gap*float64(cols-1))*w, (r.Height-gap*float64(rows-1))*h
		if count == 2 && (mode == "columns" || mode == "rows") {
			start, fraction := 0.0, ratio
			if i == 1 {
				start, fraction = ratio, 1-ratio
			}
			if mode == "columns" {
				x, w = start, fraction
				left, width = start*(r.Width-gap)+gap*float64(i), fraction*(r.Width-gap)
			}
			if mode == "rows" {
				y, h = start, fraction
				top, height = start*(r.Height-gap)+gap*float64(i), fraction*(r.Height-gap)
			}
		}
		if width < 32 || height < 32 {
			return nil, errors.New("工作区不足以容纳全部 WebView")
		}
		out = append(out, []string{dim(r.X+left, x, r.ViewportWidth), dim(r.Y+top, y, r.ViewportHeight), dim(width, w, r.ViewportWidth), dim(height, h, r.ViewportHeight)})
	}
	return out, nil
}

func dim(px, fraction, viewport float64) string {
	px -= fraction * viewport
	if fraction == 0 {
		return fmt.Sprintf("%.3fpx", px)
	}
	return fmt.Sprintf("%.6f%%%+.3fpx", fraction*100, px)
}

func nativeBounds(v []string) *jade.WebViewBounds {
	return &jade.WebViewBounds{X: v[0], Y: v[1], Width: v[2], Height: v[3]}
}

type paneSettings struct {
	Incognito       int     `json:"incognito"`
	Autoplay        *int    `json:"autoplay"`
	AllowRightClick *int    `json:"allow_right_click"`
	DevTools        int32   `json:"devtools"`
	Zoom            float64 `json:"zoom_factor"`
	UserAgent       string  `json:"user_agent"`
	Throttling      int     `json:"background_throttling"`
}

func (h *host) paneCommand(parent uint32, c command) (any, error) {
	ids := []uint32{}
	for _, id := range sorted(h.panes) {
		if h.panes[id].parent == parent {
			ids = append(ids, id)
		}
	}
	action := c.str("action")
	switch action {
	case "pane.add":
		if len(ids) >= 6 {
			return nil, errors.New("每个窗口最多支持 6 个工作区视图")
		}
		cfg := decode(c, "settings", paneSettings{})
		if cfg.Zoom < 0 || cfg.Zoom > 5 || (cfg.Zoom > 0 && cfg.Zoom < .25) || cfg.DevTools < 0 || cfg.DevTools > 2 {
			return nil, errors.New("无效的 WebView 设置")
		}
		s := h.settings(c.flag("isolatedProfile"))
		s.Incognito = cfg.Incognito != 0
		s.Autoplay = defaultTo(cfg.Autoplay, 1) != 0
		s.AllowRightClick = defaultTo(cfg.AllowRightClick, 1) != 0
		s.DevTools, s.ZoomFactor, s.UserAgent, s.BackgroundThrottling = cfg.DevTools, cfg.Zoom, cfg.UserAgent, cfg.Throttling != 0
		theme := strings.ToLower(fmt.Sprint(h.windows[parent].data["theme"]))
		local := h.page("pane.html", url.Values{"label": {"WebView"}, "theme": {theme}, "profile": {first(s.ProfileName, "Default")}})
		bounds := []string{"0px", "0px", "1px", "1px"}
		var view uint32
		if c.flag("detached") {
			view = jade.CreateWebView(local, &s)
			if view == 0 {
				return accepted(false, "create_webview")
			}
			if _, err := resultOf(jade.AttachWebView(parent, view, nativeBounds(bounds))); err != nil {
				_, _ = resultOf(jade.DestroyWebView(view))
				return nil, err
			}
		} else {
			view = jade.AddWebView(parent, local, &s, nativeBounds(bounds))
		}
		if view == 0 {
			return accepted(false, action)
		}
		h.panes[view] = &pane{parent: parent, localURL: local, settings: s, data: object{
			"id": view, "parentId": parent, "windowId": parent, "label": "WebView " + strconv.FormatUint(uint64(view), 10),
			"visible": true, "profile": first(s.ProfileName, "Default"), "bounds": bounds}}
		return h.snapshot(parent), nil
	case "layout":
		layout, err := cells(c.str("mode"), len(ids), decode(c, "rect", rect{}), decode(c, "ratio", .5))
		if err != nil {
			return nil, err
		}
		for i, id := range ids {
			if _, err = resultOf(jade.SetWebViewBounds(id, nativeBounds(layout[i]))); err != nil {
				return nil, err
			}
			h.panes[id].data["bounds"] = layout[i]
		}
		return layout, nil
	case "panes.visible":
		for _, id := range ids {
			visible := c.flag("visible") && h.panes[id].data["visible"] == true
			if _, err := resultOf(jade.SetWebViewVisible(id, visible)); err != nil {
				return nil, err
			}
		}
		return true, nil
	}
	id := c.id("id")
	p := h.panes[id]
	if p == nil || p.parent != parent {
		return nil, errors.New("目标 WebView 不属于这个工作区")
	}
	if action == "layout.custom" {
		b := decode(c, "bounds", []string{})
		if len(b) != 4 {
			return nil, errors.New("必须提供四个坐标表达式")
		}
		for _, v := range b {
			if len(v) == 0 || len(v) > 160 || strings.ContainsRune(v, 0) {
				return nil, errors.New("坐标表达式无效")
			}
		}
		if _, err := resultOf(jade.SetWebViewBounds(id, nativeBounds(b))); err != nil {
			return nil, err
		}
		p.data["bounds"] = b
		return h.snapshot(parent), nil
	}
	op := c.str("operation")
	var request jade.RequestID
	switch op {
	case "destroy":
		request = jade.DestroyWebView(id)
	case "visible":
		request = jade.SetWebViewVisible(id, p.data["visible"] != true)
	case "focus":
		request = jade.FocusWebView(id)
	case "recreate":
		request = jade.RecreateWebView(id, p.localURL, &p.settings)
	case "reload":
		request = jade.ReloadWebView(id)
	case "navigate":
		to := c.str("url")
		if !httpURL(to) && !pageAllowed(h.entry, to) {
			return nil, errors.New("仅限 HTTP/HTTPS 或宿主本地页面")
		}
		request = jade.NavigateWebView(id, to)
	case "script":
		// 用户输入的脚本只在手动选择的工作区 WebView 执行，不用于控制器或系统视图。
		return resultOf(jade.ExecuteJavaScript(id, c.str("script")))
	case "back", "forward", "devtools", "zoom":
		var ok bool
		switch op {
		case "back":
			ok = jade.GoBack(id)
		case "forward":
			ok = jade.GoForward(id)
		case "devtools":
			ok = jade.OpenDevtools(id)
		case "zoom":
			v := decode(c, "zoom", float64(0))
			ok = v >= .25 && v <= 5 && jade.SetZoom(id, v)
		}
		if !ok {
			return accepted(false, op)
		}
		return h.snapshot(parent), nil
	default:
		return nil, errors.New("未知 WebView 动作")
	}
	if _, err := resultOf(request); err != nil {
		return nil, err
	}
	if op == "destroy" {
		delete(h.panes, id)
		h.eventMu.Lock()
		delete(h.firstLoads, id)
		h.eventMu.Unlock()
	}
	if op == "visible" {
		p.data["visible"] = p.data["visible"] != true
	}
	return h.snapshot(parent), nil
}
