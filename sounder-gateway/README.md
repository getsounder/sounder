# sounder-gateway

Worker 反向隧道的接入点。桌面 Worker 在 NAT 后面，公网控制端不能直连，
所以由 Worker 主动出站建立 WebSocket，Gateway 按 `workerId` 把 RPC 转发进去。

## 职责（只有四件）

1. 接受 Worker 的 `GET /v1/tunnel` WebSocket 连接并注册。
2. 心跳与超时剔除（默认 90s）。
3. `POST /v1/workers/{id}/rpc`：把 `{method,path,body}` 转发给 Worker，等结果。
4. `GET /v1/workers/{id}/events`：把 Worker 上行事件扇出给多个控制端（SSE）。

**不做业务解析**：Gateway 不认识 prompt、工具、审批，只转发字节。

## 运行

```bash
go mod tidy
go run ./cmd/sounder-gateway -addr=127.0.0.1:9090 -token=dev-token
```

## 隧道协议

首帧必须是 `register`，随后是心跳与双向帧：

```json
{"kind":"register","workerId":"dev_7f3a","workerKind":"desktop","capacity":4,"version":"0.1.0"}
{"kind":"heartbeat","used":2}
{"kind":"rpc","id":"a1","method":"POST","path":"/v1/sessions","body":{...}}
{"kind":"rpc_result","id":"a1","status":201,"body":{...}}
{"kind":"event","sessionId":"ses_1","event":{"seq":1,"type":"message.delta"}}
```

## 已知限制

- 注册表在进程内存里，**多实例部署需要换成 Redis**（见架构文档 6.10）。
- 没有 mTLS / 设备证书，只有静态 Bearer token。
- 事件 ring 只有 512 条，更早的要从 Worker 的 Session JSONL 回源。
- `CheckOrigin` 恒为 true，生产必须收紧。
