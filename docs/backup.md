# 备份与恢复

MVP 阶段需要保护两类数据：

| 数据 | 位置 | 丢失后果 |
|---|---|---|
| 用户 / 设备 / 会话元数据 / 审计 | PostgreSQL `sounder` | 账号、归属、会话列表丢失 |
| 会话内容（PiG JSONL、workspace） | Worker 的 `/data` 卷 | 会话历史无法回放 |

两者必须一起备份，单独恢复数据库会出现“有会话记录但没有内容”。

## 快速开始

```bash
# 备份到 backups/<时间戳>/
./scripts/backup.sh

# 备份到指定目录
./scripts/backup.sh /var/backups/sounder/2026-01-01

# 恢复（会二次确认）
./scripts/restore.sh /var/backups/sounder/2026-01-01
```

`backup.sh` 生成：

```
postgres.dump        pg_dump -Fc
worker-data.tar.gz   worker 的 /data 卷（没有 worker 时跳过）
manifest.txt         时间戳、sha256
```

恢复前脚本会 `stop sounder-server sounder-worker`，恢复后 `start`，避免写入覆盖中的文件。

## 定时执行

单机 Compose 可以用 cron：

```cron
# 每天 03:15 备份，保留 14 天
15 3 * * * cd /srv/sounder && ./scripts/backup.sh /var/backups/sounder/$(date +\%F) >> /var/log/sounder-backup.log 2>&1
30 3 * * * find /var/backups/sounder -maxdepth 1 -type d -mtime +14 -exec rm -rf {} +
```

备份目录不要放在仓库或容器卷里，最好同步到对象存储（`rclone` / `aws s3 sync`）。

Kubernetes：

```bash
POD=$(kubectl -n sounder get pod -l app=sounder-postgres -o jsonpath='{.items[0].metadata.name}')
kubectl -n sounder exec "$POD" -- pg_dump -U sounder -d sounder -Fc > postgres.dump
kubectl -n sounder exec sounder-worker-0 -- tar czf - -C /data . > worker-data.tar.gz
```

生产环境更推荐用存储层快照（PVC snapshot）或托管数据库的自动备份；脚本只作为最小可用方案。

## 恢复演练（发版前必须做一次）

1. `./scripts/backup.sh /tmp/sounder-restore-test`
2. `docker compose down`（保留卷）
3. `docker compose up -d sounder-postgres`
4. `./scripts/restore.sh /tmp/sounder-restore-test -y`
5. 验证：
   - `curl localhost:8000/healthz`
   - `curl localhost:8000/v1/meta` 能看到 `version` 与 `store: postgres`
   - 登录后能看到原有会话，打开一个会话能回放历史
   - `sounder_sessions` / `sounder_devices` 指标与备份前一致
6. 记录耗时，作为 RTO 基线。

## 版本与迁移

- 服务启动时自动执行 `sounder-server/internal/store/migrations/*.sql`，已应用版本记录在 `schema_migrations`。
- 恢复旧备份后启动新版本会自动补迁移；**不要用旧版本二进制恢复新 schema**。
- 迁移在 `pg_advisory_lock` 下串行执行，多副本同时启动也安全。
- 新增迁移文件只允许追加，不要修改已发布的文件；文件名用 `0003_xxx.sql` 递增。
