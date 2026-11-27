from __future__ import annotations
import argparse
from base64 import b64decode
from collections import deque
from concurrent.futures import ThreadPoolExecutor
from contextlib import nullcontext
import ctypes as C
import json
import os
from pathlib import Path
import platform
import re
from struct import unpack_from
import sys
import threading
import time
from urllib.parse import urlsplit
import webbrowser
from .dev import vite_frontend
from .layout import layout_bounds
from .native import (Native, Bounds, WindowOptions, WebViewSettings, ObjectState,
    MountCapabilities, Notification, IPC_CALLBACK, PERMISSION_CALLBACK, encode)
from .sdk import JadeViewError, Menu, SplashOptions, WebView, Window, WindowFeatures

def resolve_root() -> Path:
    """Resolve the workspace/bundle root for both source runs and compiled executables."""
    onefile_dir = os.environ.get("NUITKA_ONEFILE_DIRECTORY")
    if onefile_dir:
        exe_dir = Path(onefile_dir).resolve()
    elif "__compiled__" in globals():
        argv0 = Path(sys.argv[0]).resolve() if sys.argv and sys.argv[0] else None
        exe_dir = argv0.parent if argv0 and (argv0.exists() or argv0.suffix.lower() == ".exe") else Path(sys.executable).resolve().parent
    elif getattr(sys, "frozen", False):
        exe_dir = Path(sys.executable).resolve().parent
    else:
        return Path(__file__).resolve().parents[2]

    for candidate in (exe_dir, exe_dir.parent):
        if (candidate / "runtime").is_dir() and (
            (candidate / "web" / "index.html").is_file()
            or (candidate / "web" / "dist" / "index.html").is_file()
        ):
            return candidate
    for candidate in (exe_dir, exe_dir.parent):
        if (candidate / "runtime").is_dir() or (candidate / "web").is_dir():
            return candidate
    return exe_dir


def resolve_default_frontend(root: Path) -> Path:
    """Prefer `web/dist` when running in repo, or `web/` when deployed flat next to the exe."""
    for candidate in (root / "web" / "dist", root / "web"):
        if (candidate / "index.html").is_file():
            return candidate
    return root / "web" / "dist"


ROOT = resolve_root()
CHANNEL = "playground"
APP_SIGNATURE = "jadeview-next-playground"
APP_TITLE = "JadeView Playground"
LOGO = ROOT / "assets/playground.png"
MAIN_WINDOW_BACKGROUND = "#202020"
LIGHT_WINDOW_BACKGROUND = "#f3f3f3"
MAIN_TITLEBAR_HEIGHT = 48
DWMSBT_MIN_BUILD = 22621
VER_NT_WORKSTATION = 1
MATERIALS = {"default", "mica", "micaAlt", "acrylic", "mica_keepActive", "micaAlt_keepActive", "acrylic_keepActive"}


def windows_system_dark() -> bool:
    if os.name != "nt":
        return True
    try:
        import winreg
        with winreg.OpenKey(
            winreg.HKEY_CURRENT_USER,
            r"Software\Microsoft\Windows\CurrentVersion\Themes\Personalize",
        ) as key:
            value, _ = winreg.QueryValueEx(key, "AppsUseLightTheme")
            return int(value) == 0
    except OSError:
        return True

def decode_splash_logo(encoded):
    """Validate the small PNG produced by the page before passing it to JadeView."""
    if not isinstance(encoded, str) or len(encoded) > 1_500_000:
        raise ValueError("启动画面 LOGO 数据无效或过大")
    try:
        data = b64decode(encoded, validate=True)
    except ValueError as error:
        raise ValueError("启动画面 LOGO 数据无效") from error
    if len(data) < 24 or data[:16] != bytes.fromhex("89504e470d0a1a0a0000000d49484452"):
        raise ValueError("启动画面 LOGO 必须为 PNG")
    width, height = unpack_from(">II", data, 16)
    if width != height or not 1 <= width <= 512:
        raise ValueError("启动画面 LOGO 必须是正方形，且最大为 512 × 512")
    return data


def splash_color(value, default):
    if value is None:
        return default
    if not isinstance(value, str) or not re.fullmatch(r"#[0-9a-fA-F]{6}", value):
        raise ValueError("启动画面的颜色应为 #RRGGBB")
    # The picker supplies RGB, while JadeSplashOptions packs colors as 0xAARRGGBB.
    return 0xFF000000 | int(value[1:], 16)


