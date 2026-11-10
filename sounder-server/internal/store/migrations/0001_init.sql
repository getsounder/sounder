-- 0001 初始基线：一次建好控制面所需的全部表。
-- 已在旧版本运行过的库也能安全执行（全部 IF NOT EXISTS），
-- 但新库依赖这里的顺序：users 必须早于 devices（外键）。
CREATE TABLE IF NOT EXISTS users (
    id             TEXT PRIMARY KEY,
    email          TEXT,
    email_verified BOOLEAN NOT NULL DEFAULT FALSE,
    display_name   TEXT NOT NULL DEFAULT '',
    avatar_url     TEXT NOT NULL DEFAULT '',
    is_admin       BOOLEAN NOT NULL DEFAULT FALSE,
    status         TEXT NOT NULL DEFAULT 'active',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS users_email_key
    ON users (lower(email)) WHERE email IS NOT NULL;

CREATE TABLE IF NOT EXISTS identities (
    id             TEXT PRIMARY KEY,
    user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider       TEXT NOT NULL,
    subject        TEXT NOT NULL,
    email          TEXT,
    email_verified BOOLEAN NOT NULL DEFAULT FALSE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at  TIMESTAMPTZ,
    UNIQUE (provider, subject)
);
CREATE INDEX IF NOT EXISTS identities_user_idx ON identities (user_id);
CREATE UNIQUE INDEX IF NOT EXISTS identities_user_provider_key
    ON identities (user_id, provider);

CREATE TABLE IF NOT EXISTS local_credentials (
    user_id             TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    password_hash       TEXT NOT NULL,
    password_updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS auth_sessions (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash   BYTEA NOT NULL UNIQUE,
    user_agent   TEXT NOT NULL DEFAULT '',
    ip           TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS auth_sessions_user_idx ON auth_sessions (user_id);
CREATE INDEX IF NOT EXISTS auth_sessions_expires_idx ON auth_sessions (expires_at);

CREATE TABLE IF NOT EXISTS auth_tokens (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL,
    token_hash BYTEA NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS auth_tokens_user_idx ON auth_tokens (user_id);

CREATE TABLE IF NOT EXISTS user_quotas (
    user_id        TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    max_devices    INT NOT NULL DEFAULT 1,
    device_cpu     INT NOT NULL DEFAULT 2,
    device_mem_mb  INT NOT NULL DEFAULT 4096,
    device_disk_gb INT NOT NULL DEFAULT 20,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sessions (
    id         TEXT PRIMARY KEY,
    owner_id   TEXT,
    worker_id  TEXT NOT NULL,
    model      TEXT NOT NULL DEFAULT '',
    state      TEXT NOT NULL DEFAULT 'created',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- 兼容更早版本建出的 sessions（缺 owner_id）。
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS owner_id TEXT;
CREATE INDEX IF NOT EXISTS sessions_created_at_idx ON sessions (created_at DESC);
CREATE INDEX IF NOT EXISTS sessions_owner_idx ON sessions (owner_id, created_at DESC);

CREATE TABLE IF NOT EXISTS devices (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    worker_id    TEXT NOT NULL,
    name         TEXT NOT NULL DEFAULT '',
    kind         TEXT NOT NULL DEFAULT 'cloud',
    form         TEXT NOT NULL DEFAULT 'sandbox',
    platform     TEXT NOT NULL DEFAULT '',
    version      TEXT NOT NULL DEFAULT '',
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, worker_id)
);
-- 兼容更早版本建出的 devices（缺 form）。
ALTER TABLE devices ADD COLUMN IF NOT EXISTS form TEXT NOT NULL DEFAULT 'sandbox';
CREATE INDEX IF NOT EXISTS devices_user_idx ON devices (user_id, last_seen_at DESC);

CREATE TABLE IF NOT EXISTS audit_events (
    id         BIGSERIAL PRIMARY KEY,
    user_id    TEXT,
    action     TEXT NOT NULL,
    ip         TEXT NOT NULL DEFAULT '',
    user_agent TEXT NOT NULL DEFAULT '',
    detail     JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS audit_events_created_idx ON audit_events (created_at DESC);
