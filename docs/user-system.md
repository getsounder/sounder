# 用户体系调研：自建账号 + Google / GitHub 登录

> 状态：Phase 1–3 已实现（本地账号、Google/GitHub、会话归属、Web/桌面/Android 登录）
> 范围：`sounder-server`、`sounder-web`、`sounder-desktop`（桌面 / Android）
> 目标：把「一个共享 token」升级为「多用户 + 三种登录方式」，同时不引入过重的 IdP。

## 0. 结论

1. **在 `sounder-server` 内做一层轻量自研身份层**，不引入 Keycloak/Zitadel。
   当前规模只有一个 Go 服务、一张 `sessions` 表，自研成本远低于运维一个 IdP。
2. **统一身份模型**：`users`（人）+ `identities`（登录方式）+ `auth_sessions`（登录态）。
   本地账号、Google、GitHub 都是 `identities` 里的一行，账号合并/解绑都围绕它做。
3. **本地账号**：邮箱 + 密码，Argon2id 哈希，邮箱验证 + 找回密码。
4. **Google**：OIDC Authorization Code + PKCE，校验 `id_token`（iss/aud/exp/nonce）。
5. **GitHub**：OAuth 2.0 Web Flow（已支持 PKCE）或 Device Flow（原生客户端）。
6. **客户端分流**：
   - Web：HttpOnly Cookie + CSRF；
   - 桌面：系统浏览器 + `127.0.0.1` 回环回调 + PKCE（RFC 8252）；
   - Android：系统浏览器 / Custom Tabs + 自定义 scheme 回调 + PKCE，**不要用内嵌 WebView 登 Google**；
   - GitHub 在 Android 上可优先用 Device Flow（无需回调、无需 client_secret）。
7. **授权必须落到 owner**：`sessions` 加 `owner_id`，所有会话/审批接口按当前用户过滤；
   否则任何登录用户都能看到别人的会话（目前共享 token 就是这个状态）。
8. **保留 `-token` 作为 bootstrap/开发模式**，生产切到用户体系后不再作为客户端凭证。

## 1. 现状评估

### 1.1 已有实现

| 位置 | 现状 |
|---|---|
| `sounder-server/cmd/sounder-server/main.go:24` | `-token` 是全局静态 Bearer token，为空则不校验 |
| `sounder-server/internal/api/server.go:66-80` | `auth` 只比较 `Authorization: Bearer <token>`，没有用户身份 |
| `sounder-server/internal/store/store.go:17-25` | `Session` 没有 `owner_id`，`List()` 返回全部会话 |
| `sounder-server/internal/api/server.go:181` | `GET /v1/sessions` 直接返回 `store.List()` |
| `sounder-server/internal/api/server.go:218-247` | 查询/删除/发 prompt/取消都不校验归属 |
| `sounder-server/internal/api/server.go:333-357` | 审批按 `workerId` 直通 Worker，没有会话归属校验 |
| `sounder-web/index.html:118,153-158` | token 存在 `localStorage`，用户手动输入 |
| `sounder-desktop` Android | 同样把 token 存 `localStorage`（WebView 内） |
| `deploy/k8s/server.yaml:42` | 客户端 token 与 Gateway token 复用同一个 `SOUNDER_TOKEN` |

### 1.2 问题

- **没有「用户」概念**：所有客户端共享一个 token，无法审计、无法隔离、无法注销单个设备。
- **会话无归属**：`/v1/sessions` 全量返回；审批接口只要知道 `workerId` 就能操作。
- **凭证长期有效**：token 写死在 `.env`/K8s Secret，轮换需要重启所有客户端。
- **Web 端 token 在 `localStorage`**：一旦 XSS，token 可被直接读走。
- **无法接第三方登录**：没有 `state`/`nonce`/回调地址/账号绑定等基础设施。

## 2. 需求与边界

### 2.1 必须

- 本地账号：注册、登录、登出、改密码、找回密码、邮箱验证。
- Google 登录、GitHub 登录。
- 账号绑定/解绑：一个用户可绑定多种登录方式。
- 登录态可撤销：登出、改密、管理员禁用后立即失效。
- 会话归属：列表、详情、prompt、cancel、history、SSE、审批都按 owner 鉴权。
- Web / 桌面 / Android 三种客户端都能登录。
- 兼容现有部署：本地开发可以继续用 `-token` 一键跑。

