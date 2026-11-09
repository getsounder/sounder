#!/usr/bin/env bash
# Sounder 本地用户体系端到端测试。
#
# 默认针对 `docker compose up -d` 启动的本地栈：
#   API: http://localhost:8000
#   Web: http://localhost:3000
#
# 用法:
#   scripts/e2e-auth.sh
#   API_BASE=http://localhost:8000 WEB_BASE=http://localhost:3000 scripts/e2e-auth.sh
#
# 说明:
#   - 会在本地数据库创建随机邮箱的测试账号和会话，不会删除已有数据。
#   - 需要 jq 和 curl。
#   - 邮件使用 log mailer 时，会从 sounder-server 容器日志中读取重置链接。
set -uo pipefail

API_BASE=${API_BASE:-http://localhost:8000}
WEB_BASE=${WEB_BASE:-http://localhost:3000}
LEGACY_TOKEN=${LEGACY_TOKEN:-dev-token}
SERVER_CONTAINER=${SERVER_CONTAINER:-sounder-sounder-server-1}
WORKER_WAIT_SECONDS=${WORKER_WAIT_SECONDS:-120}

PASS=0
FAIL=0
TMP=$(mktemp -d)
BODY="$TMP/body"
COOKIE="$TMP/cookies"
ses_a=""
cleanup() {
  if [ -n "$ses_a" ]; then
    curl -sS --max-time 10 -o /dev/null -X DELETE \
      -H "Authorization: Bearer $LEGACY_TOKEN" "$API_BASE/v1/sessions/$ses_a" 2>/dev/null || true
  fi
  rm -rf "$TMP"
}
trap cleanup EXIT
# 每次运行使用独立的模拟客户端 IP，避免本地重复跑测试时耗尽注册限流桶。
E2E_IP=${E2E_CLIENT_IP:-10.42.$((RANDOM % 250 + 1)).$((RANDOM % 250 + 1))}

red() { printf '\033[31m%s\033[0m' "$1"; }
green() { printf '\033[32m%s\033[0m' "$1"; }
section() { printf '\n== %s ==\n' "$1"; }
ok() { PASS=$((PASS + 1)); printf '  %s %s\n' "$(green PASS)" "$1"; }
bad() {
  FAIL=$((FAIL + 1))
  printf '  %s %s\n' "$(red FAIL)" "$1"
  [ -n "${2:-}" ] && printf '       %s\n' "$2"
}
expect_eq() { # name actual expected
  if [ "$2" = "$3" ]; then ok "$1"; else bad "$1" "expected [$3], got [$2]"; fi
}
expect_true() { # name condition(0=true stmt result)
  if eval "$2"; then ok "$1"; else bad "$1" "${3:-condition failed}"; fi
}
expect_nonempty() { # name value
  if [ -n "$2" ] && [ "$2" != "null" ]; then ok "$1"; else bad "$1" "empty value"; fi
}

# call METHOD PATH [JSON_BODY] [BEARER] -> HTTP status; response in $BODY
call() {
  local method=$1 path=$2 data=${3-} token=${4-}
  local args=(-sS --max-time 20 -o "$BODY" -w '%{http_code}' -X "$method" "$API_BASE$path")
  args+=(-H "X-Forwarded-For: $E2E_IP")
  [ -n "$data" ] && args+=(-H 'Content-Type: application/json' --data-binary "$data")
  [ -n "$token" ] && args+=(-H "Authorization: Bearer $token")
  curl "${args[@]}"
}

json() { jq -r "$1" "$BODY" 2>/dev/null; }

wait_http() { # url tries
  local url=$1 tries=${2:-60} i=0
  until curl -fsS --max-time 3 "$url" >/dev/null 2>&1; do
    i=$((i + 1))
    [ "$i" -ge "$tries" ] && return 1
    sleep 1
  done
}

ts=$(date +%s)
email_a="e2e-a-$ts@example.com"
email_b="e2e-b-$ts@example.com"
pw_a='e2e-password-a1'
pw_a2='e2e-password-a2'
pw_b='e2e-password-b1'
pw_b2='e2e-password-b2'

section "0. 服务可用性"
if wait_http "$API_BASE/healthz" 30; then ok "API /healthz 可访问"; else bad "API /healthz 不可访问"; fi
if wait_http "$WEB_BASE/" 30; then ok "Web 页面可访问"; else bad "Web 页面不可访问"; fi
code=$(call GET /v1/meta)
expect_eq "GET /v1/meta 返回 200" "$code" "200"
expect_eq "meta protocolVersion=v1" "$(json '.protocolVersion')" "v1"

section "1. 认证配置"
code=$(call GET /v1/auth/providers)
expect_eq "GET /v1/auth/providers 返回 200" "$code" "200"
expect_eq "local 登录默认开启" "$(json '.local')" "true"
expect_eq "注册默认开启" "$(json '.registration')" "true"
expect_eq "默认不暴露 dev provider" "$(json '.providers | length')" "0"

section "2. 本地账号注册/登录"
code=$(call POST /v1/auth/register "{\"email\":\"$email_a\",\"password\":\"$pw_a\",\"displayName\":\"E2E A\"}")
expect_eq "注册账号 A 返回 201" "$code" "201"
if [ "$code" = "429" ]; then
  echo "  提示: 注册限流已触发（10 次/小时/IP）。可执行 'docker compose restart sounder-server' 重置进程内限流后重试。"
fi
token_a=$(json '.token')
user_a=$(json '.user.id')
expect_nonempty "注册返回 A 的 token" "$token_a"
expect_nonempty "注册返回 A 的用户 ID" "$user_a"
expect_eq "注册返回 A 的邮箱" "$(json '.user.email')" "$email_a"

code=$(call POST /v1/auth/register "{\"email\":\"$email_a\",\"password\":\"$pw_a\"}")
expect_eq "重复注册返回 409" "$code" "409"

code=$(call POST /v1/auth/register "{\"email\":\"weak-$ts@example.com\",\"password\":\"short\"}")
expect_eq "弱密码注册返回 400" "$code" "400"

code=$(call GET /v1/auth/me "" "$token_a")
expect_eq "Bearer token 访问 /me 返回 200" "$code" "200"
expect_eq "/me 返回正确邮箱" "$(json '.user.email')" "$email_a"
expect_eq "/me hasPassword=true" "$(json '.hasPassword')" "true"
expect_eq "/me 包含 local 身份" "$(json '[.identities[] | select(.provider=="local")] | length')" "1"

code=$(call GET /v1/auth/me)
expect_eq "匿名访问 /me 返回 401" "$code" "401"

code=$(call POST /v1/auth/login "{\"email\":\"$email_a\",\"password\":\"wrong-password-1\"}")
expect_eq "错误密码登录返回 401" "$code" "401"

code=$(call POST /v1/auth/login "{\"email\":\"$email_a\",\"password\":\"$pw_a\"}")
expect_eq "正确密码登录返回 200" "$code" "200"
token_a_login=$(json '.token')
expect_nonempty "登录返回 token" "$token_a_login"

section "3. Web Cookie 会话"
code=$(curl -sS --max-time 20 -c "$COOKIE" -o "$BODY" -w '%{http_code}' \
  -H 'Content-Type: application/json' \
  --data-binary "{\"email\":\"$email_a\",\"password\":\"$pw_a\"}" \
  "$WEB_BASE/v1/auth/login")
expect_eq "通过 Web 反向代理登录返回 200" "$code" "200"
expect_nonempty "登录写入 Cookie" "$(awk 'NF == 7 {print $7}' "$COOKIE" 2>/dev/null | tail -1)"
code=$(curl -sS --max-time 20 -b "$COOKIE" -o "$BODY" -w '%{http_code}' "$WEB_BASE/v1/auth/me")
expect_eq "使用 Cookie 通过 Web 代理访问 /me 返回 200" "$code" "200"
expect_eq "Cookie 会话邮箱正确" "$(json '.user.email')" "$email_a"

section "4. 会话归属与 legacy token"
code=$(call POST /v1/auth/register "{\"email\":\"$email_b\",\"password\":\"$pw_b\",\"displayName\":\"E2E B\"}")
expect_eq "注册账号 B 返回 201" "$code" "201"
user_b=$(json '.user.id')
token_b=$(json '.token')
expect_nonempty "注册返回 B 的用户 ID" "$user_b"

# 等 worker 重新连上 gateway（本地栈的 tunnel 会周期性重连）。
workers=0
deadline=$((SECONDS + WORKER_WAIT_SECONDS))
while [ "$SECONDS" -lt "$deadline" ]; do
  call GET /v1/workers "" "$LEGACY_TOKEN" >/dev/null
  workers=$(json '.workers | length')
  [ "$workers" -gt 0 ] 2>/dev/null && break
  sleep 2
done
if [ "$workers" -gt 0 ] 2>/dev/null; then
  ok "gateway 有可用 worker"
else
  bad "gateway 无可用 worker（会话创建用例将跳过）"
fi

if [ "$workers" -gt 0 ] 2>/dev/null; then
  code=$(call POST /v1/sessions '{"model":"e2e/noop"}' "$token_a")
  expect_eq "账号 A 创建会话返回 201" "$code" "201"
  if [ "$code" = "201" ]; then
    ses_a=$(json '.id')
    expect_nonempty "创建会话返回 ID" "$ses_a"
    expect_eq "会话 ownerId 为 A" "$(json '.ownerId')" "$user_a"

    code=$(call GET /v1/sessions "" "$token_a")
    expect_eq "账号 A 列出会话返回 200" "$code" "200"
    expect_eq "账号 A 只看到自己的会话" "$(json '[.sessions[] | select(.ownerId != "'"$user_a"'")] | length')" "0"
    expect_true "账号 A 的列表包含新建会话" "[ \"\$(json '[.sessions[] | select(.id == \"$ses_a\")] | length')\" = '1' ]"

    code=$(call GET "/v1/sessions/$ses_a" "" "$token_b")
    expect_eq "账号 B 读取 A 的会话返回 404" "$code" "404"

    code=$(call DELETE "/v1/sessions/$ses_a" "" "$token_b")
    expect_eq "账号 B 删除 A 的会话返回 404" "$code" "404"

    code=$(call GET /v1/sessions "" "$LEGACY_TOKEN")
    expect_eq "legacy token 列出全部会话返回 200" "$code" "200"
    expect_true "legacy token 能看到 A 的会话" "[ \"\$(json '[.sessions[] | select(.id == \"$ses_a\")] | length')\" = '1' ]"

    code=$(call GET "/v1/sessions/$ses_a" "" "$LEGACY_TOKEN")
    expect_eq "legacy token 能读取 A 的会话" "$code" "200"
  else
    echo "  SKIP 其余会话归属用例（创建会话失败；worker 可能已达容量，可 docker compose restart sounder-worker）"
  fi

  code=$(call GET /v1/sessions "")
  expect_eq "匿名访问会话列表返回 401" "$code" "401"
  code=$(call POST /v1/sessions '{"model":"e2e/noop"}')
  expect_eq "匿名创建会话返回 401" "$code" "401"
else
  echo "  SKIP 会话归属用例（无 worker）"
fi

section "5. 会话吊销与登出"
code=$(call GET /v1/auth/sessions "" "$token_a_login")
expect_eq "列出登录会话返回 200" "$code" "200"
sid_login=$(json '.current')
expect_nonempty "返回当前登录会话 ID" "$sid_login"

code=$(call DELETE "/v1/auth/sessions/$sid_login" "" "$token_a_login")
expect_eq "吊销当前登录会话返回 204" "$code" "204"
code=$(call GET /v1/auth/me "" "$token_a_login")
expect_eq "被吊销的 token 访问 /me 返回 401" "$code" "401"

code=$(call POST /v1/auth/logout "" "$token_a")
expect_eq "登出返回 204" "$code" "204"
code=$(call GET /v1/auth/me "" "$token_a")
expect_eq "登出后的 token 访问 /me 返回 401" "$code" "401"

section "6. 修改密码"
token_a=$(call POST /v1/auth/login "{\"email\":\"$email_a\",\"password\":\"$pw_a\"}" >/dev/null; json '.token')
expect_nonempty "重新登录 A 获取 token" "$token_a"
code=$(call POST /v1/auth/password "{\"oldPassword\":\"$pw_a\",\"newPassword\":\"$pw_a2\"}" "$token_a")
expect_eq "修改密码返回 200" "$code" "200"
code=$(call GET /v1/auth/me "" "$token_a")
expect_eq "修改密码后旧 token 失效（401）" "$code" "401"
code=$(call POST /v1/auth/login "{\"email\":\"$email_a\",\"password\":\"$pw_a\"}")
expect_eq "旧密码登录返回 401" "$code" "401"
code=$(call POST /v1/auth/login "{\"email\":\"$email_a\",\"password\":\"$pw_a2\"}")
expect_eq "新密码登录返回 200" "$code" "200"

section "7. 忘记密码/重置密码"
code=$(call POST /v1/auth/password/forgot "{\"email\":\"$email_b\"}")
expect_eq "忘记密码返回 202" "$code" "202"
sleep 1
reset_token=""
if command -v docker >/dev/null 2>&1; then
  reset_token=$(docker logs --since 90s "$SERVER_CONTAINER" 2>&1 | grep -o 'reset=[A-Za-z0-9_-]*' | tail -1 | cut -d= -f2)
fi
if [ -n "$reset_token" ]; then
  code=$(call POST /v1/auth/password/reset "{\"token\":\"$reset_token\",\"password\":\"$pw_b2\"}")
  expect_eq "使用邮件中的 token 重置密码返回 200" "$code" "200"
  code=$(call POST /v1/auth/login "{\"email\":\"$email_b\",\"password\":\"$pw_b\"}")
  expect_eq "重置后旧密码登录返回 401" "$code" "401"
  code=$(call POST /v1/auth/login "{\"email\":\"$email_b\",\"password\":\"$pw_b2\"}")
  expect_eq "重置后新密码登录返回 200" "$code" "200"
else
  echo "  SKIP 重置密码用例（未从容器日志找到 reset token）"
fi
code=$(call POST /v1/auth/password/reset "{\"token\":\"invalid-token\",\"password\":\"$pw_b2\"}")
expect_eq "无效重置 token 返回 401" "$code" "401"

section "8. 身份绑定查询"
token_b=$(call POST /v1/auth/login "{\"email\":\"$email_b\",\"password\":\"$pw_b2\"}" >/dev/null; json '.token')
code=$(call GET /v1/auth/identities "" "$token_b")
expect_eq "查询身份绑定返回 200" "$code" "200"
expect_eq "B 有 local 身份" "$(json '[.identities[] | select(.provider=="local")] | length')" "1"
code=$(call DELETE /v1/auth/identities/local "" "$token_b")
expect_eq "解绑唯一登录方式返回 409" "$code" "409"

section "9. CORS 白名单"
headers=$(curl -sS --max-time 10 -D - -o /dev/null -H 'Origin: http://localhost:3000' "$API_BASE/v1/meta")
expect_true "允许 Web Origin 回显" "printf '%s' \"\$headers\" | grep -qi '^Access-Control-Allow-Origin: http://localhost:3000'" "$headers"
expect_true "允许 Web Origin 时带 credentials" "printf '%s' \"\$headers\" | grep -qi '^Access-Control-Allow-Credentials: true'" "$headers"
headers=$(curl -sS --max-time 10 -D - -o /dev/null -H 'Origin: https://evil.example' "$API_BASE/v1/meta")
expect_true "未知 Origin 返回 *" "printf '%s' \"\$headers\" | grep -qi '^Access-Control-Allow-Origin: \\*'" "$headers"
expect_true "未知 Origin 不返回 credentials" "! printf '%s' \"\$headers\" | grep -qi '^Access-Control-Allow-Credentials:'" "$headers"
code=$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' -X OPTIONS \
  -H 'Origin: http://localhost:3000' -H 'Access-Control-Request-Method: POST' "$API_BASE/v1/auth/login")
expect_true "CORS 预检返回 2xx" "[ \"$code\" -ge 200 ] && [ \"$code\" -lt 300 ]" "status $code"

section "10. OAuth 未配置时的行为"
code=$(call GET "/v1/auth/oauth/google/start?mode=json&client=web")
expect_true "未配置 Google client 时 start 返回 4xx" "[ \"$code\" -ge 400 ] && [ \"$code\" -lt 500 ]" "status $code"
code=$(call GET "/v1/auth/oauth/github/start?mode=json&client=web")
expect_true "未配置 GitHub client 时 start 返回 4xx" "[ \"$code\" -ge 400 ] && [ \"$code\" -lt 500 ]" "status $code"
code=$(call GET "/v1/auth/oauth/dev/start?mode=json&client=web")
expect_true "dev provider 默认关闭" "[ \"$code\" -ge 400 ]" "status $code"

section "11. Web 页面与请求体限制"
html=$(curl -sS --max-time 10 "$WEB_BASE/")
expect_true "Web 首页包含登录弹窗" "printf '%s' \"\$html\" | grep -q 'id=\"authModal\"'"
expect_true "Web 首页包含注册 tab" "printf '%s' \"\$html\" | grep -q 'data-tab=\"register\"'"
big="$TMP/big.json"
python3 - "$big" <<'PY'
import json, sys
with open(sys.argv[1], "w") as f:
    json.dump({"email": "big@example.com", "password": "x" * (2 * 1024 * 1024)}, f)
PY
code=$(curl -sS --max-time 20 -o "$BODY" -w '%{http_code}' -H 'Content-Type: application/json' --data-binary "@$big" "$API_BASE/v1/auth/register")
expect_eq "超过 1MiB 的注册请求返回 400" "$code" "400"

section "12. 注册限流"
saved_ip=$E2E_IP
E2E_IP="10.43.$((RANDOM % 250 + 1)).$((RANDOM % 250 + 1))"
first_code=""
for i in $(seq 1 10); do
  code=$(call POST /v1/auth/register "{\"email\":\"rl-$ts-$i@example.com\",\"password\":\"short\"}")
  [ "$i" -eq 1 ] && first_code=$code
done
expect_eq "同 IP 前 10 次注册尝试返回 400" "$first_code" "400"
code=$(call POST /v1/auth/register "{\"email\":\"rl-$ts-11@example.com\",\"password\":\"short\"}")
expect_eq "同 IP 第 11 次注册返回 429" "$code" "429"
E2E_IP=$saved_ip

section "结果"
printf 'PASS=%d FAIL=%d\n' "$PASS" "$FAIL"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
