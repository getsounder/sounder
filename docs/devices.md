# 设备模型：Computer / Sandbox

> 一个 Worker 运行环境 = 一个设备。桌面容器、桌面进程、云端容器都是设备。

## 1. 概念

| 概念 | 说明 |
| --- | --- |
| 设备（device） | 用户绑定的一台 Worker 运行环境，`user_id + worker_id` 唯一 |
| 形态（form） | `computer` 或 `sandbox`，决定关机/开机时怎么持久化 |
| 规格 | 来自用户配额：云端设备数、vCPU、内存、磁盘 |
| 凭据 | 设备密钥（长期）+ 网关令牌（1 小时，自动续期） |

设备与 Worker 通过网关反向隧道连接：Worker 连 `ws://gateway/v1/tunnel`，
服务端只认已登记且在线（网关注册表里有）的设备。

## 2. 两种形态

### Computer
- 创建：容器不挂数据卷，状态留在容器文件系统里
- 关机：`docker commit` 成镜像 `sounder-device-<id>:latest`，然后 `docker stop`（容器保留）
- 开机：`docker start` 原容器，**容器 ID 不变**，状态原地恢复
- 容器丢了：用已提交的镜像重建

### Sandbox
- 创建：挂载命名卷 `sounder-dev-<id>:/data`
- 关机：删除容器（卷保留）
- 开机：新建容器（**新容器 ID**），挂回同一个卷

K8s provisioner 只支持 Sandbox（集群里没有"提交容器成镜像"的能力），
Computer 会返回明确错误。

## 3. 生命周期接口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/v1/devices` | 设备列表 + 在线状态 + 占用 + 配额 |
| POST | `/v1/devices` | `{kind, form, workerId?, name?}`；云端设备走 provisioner |
| POST | `/v1/devices/{id}/start` | 开机（仅云端设备） |
| POST | `/v1/devices/{id}/stop` | 关机（仅云端设备） |
| PATCH | `/v1/devices/{id}` | 改名 |
| DELETE | `/v1/devices/{id}` | 移除（云端设备连同容器/卷/镜像一起回收） |
| GET | `/v1/quota` | 配额 + 用量（管理员可 `?email=` 查他人） |
| PUT | `/v1/quota` | 管理员调整配额 |

桌面端设备由桌面 App 自己管理（`shell/devices.go`），服务端只负责登记与路由。

## 4. Provisioner

`-provisioner=none|docker|k8s`

- **none**：云端设备退化为"认领网关里的空闲 Worker"
- **docker**：通过 Docker Engine API（unix socket）创建容器
  - `-docker-host unix:///var/run/docker.sock`
  - 配额 → `NanoCpus` / `Memory`
- **k8s**：REST API + ServiceAccount 创建 PVC + Pod
  - 集群内读 `/var/run/secrets/kubernetes.io/serviceaccount`
  - 集群外读 `KUBECONFIG`（支持 token 与客户端证书）
  - `-k8s-namespace`、`-k8s-storage-class`、`-k8s-service-account`

Worker 镜像必须能被 provisioner 创建的环境直接使用（Docker 本地镜像 / 集群可拉取的镜像）。

## 5. 凭据与续期

```
device key（90 天）  --POST /v1/worker-tokens/refresh-->  gateway token（1 小时）  -->  ws 隧道
```

- 设备密钥 `use=device-key` 只能用于换令牌，网关拒绝用它直连隧道
- Worker 启动即换一次，之后每 20 分钟续期；隧道重连时取最新令牌
- 容器/ Pod 里不再放长期凭据

## 6. 已实测

- Docker：Computer（镜像 236MB、容器 ID 不变）与 Sandbox（删容器保留卷、开机新容器 ID）双向验证
- OrbStack K8s：PVC 绑定 20Gi、Pod 运行、Worker 换到短期令牌并连上网关、经服务端在 Pod 上建会话成功；
  关机删 Pod（PVC 保留）、开机重建 Pod、删除设备清理 Pod + PVC

## 7. 运维与可观测

- `GET /metrics`（Prometheus 文本）：请求计数/耗时、会话按状态、设备在线/离线、网关 Worker 数
- `POST /v1/devices/prune`：清理「离线且没有会话」的设备；管理端设置面板也有「清理离线设备」
- 服务端后台巡检（每进程启动即跑）：
  - 每分钟重试「worker 离线时没删掉」的会话（状态 `deleting`）
  - 每小时清理 last_seen 超过 7 天且没有会话的设备
- 审计：登录/注册/改密、管理员对用户的操作、设备创建/改名/启停/删除、会话删除/改名
- 客户端事件流断线会自动重连（指数退避，按 `after=lastSeq` 续传），顶栏状态胶囊可点击立即重连
- 审批超时（默认 5 分钟）后，聊天里的卡片会被置灰标记「已超时或已在别处处理」