### 2.2 暂不做

- 组织/团队/多租户计费。
- 企业 SSO（SAML/LDAP）、SCIM。
- 手机号/微信/Gitee 登录（可作为后续扩展，模型已支持）。
- 细粒度 RBAC（先分 `user` / `admin` 两级）。

## 3. 方案对比：自研 vs 现成 IdP

| 方案 | 本地账号 | Google/GitHub | 运维成本 | 适配度 |
|---|---|---|---|---|
| **自研（推荐）** | 自己实现 | 自己实现 | 低，复用 PostgreSQL | 高：直接嵌进 `sounder-server` |
| Keycloak | 内置 | 内置 | 高：JVM、独立 DB、升级/主题 | 过重 |
| Zitadel | 内置 | 内置 | 中 | 可选；Go 技术栈，但仍是独立服务 |
| Ory Kratos | 内置 | 内置 | 中高：配置复杂、概念多 | 规模大时再考虑 |
| Supabase GoTrue | 内置 | 内置 | 中 | 适合已用 Supabase 的团队 |
| Auth0 / Clerk / Logto | 内置 | 内置 | 低 | 数据出境、国内网络与合规风险 |

**结论**：当前只有三种登录方式、单服务、单库，自研大约 1–2 周可以做到可用；
引入 IdP 会把「一个二进制 + 一个 PG」变成「多一个需要运维的有状态服务」，不划算。
如果未来要做企业 SSO / 多租户，再把 `identities` 表对接 OIDC 即可平滑迁移。

> 国内网络注意：Google / GitHub 的 OAuth 端点在国内服务器上可能不稳定。
> 建议把 provider 做成可开关（`-auth-google-enabled=false`），并预留 Gitee/微信 provider；
> 不要把「Google 登录」设为唯一入口，否则大陆用户无法登录。

## 4. 身份模型

### 4.1 关系

```
users 1 ── n identities          (local / google / github，唯一键 provider+subject)
  │
  ├── 1 local_credentials        (仅本地账号有)
  ├── n auth_sessions            (登录态，可多设备)
  └── n sessions                 (Agent 会话，owner_id)
```

### 4.2 DDL（PostgreSQL）

```sql
-- 人
CREATE TABLE IF NOT EXISTS users (
    id             TEXT PRIMARY KEY,              -- usr_<rand>
    email          TEXT,                          -- 统一小写；可为空（纯 OAuth 且未取到邮箱）
    email_verified BOOLEAN NOT NULL DEFAULT FALSE,
    display_name   TEXT NOT NULL DEFAULT '',
    avatar_url     TEXT NOT NULL DEFAULT '',
    is_admin       BOOLEAN NOT NULL DEFAULT FALSE,
    status         TEXT NOT NULL DEFAULT 'active',-- active | disabled
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS users_email_key
    ON users (lower(email)) WHERE email IS NOT NULL;

-- 登录方式
CREATE TABLE IF NOT EXISTS identities (
    id             TEXT PRIMARY KEY,              -- idn_<rand>
    user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider       TEXT NOT NULL,                 -- local | google | github
    subject        TEXT NOT NULL,                 -- provider 侧唯一 ID
    email          TEXT,
    email_verified BOOLEAN NOT NULL DEFAULT FALSE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at  TIMESTAMPTZ,
    UNIQUE (provider, subject)
);
CREATE INDEX IF NOT EXISTS identities_user_idx ON identities (user_id);

-- 本地密码（与 identities 分开，避免 OAuth 行出现空密码）
CREATE TABLE IF NOT EXISTS local_credentials (
    user_id            TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    password_hash      TEXT NOT NULL,             -- Argon2id PHC 字符串
    password_updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 登录态（不存明文 token）
CREATE TABLE IF NOT EXISTS auth_sessions (
    id          TEXT PRIMARY KEY,                 -- ase_<rand>
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash  BYTEA NOT NULL UNIQUE,            -- SHA-256(token)
    user_agent  TEXT NOT NULL DEFAULT '',
    ip          INET,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS auth_sessions_user_idx ON auth_sessions (user_id);

-- 邮箱验证 / 找回密码 / 原生一次性兑换码
CREATE TABLE IF NOT EXISTS auth_tokens (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL,                     -- verify_email | reset_password | native_ticket
    token_hash BYTEA NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 审计
CREATE TABLE IF NOT EXISTS audit_events (
    id         BIGSERIAL PRIMARY KEY,
    user_id    TEXT,
    action     TEXT NOT NULL,                     -- login.success / login.failed / oauth.link ...
    ip         INET,
    user_agent TEXT NOT NULL DEFAULT '',
    detail     JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 会话归属
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS owner_id TEXT REFERENCES users(id);
CREATE INDEX IF NOT EXISTS sessions_owner_idx ON sessions (owner_id, created_at DESC);
```

