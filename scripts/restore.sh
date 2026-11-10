#!/usr/bin/env bash
# 从 backup.sh 产出的目录恢复控制面数据库与 Worker 会话数据。
#
#   ./scripts/restore.sh backups/20260101-120000 [-y]
#
# 环境变量：
#   COMPOSE_FILE   默认 docker-compose.yml
#   WORKER_VOLUME  直接指定 worker 的 /data 卷（worker 容器不存在时用）
set -euo pipefail

cd "$(dirname "$0")/.."

src="${1:-}"
if [ -z "$src" ] || [ ! -d "$src" ]; then
  echo "用法: $0 <备份目录> [-y]" >&2
  exit 1
fi
src="$(cd "$src" && pwd)"

compose_file="${COMPOSE_FILE:-docker-compose.yml}"
assume_yes="${2:-}"
compose() { docker compose -f "$compose_file" "$@"; }

if [ ! -f "$src/postgres.dump" ]; then
  echo "缺少 $src/postgres.dump，拒绝恢复" >&2
  exit 1
fi

if [ "$assume_yes" != "-y" ] && [ "${ASSUME_YES:-0}" != "1" ]; then
  echo "即将用 $src 覆盖当前数据库与 worker 数据。"
  read -r -p "输入 yes 继续: " answer
  [ "$answer" = "yes" ] || { echo "已取消"; exit 1; }
fi

worker="$(compose ps -q sounder-worker 2>/dev/null || true)"
volume="${WORKER_VOLUME:-}"
if [ -z "$volume" ] && [ -n "$worker" ]; then
  volume="$(docker inspect -f '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Name}}{{end}}{{end}}' "$worker")"
fi

echo "==> 停止控制面与 worker（避免写入覆盖中的数据）"
compose stop sounder-server sounder-worker >/dev/null

echo "==> 恢复 PostgreSQL"
compose exec -T sounder-postgres \
  pg_restore -U "${POSTGRES_USER:-sounder}" -d "${POSTGRES_DB:-sounder}" \
  --clean --if-exists --no-owner --no-privileges \
  <"$src/postgres.dump"
echo "ok postgres"

if [ -f "$src/worker-data.tar.gz" ] && [ -n "$volume" ]; then
  echo "==> 恢复 Worker 数据卷 $volume"
  docker run --rm \
    -v "$volume":/data \
    -v "$src":/backup:ro \
    alpine:3.20 sh -c 'rm -rf /data/* /data/.[!.]* /data/..?* 2>/dev/null || true; tar xzf /backup/worker-data.tar.gz --numeric-owner -C /data'
  echo "ok worker-data"
elif [ -f "$src/worker-data.tar.gz" ]; then
  echo "warn: 备份里含 worker-data，但找不到 worker 卷；设置 WORKER_VOLUME 后重跑" >&2
fi

echo "==> 启动服务"
compose start sounder-server sounder-worker >/dev/null
echo "完成。检查 /healthz 与 /v1/meta 的 version 是否与备份时一致。"