class Playground:
    def __init__(self, library: Path, frontend: Path, data: Path, example=None, *, dev_origin=None):
        self.native = Native(library)
        self.frontend, self.data, self.example = frontend, data, example
        self.dev_origin = dev_origin
        self.ready, self.stopped = threading.Event(), threading.Event()
        self.lock = threading.RLock()
        self.commands = ThreadPoolExecutor(max_workers=1, thread_name_prefix="playground")
        self.events = deque(maxlen=400)
        self.serial = 0
        self.windows, self.panes, self.trusted = {}, {}, set()
        self.controllers = {}
        self.menu_selections = {}
        self.context = threading.local()
        self.resources = {}
        self.main_window = self.controller = self.tray = 0
        self.next_profile = 0
        self.permission_policies = {}
        self.tray_topmost = False
        self.initialized = False
        self.trace_enabled = os.environ.get("JADEVIEW_PLAYGROUND_TRACE") == "1"
        self.open_started = {}
        self.theme = "Dark" if windows_system_dark() else "Light"
        self.is_windows = os.name == "nt"
        self.is_win11 = bool(self.native.lib.is_windows_11()) if self.is_windows else False
        self.keep_active_backdrop = self.is_win11 and sys.getwindowsversion().build >= DWMSBT_MIN_BUILD
        self.native.subscribe("app", 0, self.on_event)
        self.bind_callbacks()

    def record(self, event):
        with self.lock:
            self.serial += 1
            # Large IPC bodies and personal file contents never enter the event log.
            body = json.dumps(event, ensure_ascii=False, default=str)
            if len(body) > 4096:
                event = {**event, "data": body[:4000] + "…"}
            self.events.append({"index": self.serial, "timeNs": str(time.perf_counter_ns()), **event})

    def trace(self, name, started, **fields):
        if self.trace_enabled:
            print("PLAYGROUND_TIMING " + json.dumps({"stage": name,
                "elapsedNs": str(time.perf_counter_ns() - started), **fields}), flush=True)

    def on_event(self, event):
        self.record(event)
        if event["name"] == "app-ready":
            if isinstance(event["data"], dict) and event["data"].get("ok"):
                self.ready.set()
        if event["name"] == "window-created":
            started = self.open_started.get(event["windowId"])
            if started is not None:
                self.trace("window-ready", started, windowId=event["windowId"])
        if event["name"] == "window-closed":
            with self.lock:
                self.windows.pop(event["windowId"], None)
                self.controllers.pop(event["windowId"], None)
                self.menu_selections.pop(event["windowId"], None)
            if event["windowId"] == self.main_window:
                self.stopped.set()
        if event["name"] == "webview-destroyed":
            view = event["webviewId"]
            self.panes.pop(view, None)
            self.trusted.discard(view)
            self.permission_policies.pop(view, None)
            for url, resource in list(self.resources.items()):
                if resource.webview_id == view:
                    resource.closed = True
                    self.resources.pop(url, None)
        if event["name"] == "window-all-closed":
            self.stopped.set()
        if event["name"] == "tray-event" and isinstance(event["data"], dict):
            print("TRAY_EVENT " + str(event["data"].get("event")), flush=True)
            if event["data"].get("event") == "right-click":
                self.show_tray_menu()
        if event["name"] in ("menu-command", "context-menu-click") and isinstance(event.get("data"), dict):
            data = event["data"]
            item = str(data.get("item_id", ""))
            if event["name"] == "menu-command" and item.startswith("tray."):
                self.tray_command(item.removeprefix("tray."))
            identity = data.get("window_id") or event.get("windowId")
            if identity and item.startswith(("sample.", "context.")):
                with self.lock:
                    window = self.windows.get(identity)
                    if window is None:
                        return
                    selection = {"name": event["name"], **data}
                    if event["name"] == "menu-command" and item == "sample.checked" and window.get("customMenubar"):
                        checked = not window["menuChecked"]
                        try:
                            self.set_example_menu(identity, checked)
                        except JadeViewError as error:
                            self.record({"name": "menu-update-error", "windowId": identity, "data": str(error)})
                        else:
                            selection["checked"] = checked
                    self.menu_selections[identity] = selection

    def set_example_menu(self, identity, checked):
        menu = Menu.build_from_template(self.native, [
            {"label": "示例", "submenu": [
                {"id": "sample.hello", "label": "发送事件", "icon_path": LOGO},
                {"type": "separator"},
                {"id": "sample.checked", "label": "复选示例", "type": "checkbox", "checked": checked},
            ]},
        ])
        Window(self.native, identity).set_menu(menu)
        self.windows[identity].update({"customMenubar": True, "menuChecked": checked})

    def notify(self, text, title=APP_TITLE):
        params = Notification(encode(title), encode(text), None, -1, None, None, None, None)
        return self.native.lib.show_notification(C.byref(params))

    def tray_command(self, item):
        """托盘菜单动作。菜单项见 show_tray_menu。"""
        n = self.native.lib
        if item == "quit":
            self.stopped.set()
        elif item == "show" and self.main_window:
            n.set_window_visible(self.main_window, 1)
            n.set_window_focus(self.main_window)
        elif item == "minimize" and self.main_window:
            n.minimize_window(self.main_window)
        elif item == "topmost" and self.main_window:
            self.tray_topmost = not self.tray_topmost
            n.set_window_always_on_top(self.main_window, 1 if self.tray_topmost else 0)
            self.notify(f"窗口置顶已{'开启' if self.tray_topmost else '关闭'}")
        elif item == "reset" and self.main_window:
            n.set_window_size(self.main_window, 1380, 880)
            n.set_window_position(self.main_window, 120, 120)
            self.notify("窗口位置与大小已重置")
        elif item == "devtools" and self.controller:
            n.open_devtools(self.controller)
        elif item == "about":
            self.notify(f"{APP_TITLE} · 托盘菜单演示", "关于 Playground")

    def show_tray_menu(self):
        menu = Menu.build_from_template(self.native, [
            {"id": "tray.show", "label": "显示主窗口", "icon_path": LOGO},
            {"label": "窗口", "submenu": [
                {"id": "tray.topmost", "label": "窗口置顶", "type": "checkbox",
                 "checked": self.tray_topmost},
                {"id": "tray.reset", "label": "重置位置与大小"},
                {"id": "tray.minimize", "label": "最小化到任务栏"},
            ]},
            {"id": "tray.devtools", "label": "开发者工具"},
            {"id": "tray.about", "label": "关于 Playground"},
            {"type": "separator"},
            {"id": "tray.quit", "label": "退出 Playground"},
        ])
        menu.popup()

    def configure_context_menu(self, source, payload):
        webview = WebView(self.native, source)
        mode = payload["mode"]
        if mode == "default":
            webview.clear_context_menu()
            return True
        if mode != "sample":
            raise ValueError("未知菜单模式")
        candidates = (
            Path(os.environ.get("WINDIR", r"C:\Windows")) / "Fonts/seguisym.ttf",
            Path("/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf"),
        )
        font = next((path for path in candidates if path.is_file()), None)
        items = [
            {"id": "context.image", "label": "PNG 图标", "icon_path": LOGO},
        ]
        if font:
            items.append({"id": "context.glyph", "label": "字体图标", "icon_path": font,
                          "icon_codepoint": 0x2605})
        items.extend([
            {"id": "context.separator", "kind": "separator"},
            {"role": "copy"},
            {"role": "selectAll"},
            {"id": "context.more", "kind": "submenu", "label": "更多"},
            {"id": "context.action", "parent_id": "context.more", "label": "发送自定义事件"},
        ])
        webview.set_context_menu(items)
        return True

    def bind_callbacks(self):
        native = self.native
        @IPC_CALLBACK
        def receive(source, raw):
            started = time.perf_counter_ns()
            try:
                payload = json.loads(raw.decode("utf-8")) if raw else {}
                if not isinstance(payload, dict):
                    raise ValueError("Expected an object")
                result = self.commands.submit(self.dispatch, source, payload).result()
                self.trace("command", started, action=payload.get("action", "hello"), source=source)
                response = {"ok": True, "result": result,
                    "hostProcessingNs": str(time.perf_counter_ns() - started)}
            except Exception as error:
                response = {"ok": False, "error": str(error)}
                self.record({"name": "command-error", "webviewId": source,
                    "data": str(error)})
            return native.lib.jade_text_create(encode(json.dumps(response, ensure_ascii=False)))
        @PERMISSION_CALLBACK
        def permission(source, raw):
            try:
                event = json.loads(raw.decode("utf-8")) if raw else {}
                decision = self.permission_policies.get(source, 0)
                self.record({"name": "permission", "webviewId": source,
                    "data": event, "decision": decision})
                return decision
            except Exception:
                return -1
        @IPC_CALLBACK
        def dropped(source, raw):
            self.record({"name": "drag-drop", "webviewId": source,
                "data": raw.decode("utf-8", "replace") if raw else ""})
            return None
        native.callbacks.extend([receive, permission, dropped])
        self.receive, self.permission = receive, permission
        self.dropped = dropped

    def settings(self, values=None, *, dedicated=False):
        self.next_profile += 1
        allowed = {"autoplay", "allow_right_click", "incognito", "devtools",
            "zoom_factor", "background_color", "background_throttling",
            "password_autofill", "form_autofill", "user_agent", "preload_js"}
        values = {key: value for key, value in (values or {}).items() if key in allowed}
        values.setdefault("devtools", 1)
        values.setdefault("background_color", "#151a22")
        # Ordinary Windows views reuse the live environment. Local Linux views,
        # native file resources and explicit privacy/session tests need isolation.
        isolated = (dedicated or not self.is_windows or values.get("incognito")
            or values.get("background_throttling"))
        profile = "playground-" + str(self.next_profile) if isolated else None
        return WebViewSettings(profile_name=profile, cors_allowed_origins=self.dev_origin,
            postmessage_allowed_origins=self.dev_origin, **values)

    def detach_menu(self, identity):
        try:
            self.native.wait_ready("window", identity)
            with self.lock:
                if self.windows.get(identity, {}).get("menuBarVisible"):
                    return
                ok = Window(self.native, identity).set_menu(None)
            print(f"MENU_DETACHED {identity} ok={ok}", flush=True)
        except Exception as error:
            print(f"MENU_DETACH_FAILED {identity} {error!r}", flush=True)

    def subscribe_window(self, identity, kind):
        self.windows[identity] = {"id": identity, "kind": kind,
            "theme": self.theme, "material": "default", "frameless": False,
            "style": "default", "topmost": False, "resizable": True,
            "protected": False, "fullscreen": False,
            "components": {name: True for name in ("back", "refresh", "icon", "minimize", "maximize")}}
        # 只有「菜单栏演示」窗口保留菜单栏；主窗口与其它示例窗口一律移除。
        # 窗口要在注册表里就绪后菜单接口才接受调用，放到线程里等。
        if kind != "menubar":
            threading.Thread(target=self.detach_menu, args=(identity,), daemon=True).start()
        self.native.subscribe("window", identity, self.on_event)

    def subscribe_view(self, identity, trusted=True):
        if trusted:
            self.trusted.add(identity)
        self.native.subscribe("webview", identity, self.on_event)

    def resolve_theme_name(self, theme: str) -> str:
        if theme in ("Dark", "Light"):
            return theme
        return "Dark" if windows_system_dark() else "Light"

    def window_background_for_theme(self, theme: str) -> str:
        return LIGHT_WINDOW_BACKGROUND if self.resolve_theme_name(theme) == "Light" else MAIN_WINDOW_BACKGROUND

    def overlay_colors_for_theme(self, theme: str):
        if self.resolve_theme_name(theme) == "Light":
            return b"#1b1b1b", b"#00000008", b"#00000014"
        return b"#f4f6fb", b"#ffffff10", b"#ffffff20"

    def notify_window_theme(self, window_id: int, theme: str):
        payload = encode(json.dumps({"windowId": window_id, "theme": theme}, ensure_ascii=False))
        targets = []
        controller = self.controllers.get(window_id)
        if controller and controller in self.trusted:
            targets.append(controller)
        for pane_id, pane in list(self.panes.items()):
            if pane.get("windowId") == window_id and pane_id in self.trusted:
                targets.append(pane_id)
        for view_id in targets:
            try:
                self.native.accepted("send_ipc_message", view_id, b"playground.theme", payload)
            except Exception:
                pass

    def apply_window_theme(self, identity: int, value: str, *, follow_main: bool | None = None):
        self.native.accepted("set_window_theme", identity, encode(value))
        record = self.windows[identity]
        record["theme"] = value
        if follow_main is not None:
            record["followMainTheme"] = follow_main
        if record.get("material", "default") == "default" and not record.get("transparentView"):
            color = self.window_background_for_theme(value) + "ff"
            self.native.accepted("set_window_background_color", identity, encode(color))
        if record.get("kind") == "overlay" and self.is_windows and not record.get("customOverlayStyle"):
            fg, btn_bg, hover_bg = self.overlay_colors_for_theme(value)
            self.native.accepted("set_titlebar_style", identity, 40, None, fg, btn_bg, hover_bg, None)
        self.notify_window_theme(identity, value)

    def start(self):
        started = time.perf_counter_ns()
        timings = {}
        phase = started
        def mark(name):
            nonlocal phase
            now = time.perf_counter_ns()
            timings[name] = now - phase
            self.trace("startup." + name, phase)
            phase = now
        self.data.mkdir(parents=True, exist_ok=True)
        self.native.accepted("JadeView_init", 1, None, encode(str(self.data)),
            encode(APP_TITLE), encode(APP_SIGNATURE), 0)
        self.initialized = True
        mark("initCall")
        if not self.ready.wait(30):
            raise RuntimeError("JadeView did not report app-ready")
        mark("guiReady")
        self.native.accepted("register_ipc_handler", encode(CHANNEL), self.receive)
        self.native.accepted("jade_on", b"drag-drop", self.dropped)
        self.native.accepted("set_webview_permission_handler", self.permission)
        if self.dev_origin:
            self.entry = self.dev_origin
        else:
            entry = C.create_string_buffer(1024)
            self.native.accepted("set_protocol_service_path", encode(str(self.frontend)),
                entry, len(entry), 0)
            self.entry = entry.value.decode("utf-8")
        mark("handlersAndResources")
        # The page owns the title and drag region. JadeView supplies the window controls.
        overlay = self.is_windows or platform.system() == "Linux"
        style = "BarOverlay" if overlay else "default"
        windows_version = sys.getwindowsversion() if self.is_win11 else None
        # DWM's system backdrop attribute is available starting with Windows 11 22H2.
        mica = (windows_version is not None and windows_version.build >= DWMSBT_MIN_BUILD
                and windows_version.product_type == VER_NT_WORKSTATION)
        background = "#00000000" if mica else MAIN_WINDOW_BACKGROUND
        url = self.entry + ("/index.html?surface=material" if mica else "/index.html")
        options = WindowOptions(title=APP_TITLE, width=1380, height=880,
            min_width=1000, min_height=640, title_bar_style=style, theme=self.theme,
            titlebar_back=int(not overlay), titlebar_refresh=int(not overlay),
            background_material="mica" if mica else None, transparent=int(mica),
            background_color=background)
        window = self.native.create_window(url, options=options,
            settings=self.settings({"background_color": background}))
        self.main_window = window.id
        self.open_started[window.id] = started
        self.subscribe_window(window.id, "main")
        main = self.windows[window.id]
        main["style"] = style
        main["material"] = "mica" if mica else "default"
        if overlay:
            main["components"].update(back=False, refresh=False)
        # Combined creation reserves both IDs before the UI starts building.
        # Register page authority before navigation can send its first hello.
        self.controller = self.native.lib.get_default_webview(window.id)
        if not self.controller:
            raise RuntimeError("Combined window creation did not reserve a default WebView")
        self.subscribe_view(self.controller)
        self.controllers[window.id] = self.controller
        if self.is_windows:
            # Keep this in sync with --pg-titlebar-height in window-chrome.css.
            self.native.accepted("set_titlebar_style", window.id, MAIN_TITLEBAR_HEIGHT,
                None, None, None, None, None)
        mark("windowSubmitted")
        self.native.wait_ready("window", window.id)
        mark("windowReady")
        self.native.wait_ready("webview", self.controller)
        mark("webviewReady")
        print("PLAYGROUND_READY " + json.dumps({"windowId": self.main_window,
            "webviewId": self.controller, "version": self.native.text("jadeview_version"),
            "pointerBits": C.sizeof(C.c_void_p) * 8,
            "startupNs": time.perf_counter_ns() - started, "timingsNs": timings}, ensure_ascii=False), flush=True)
        target = self.example or os.environ.get("PLAYGROUND_OPEN")
        if target:
            def open_example():
                try:
                    time.sleep(0.8)
                    self.commands.submit(self.spawn, {"kind": target}).result()
                    print("EXAMPLE_OPENED " + target, flush=True)
                except Exception as error:
                    print("EXAMPLE_OPEN_FAILED " + repr(error), flush=True)
            threading.Thread(target=open_example, daemon=True).start()

    def authorize(self, source, action):
        if source not in self.trusted:
            raise PermissionError("This WebView does not have access to playground controls")
        # Navigating a trusted local view to a remote site must remove host authority.
        url = self.native.text("get_webview_url", source)
        parsed = urlsplit(url)
        origin = (parsed.scheme, parsed.netloc.lower())
        entry = urlsplit(self.entry)
        if self.dev_origin:
            local = origin == (entry.scheme, entry.netloc) and parsed.path in ("/index.html", "/pane.html")
        else:
            authority = entry.netloc.lower()
            local = origin in (("jade", authority), ("https", "jade." + authority))
        if not local:
            raise PermissionError("Only the local demonstration pages can call host controls")
        if source not in self.controllers.values() and action not in (
            "hello", "echo", "message", "child.action", "media.report"):
            raise PermissionError("Use the main window for this operation")

    def owned_window(self, identity):
        own = getattr(self.context, "window_id", self.main_window)
        identity = int(identity or own)
        if identity not in self.windows:
            raise ValueError("Unknown playground Window ID")
        if getattr(self.context, "source_id", self.controller) != self.controller and identity != own:
            raise PermissionError("This example controls only its own window")
        return identity

    def owned_view(self, identity):
        identity = int(identity)
        if identity in self.controllers.values():
            raise ValueError("The controller WebView is reserved")
        if identity not in self.panes and identity not in self.trusted:
            raise ValueError("Unknown playground WebView ID")
        if self.native.lib.get_webview_parent_window_id(identity) != self.owned_window(None):
            raise PermissionError("This WebView belongs to a different example window")
        return identity

    def state(self, identity, kind):
        state = ObjectState()
        self.native.zero("get_" + kind + "_state", identity, C.byref(state))
        return {field: getattr(state, field) for field, _ in state._fields_}

    def snapshot(self):
        current = getattr(self.context, "window_id", self.main_window)
        inspect_all = current == self.main_window or self.windows.get(current, {}).get("kind") == "events"
        windows = []
        for identity, record in list(self.windows.items()):
            if not inspect_all and identity != current:
                continue
            state = self.state(identity, "window")
            if state["state"] == 7:
                continue
            children = [view.id for view in Window(self.native, identity).webviews]
            windows.append({**record, "state": state["state"], "webviewIds": children})
        panes = []
        for identity, pane in list(self.panes.items()):
            if not inspect_all and pane["windowId"] != current:
                continue
            state = self.state(identity, "webview")
            if state["state"] == 7:
                continue
            panes.append({**pane, "id": identity, "state": state["state"],
                "parentId": state["parent_window_id"]})
        return {"windows": windows, "panes": panes,
            "controllerId": self.controllers.get(current, 0),
            "windowId": current, "appWindowId": self.main_window, "theme": self.theme}

    def create_pane(self, payload):
        window_id = self.owned_window(None)
        if len([pane for pane in self.panes.values() if pane["windowId"] == window_id]) >= 6:
            raise ValueError("This example supports up to six layout WebViews")
        parent_theme = self.windows.get(window_id, {}).get("theme", self.theme)
        settings_payload = {"background_color": self.window_background_for_theme(parent_theme), **(payload.get("settings") or {})}
        settings = self.settings(settings_payload, dedicated=bool(payload.get("isolatedProfile")))
        url = self.entry + "/pane.html?pane=" + str(self.next_profile) + "&theme=" + parent_theme.lower()
        bounds = Bounds("0", "0", "1px", "1px")
        if payload.get("detached"):
            identity = self.native.accepted("create_webview", encode(url), C.byref(settings))
            self.subscribe_view(identity)
            try:
                self.native.request("add_webview_to_window", window_id, identity, C.byref(bounds))
            except Exception:
                WebView(self.native, identity).destroy().wait()
                raise
        else:
            view = Window(self.native, window_id).add_webview(url, bounds, settings)
            identity = view.id
            self.subscribe_view(identity)
        self.panes[identity] = {"label": "WebView " + str(identity), "windowId": window_id, "visible": True,
            "profile": settings.profile_name.decode("utf-8") if settings.profile_name else "Default",
            "bounds": ["0", "0", "1px", "1px"]}
        try:
            self.native.wait_ready("webview", identity)
        except Exception:
            self.panes.pop(identity, None)
            self.trusted.discard(identity)
            raise
        return self.snapshot()

    def layout(self, payload):
        own = self.owned_window(None)
        panes = [identity for identity, pane in self.panes.items() if pane["windowId"] == own]
        if not panes:
            return []
        cells = layout_bounds(payload["mode"], len(panes), payload["rect"], payload.get("ratio", .5))
        for identity, cell in zip(panes, cells):
            WebView(self.native, identity).set_bounds(Bounds(*cell)).wait()
            self.panes[identity]["bounds"] = cell
        return cells

    def validate_material(self, value):
        if not isinstance(value, str) or value not in MATERIALS:
            raise ValueError("未知的背景材料")
        if value != "default" and not self.is_win11:
            raise ValueError("背景材料需要 Windows 11")
        if value.endswith("_keepActive") and not self.keep_active_backdrop:
            raise ValueError("此背景材料需要 Windows 11 22H2 或更新版本")
        return value

    def validate_titlebar_style(self, style):
        if not self.is_windows:
            raise ValueError("标题栏样式设置仅支持 Windows")
        if not isinstance(style, dict):
            raise ValueError("标题栏样式配置无效")
        height = style.get("height")
        if type(height) is not int or (height != 0 and not 20 <= height <= 100):
            raise ValueError("标题栏高度须为 20–100 的整数，或 0（恢复默认）")
        requested = style.get("colors")
        if not isinstance(requested, dict):
            raise ValueError("标题栏颜色配置无效")
        # 空字符串恢复主题默认色，非空颜色可以带 Alpha 通道。
        colors = {key: requested.get(key, "") for key in (
            "background_color", "foreground_color", "button_background_color",
            "button_hover_background_color", "button_hover_foreground_color")}
        if any(not isinstance(color, str) or color and not re.fullmatch(
                r"#[0-9a-fA-F]{6}(?:[0-9a-fA-F]{2})?", color) for color in colors.values()):
            raise ValueError("标题栏颜色须为 #RRGGBB 或 #RRGGBBAA")
        return height, colors

    def apply_titlebar_style(self, identity, height, colors):
        Window(self.native, identity).set_titlebar_style(height, **colors)
        self.windows[identity].update(titlebarHeight=height, titlebarColors=colors)

    def spawn(self, payload):
        started = time.perf_counter_ns()
        kind = payload["kind"]
        titles = {
            "layout": "多 WebView 工作区", "bounds": "坐标表达式", "navscript": "导航与脚本",
            "dwm": "标题栏组件", "overlay": "覆盖层标题栏", "hidden": "无标题栏窗口",
            "frameless": "无边框窗口",
            "theme": "主题与背景材料", "flags": "窗口标志与尺寸", "empty": "独立 Window",
            "resources": "资源注册与预览", "ipc": "IPC 请求与响应", "messages": "视图间消息",
            "events": "事件查看器", "dialogs": "原生对话框", "tray": "通知与托盘",
            "clipboard": "剪贴板", "storage": "YAML 持久化", "runtime": "运行环境",
            "permissions": "权限决策", "profiles": "独立 Profile",
            "menubar": "原生菜单栏",
            "splash": "窗口启动画面", "contextmenu": "网页右键菜单", "webpopup": "网页弹窗",
        }
        if kind not in titles:
            raise ValueError("Unknown window type")
        if kind == "splash" and not self.is_windows:
            raise ValueError("窗口启动画面需要 Windows")
        if kind == "webpopup" and not self.is_windows:
            raise ValueError("自绘网页弹窗需要 Windows")
        frameless = bool(payload.get("frameless", kind == "frameless"))

        # 前端二级页面传来的启动参数。缺省时与原行为一致（尺寸、样式、材料都不变），
        # 但每一格都校验：越界或类型不对就报错，不猜也不静默夹取。
        launch = payload.get("launch") or {}
        if not isinstance(launch, dict):
            raise ValueError("Launch options must be an object")
        titlebar_style = None
        if self.is_windows and kind == "dwm" and launch.get("titlebarStyle") is not None:
            height_dip, colors = self.validate_titlebar_style(launch["titlebarStyle"])
            if height_dip or any(colors.values()):
                titlebar_style = (height_dip, colors)

        def integer(name, fallback, low, high):
            value = launch.get(name, fallback)
            if value is None:
                return fallback
            if isinstance(value, bool) or not isinstance(value, (int, float)):
                raise ValueError("Window " + name + " must be a number")
            value = int(value)
            if not low <= value <= high:
                raise ValueError("Window " + name + " is out of range")
            return value

        def position(name):
            value = launch.get(name)
            if value is None:
                return -1
            if isinstance(value, bool) or not isinstance(value, (int, float)):
                raise ValueError("Window position must be a number")
            value = int(value)
            if not -20000 <= value <= 20000:
                raise ValueError("Window position is out of range")
            return value

        def flag(name, fallback):
            value = launch.get(name, fallback)
            if not isinstance(value, bool):
                raise ValueError("Window " + name + " must be a boolean")
            return value

        frameless = flag("frameless", frameless)

        width = integer("width", 1280 if kind == "layout" else 1060, 480, 3840)
        height = integer("height", 820, 360, 2160)
        x, y = position("x"), position("y")
        style = launch.get("style", "default")
        if style not in ("default", "BarOverlay", "hidden"):
            raise ValueError("Unknown title bar style")
        # 覆盖层示例的标题栏就是它要演示的东西，允许前端改会让页面与窗口对不上。
        style = {"dwm": "default", "overlay": "BarOverlay",
                 "hidden": "hidden", "frameless": "hidden"}.get(kind, style)
        title = launch.get("title", "")
        if not isinstance(title, str):
            raise ValueError("Window title must be a string")
        title = title.strip()[:120] or "NEXT · " + titles[kind]
        supports_launch_theme = kind in ("theme", "overlay", "splash")
        requested_theme = launch.get("theme") if supports_launch_theme else None
        explicit_theme = requested_theme is not None
        theme = requested_theme or self.theme
        if theme not in ("System", "Dark", "Light"):
            raise ValueError("Unknown theme")
        follow_main_theme = (not supports_launch_theme) or (not explicit_theme) or theme == self.theme
        resizable = flag("resizable", True)
        topmost = flag("topmost", False)
        maximized = flag("maximized", False)
        protected = flag("contentProtection", False)
        splash = None
        if kind == "splash":
            show_logo = flag("splashLogo", True)
            encoded_logo = launch.get("splashLogoData")
            logo_data = decode_splash_logo(encoded_logo) if show_logo and encoded_logo is not None else None
            splash = SplashOptions(
                logo_path=LOGO if show_logo and logo_data is None else None,
                logo_data=logo_data,
                background_color=splash_color(
                    launch.get("splashBackground"), 0xFFF5F7F6 if theme == "Light" else 0xFF172622),
                accent_color=splash_color(
                    launch.get("splashForeground"), 0xFF237B61 if theme == "Light" else 0xFF66D6AE),
                minimum_ms=integer("splashDuration", 1400, 0, 5000),
            )
        features = WindowFeatures(content_protection=protected,
            auto_save_state=flag("autoSaveState", False), splash=splash)

        material = self.validate_material(launch.get(
            "material", "mica" if kind == "overlay" and self.is_win11 else "default"))
        # 背景材料示例在运行时才切换 Mica / Acrylic，set_window_backdrop 只改 DWM 属性，
        # 不会回填窗口底色。窗口与 WebView 必须从创建起就是透明底，否则会盖住材质。
        transparent_view = kind == "theme" or material != "default"
        window_background = "#00000000" if transparent_view else self.window_background_for_theme(theme)
        options = WindowOptions(title=title, width=width, height=height, x=x, y=y,
            # 最小尺寸不能大过自己：否则选了 600 宽也会被系统顶回去。
            min_width=min(1000 if kind == "layout" else 800, width),
            min_height=min(600, height),
            resizable=int(resizable), maximized=int(maximized),
            always_on_top=int(topmost),
            hide_window=int(titlebar_style is not None),
            noframe=int(frameless), title_bar_style=style,
            background_material=material, transparent=int(transparent_view),
            background_color=window_background,
            theme=theme)
        if kind == "empty":
            identity = self.native.accepted("create_window", 0, C.byref(options), C.byref(features))
        else:
            settings = self.settings({"background_color": window_background},
                dedicated=kind in ("resources", "permissions", "profiles"))
            # 带材质的窗口从创建起就是透明的，页面这一帧就要知道别铺不透明底色。
            url = self.entry + "/index.html?example=" + kind + "&theme=" + theme.lower()
            if transparent_view:
                url += "&surface=material"
            identity = self.native.create_window(url, options=options, settings=settings, features=features).id
        self.subscribe_window(identity, kind)
        self.open_started[identity] = started
        self.windows[identity].update({"material": material, "frameless": frameless,
            "style": style, "theme": theme, "title": title,
            "width": width, "height": height, "x": x, "y": y,
            "topmost": topmost, "resizable": resizable,
            "protected": protected, "maximized": maximized,
            "transparentView": transparent_view,
            "followMainTheme": follow_main_theme,
            "customOverlayStyle": False,
            "splashDuration": splash.minimum_ms if splash is not None else None})
        view = self.native.lib.get_default_webview(identity)
        if view:
            self.subscribe_view(view)
            self.controllers[identity] = view
        if titlebar_style is not None:
            # 建窗、调整样式、显示依次提交，避免出现默认配色的一帧。
            self.apply_titlebar_style(identity, *titlebar_style)
            self.native.accepted("set_window_focus", identity)
        elif kind == "overlay" and self.is_windows:
            fg, btn_bg, hover_bg = self.overlay_colors_for_theme(theme)
            self.native.accepted("set_titlebar_style", identity, 40, None, fg, btn_bg, hover_bg, None)
        self.trace("window-submitted", started, windowId=identity, kind=kind)
        return self.snapshot()

    def dispatch(self, source, payload):
        action = payload.get("action", "hello")
        self.authorize(source, action)
        n = self.native
        self.context.source_id = source
        self.context.window_id = n.lib.get_webview_parent_window_id(source)
        if action == "hello":
            return {"sourceId": source, "windowId": n.lib.get_webview_parent_window_id(source),
                "version": n.text("jadeview_version"), "engine": n.text("get_webview_version"),
                "platform": platform.system(), "win11": self.is_win11,
                "keepActiveBackdrop": self.keep_active_backdrop,
                "pointerBits": C.sizeof(C.c_void_p) * 8, "snapshot": self.snapshot(),
                "host": {
                    "language": "python",
                    "name": f"Python {platform.python_version()}",
                    "version": "0.2.0",
                    "launchCommand": "npm ci\npython hosts/python/run.py --dev",
                    "repository": "https://github.com/tuyangJs/jadeview-playground",
                }}
        if action == "frontend.ready":
            identity = self.context.window_id
            started = self.open_started.pop(identity, None)
            if started is not None:
                self.trace("frontend-ready", started, windowId=identity,
                    frontendNs=payload.get("elapsedNs"))
            return True
        if action == "frontend.background":
            if source != self.controller:
                raise PermissionError("只有主窗口可以同步页面底色")
            color = payload.get("color")
            if not isinstance(color, str) or not re.fullmatch(r"#[0-9a-fA-F]{6}", color):
                raise ValueError("无效的页面底色")
            if self.windows[self.main_window]["material"] != "default":
                return True
            # JadeView updates the window and its child WebView backgrounds together.
            n.accepted("set_window_background_color", self.main_window, encode(color + "ff"))
            return True
        if action == "example.open":
            return self.spawn({"kind": payload["example"], "launch": payload.get("options") or {}})
        if action == "snapshot":
            return self.snapshot()
        if action == "events":
            with self.lock:
                return [item for item in self.events if item["index"] > int(payload.get("after", 0))]
        if action == "pane.add":
            return self.create_pane(payload)
        if action == "layout":
            return self.layout(payload)
        if action == "layout.custom":
            identity = self.owned_view(payload["id"])
            values = payload["bounds"]
            if not isinstance(values, list) or len(values) != 4 or any(
                not isinstance(value, str) or len(value) > 160 for value in values):
                raise ValueError("Use four bounds expressions")
            WebView(n, identity).set_bounds(Bounds(*values)).wait()
            self.panes[identity]["bounds"] = values
            return self.snapshot()
        if action == "panes.visible":
            own = self.owned_window(None)
            for identity, pane in self.panes.items():
                if pane["windowId"] == own:
                    n.request("set_webview_visible", identity, int(payload["visible"] and pane["visible"]))
            return True
        if action == "pane.action":
            identity = self.owned_view(payload["id"])
            operation = payload["operation"]
            if operation == "destroy":
                WebView(n, identity).destroy().wait()
                self.panes.pop(identity, None)
                self.trusted.discard(identity)
            elif operation == "visible":
                visible = not self.panes[identity]["visible"]
                n.request("set_webview_visible", identity, int(visible))
                self.panes[identity]["visible"] = visible
            elif operation == "focus":
                n.request("focus_webview", identity)
            elif operation == "recreate":
                settings = self.settings()
                n.request("recreate_webview", identity, encode(self.entry + "/pane.html?recreated=1"), C.byref(settings))
            elif operation == "devtools":
                n.accepted("open_devtools", identity)
            elif operation in ("reload", "back", "forward"):
                name = {"reload": "reload_webview", "back": "webview_go_back", "forward": "webview_go_forward"}[operation]
                n.request(name, identity) if operation == "reload" else n.accepted(name, identity)
            elif operation == "navigate":
                target = str(payload["url"])
                if urlsplit(target).scheme.lower() not in ("https", "http", "jade"):
                    raise ValueError("Use an HTTPS, HTTP or jade URL")
                WebView(n, identity).navigate(target).wait()
            elif operation == "zoom":
                zoom = float(payload["zoom"])
                if not .25 <= zoom <= 5:
                    raise ValueError("Zoom must be between 0.25 and 5")
                n.accepted("set_webview_zoom", identity, zoom)
            elif operation == "script":
                return WebView(n, identity).evaluate(str(payload["script"])).wait()
            else:
                raise ValueError("Unknown WebView operation")
            return self.snapshot()
        if action == "window.attach":
            identity = self.owned_window(payload["id"])
            if self.native.lib.get_default_webview(identity):
                raise ValueError("This window already has a WebView")
            parent_theme = self.windows.get(identity, {}).get("theme", self.theme)
            settings = self.settings({"background_color": self.window_background_for_theme(parent_theme)})
            view = n.accepted("create_webview", encode(self.entry + "/pane.html?window=attached&theme=" + parent_theme.lower()), C.byref(settings))
            self.subscribe_view(view)
            try:
                n.request("add_webview_to_window", identity, view, C.byref(Bounds()))
            except Exception:
                WebView(n, view).destroy().wait()
                raise
            n.zero("set_default_webview", identity, view)
            return self.snapshot()
        if action == "window.components":
            flags = [payload[name] for name in ("back", "refresh", "icon", "minimize", "maximize")]
            if any(type(flag) is not bool for flag in flags):
                raise ValueError("Component flags must be booleans")
            identity = self.owned_window(payload.get("id"))
            Window(n, identity).set_titlebar_components(*flags)
            self.windows[identity]["components"] = dict(zip(
                ("back", "refresh", "icon", "minimize", "maximize"), flags))
            return True
        if action == "window.style":
            identity = self.owned_window(payload.get("id"))
            operation = payload["operation"]
            if operation == "theme":
                value = payload["value"]
                if value not in ("System", "Dark", "Light"):
                    raise ValueError("Unknown theme")
                if identity == self.main_window:
                    n.accepted("set_window_theme", identity, encode(value))
                    self.windows[identity]["theme"] = value
                    self.theme = value
                    for wid, record in list(self.windows.items()):
                        if wid != self.main_window and record.get("followMainTheme", True):
                            self.apply_window_theme(wid, value)
                else:
                    self.apply_window_theme(identity, value, follow_main=False)
            elif operation == "material":
                if not self.is_win11:
                    raise ValueError("Background materials require Windows 11")
                value = self.validate_material(payload["value"])
                n.accepted("set_window_backdrop", identity, encode(value))
                self.windows[identity]["material"] = value
            elif operation == "menubar":
                record = self.windows[identity]
                if record["kind"] != "theme":
                    raise PermissionError("请在主题与背景材料示例中切换菜单栏")
                enabled = payload["value"]
                if type(enabled) is not bool:
                    raise ValueError("菜单栏开关必须是布尔值")
                with self.lock:
                    if enabled:
                        self.set_example_menu(identity, record.get("menuChecked", True))
                    else:
                        Window(n, identity).set_menu(None)
                        record["customMenubar"] = False
                    record["menuBarVisible"] = enabled
            elif operation == "titlebar-style":
                self.apply_titlebar_style(identity, *self.validate_titlebar_style(payload))
            elif operation == "overlay":
                # 参数顺序：height, background, foreground, button_background,
                # button_hover_background, button_hover_foreground。
                # button_background 对 BarOverlay 同样生效，之前误传 None；空串表示恢复默认。
                n.accepted("set_titlebar_style", identity, int(payload["height"]), None,
                    encode(payload["color"]), encode(payload.get("button_bg", "")),
                    encode(payload["hover"]), None)
                self.windows[identity]["customOverlayStyle"] = True
            elif operation == "frame":
                Window(n, identity).set_frame(payload["style"])
                self.windows[identity]["style"] = payload["style"]
            elif operation == "size":
                width, height = int(payload["width"]), int(payload["height"])
                if not 320 <= width <= 7680 or not 240 <= height <= 4320:
                    raise ValueError("Window size is out of range")
                n.accepted("set_window_size", identity, width, height)
            elif operation in ("topmost", "resizable", "protected", "fullscreen"):
                name = {"topmost": "set_window_always_on_top", "resizable": "set_window_resizable",
                    "protected": "set_content_protection", "fullscreen": "set_window_fullscreen"}[operation]
                n.accepted(name, identity, int(bool(payload["value"])))
                self.windows[identity][operation] = bool(payload["value"])
            else:
                raise ValueError("Unknown window operation")
            return True
        if action in ("window.close", "child.action"):
            identity = n.lib.get_webview_parent_window_id(source) if action == "child.action" else self.owned_window(payload["id"])
            operation = payload.get("operation", "close")
            if action == "child.action" and operation == "back":
                return n.accepted("webview_go_back", source)
            names = {"close": "close_window", "minimize": "minimize_window", "maximize": "toggle_maximize_window"}
            if operation not in names or (action == "child.action" and identity == self.main_window):
                raise ValueError("Invalid child window operation")
            return n.accepted(names[operation], identity)
        if action == "window.focus":
            identity = self.owned_window(payload["id"])
            n.accepted("set_window_visible", identity, 1)
            return n.accepted("set_window_focus", identity)
        if action == "echo":
            value = payload.get("value", "")
            if len(json.dumps(value).encode("utf-8")) > 8 * 1024 * 1024:
                raise ValueError("The example limits a message to 8 MiB")
            return {"value": value, "utf8Bytes": len(json.dumps(value, ensure_ascii=False).encode("utf-8")),
                "webviewId": source, "receivedNs": str(time.perf_counter_ns())}
        if action == "message":
            message = json.dumps({"from": source, "text": str(payload.get("text", ""))[:4096]}, ensure_ascii=False)
            targets = [int(payload["target"])] if payload.get("target") else list(self.trusted)
            for identity in targets:
                if identity not in self.trusted:
                    raise ValueError("Unknown message target")
                n.accepted("send_ipc_message", identity, b"playground.message", encode(message))
            return {"targets": targets}
        if action == "resource.open":
            path = Path(payload["path"]).expanduser().resolve(strict=True)
            if not path.is_file():
                raise ValueError("Choose a file")
            view = WebView(n, source)
            resource = view.resource(str(path), ttl_seconds=0xffffffff)
            url = resource.url
            if self.dev_origin and self.is_windows and url.startswith("/"):
                # WebView2 将 JADE://<签名>.v 映射到此 HTTPS origin。Vite 只负责页面。
                url = f"https://jade.{APP_SIGNATURE}.v{url}"
            self.resources[url] = resource
            capabilities = view.resource_capabilities
            return {"url": url, "name": path.name, "bytes": path.stat().st_size,
                "delivery": resource.delivery_info, "capabilities":
                {key: getattr(capabilities, key) for key, _ in capabilities._fields_}}
        if action == "resource.close":
            resource = self.resources[payload["url"]]
            if resource.webview_id != source:
                raise PermissionError("This resource belongs to another example")
            resource.close()
            del self.resources[payload["url"]]
            return True
        if action == "media.report":
            self.record({"name": "media", "webviewId": source, "data": payload.get("value")})
            return True
        if action == "permission":
            value = int(payload["value"])
            if value not in (-1, 0, 1):
                raise ValueError("Invalid permission policy")
            self.permission_policies[source] = value
            return True
        if action == "menu.selection":
            with self.lock:
                return self.menu_selections.get(self.context.window_id)
        if action == "menu.install":
            identity = self.owned_window(None)
            if self.windows[identity]["kind"] != "menubar":
                raise PermissionError("请在菜单栏示例中调用")
            with self.lock:
                self.set_example_menu(identity, self.windows[identity].get("menuChecked", True))
            return True
        if action == "context.configure":
            identity = self.owned_window(None)
            if self.windows[identity]["kind"] != "contextmenu":
                raise PermissionError("请在右键菜单示例中调用")
            self.configure_context_menu(source, payload)
            self.windows[identity]["customContextMenu"] = payload["mode"] != "default"
            return True
        if action == "clipboard.read":
            return n.text("clipboard_read_text")
        if action == "clipboard.write":
            return n.accepted("clipboard_write_text", encode(str(payload["text"])))
        if action == "notification":
            params = Notification(encode(APP_TITLE), encode(payload.get("text", "来自 Python 宿主的系统通知")),
                None, -1, None, None, None, None)
            return n.accepted("show_notification", C.byref(params))
        if action == "tray":
            if payload["enabled"] and not self.tray:
                self.tray = n.accepted("tray_create")
                n.accepted("tray_set_tooltip", self.tray, encode(APP_TITLE))
            elif not payload["enabled"] and self.tray:
                n.accepted("tray_destroy", self.tray)
                self.tray = 0
            return bool(self.tray)
        if action == "system":
            return {"displays": json.loads(n.text("get_displays_info")), "locale": n.text("getLocale"),
                "paths": {name: n.text("getPath", encode(name)) for name in ("app", "userData", "temp")},
                "mount": self.mount_capabilities()}
        if action == "external.open":
            url = str(payload.get("url", "")).strip()
            parsed = urlsplit(url)
            if parsed.scheme.lower() not in ("https", "http") or not parsed.netloc:
                raise ValueError("Only HTTP/HTTPS URLs can be opened externally")
            return bool(webbrowser.open(url))
        if action == "nav.state":
            return {
                "canGoBack": bool(n.lib.webview_can_go_back(source)),
                "canGoForward": bool(n.lib.webview_can_go_forward(source)),
            }
        if action == "nav.go":
            direction = payload.get("direction")
            if direction == "back":
                return bool(n.accepted("webview_go_back", source))
            if direction == "forward":
                return bool(n.accepted("webview_go_forward", source))
            raise ValueError("Unknown navigation direction")
        if action == "storage.save":
            result = n.lib.yaml_set(b"playground.yaml", b"example.value", encode(json.dumps(payload["value"], ensure_ascii=False)))
            if result != 1:
                raise RuntimeError("YAML write failed: " + str(result))
            return True
        if action == "storage.read":
            size = n.lib.yaml_get(b"playground.yaml", b"example.value", None, 0)
            if size == 0:
                return None
            if size < 2:
                raise RuntimeError("YAML read failed: " + str(size))
            buffer = C.create_string_buffer(size)
            if n.lib.yaml_get(b"playground.yaml", b"example.value", buffer, size) != 1:
                raise RuntimeError("YAML value changed while reading")
            return json.loads(buffer.value)
        raise ValueError("Unknown action: " + str(action))

    def mount_capabilities(self):
        value = MountCapabilities()
        self.native.zero("get_japk_mount_capabilities", C.byref(value))
        return {name: getattr(value, name) for name, _ in value._fields_}

    def close(self):
        # Finish accepted jobs before asking the runtime to drain callbacks.
        self.commands.shutdown(wait=True, cancel_futures=True)
        for resource in list(self.resources.values()):
            try:
                resource.close()
            except Exception as error:
                print("Resource cleanup:", error, flush=True)
        if self.initialized:
            while self.native.lib.jadeview_exit_wait(1000) != 1:
                print("Waiting for native handles and callbacks to drain…", flush=True)