迁移时 `owner_id` 先允许 NULL，把历史会话挂到 bootstrap 管理员，再改为 `NOT NULL`。

## 5. 登录流程

### 5.1 本地账号

**注册**

1. `POST /v1/auth/register {email, password, displayName}`。
2. 校验邮箱格式、密码强度（≥ 10 位，或 zxcvbn 分数）；邮箱统一小写。
3. `password_hash = argon2id(password, salt)`，参数建议 `m=64MiB, t=3, p=2`（可调）。
4. 创建 `users`（`email_verified=false`）+ `identities(provider='local', subject=user.id)` + `local_credentials`。
5. 生成 `verify_email` token（32 字节随机，库内存 SHA-256），发邮件；开发模式直接打日志。
6. 注册接口返回 201，但不直接给登录态（需要先验证邮箱；也可配置为「先登录后验证」）。

**登录**

1. `POST /v1/auth/login {email, password}`。
2. 按 `lower(email)` 查 user，读取 `local_credentials`；不存在的用户也要做一次假哈希，避免时间侧信道枚举。
3. 校验通过 → 创建 `auth_sessions`，返回 `Set-Cookie: sounder_session=<token>; HttpOnly; Secure; SameSite=Lax; Path=/`。
4. 原生客户端用 `Accept: application/json` 或 `?client=native` 拿 `{token, expiresAt}`。
5. 失败计数写 Redis（`auth:fail:<ip>` / `auth:fail:<user>`），5 次后指数退避/锁定 15 分钟。

**找回密码**

- `POST /v1/auth/password/forgot {email}`：无论邮箱是否存在都返回 204（防枚举）。
- 邮件链接带 `reset_password` token，30 分钟有效、单次使用；重置成功后删除该用户所有 `auth_sessions`。

**改密码**

- `POST /v1/auth/password {oldPassword, newPassword}`；成功后撤销其他设备登录态。

### 5.2 Google（OIDC）

使用 `golang.org/x/oauth2` + `github.com/coreos/go-oidc/v3/oidc`，走标准 OIDC Discovery。

1. `GET /v1/auth/oauth/google/start?redirect=<同源相对路径>`：
   - 生成 `state`（32B）与 `nonce`（32B），存 Redis/加密 Cookie（10 分钟）；
   - 生成 PKCE `code_verifier`/`code_challenge(S256)`；
   - 302 到 `https://accounts.google.com/o/oauth2/v2/auth`，
     `scope=openid email profile`，`redirect_uri=https://<host>/v1/auth/oauth/google/callback`。
2. 回调校验 `state`，用 code + verifier 换 token，验证 `id_token`：
   `iss=https://accounts.google.com`、`aud=client_id`、`exp`、`nonce`。
3. 取 `sub` 作为 `identities.subject`，`email` / `email_verified` / `name` / `picture` 落库。
4. 按 `(google, sub)` 找 identity：
   - 找到 → 登录；
   - 没有 → 若邮箱已验证且 `users.email` 已存在，走「绑定确认」而不是静默合并；
     否则创建新 user。
5. 生成 `auth_sessions`，Web 302 回 `/`，原生 302 到 `sounder://auth/callback?ticket=...`。

配置：`GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET`，回调地址必须与 Google Cloud Console 完全一致。

### 5.3 GitHub（OAuth 2.0）

GitHub 不是 OIDC，需要两步：

1. `GET /v1/auth/oauth/github/start`：
   - `scope=read:user user:email`（只要登录，不要 `repo`）；
   - 同样带 `state` + PKCE（GitHub 的 Web Flow 现已支持 `code_challenge`，S256）；
