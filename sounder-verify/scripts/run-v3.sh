#!/usr/bin/env bash
# V3 一键验证：建会话 -> kill -9 -> 检查日志 -> 可选 resume
set -euo pipefail

OUT="${1:-/tmp/pig-v3}"
MODEL="${PIG_VERIFY_MODEL:-}"
BIN="./bin/v3-recovery"

if [[ -z "$MODEL" ]]; then
  echo "需要 PIG_VERIFY_MODEL=provider/model" >&2
  exit 2
fi

go build -o "$BIN" ./cmd/v3-recovery
rm -rf "$OUT"
mkdir -p "$OUT"

"$BIN" -mode=create -out "$OUT" -model "$MODEL" > "$OUT/create.log" 2>&1 &
PID=$!

for _ in $(seq 1 90); do
  grep -q READY_TO_KILL "$OUT/create.log" 2>/dev/null && break
  sleep 1
done

if ! grep -q READY_TO_KILL "$OUT/create.log" 2>/dev/null; then
  echo "create 未就绪，日志如下：" >&2
  cat "$OUT/create.log" >&2
  kill -9 "$PID" 2>/dev/null || true
  exit 1
fi

kill -9 "$PID" 2>/dev/null || true
wait "$PID" 2>/dev/null || true

SESSION="$(grep '^SESSION_FILE=' "$OUT/create.log" | cut -d= -f2-)"
echo "session file: $SESSION"
echo

"$BIN" -mode=inspect -session "$SESSION"
echo

"$BIN" -mode=resume -out "$OUT" -session "$SESSION" -model "$MODEL"
