# Sounder Desktop

Wails v3 壳。**UI 与 Worker 分离**：壳只负责启停 Worker、把 endpoint 注入页面，Agent 逻辑全在 Worker。

```
Wails 进程
 ├─ 启停 Worker（容器 / 进程）
 ├─ 打开窗口：/?api=<base>&token=<token>
 └─ 前端复用 sounder-web/index.html，通过 query 拿地址
```

## 运行

```bash
# process 模式（需要本机 worker 二进制）
go run . -mode=process -worker=/path/to/sounder-worker -model=minimax-cn/MiniMax-M3

# container 模式（默认；需要本地有镜像）
docker build -t ghcr.io/getsounder/sounder-worker:dev ../sounder-worker
go run . -mode=container

# 接入控制面：本机设备登记到服务端，Web/移动端也能看到并切换过来
go run . -mode=container \
  -server http://localhost:8000 \
  -gateway ws://127.0.0.1:19090/v1/tunnel -gateway-token dev-token \
  -token <用户访问 token>
```

## 设备

一个 Worker 运行环境就是一个设备。桌面端把设备列表存在
`~/Library/Application Support/Sounder/devices.json`，每台设备对应一个独立容器和独立
`/data`（会话历史互不干扰）：

- 界面顶栏设备 chip 可以「新建设备」（再拉一个容器）、切换、改名、删除
- 切换设备 = 让对应容器启动并重载到它的 endpoint（URL 带 `device=<id>`）
- 首台设备复用旧版的单容器数据目录，升级不会丢会话
- 只传 `-server` 时，窗口直接连服务端，首屏是登录/注册；登录成功后界面会把本机设备
  自动登记到控制面（`publishLocalDevices`）。调试时可用 `-token` 预置登录态，省掉交互。
- 传了 `-server` + `-token` 时，设备启动后会：
  1. 以 `-gateway` 连上网关（留空则从 `/v1/meta.gatewayUrl` 读取；容器内自动把
     127.0.0.1 换成 host.docker.internal）
  2. 用 `worker-id=<设备 id>`、`worker-kind=desktop` 注册隧道
  3. 调 `POST /v1/devices` 登记，云端/移动端据此看到这台设备及其在线状态

## 结构

```
main.go              启动 Worker → 拼 URL → 开窗
shell/shell.go       StartWorker / StopWorker / WorkerEndpoint
  ├─ ContainerRunner docker run -d -p 127.0.0.1::8080
  └─ ProcessRunner   exec worker，解析 READY 行
shell/devices.go     多设备：devices.json + 每设备容器/数据目录 + 控制面登记
cmd/shellcheck/      不开 GUI 验证 Worker 生命周期
frontend/            Vite + 复用 sounder-web/index.html
```

## 已验证

- Desktop `go build` 通过（19MB）
- process 模式：启动 → 解析 READY → healthz → 优雅停止
- 前端 `vite build` 通过
- **Android APK 构建成功**：`bin/sounder.apk`（约 19MB，arm64-v8a）
- **Android 账号流程（模拟器）**：邮箱注册 → token 持久化 → 重启后直接进入会话列表。
- **Android 真机流程（模拟器 Pixel_3a_API_34）**：安装 → 首屏 → 配置 API → 会话列表 →
  历史 → 发送 prompt → 收到 SSE 事件
- `shell` 包交叉编译 android/ios arm64 通过

> Wails 库需 ≥ **v3.1.0**：alpha.78 的 android 分支编译不过（`events.Android` / `runtime.Core` 签名不匹配）。

## Mobile（同一份 Wails 工程）

`main.go` 用 `runtime.GOOS` 区分：iOS / Android **不启动本地 Worker**（iOS 禁止 fork 子进程，
Android 也不适合常驻），只作为控制端连云端。

### 构建

```bash
# 前置：JDK 17 + Android SDK + NDK r26d（26.1.10909125）
export ANDROID_HOME="$HOME/Library/Android/sdk"
export ANDROID_NDK_HOME="$ANDROID_HOME/ndk/26.1.10909125"
export JAVA_HOME=/Library/Java/JavaVirtualMachines/jdk-17.jdk/Contents/Home
chmod +x build/android/gradlew

# 1. 编译 Go 侧（c-shared）+ 生成 overlay / bindings / 前端
wails3 task android:build PRODUCTION=true
# 2. 打包 APK
cd build/android && ./gradlew assembleDebug
cp app/build/outputs/apk/debug/app-debug.apk ../../bin/sounder.apk
```

`wails3 android overlay:gen` 会生成 `build/android/overlay.json`，把
`main_android.gen.go`（`init()` 里调用 `application.RegisterAndroidMain(main)`）注入
`main` 包。**必须**在 `go build -buildmode=c-shared` 时加 `-overlay build/android/overlay.json`，
否则 JNI 侧报 `No main function registered` / 白屏。Taskfile 已内置该步骤。

### 连接云端

Android 上 Wails 固定加载 `https://wails.localhost/`，**窗口 URL 的 query 不会生效**，
因此 `-api` / `-token` 在移动端不可用。取而代之：

1. 打开 App，顶部会显示「API 地址」输入栏（`mode=mobile` 或 Wails 壳无 `api` query 时自动出现）；
2. 填云端地址（模拟器访问宿主机用 `http://10.0.2.2:8000`）；有账号可直接登录，
   或在「使用访问 token」里填兼容 token；
3. 点「连接」。

地址与 token 存在 `localStorage`，下次启动自动带上。调试构建
（`MIXED_CONTENT_ALWAYS_ALLOW`）允许 `http://` 方便本地联调，release 仍强制 HTTPS。

> 需要在 `sounder-server` 侧允许 CORS：白名单 Web Origin 回显并允许 Cookie，
> 其他 Origin 返回 `*`；原生端使用 Bearer，不依赖 Cookie。

### 安装到模拟器

```bash
adb install -r bin/sounder.apk
adb shell am start -n com.wails.app/.MainActivity
```

### iOS

需要完整 Xcode（不是 Command Line Tools）：

```bash
wails3 task ios:build
wails3 ios xcode:gen
```

## 未验证 / 已知限制

- **桌面 GUI 未启动**：当前无图形会话，窗口渲染未验证；Android GUI 已在模拟器验证。
- container 模式已随 `docker compose` 验证（worker 容器能连 Gateway 并被调度）。
- `PickDirectory` / Keychain 还没接（Wails v3 的 dialog / keyring API 需要单独确认）。
- 自动更新未配置（Wails v3 有内置 updater，需要签名密钥）。
- Wails 版本是 **v3.1.0**，仍是 v3 预发布线；升级时注意 API 变化。
- 真机验证依赖真实模型 key；`.env` 里是占位 key 时，prompt 会返回 `401`，但会话、事件流与 UI 流程仍然可用。