2. 回调 `POST https://github.com/login/oauth/access_token`（`Accept: application/json`），
   用 `client_secret` + `code` + `code_verifier` 换 token。
3. `GET https://api.github.com/user` 拿 `id`、`login`、`name`、`avatar_url`、`email`；
   若 `email` 为空，再 `GET /user/emails` 取 `primary && verified` 的邮箱。
4. `subject = strconv(id)`（不要用 login，login 可改）。
5. 后续同 Google：按 `(github, subject)` 查找/创建/绑定。

**Device Flow（推荐给 Android / CLI）**

1. App `POST https://github.com/login/device/code`（`client_id` + `scope`）；
2. 展示 `user_code`，打开 `verification_uri`；
3. App 按 `interval` 轮询 `POST /login/oauth/access_token`，`grant_type=urn:ietf:params:oauth:grant-type:device_code`；
4. 拿到 GitHub token 后，调用 `POST /v1/auth/oauth/github/device {accessToken}`，
   由服务端再校验一次 `GET /user` 并签发自己的登录态。
   - 或者把 `client_id` 放 App、Device Flow 全程在服务端代理，避免 App 直接持有第三方 token。

> 注意：GitHub OAuth App 需要在设置里手动启用 **Device Flow**；Device Flow 不需要 `client_secret`。

### 5.4 账号绑定与合并

- `identities(provider, subject)` 唯一，是唯一可信的账号锚点。
- **不要仅凭邮箱自动合并**：邮箱可能被 provider 标记未验证，或用户在各 provider 用了不同邮箱。
  推荐规则：
  1. provider 返回 `email_verified=true` 且与现有 `users.email`（已验证）相同 →
     展示「检测到已有账号，是否绑定？」；
  2. 用户确认后写入新 identity；
  3. 本地账号必须已验证邮箱才允许被 OAuth 绑定；
  4. 一个用户至少保留一种可用登录方式，解绑前校验（例如不能解绑最后一个）。
- 管理员可手动合并两个 user（后续管理台再做）。

### 5.5 三种客户端的回调方式

| 客户端 | 方式 | 说明 |
|---|---|---|
| Web | 服务端 302 → `/v1/auth/oauth/{p}/callback` → 302 回站点 | Cookie 登录态，最顺 |
| 桌面（Wails） | `127.0.0.1:<临时端口>` 回环 + PKCE | RFC 8252 推荐；监听只绑 127.0.0.1，收到 code 立即关闭 |
| Android（Wails） | 系统浏览器 / Custom Tabs → `sounder://auth/callback` | Google 明确禁止内嵌 WebView（`disallowed_useragent`） |
| Android（GitHub） | Device Flow | 无回调、无 secret，体验最稳 |

**绝对不要**在 Wails WebView 里直接加载 Google 登录页：
Google 会返回 `disallowed_useragent`，GitHub 也不建议。登录必须跳出到系统浏览器。

Android 侧需要：

- `AndroidManifest.xml` 给 `MainActivity` 加 `VIEW` intent-filter：
  `scheme="sounder"`、`host="auth"`、`path="/callback"`；
- Wails v3 已提供 `application.HandleOpenURL` / `mobile.OpenURL`，
  用它把回调 URL 交给 Go 侧，再换一次性 ticket。

### 5.6 原生登录态落地

不要长期把 token 放 `localStorage`（Android WebView 里尤其危险）。

1. 原生 OAuth 成功后，服务端签发一次性 `native_ticket`（60 秒、单次使用）。
2. App 通过深链拿到 ticket，调用 `POST /v1/auth/native/exchange {ticket}` 换 `{token, expiresAt}`。
3. token 存平台安全存储：
   - Android：`EncryptedSharedPreferences`（已引入 `androidx.security:security-crypto`）；
   - 桌面：macOS Keychain / Windows Credential Manager（如 `github.com/zalando/go-keyring`）；
   - Wails 自带的 `kvstore` 不是加密存储，不建议直接放 token。
4. 之后 API 用 `Authorization: Bearer <token>`；过期前用刷新接口续期，或重新走一次登录。

## 6. 令牌与会话

