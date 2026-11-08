-- 控制面的会话元数据。Worker 侧的 Session JSONL 不在 PG 里。
CREATE SCHEMA IF NOT EXISTS sounder;

CREATE TABLE IF NOT EXISTS sessions (
    id         TEXT PRIMARY KEY,
    worker_id  TEXT NOT NULL,
    model      TEXT NOT NULL DEFAULT '',
    state      TEXT NOT NULL DEFAULT 'created',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS sessions_created_at_idx ON sessions (created_at DESC);
