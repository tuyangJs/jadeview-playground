package main

import (
	"encoding/json"
	"errors"
	"os/exec"
	"runtime"
	"strings"

	jade "github.com/luoxueyousheng/JadeViewGo/v3"
)

func (h *host) menuInstall(id uint32, w *window, enabled bool) (any, error) {
	k := w.data["kind"]
	if k != "menubar" && k != "theme" {
		return nil, errors.New("菜单只能在指定示例中安装")
	}
	var menu []jade.MenuItem
	if enabled {
		menu = []jade.MenuItem{
			{ID: "sample.root", Label: "示例", Kind: jade.MenuSubmenu},
			{ID: "sample.hello", ParentID: "sample.root", Label: "发送事件", Icon: jade.MenuIcon{Path: h.asset()}},
			{ID: "sample.sep", ParentID: "sample.root", Kind: jade.MenuSeparator},
			{ID: "sample.checked", ParentID: "sample.root", Label: "复选示例", Kind: jade.MenuCheckbox, Checked: w.data["menuChecked"] != false},
		}
	}
	if !jade.SetWindowMenu(id, menu) {
		return accepted(false, "menu.install")
	}
	w.data["menuBarVisible"], w.data["customMenubar"] = enabled, enabled
	return true, nil
}

func (h *host) contextMenu(parent, source uint32, mode string) (any, error) {
	w := h.windows[parent]
	if w.data["kind"] != "contextmenu" {
		return nil, errors.New("仅在网页右键菜单示例可用")
	}
	var items []jade.ContextMenuItem
	if mode == "sample" {
		items = []jade.ContextMenuItem{
			{MenuItem: jade.MenuItem{ID: "context.image", Label: "PNG 图标", Icon: jade.MenuIcon{Path: h.asset()}}},
			{MenuItem: jade.MenuItem{ID: "context.separator", Kind: jade.MenuSeparator}},
			{MenuItem: jade.MenuItem{ID: "context.copy"}, Role: "copy"},
			{MenuItem: jade.MenuItem{ID: "context.select"}, Role: "selectAll"},
			{MenuItem: jade.MenuItem{ID: "context.more", Kind: jade.MenuSubmenu, Label: "更多"}},
			{MenuItem: jade.MenuItem{ID: "context.action", ParentID: "context.more", Label: "发送自定义事件"}},
		}
	} else if mode != "default" {
		return nil, errors.New("未知菜单模式")
	}
	if !jade.SetContextMenu(source, items) {
		return accepted(false, "context.configure")
	}
	w.data["customContextMenu"] = mode == "sample"
	return true, nil
}

func (h *host) toggleTray(enabled bool) (any, error) {
	if enabled && h.tray == 0 {
		if runtime.GOOS == "linux" {
			out, e := exec.Command("dbus-send", "--session", "--dest=org.freedesktop.DBus", "--type=method_call", "--print-reply",
				"/org/freedesktop/DBus", "org.freedesktop.DBus.NameHasOwner", "string:org.kde.StatusNotifierWatcher").Output()
			if e != nil || !strings.Contains(string(out), "boolean true") {
				return nil, errors.New("桌面未启用 StatusNotifier 托盘")
			}
		}
		h.tray = jade.TrayCreate()
		if h.tray == 0 {
			return accepted(false, "tray_create")
		}
		jade.TraySetTooltip(h.tray, title)
		jade.TraySetIconFromFile(h.tray, h.asset())
		jade.TraySetVisible(h.tray, true)
	} else if !enabled && h.tray != 0 {
		if !jade.TrayDestroy(h.tray) {
			return accepted(false, "tray_destroy")
		}
		h.tray = 0
	}
	return h.tray != 0, nil
}

func (h *host) menuEvent(name string, source uint32, raw string) {
	var c command
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if name == jade.EventTrayEvent {
		if h.tray == 0 || c.str("event") != "right-click" {
			return
		}
		jade.PopupMenu(0, []jade.MenuItem{
			{ID: "tray.show", Label: "显示主窗口", Icon: jade.MenuIcon{Path: h.asset()}},
			{ID: "tray.minimize", Label: "最小化"},
			{ID: "tray.sep", Kind: jade.MenuSeparator},
			{ID: "tray.quit", Label: "退出 Playground"},
		}, nil)
		return
	}
	item := c.str("item_id")
	if strings.HasPrefix(item, "tray.") && h.tray != 0 {
		switch item {
		case "tray.show":
			jade.SetVisible(h.main, true)
			jade.SetFocus(h.main)
		case "tray.minimize":
			jade.Minimize(h.main)
		case "tray.quit":
			jade.Close(h.main)
		}
		return
	}
	id := c.id("window_id")
	if id == 0 {
		id = source
		if name == jade.EventContextMenuClick {
			id = jade.WebViewParentWindow(source)
		}
	}
	w := h.windows[id]
	if w == nil || item == "" {
		return
	}
	var selection object
	if json.Unmarshal([]byte(raw), &selection) != nil {
		return
	}
	selection["name"] = name
	if item == "sample.checked" && w.data["customMenubar"] == true {
		w.data["menuChecked"] = w.data["menuChecked"] == false
		if _, err := h.menuInstall(id, w, true); err == nil {
			selection["checked"] = w.data["menuChecked"]
		}
	}
	h.selections[id] = selection
}