| 项 | 建议 |
|---|---|
| 令牌形态 | 256-bit 随机不透明串（base64url），库内只存 SHA-256 |
| 传输 | Web：HttpOnly Cookie；原生：`Authorization: Bearer` |
| 有效期 | 30 天滑动（每次使用更新 `last_seen_at`，超过 30 天未用失效） |
| 撤销 | 登出删除当前行；改密/禁用删除该用户全部行 |
| 并发设备 | 允许，管理页可「退出其他设备」 |
| 刷新 | M1 先用滑动过期；需要更短 TTL 时再加 refresh token |
| CSRF | Cookie 模式必须加：`SameSite=Lax` + `X-CSRF-Token` 双提交 |
| CORS | 从 `*` 收紧为允许列表：`https://wails.localhost`、桌面 `wails://`、Web 同源 |

> 不推荐直接上 JWT：当前单实例、需要即时撤销，DB 查一次 token 哈希更简单可靠。
> 将来水平扩展时，可换成「短 JWT + 刷新令牌」，接口不变。

## 7. API 设计

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/v1/auth/providers` | 返回已启用的登录方式，前端按需渲染按钮 |
| POST | `/v1/auth/register` | 本地注册 |
| POST | `/v1/auth/login` | 本地登录（Cookie 或 JSON token） |
| POST | `/v1/auth/logout` | 撤销当前登录态 |
| GET | `/v1/auth/me` | 当前用户 + 已绑定 identities |
| POST | `/v1/auth/password/forgot` | 发送重置邮件 |
| POST | `/v1/auth/password/reset` | 用 token 重置密码 |
| POST | `/v1/auth/password` | 已登录改密 |
| POST | `/v1/auth/email/verify` | 验证邮箱 |
| GET | `/v1/auth/oauth/{provider}/start` | 302 到 provider |
| GET | `/v1/auth/oauth/{provider}/callback` | provider 回调 |
| POST | `/v1/auth/oauth/github/device` | 原生 Device Flow 换登录态 |
| POST | `/v1/auth/native/exchange` | 一次性 ticket 换 token |
| GET | `/v1/auth/sessions` | 当前用户的登录设备列表 |
| DELETE | `/v1/auth/sessions/{id}` | 踢掉某台设备 |
| DELETE | `/v1/auth/identities/{provider}` | 解绑（校验至少剩一种） |

错误统一 `{"error":"..."}`，登录失败一律返回 `invalid credentials`，不区分「用户不存在/密码错」。

## 8. 授权改造（重点）

登录只是第一步，**不做 owner 校验等于没做用户体系**。

1. `store.Session` 增加 `OwnerID`；`Store` 接口改为：
   ```go
   ListByOwner(ownerID string) []Session
   GetOwned(id, ownerID string) (Session, error)
   ```
   `Get` 保留给内部/管理员。
2. 中间件解析登录态后把 `user` 放进 `context`；所有 handler 从 context 取。
3. 逐个改造：
   - `handleList` → `ListByOwner(user.ID)`；
   - `handleGet` / `handleDelete` / `handlePrompt` / `handleCancel` / `handleEvents` /
     `handleHistory` → `GetOwned`，不属于当前用户返回 **404**（不要返回 403，避免泄露存在性）；
   - `handleCreate` → 写入 `owner_id`；
   - `handleApprovals` / `handleApproval` → 先按 `workerId` 拉列表，再按「当前用户拥有的 sessionId」过滤。
4. **Worker 侧需要配合**：`/v1/approvals` 目前只按 worker 过滤，返回该 Worker 上所有审批。
   建议给审批项加 `sessionId`，服务端据此校验归属；否则同一 Worker 上的用户会互相看到审批。
5. 管理员：`is_admin` 可查看全部，但默认仍按 owner 过滤。
6. 兼容：`-auth=false` 或仅用 `-token` 时，请求注入 `usr_legacy` 用户（管理员），
   所有历史会话归它，保证 `docker compose up` 仍然开箱即用。

## 9. 安全清单

- [x] 密码 Argon2id，参数可配置；不记录明文/哈希到日志。
- [ ] 登录/注册/找回密码限流：已实现进程内按 IP + 账号限流，Redis 共享限流待做。
- [x] 统一错误信息，防用户枚举；不存在的用户也执行假哈希。
- [x] OAuth `state` 一次性、10 分钟过期；Google 校验 `nonce`；PKCE `S256`。
- [x] 回调 `redirect_uri` 只允许同源相对路径；原生端只允许 `sounder://` 与 loopback。
- [x] Cookie：`HttpOnly; Secure; SameSite=Lax`；生产必须 HTTPS。
- [ ] Cookie 模式下写操作目前依赖 SameSite=Lax + CORS 白名单，CSRF token 待做。
- [x] 令牌只存哈希；登出、改密、删除会话即时失效。
- [x] 邮箱验证链接/重置链接单次使用、短 TTL。
- [ ] 防暴力破解：进程内退避已做，跨实例锁定/告警待做。
- [x] 审计：登录成功/失败、OAuth 绑定/解绑、token 撤销写入 `audit_events`。
- [ ] 账号删除：表结构已级联，管理员删除接口待做。
- [x] 第三方 token 默认不落库；确需调用 GitHub API 时再存加密后的 token。
- [x] 日志脱敏：`Authorization`、Cookie、code、token 一律不打印。
- [x] 依赖：`golang.org/x/oauth2`、`go-oidc`、`x/crypto/argon2`、`pgx`。

