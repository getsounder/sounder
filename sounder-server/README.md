# sounder-server

控制面。只做三件事：**会话编排、鉴权、事件转发**，不执行 Agent。

```
控制端 ──► sounder-server ──► sounder-gateway ──隧道──► sounder-worker
```

## API

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/v1/meta` | 版本与存储类型 |
| GET | `/v1/workers` | 代理 Gateway 的 Worker 列表 |
| POST | `/v1/sessions` | 创建会话（自动选 Worker） |
| GET | `/v1/sessions` | 会话列表 |
| GET | `/v1/sessions/{id}` | 会话详情 |
| DELETE | `/v1/sessions/{id}` | 销毁会话 |
| POST | `/v1/sessions/{id}/prompts` | 发送 prompt |
| POST | `/v1/sessions/{id}/cancel` | 取消当前 turn |
| GET | `/v1/sessions/{id}/events` | SSE，按 sessionId 过滤 |
| GET | `/v1/sessions/{id}/history` | 代理 Worker 的 JSONL 历史投影 |
| GET | `/v1/approvals?workerId=` | 待审批列表 |
| POST | `/v1/approvals/{id}?workerId=` | 批准 / 拒绝 |

## 运行

```bash
go run ./cmd/sounder-server -addr=0.0.0.0:8000 \
  -gateway=http://127.0.0.1:9090 -token=dev-token
```

## 当前实现范围

- 存储：`-store=memory`（默认，重启丢失）或 `-store=postgres -dsn=...`（已实现，验证过 kill -9 后会话仍在）。
- 选 Worker：优先 `kind=cloud` 且 `used < capacity`。
- 事件：SSE 透传，收到 `session.settled` 时把会话状态改回 `idle`。
- 鉴权：默认启用用户体系（本地账号 + Google/GitHub），Web 用 Cookie，原生端用 Bearer；
  `-token` 仍兼容，作为管理员/服务 token。
- CORS：白名单 Origin（`SOUNDER_WEB_URL` / `SOUNDER_PUBLIC_URL`）回显并允许 Cookie；
  其他 Origin 返回 `*` 且不允许凭证。允许 `Authorization` / `Content-Type`，
  `GET/POST/DELETE/OPTIONS`。

## 典型调用

```bash
TOKEN=dev-token
SID=$(curl -s -X POST localhost:8000/v1/sessions \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"model":"openai/gpt-5"}' | jq -r .id)

curl -s -X POST localhost:8000/v1/sessions/$SID/prompts \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"message":"列出当前目录的文件"}'

curl -N localhost:8000/v1/sessions/$SID/events \
  -H "Authorization: Bearer $TOKEN"
```