def fail_startup(parser: argparse.ArgumentParser, message: str):
    if os.name == "nt" and sys.stderr is None:
        try:
            C.windll.user32.MessageBoxW(None, message, APP_TITLE, 0x10)
        except Exception:
            pass
        raise SystemExit(2)
    parser.error(message)


def main():
    parser = argparse.ArgumentParser(description=APP_TITLE)
    parser.add_argument("--dll", type=Path, help="JadeView.dll or libJadeView.so")
    frontend = parser.add_mutually_exclusive_group()
    frontend.add_argument("--frontend", type=Path, default=resolve_default_frontend(ROOT))
    frontend.add_argument("--dev", nargs="?", const="", metavar="URL",
        help="启动 Vite 并加载其地址，也可连接已运行的本机 Vite 地址")
    parser.add_argument("--data", type=Path)
    parser.add_argument("--example", help="启动后打开指定示例，例如 splash、contextmenu 或 menubar")
    args = parser.parse_args()
    library = args.dll or Path(os.environ.get("JADEVIEW_LIBRARY",
        str(ROOT / "runtime" / ("JadeView.dll" if os.name == "nt" else "libJadeView.so"))))
    if not library.is_file():
        fail_startup(parser, f"未找到运行库: {library}\n请将 JadeView.dll 放入 runtime/ 或通过 --dll 指定路径。")
    if args.dev is None and not ((args.frontend / "index.html").is_file() or
            (args.frontend.is_file() and args.frontend.suffix.lower() in (".asar", ".japk"))):
        fail_startup(parser, f"未找到前端产物: {args.frontend}\n请先执行 npm run build 生成 web/dist，或使用 --dev。")
    data = args.data or ROOT / (".data" if args.dev is None else ".data/dev")
    server = nullcontext((None, None)) if args.dev is None else vite_frontend(ROOT, args.dev)
    try:
        with server as (origin, process):
            app = Playground(library.resolve(), args.frontend.resolve(), data.resolve(), args.example,
                dev_origin=origin)
            try:
                app.start()
                while not app.stopped.wait(.5):
                    if process is not None and process.poll() is not None:
                        raise RuntimeError(f"Vite 已退出，退出码 {process.returncode}")
            finally:
                app.close()
    except KeyboardInterrupt:
        pass
    except (OSError, RuntimeError, ValueError) as error:
        if getattr(error, "winerror", None) == 193:
            bits = C.sizeof(C.c_void_p) * 8
            fail_startup(parser, f"当前可执行程序是 {bits} 位，请使用与当前 JadeView.dll 相同位数的编译产物。")
        fail_startup(parser, str(error))
    return 0