## 10. 配置与部署

新增环境变量（Compose 与 K8s 同名，均可映射为 `sounder-server` 的 flag）：

```bash
# 用户体系总开关
SOUNDER_AUTH=true

# 对外地址：OAuth 回调前缀 + 邮件链接前缀
SOUNDER_PUBLIC_URL=https://sounder.example.com
SOUNDER_WEB_URL=https://sounder.example.com

# 本地账号
SOUNDER_LOCAL_AUTH=true
SOUNDER_REGISTRATION=true                 # 可关闭，改成邀请制
SOUNDER_REQUIRE_EMAIL_VERIFY=false

# Google：Authorized redirect URI 填 ${SOUNDER_PUBLIC_URL}/v1/auth/oauth/google/callback
SOUNDER_GOOGLE_CLIENT_ID=
SOUNDER_GOOGLE_CLIENT_SECRET=

# GitHub：Authorization callback URL 填 ${SOUNDER_PUBLIC_URL}/v1/auth/oauth/github/callback
SOUNDER_GITHUB_CLIENT_ID=
SOUNDER_GITHUB_CLIENT_SECRET=

# 原生端回调（桌面 loopback / Android 深链）
SOUNDER_NATIVE_REDIRECT=sounder://auth/callback

# 本地联调登录（code 即邮箱），生产必须 false
SOUNDER_DEV_PROVIDER=false

# 邮件
SOUNDER_SMTP_ADDR=
SOUNDER_SMTP_USER=
SOUNDER_SMTP_PASS=
SOUNDER_SMTP_FROM="no-reply@sounder.local"

# 登录态
SOUNDER_COOKIE_NAME=sounder_session
SOUNDER_COOKIE_SECURE=true                # 生产 HTTPS 必开
SOUNDER_COOKIE_DOMAIN=
```

- K8s：client secret 放 `sounder-secrets`，不要放 ConfigMap。
- Compose：`.env.example` 已包含上述键；默认关闭 Google/GitHub，避免本地必须配 OAuth 才能启动。
- 反向代理：回调路径 `/v1/auth/oauth/*` 不能被缓存；SSE 仍是 `/v1/sessions/{id}/events`。
- **OAuth state 目前存在单进程内存**：多副本部署必须使用粘性会话，或先把 state 迁到 Redis/共享存储。
  仓库里的 K8s 示例因此把 `sounder-server` 副本数设为 1。

### 本地端到端测试

`docker compose up -d` 之后运行：

```bash
./scripts/e2e-auth.sh
# API_BASE=http://localhost:8000 WEB_BASE=http://localhost:3000 ./scripts/e2e-auth.sh
```

脚本需要 `curl`、`jq`；会创建随机邮箱的临时账号，并在退出时删除自己创建的会话。
截至 2026-11-09，在本地 compose 栈上 **74 项断言全部通过**，覆盖：

