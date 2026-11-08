# Sounder Worker

唯一执行层。桌面（本地容器 / 裸进程）与云端（K8s Pod）运行同一份二进制。
内部用 PiG Go SDK，**每会话一个 `coding.Runtime`**。

> 代码未在本机编译（项目规则禁止构建）。`API-CHECK` 注释处需按 pin 的 PiG commit 校准签名。

## 结构

```
cmd/sounder-worker/        启动器：flags、监听、信号、优雅终止
internal/protocol/     控制端与 Worker 的唯一契约（Envelope / 请求体）
internal/session/      Actor（PiG 生命周期）与 Manager（多会话 + 配额）
internal/api/          HTTP + SSE + 多端订阅
Dockerfile             与云端同 digest
```

## 运行

```bash
go mod tidy
go run ./cmd/sounder-worker -addr=127.0.0.1:8080 -data=/tmp/sounder-data -model=openai/gpt-5 -max=4

# 需要被远程控制时，连上 Gateway（桌面 Worker 在 NAT 后面走这条）
go run ./cmd/sounder-worker -addr=127.0.0.1:8080 -data=/tmp/sounder-data \
  -model=openai/gpt-5 -gateway=ws://127.0.0.1:9090/v1/tunnel -worker-kind=desktop
```

启动后输出一行，供桌面启动器解析：

```text
READY {"addr":"127.0.0.1:8080","token":"...","capacity":4}
```

## API

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/v1/meta` | 协议版本、PiG 版本、feature 列表 |
| POST | `/v1/sessions` | 创建会话 |
| GET | `/v1/sessions` | 列表 + 容量 |
| DELETE | `/v1/sessions/{id}` | 销毁会话 |
| POST | `/v1/sessions/{id}/prompts` | 发送 prompt，立即返回 `started` |
| POST | `/v1/sessions/{id}/cancel` | 取消当前 turn |
| GET | `/v1/sessions/{id}/events?after=N` | SSE，多端可同时订阅 |
| GET | `/v1/sessions/{id}/history?limit=200` | 从 Session JSONL 投影历史消息 |
| GET | `/v1/approvals` | 待审批列表 |
| POST | `/v1/approvals/{id}` | 批准 / 拒绝 / 改参数 |
| GET | `/healthz` `/readyz` | 存活 / 就绪（满员时 503） |

## 已验证的模型

- `minimax-cn/MiniMax-M3`（`MINIMAX_CN_API_KEY`）：文本流、历史、settled 均正常。

## 已实现的设计要点

- **每会话四件套独立**：`CWD` / `AgentDir` / `SettingsManager` / `SessionManager` 由 Manager 按 `sessions/<id>/` 分配。
- **事件归一化**：PiG 的 `AgentEvent` 不透给前端，统一成 `Envelope`；未知类型以 `raw.<type>` 保留。
- **多端订阅**：`Actor.Subscribe(after)` 每人一条 channel + 内存历史补发，慢消费者丢弃不阻塞 Agent。
- **立即返回**：`prompts` 返回 202 + `started`，结果走 SSE，与 PiG RPC 语义一致。
- **优雅终止**：SIGTERM → 停止接新请求 → `CloseAll()`（session → runtime → services）。
- **固定 `PIG_HOME`**：绝不读宿主 `~/.pig`。

## 尚未实现（刻意留空）

- **审批**：通过 PiG 官方 `SessionStartOptions.BeforeToolCall` 钩子挂载。L2（`bash`）等工具执行前会阻塞等待人工确认，拒绝时以 error tool result 回给模型。
- **工具白名单**：当前用 PiG 默认工具集，按级别审批但不裁剪。
- **重放回源**：`after` 目前只回放内存历史（2048 条），更早的需要读 Session JSONL。
- **持久化恢复**：已实现。启动时扫描 `sessions/*/agent`，用 `ResumePath` 重新打开；坏会话跳过不影响其他。
- **认证**：只有静态 Bearer token，没有 mTLS / 设备证书。
