# Sounder

[English](README.md) | **中文**

基于 [PiG](https://pi-in-go.dev)（Pi 的 Go 实现）构建的 Agent 云平台。

Sounder 把控制面和执行面分开：客户端不跑 Agent，所有工具调用都发生在 Worker 里。
Worker 可以放在容器、Kubernetes Pod，或者你自己的笔记本上。

```
Web / 桌面 / Android
        │  HTTP + SSE
        ▼
   sounder-server          会话编排、鉴权、事件转发
        │  RPC
        ▼
  sounder-gateway          反向隧道 + Worker 注册表 + SSE 扇出
        │  WebSocket
        ▼
  sounder-worker           Go + PiG SDK，每个会话一个 Runtime
```

## 为什么这样切

- **Worker 可以随便扔。** 会话在 `kill -9` 之后还能继续，因为 PiG 把对话写成 append-only
  的 JSONL；Worker 启动时 `Manager.Restore` 会把它重新打开。
- **桌面 Worker 在 NAT 后面。** 它主动连 Gateway，不需要开放任何入站端口。
- **UI 只是瘦客户端。** Web、桌面、Android 共用同一份 HTML，唯一差别是 API 地址从哪来。

## 目录与产物

| 目录 | 产物 | 职责 |
|---|---|---|
| `sounder-worker/` | `sounder-worker` | 跑 PiG 会话、工具、策略与审批 |
| `sounder-gateway/` | `sounder-gateway` | Worker 隧道、注册表、事件扇出 |
| `sounder-server/` | `sounder-server` | 会话生命周期、PG 持久化、事件代理 |
| `sounder-web/` | nginx 静态站点 | Web 客户端 |
| `sounder-desktop/` | `sounder-desktop`、`sounder.apk` | Wails v3 桌面壳与 Android 壳 |
| `sounder-verify/` | `v2-isolation`、`v3-recovery` | 验证架构假设的工具 |

## 三种部署方式

| 方式 | 入口 | 说明 |
|---|---|---|
| 开发 | `docker-compose.yml` | 从源码构建镜像 |
| 生产（Compose） | `docker-compose.prod.yml` | 拉 registry 镜像，缺凭证直接报错，带资源限制 |
| Kubernetes | `deploy/k8s/` | Kustomize；worker 用 StatefulSet，含 NetworkPolicy 与 HPA |

## 快速开始

```bash
cp .env.example .env
# 在 .env 里填模型 key，例如：
#   MINIMAX_CN_API_KEY=...
make up
open http://localhost:3000
```

`make up` 会启动八个服务：`sounder-web`、`sounder-server`、`sounder-gateway`、
`sounder-worker`、`postgres`、`redis`、`minio`、`minio-init`。

`.env` 里的 `SOUNDER_TOKEN` 同时用于网页登录和 Gateway 鉴权。

## 从源码运行

```bash
# 终端 1
cd sounder-gateway && go run ./cmd/sounder-gateway -addr=127.0.0.1:9090 -token=dev

# 终端 2
cd sounder-worker && go run ./cmd/sounder-worker \
  -addr=127.0.0.1:8080 -data=/tmp/sounder -model=minimax-cn/MiniMax-M3 \
  -gateway=ws://127.0.0.1:9090/v1/tunnel -gateway-token=dev -worker-id=w1

# 终端 3
cd sounder-server && go run ./cmd/sounder-server \
  -addr=127.0.0.1:8000 -gateway=http://127.0.0.1:9090 \
  -gateway-token=dev -token=dev
```

建会话并发一条指令：

```bash
TOKEN=dev
SID=$(curl -s -X POST localhost:8000/v1/sessions \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"model":"minimax-cn/MiniMax-M3"}' | jq -r .id)

curl -s -X POST localhost:8000/v1/sessions/$SID/prompts \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"message":"用 bash 执行 echo hello"}'

curl -N localhost:8000/v1/sessions/$SID/events -H "Authorization: Bearer $TOKEN"
```

工具需要审批时，事件流里会出现 `approval.requested`：

```bash
curl -s -X POST "localhost:8000/v1/approvals/<id>?workerId=w1" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"allow":true}'
```

## 本地端到端验证

不需要云账号也能把整条链路跑通：

```bash
docker compose up -d --build          # 8 个服务
curl localhost:8000/healthz           # {"status":"ok"}
curl -H "Authorization: Bearer dev-token" localhost:8000/v1/workers
./scripts/e2e-auth.sh                 # 用户体系 e2e：74 项断言
```

认证 e2e 脚本覆盖本地注册/登录、Cookie + Bearer 会话、会话归属隔离、admin account
管理员可见性、会话吊销、改密/重置密码、身份解绑、CORS、OAuth 未配置行为、请求体上限
和注册限流。脚本会在本地库创建随机邮箱的临时账号，并在结束时删除自己创建的会话。

已在本机 compose 栈（PostgreSQL + Redis + MinIO + gateway + server + worker + web）上验证：

- 建会话 → 发 prompt → SSE（`message.*`、`turn.end`、`error`）全链路通；
- Worker 容器能写 `/data`（Dockerfile 里已把具名卷 chown 给 `pig`）；
- `OPTIONS` 预检返回 `204`；白名单 Web Origin 回显并允许 Cookie，其他 Origin 返回 `*`；
- Android APK（`sounder-desktop/bin/sounder.apk`）在模拟器里连 `http://10.0.2.2:8000`，
  能列会话、看历史、收事件流。

真实模型输出需要在 `.env` 里填真 key（`MINIMAX_CN_API_KEY`、`OPENAI_API_KEY` 或
`ANTHROPIC_API_KEY`）。用占位 key 时本轮会以 provider `401` 结束，这是预期行为；
上面的控制面链路仍然可用。

## 已验证的能力

| 能力 | 状态 |
|---|---|
| 真实模型流式输出（MiniMax M3） | ✅ |
| 工具执行 + 人工审批 | ✅ |
| `kill -9` 后会话恢复 | ✅ |
| 控制面 PostgreSQL 持久化 | ✅ |
| Gateway 隧道端到端 | ✅ |
| Android APK 构建 + 模拟器会话流程 | ✅ |
| 本地认证 e2e（`scripts/e2e-auth.sh`，74 项断言） | ✅ |
| docker-compose 全栈（8 服务） | ✅ |
| CI（build/vet/test + 空库迁移） | ✅ |
| 版本化迁移 + 备份恢复演练 | ✅ |
| 桌面 GUI（图形会话） | ⚠️ 存在自行退出问题，排查中 |
| iOS 构建 | ⚠️ 需要完整 Xcode |
| Kubernetes manifests（kustomize） | ✅ |

## 策略与安全

PiG 不对模型输出做沙箱。Sounder 在每个工具调用前加了一层策略：

- `read` / `grep` / `ls` 自由执行。
- `write` / `edit` 会校验路径必须落在 workspace 内。
- `bash` 和对外访问类工具会阻塞等待人工审批；拒绝会作为 error tool result 回给模型。
- 十条高危命令模式（`curl | sh`、写 `.git/hooks`、`rm -rf /` 等）无论会话状态如何都必须审批。

## 发布与备份

```bash
make check-js        # 内联前端语法 + web/desktop 源文件一致性
make test            # server / worker / gateway
./scripts/backup.sh  # PostgreSQL + worker /data
./scripts/restore.sh backups/<时间戳> -y
```

打 `v*` tag 会触发 `.github/workflows/release.yml`，把 `VERSION`/`COMMIT` 注入三个镜像并推送到
GHCR；`/v1/meta` 会返回版本号与当前存储实现。

## 文档

- [架构与设计说明](pig-agent-cloud-architecture.md)
- [用户体系调研：自建账号 + Google/GitHub 登录](docs/user-system.md)
- [备份与恢复](docs/backup.md)
- [Worker](sounder-worker/README.md)
- [Gateway](sounder-gateway/README.md)
- [控制面](sounder-server/README.md)
- [Web](sounder-web/README.md)
- [桌面 / Android](sounder-desktop/README.md)

## 状态

早期项目。核心链路已经端到端跑通，但还没到 1.0，PiG 本身也仍是 0.x。
升级时请锁定 `go.sum` 里的 PiG commit，并预期有破坏性变更。

License: [MIT](LICENSE).
