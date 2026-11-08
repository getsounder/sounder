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
```

## 结构

```
main.go              启动 Worker → 拼 URL → 开窗
shell/shell.go       StartWorker / StopWorker / WorkerEndpoint
  ├─ ContainerRunner docker run -d -p 127.0.0.1::8080
  └─ ProcessRunner   exec worker，解析 READY 行
cmd/shellcheck/      不开 GUI 验证 Worker 生命周期
frontend/            Vite + 复用 sounder-web/index.html
```

## 已验证

- Desktop `go build` 通过（19MB）
- process 模式：启动 → 解析 READY → healthz → 优雅停止
- 前端 `vite build` 通过
- **Android APK 构建成功**：`bin/sounder.apk`（12MB，arm64-v8a）
- `shell` 包交叉编译 android/ios arm64 通过

> Wails 库需 ≥ **v3.1.0**：alpha.78 的 android 分支编译不过（`events.Android` / `runtime.Core` 签名不匹配）。

## Mobile（同一份 Wails 工程）

`main.go` 用 `runtime.GOOS` 区分：iOS / Android **不启动本地 Worker**（iOS 禁止 fork 子进程，
Android 也不适合常驻），只作为控制端连云端。

```bash
# 前置：Android SDK + NDK r26d（26.1.10909125）
export ANDROID_HOME="$HOME/Library/Android/sdk"
export ANDROID_NDK_HOME="$ANDROID_HOME/ndk/26.1.10909125"
chmod +x build/android/gradlew

wails3 task android:build PRODUCTION=true     # 产出 14MB libwails.so
wails3 task android:assemble:apk              # 产出 bin/sounder.apk（12MB, arm64-v8a）

# iOS（需要完整 Xcode，不是 Command Line Tools）
wails3 task ios:build
wails3 ios xcode:gen
```

构建时传入云端地址：

```bash
-go="..."   # 或在 main 的 flag 默认值里写死
-api=https://sounder.example.com -token=<token>
```

## 未验证 / 已知限制

- **container 模式未实测**：`docker build` 在容器内拉 PiG 依赖超时（10min+）。网络好时可直接构建，或先本机 `GOOS=linux go build` 再 COPY 进镜像。
- **GUI 未启动**：当前无图形会话，窗口渲染未验证。
- `PickDirectory` / Keychain 还没接（Wails v3 的 dialog / keyring API 需要单独确认）。
- 自动更新未配置（Wails v3 有内置 updater，需要签名密钥）。
- Wails 版本是 **v3.0.0-alpha.78**，不是 stable；升级时注意 API 变化。
