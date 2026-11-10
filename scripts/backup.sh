#!/usr/bin/env bash
# 备份控制面数据库与 Worker 会话数据。
#
#   ./scripts/backup.sh [输出目录]
#
# 环境变量：
#   COMPOSE_FILE        默认 docker-compose.yml
#   POSTGRES_DSN        设置后直接用本机 pg_dump，不走 compose
#   BACKUP_WORKER_DATA  默认 1；没有 worker 容器时自动跳过
set -euo pipefail

cd "$(dirname "$0")/.."

out="${1:-backups/$(date +%Y%m%d-%H%M%S)}"
compose_file="${COMPOSE_FILE:-docker-compose.yml}"
backup_worker="${BACKUP_WORKER_DATA:-1}"

mkdir -p "$out"
out="$(cd "$out" && pwd)"

hash_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

compose() { docker compose -f "$compose_file" "$@"; }

echo "==> 备份到 $out"

# 1) PostgreSQL
if [ -n "${POSTGRES_DSN:-}" ]; then
  pg_dump "$POSTGRES_DSN" -Fc -f "$out/postgres.dump"
else
  container="$(compose ps -q sounder-postgres 2>/dev/null || true)"
  if [ -z "$container" ]; then
    echo "找不到 sounder-postgres 容器；设置 POSTGRES_DSN 或检查 COMPOSE_FILE" >&2
    exit 1
  fi
  compose exec -T sounder-postgres \
    pg_dump -U "${POSTGRES_USER:-sounder}" -d "${POSTGRES_DB:-sounder}" -Fc \
    >"$out/postgres.dump"
fi
echo "ok postgres.dump ($(du -h "$out/postgres.dump" | cut -f1))"

# 2) Worker /data（会话 JSONL）
if [ "$backup_worker" = "1" ]; then
  worker="$(compose ps -q sounder-worker 2>/dev/null || true)"
  volume=""
  if [ -n "$worker" ]; then
    volume="$(docker inspect -f '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Name}}{{end}}{{end}}' "$worker")"
  fi
  if [ -n "$volume" ]; then
    docker run --rm \
      -v "$volume":/data:ro \
      -v "$out":/backup \
      alpine:3.20 sh -c 'tar czf /backup/worker-data.tar.gz --numeric-owner -C /data .'
    echo "ok worker-data.tar.gz (volume=$volume)"
  else
    echo "warn: 未找到 worker /data 卷，跳过 Worker 数据（仅备份控制面）" >&2
  fi
fi

{
  echo "createdAt=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "composeFile=$compose_file"
  echo "postgres=$(hash_file "$out/postgres.dump")"
  [ -f "$out/worker-data.tar.gz" ] && echo "workerData=$(hash_file "$out/worker-data.tar.gz")"
} >"$out/manifest.txt"

echo "==> 完成"
cat "$out/manifest.txt"