- 本地注册/登录、重复注册、弱密码、错误密码、匿名访问；
- Bearer token、Web 反向代理 Cookie 会话；
- 账号 A/B 的会话创建、列表、读取、删除隔离，以及 legacy token 可见全部；
- 会话吊销、登出、改密后旧 token 失效、忘记密码/重置密码（从容器日志读取重置链接）；
- 身份绑定查询、解绑唯一登录方式返回 409；
- CORS 白名单（回显 Origin + credentials，未知 Origin 返回 `*`）；
- Google/GitHub 未配置时返回 4xx、dev provider 默认关闭；
- Web 页面关键元素、1MiB 请求体上限、注册限流（10 次/小时/IP）。

脚本每次运行使用独立的模拟客户端 IP（`X-Forwarded-For`），避免重复运行时耗尽
进程内注册限流桶；`sounder-server` 现在只信任来自本机/内网代理的转发头。

未覆盖：真实 Google/GitHub 授权（需要真实 client ID/secret）、真实 SMTP 投递、
多副本部署下的共享限流与 OAuth state。

## 11. 迁移步骤与工作量

### Phase 1：本地账号 + 归属（已实现）

- [x] `internal/auth` 包：argon2id、随机 token、`auth_sessions`。
- [x] `store` 增加 users/identities/credentials 表与 `OwnerID`。
- [x] 注册/登录/登出/me/改密/找回密码接口。
- [x] 中间件：Cookie + Bearer 双读，context 注入 user。
- [x] 所有 session/approval 接口改为 owner 校验。
- [x] `sounder-web` 登录页、注册页、登出、错误提示。
- [x] 兼容模式：`-token` 注入 dev 用户，`make up` 行为不变。

### Phase 2：Google + GitHub（已实现）

- [x] OAuth 配置与 provider 开关。
- [x] Google OIDC（state/nonce/PKCE/id_token 校验）。
- [x] GitHub Web Flow（state/PKCE/user + emails）。
- [x] 账号绑定/解绑页面；邮箱冲突确认流程。
- [x] Web 回调页 + 登录按钮。

### Phase 3：原生客户端（已实现）

- [x] 桌面：loopback 回调 + 系统浏览器打开。
- [x] Android：系统浏览器 + `sounder://auth/callback` intent-filter。
- [x] GitHub Device Flow。
- [x] 一次性 ticket 兑换；原生端把 token 存在应用本地存储。
- [x] 登录态过期处理与「退出登录」。

### Phase 4：加固（持续）

- [ ] Redis 限流、审计日志、管理员禁用用户。
- [ ] 邮件模板与投递重试。
- [ ] 登录设备管理、强制下线。
- [ ] 可选：MFA（TOTP）、Passkey。

## 12. 风险与开放问题

1. **国内网络**：Google/GitHub OAuth 在部分网络不可达。需要 provider 开关 + 备用登录方式（Gitee/微信）。
2. **邮箱唯一性**：同一个邮箱可能在 Google 和 GitHub 都验证过；是否自动合并需要产品决策（建议确认后合并）。
3. **Worker 审批隔离**：Worker 的 `ApprovalRequest` 已包含 `sessionId`，控制面按 owner 过滤；Worker 本身仍信任控制面 token。
4. **邮件投递**：本地账号依赖 SMTP；没有邮件服务时可先允许「开发模式免验证」。
5. **桌面本地 Worker 模式**：本地单机模式可以继续无登录；只有连云端时才要求登录。
6. **数据合规**：收集邮箱/头像属于个人信息，需要隐私政策与删除账号能力（PIPL/GDPR）。
7. **OAuth 应用审核**：Google 的 `email/profile` 属于非敏感 scope，一般无需审核；GitHub OAuth App 即开即用。
8. **账号迁移**：现有共享 token 的用户没有邮箱，只能全部归到 bootstrap 管理员，后续再手动分配。

## 13. 参考

- RFC 8252 — OAuth 2.0 for Native Apps（系统浏览器、回环回调）
- RFC 7636 — PKCE
- RFC 9700 — OAuth 2.0 Security Best Current Practice
- OpenID Connect Core 1.0
- GitHub Docs — Authorizing OAuth apps（Web Flow / Device Flow / PKCE）
- Google Identity — OAuth 2.0 for Mobile & Desktop Apps（禁止内嵌 WebView）
- OWASP — Password Storage Cheat Sheet（Argon2id）、Session Management Cheat Sheet
- `golang.org/x/oauth2`、`github.com/coreos/go-oidc/v3`
