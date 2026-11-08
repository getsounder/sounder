# Sounder Web（M0）

零依赖单页：新建会话、发 prompt、SSE 事件流、审批。

## 运行

```bash
make up
open http://localhost:3000
```

顶部输入 `SOUNDER_TOKEN`（与 `.env` 一致），点「新建会话」即可。

## 通过 nginx 反代的原因

- 同源，避免 CORS；
- SSE 需要 `proxy_buffering off`，生产环境同样要；
- `EventSource` 不能带 Authorization，所以这里用 `fetch` + `ReadableStream` 手动解析 SSE。

## 已知限制

- `message.delta` 的文本用反射从 `Message/Partial` 里提取；PiG 的 `Partial` 是当前完整内容而非增量，前端按前缀判断替换或追加。
- 刷新后会先拉 `/v1/sessions/{id}/history`（Session JSONL 投影）回填，再开实时流；两者可能重叠少量最近消息。
- 没有登录页，token 存在 localStorage。
