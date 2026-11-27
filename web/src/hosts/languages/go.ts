import type { HostLanguageModule } from "../types";

const goHostModule: HostLanguageModule = {
  id: "go",
  language: "Go",
  displayName: "Go + JadeViewGo v3",
  snippets: {
    dwm: `// Window ID 用于原生窗口和标题栏操作。
jade.SetResizable(windowID, true)
jade.SetTitlebarComponents(windowID, true, true, true, true, true)
jade.SetTitlebarStyle(windowID, jade.TitlebarStyleOptions{
    Height: 42, BackgroundColor: "#202020", ForegroundColor: "#f5f5f5",
    ButtonHoverBackgroundColor: "#ffffff20",
})`,
    overlay: `// BarOverlay 保留原生边框，页面可用 jade-region-drag 声明拖拽区。
jade.SetWindowFrame(windowID, jade.TitleBarStyle.Overlay)
jade.SetTitlebarStyle(windowID, jade.TitlebarStyleOptions{Height: 40})`,
    theme: `jade.SetTheme(windowID, jade.Theme.Dark)
jade.SetBackdrop(windowID, jade.Backdrop.MicaAlt)`,
    layout: `settings := jade.DefaultWebViewSettings()
settings.ProfileName = "work-left"
viewID := jade.AddWebView(windowID, localPaneURL, &settings, &jade.WebViewBounds{
    X: "12px", Y: "48px", Width: "50%-20px", Height: "100%-60px",
})
request := jade.SetWebViewBounds(viewID, &jade.WebViewBounds{
    X: "12px", Y: "48px", Width: "60%-20px", Height: "100%-60px",
})
// 用 GetRequestResult 查询 request；完成后用 ReleaseRequest 释放。`,
    ipc: `jade.RegisterIPCHandler("playground", func(viewID uint32, body string) string {
    // 校验 viewID 与页面地址后，再分发 JSON action。
    return handlePlaygroundRequest(viewID, body)
})
jade.SendIPCMessage(viewID, "playground.message", \`{"text":"Go"}\`)`,
    splash: `opts := jade.DefaultWindowOptions()
opts.TitleBarStyle = jade.TitleBarStyle.Default // 使用原生标题栏，动画中也能拖拽
opts.Splash = &jade.SplashOptions{LogoPath: "assets/playground.png", MinimumMS: 1400}
windowID := jade.CreateWindow(localExampleURL, 0, &opts, nil)`,
    permissions: `jade.SetWebviewPermissionDecisionHandler(func(viewID uint32, data string) jade.PermissionDecision {
    // 此回调同步执行；示例策略只应用于受信任的视图和来源。
    if viewID == trustedViewID {
        return jade.PermissionDeny
    }
    return jade.PermissionDefault
})`,
    resources: `url, ok := jade.RegisterResource(filePath, viewID, ^uint32(0))
if ok {
    delivery, code := jade.GetResourceDeliveryInfo(url, viewID)
    // 使用 delivery.Mode 和 code 显示交付模式与诊断。
    _ = delivery
    _ = code
    // 页面停止使用资源后，再调用 jade.UnregisterResource(url)。
}`,
    clipboard: `text, ok := jade.ClipboardReadText()
if ok {
    jade.ClipboardWriteText(text + " · Go")
}`,
  },
};

export default goHostModule;
