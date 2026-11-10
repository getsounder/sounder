#!/usr/bin/env bash
# 校验单文件前端：内联 module 脚本语法 + web/desktop 源文件一致性。
set -euo pipefail

cd "$(dirname "$0")/.."

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

for file in sounder-web/index.html sounder-admin-web/index.html; do
  name="$(basename "$(dirname "$file")")"
  out="$tmpdir/$name.mjs"
  awk '/<script type="module">/{flag=1;next}/<\/script>/{flag=0}flag' "$file" >"$out"
  if [ ! -s "$out" ]; then
    echo "未找到 module 脚本: $file" >&2
    exit 1
  fi
  node --check "$out"
  echo "ok $file"
done

if ! cmp -s sounder-web/index.html sounder-desktop/frontend/index.html; then
  echo "sounder-web/index.html 与 sounder-desktop/frontend/index.html 不一致（重新复制后再提交）" >&2
  exit 1
fi
echo "ok web/desktop 源文件一致"
