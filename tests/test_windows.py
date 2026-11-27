from pathlib import Path
import tempfile
import threading
import unittest
from types import SimpleNamespace
from unittest.mock import Mock, patch

from hosts.python.app import Playground, resolve_default_frontend, resolve_root


class IndependentWindowTests(unittest.TestCase):
    def setUp(self):
        self.app = Playground.__new__(Playground)
        self.app.main_window, self.app.controller = 1, 101
        self.app.dev_origin = None
        self.app.windows = {1: {"kind": "main"}, 2: {"kind": "layout"}, 3: {"kind": "layout"}}
        self.app.controllers = {1: 101, 2: 201, 3: 301}
        self.app.panes = {
            202: {"windowId": 2, "bounds": ["0", "0", "1px", "1px"]},
            203: {"windowId": 2, "bounds": ["0", "0", "1px", "1px"]},
            302: {"windowId": 3, "bounds": ["0", "0", "1px", "1px"]},
        }
        self.app.context = threading.local()
        self.app.context.source_id, self.app.context.window_id = 201, 2
        self.app.native = SimpleNamespace(lib=SimpleNamespace(
            get_webview_parent_window_id=lambda identity: identity // 100))
        self.app.trusted = {101, 201, 301, *self.app.panes}
        self.app.stopped = threading.Event()
        self.app.menu_selections = {}
        self.app.lock = threading.RLock()
        self.app.record = Mock()

    def test_example_can_only_control_its_own_window_and_views(self):
        self.assertEqual(self.app.owned_window(None), 2)
        self.assertEqual(self.app.owned_view(202), 202)
        with self.assertRaises(PermissionError):
            self.app.owned_window(3)
        with self.assertRaises(PermissionError):
            self.app.owned_view(302)
        with self.assertRaises(ValueError):
            self.app.owned_view(201)

    def test_launcher_can_manage_other_example_windows(self):
        self.app.context.source_id, self.app.context.window_id = 101, 1
        self.assertEqual(self.app.owned_window(3), 3)

    def test_relayout_does_not_touch_another_example(self):
        operations = []

        def view(_native, identity):
            return SimpleNamespace(set_bounds=lambda bounds: (
                operations.append(identity) or SimpleNamespace(wait=lambda: None)))

        other_bounds = self.app.panes[302]["bounds"].copy()
        with patch("hosts.python.app.WebView", side_effect=view):
            self.app.layout({"mode": "columns", "rect": {
                "x": 260, "y": 120, "width": 800, "height": 500,
                "viewportWidth": 1100, "viewportHeight": 700}})
        self.assertEqual(operations, [202, 203])
        self.assertEqual(self.app.panes[302]["bounds"], other_bounds)

    def test_example_close_keeps_application_alive(self):
        self.app.on_event({"name": "window-closed", "windowId": 2})
        self.assertFalse(self.app.stopped.is_set())
        self.assertIn(3, self.app.windows)
        self.assertNotIn(2, self.app.controllers)
        self.app.on_event({"name": "window-closed", "windowId": 1})
        self.assertTrue(self.app.stopped.is_set())

    def test_windows_reuses_profile_except_when_isolation_is_required(self):
        self.app.is_windows, self.app.next_profile = True, 0
        first = self.app.settings().profile_name
        self.assertIsNone(first)
        self.assertEqual(self.app.settings().profile_name, first)
        isolated = [self.app.settings(dedicated=True).profile_name,
            self.app.settings({"incognito": 1}).profile_name,
            self.app.settings({"background_throttling": 1}).profile_name]
        self.assertEqual(len(set(isolated)), 3)
        self.assertNotIn(first, isolated)

    def test_linux_local_views_keep_exclusive_profiles(self):
        self.app.is_windows, self.app.next_profile = False, 0
        self.assertNotEqual(self.app.settings().profile_name, self.app.settings().profile_name)

    def test_main_theme_propagates_to_following_example_windows(self):
        calls = []
        self.app.is_windows = True
        self.app.theme = "Dark"
        self.app.windows = {
            1: {"kind": "main", "theme": "Dark", "material": "mica", "transparentView": True},
            2: {"kind": "resources", "theme": "Dark", "material": "default", "transparentView": False, "followMainTheme": True},
            3: {"kind": "overlay", "theme": "Dark", "material": "mica", "transparentView": True, "followMainTheme": True, "customOverlayStyle": False},
            4: {"kind": "theme", "theme": "Dark", "material": "default", "transparentView": True, "followMainTheme": False},
        }
        self.app.controllers = {1: 101, 2: 201, 3: 301, 4: 401}
        self.app.panes = {202: {"windowId": 2, "bounds": ["0", "0", "1px", "1px"]}}
        self.app.trusted = {101, 201, 301, 401, 202}
        self.app.native = SimpleNamespace(
            accepted=lambda name, *args: calls.append((name, args)) or 1,
            lib=SimpleNamespace(get_webview_parent_window_id=lambda _id: 1),
        )
        self.app.authorize = lambda _source, _action: None
        self.app.dispatch(101, {"action": "window.style", "id": 1, "operation": "theme", "value": "Light"})

        self.assertEqual(self.app.theme, "Light")
        self.assertEqual(self.app.windows[2]["theme"], "Light")
        self.assertEqual(self.app.windows[3]["theme"], "Light")
        self.assertEqual(self.app.windows[4]["theme"], "Dark")
        self.assertTrue(any(name == "set_window_theme" and args[0] == 2 and args[1] == b"Light" for name, args in calls))
        self.assertTrue(any(name == "set_window_background_color" and args[0] == 2 and args[1] == b"#f3f3f3ff" for name, args in calls))
        self.assertTrue(any(name == "set_titlebar_style" and args[0] == 3 for name, args in calls))
        ipc_targets = {args[0] for name, args in calls if name == "send_ipc_message" and args[1] == b"playground.theme"}
        self.assertEqual(ipc_targets, {201, 202, 301})

    def test_example_window_theme_override_detaches_from_main(self):
        calls = []
        self.app.is_windows = True
        self.app.theme = "Dark"
        self.app.windows = {
            1: {"kind": "main", "theme": "Dark", "material": "mica", "transparentView": True},
            4: {"kind": "theme", "theme": "Dark", "material": "default", "transparentView": True, "followMainTheme": True},
        }
        self.app.controllers = {1: 101, 4: 401}
        self.app.panes = {}
        self.app.trusted = {101, 401}
        self.app.native = SimpleNamespace(
            accepted=lambda name, *args: calls.append((name, args)) or 1,
            lib=SimpleNamespace(get_webview_parent_window_id=lambda _id: 4),
        )
        self.app.authorize = lambda _source, _action: None
        self.app.dispatch(401, {"action": "window.style", "id": 4, "operation": "theme", "value": "Light"})

        self.assertEqual(self.app.theme, "Dark")
        self.assertEqual(self.app.windows[4]["theme"], "Light")
        self.assertFalse(self.app.windows[4]["followMainTheme"])
        self.assertTrue(any(name == "send_ipc_message" and args[0] == 401 and args[1] == b"playground.theme" for name, args in calls))

    def test_non_theme_example_spawn_uses_main_theme(self):
        captured = {}
        self.app.is_windows = False
        self.app.is_win11 = False
        self.app.theme = "Light"
        self.app.entry = "jade://app"
        self.app.next_profile = 0
        self.app.dev_origin = None
        self.app.open_started = {}
        self.app.windows = {}
        self.app.controllers = {}
        self.app.trusted = set()
        self.app.subscribe_window = lambda wid, kind: self.app.windows.__setitem__(wid, {"id": wid, "kind": kind, "theme": self.app.theme, "components": {}})
        self.app.subscribe_view = lambda vid: self.app.trusted.add(vid)
        self.app.native = SimpleNamespace(
            create_window=lambda url, options=None, settings=None, features=None: (
                captured.update(url=url, theme=options.theme.decode("utf-8"), bg=options.background_color.decode("utf-8"))
                or SimpleNamespace(id=12)
            ),
            lib=SimpleNamespace(get_default_webview=lambda wid: 120),
            accepted=lambda *args: 1,
        )
        self.app.trace = lambda *args, **kwargs: None
        self.app.snapshot = lambda: {}
        self.app.spawn({"kind": "dwm", "launch": {"theme": "Dark"}})

        self.assertEqual(captured["theme"], "Light")
        self.assertEqual(captured["bg"], "#f3f3f3")
        self.assertIn("theme=light", captured["url"])
        self.assertTrue(self.app.windows[12]["followMainTheme"])

    def test_external_open_accepts_https_and_rejects_other_schemes(self):
        self.app.authorize = lambda _source, _action: None
        self.app.native = SimpleNamespace(lib=SimpleNamespace(get_webview_parent_window_id=lambda _id: 1))
        with patch("hosts.python.app.webbrowser.open", return_value=True) as opened:
            self.assertTrue(self.app.dispatch(101, {"action": "external.open", "url": "https://github.com/tuyangJs/jadeview-playground"}))
            opened.assert_called_once_with("https://github.com/tuyangJs/jadeview-playground")
        with self.assertRaises(ValueError):
            self.app.dispatch(101, {"action": "external.open", "url": "file:///C:/Windows/win.ini"})

    def test_nav_state_and_go_use_jadeview_apis(self):
        calls = []
        self.app.authorize = lambda _source, _action: None
        self.app.native = SimpleNamespace(
            accepted=lambda name, *args: calls.append((name, args)) or 1,
            lib=SimpleNamespace(
                get_webview_parent_window_id=lambda _id: 1,
                webview_can_go_back=lambda vid: 1 if vid == 101 else 0,
                webview_can_go_forward=lambda vid: 0,
            ),
        )
        state = self.app.dispatch(101, {"action": "nav.state"})
        self.assertEqual(state, {"canGoBack": True, "canGoForward": False})
        self.assertTrue(self.app.dispatch(101, {"action": "nav.go", "direction": "back"}))
        self.assertTrue(self.app.dispatch(101, {"action": "nav.go", "direction": "forward"}))
        self.assertEqual(calls, [("webview_go_back", (101,)), ("webview_go_forward", (101,))])
        with self.assertRaises(ValueError):
            self.app.dispatch(101, {"action": "nav.go", "direction": "up"})

    def test_hello_includes_host_descriptor(self):
        self.app.authorize = lambda _source, _action: None
        self.app.is_win11 = lambda: True
        self.app.keep_active_backdrop = lambda: True
        self.app.snapshot = lambda: {"windows": [], "panes": []}
        self.app.native = SimpleNamespace(
            text=lambda fn: {"get_jadeview_version": "0.2.0", "get_webview_version": "132.0"}.get(fn, ""),
            lib=SimpleNamespace(get_webview_parent_window_id=lambda _id: 1),
        )
        info = self.app.dispatch(101, {"action": "hello"})
        self.assertEqual(info["host"]["language"], "python")
        self.assertIn("Python", info["host"]["name"])
        self.assertIn("launchCommand", info["host"])
        self.assertEqual(info["host"]["repository"], "https://github.com/tuyangJs/jadeview-playground")

    def test_resolve_root_prefers_external_runtime_parent_when_frozen(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "runtime").mkdir()
            (root / "web" / "dist").mkdir(parents=True)
            (root / "web" / "dist" / "index.html").write_text("<html></html>", encoding="utf-8")
            (root / "dist" / "web").mkdir(parents=True)
            (root / "dist" / "web" / "index.html").write_text("<html></html>", encoding="utf-8")
            fake_exe = root / "dist" / "playground.exe"
            fake_exe.write_bytes(b"")
            with patch("hosts.python.app.sys.frozen", True, create=True), \
                 patch("hosts.python.app.sys.executable", str(fake_exe)):
                self.assertEqual(resolve_root(), root)
            with patch.dict("hosts.python.app.os.environ", {"NUITKA_ONEFILE_DIRECTORY": str(root / "dist")}):
                (root / "dist" / "runtime").mkdir()
                self.assertEqual(resolve_root(), root / "dist")
                self.assertEqual(resolve_default_frontend(root / "dist"), root / "dist" / "web")


if __name__ == "__main__":
    unittest.main()
