# Sounder 本地一键部署

## 启动

```bash
cp .env.example .env      # 填模型 key（可选）
make up
```

首次会构建两个镜像（worker / gateway），之后是增量构建。

## 服务与端口

| 服务 | 端口 | 用途 |
|---|---|---|
| `sounder-gateway` | 9090 | Worker 隧道、事件扇出 |
| `sounder-worker` | 8080 | Agent 执行（HTTP + SSE） |
| `sounder-postgres` | 5432 | 控制面会话元数据（server 默认写入） |
| `sounder-redis` | 6379 | 路由表 / 队列（预留） |
| `sounder-minio` | 9000 / 9001 | 对象存储（控制台 9001） |

## 验证

```bash
# worker 直连
curl -s localhost:8080/healthz
curl -s localhost:8080/v1/meta

# gateway 是否看到 worker
curl -s localhost:9090/v1/workers -H "Authorization: Bearer $(grep SOUNDER_TOKEN .env | cut -d= -f2)"

# 通过隧道建会话
curl -s -X POST localhost:9090/v1/workers/worker-local-1/rpc \
  -H "Authorization: Bearer $(grep SOUNDER_TOKEN .env | cut -d= -f2)" \
  -H 'Content-Type: application/json' \
  -d '{"method":"POST","path":"/v1/sessions","body":{"model":"openai/gpt-5"}}'
```

## 本地调试

改代码后：

```bash
make up          # 只重建有变更的服务
make logs        # 跟日志
make sh-worker   # 进 worker 容器
```

如果只想重启进程不重建镜像：

```bash
docker compose restart worker
```

## 模型凭证

- 不填 key 也能启动，但 `prompt` 会返回 provider 错误。
- MiniMax 已验证可用：`MODEL=minimax-cn/MiniMax-M3`，凭证放 `MINIMAX_CN_API_KEY`。
- 需要本地模型时，把 `MODEL` 指向本地 provider，并在 `.env` 里配 `HTTPS_PROXY`（如需要）。

## 常见问题

| 现象 | 原因 | 处理 |
|---|---|---|
| 构建卡在 `go mod download` | 无法访问 proxy.golang.org | 设 `GOPROXY=https://goproxy.cn,direct` |
| worker 一直 unhealthy | 端口被占 / 模型初始化失败 | `docker compose logs worker` |
| gateway 看不到 worker | token 不一致 | 检查 `SOUNDER_TOKEN` |
| 权限问题写不了 workspace | 容器 uid 10001 | `sudo chown -R 10001 workspace` |
| 想彻底清空 | 卷残留 | `make reset` |
