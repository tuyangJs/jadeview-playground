# Go 宿主

复用 Playground 的 React 前端和 `playground` IPC 协议，不需要 Python 运行宿主。使用 [JadeViewGo v3.0.0-beta.4](https://github.com/luoxueyousheng/JadeViewGo/tree/v3.0.0-beta.4) 与 JadeView `3.0.0-beta.4.26K01`（ABI `0x00030003`）。

## 开发模式

要求 Go 1.23+、Node.js 22+。Windows 需要 WebView2 Runtime；Linux x64 需要下文列出的运行库。以下命令从 Playground 根目录执行，不需要单独克隆 Go SDK，也不需要 Go workspace。

```sh
npm ci
go -C hosts/go run . --dev
```

如果 Go 模块代理尚未同步 SDK 标签，可临时设置 `GOPROXY=direct` 后重试；仓库的 `go.sum` 仍会校验依赖。

开发模式默认使用 `http://127.0.0.1:5173`：已有 Vite 则复用，否则启动仓库里的 Vite；退出时只关闭本宿主启动的服务。用 `--dev http://127.0.0.1:5174` 可以指定本机端口。

## 构建与启动

```powershell
npm run build
go -C hosts/go build -o ../../runtime/playground-go.exe .
.\runtime\playground-go.exe
```

默认走 JadeView 自定义协议并加载 `web/dist`。用 `--frontend <路径>` 指定前端目录或上游支持的 `.asar/.japk`（JAPK 仍须通过运行库签名校验）。`--example layout` 可直接打开布局示例。`--data <路径>` 指定状态目录，默认 `.data/go`。从仓库外启动时加 `--root <仓库根目录>`。

Windows 默认使用 Go SDK 内嵌的 DLL；要覆盖它，将同 ABI、同架构的 `JadeView.dll` 放进 `runtime/`，用上面的方式构建和启动 exe。直接 `go run` 使用 Go 的临时 exe 路径，因此不使用 `runtime/` 中的覆盖 DLL。

### Linux x64：无 cgo 构建

只需 Go 1.23+ 即可编译宿主，并可在 Windows 上交叉编译。共享前端仍需预先执行 `npm run build`，或直接使用已有的 `web/dist`。

```powershell
$env:GOOS='linux'; $env:GOARCH='amd64'; $env:CGO_ENABLED='0'
go -C hosts/go build -trimpath -o ../../runtime/playground-go-linux-amd64 .
```

在 Linux 原生 shell 中用 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go -C hosts/go build -trimpath -o ../../runtime/playground-go-linux-amd64 .`。不需要为跨编译安装 GCC。

将仓库的 `web/dist`、`assets` 与 `runtime/` 保持相对位置。在 Linux 将 [Go SDK 对应标签](https://github.com/luoxueyousheng/JadeViewGo/tree/v3.0.0-beta.4/lib/linux_amd64) 的 `libJadeView.so` 放到仓库的 `runtime/`，开发模式和构建产物都能找到它。运行已构建产物：

```sh
chmod +x runtime/playground-go-linux-amd64
./runtime/playground-go-linux-amd64 --root "$PWD"
```

也可使用 `JADEVIEW_LIBRARY=/绝对路径/libJadeView.so` 指定库。目标机仍需要兼容的 glibc、GTK 3、完整的 WebKitGTK 4.1 运行包及 GIO TLS 后端（Ubuntu 的 `glib-networking`）；无需 GCC 或开发头文件。上游没有提供 Linux arm64 的 v3 SO，本次仅适配 amd64。

## 示例与验证

已接入窗口/标题栏、空窗口挂载、原生多 WebView 布局、IPC 与事件日志、原生对话框（由 `jade.dialog` 直接提供）、菜单、托盘、剪贴板、YAML 持久化与运行时信息。网页弹窗、启动画面、三态权限及文件资源交付示例在 Go 宿主中显示未接入原因。Linux 标题栏和托盘依赖平台后端及桌面环境。

```powershell
go -C hosts/go test ./...
go -C hosts/go vet ./...
$testData = Join-Path $env:TEMP ('playground-go-smoke-' + [guid]::NewGuid())
.\runtime\playground-go.exe --smoke --data $testData
```

`--smoke` 使用真实隐藏 WebView 调用前端 IPC，验证握手、回显、原生布局、脚本结果、空窗口挂载与存储后退出。加 `--dev` 可验证 Vite 模式。
