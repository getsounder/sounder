package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Postgres 是控制面的持久化实现。会话与用户元数据量都很小，单库足够。
type Postgres struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
    id         TEXT PRIMARY KEY,
    owner_id   TEXT,
    worker_id  TEXT NOT NULL,
    model      TEXT NOT NULL DEFAULT '',
    state      TEXT NOT NULL DEFAULT 'created',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
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
ALTER TABLE devices ADD COLUMN IF NOT EXISTS form TEXT NOT NULL DEFAULT 'sandbox';
CREATE INDEX IF NOT EXISTS devices_user_idx ON devices (user_id, last_seen_at DESC);

CREATE TABLE IF NOT EXISTS user_quotas (
    user_id        TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    max_devices    INT NOT NULL DEFAULT 1,
    device_cpu     INT NOT NULL DEFAULT 2,
    device_mem_mb  INT NOT NULL DEFAULT 4096,
    device_disk_gb INT NOT NULL DEFAULT 20,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

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
`

func NewPostgres(dsn string) (*Postgres, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(time.Hour)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &Postgres{db: db}, nil
}

func (p *Postgres) Migrate() error {
	if _, err := p.db.Exec(schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

func (p *Postgres) Close() error { return p.db.Close() }

func (p *Postgres) Create(s Session) error {
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now()
	}
	s.UpdatedAt = s.CreatedAt
	if s.State == "" {
		s.State = "created"
	}

	_, err := p.db.Exec(
		`INSERT INTO sessions (id, owner_id, worker_id, model, state, created_at, updated_at)
		 VALUES ($1, NULLIF($2, ''), $3, $4, $5, $6, $7)`,
		s.ID, s.OwnerID, s.WorkerID, s.Model, s.State, s.CreatedAt, s.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

func scanSession(row interface{ Scan(...any) error }) (Session, error) {
	var s Session
	var owner sql.NullString
	err := row.Scan(&s.ID, &owner, &s.WorkerID, &s.Model, &s.State, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return Session{}, ErrNotFound
		}
		return Session{}, err
	}
	s.OwnerID = owner.String
	return s, nil
}

func (p *Postgres) Get(id string) (Session, error) {
	s, err := scanSession(p.db.QueryRow(
		`SELECT id, owner_id, worker_id, model, state, created_at, updated_at
		 FROM sessions WHERE id = $1`, id))
	if err != nil {
		return Session{}, fmt.Errorf("get session: %w", err)
	}
	return s, nil
}

func (p *Postgres) GetOwned(id, ownerID string) (Session, error) {
	s, err := scanSession(p.db.QueryRow(
		`SELECT id, owner_id, worker_id, model, state, created_at, updated_at
		 FROM sessions WHERE id = $1 AND owner_id = $2`, id, ownerID))
	if err != nil {
		if err == ErrNotFound {
			return Session{}, ErrNotFound
		}
		return Session{}, fmt.Errorf("get session: %w", err)
	}
	return s, nil
}

func (p *Postgres) List() []Session {
	return p.list(`SELECT id, owner_id, worker_id, model, state, created_at, updated_at
		 FROM sessions ORDER BY created_at DESC LIMIT 500`)
}

func (p *Postgres) ListByOwner(ownerID string) []Session {
	return p.list(`SELECT id, owner_id, worker_id, model, state, created_at, updated_at
		 FROM sessions WHERE owner_id = $1 ORDER BY created_at DESC LIMIT 500`, ownerID)
}

func (p *Postgres) list(query string, args ...any) []Session {
	rows, err := p.db.Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		if s, err := scanSession(rows); err == nil {
			out = append(out, s)
		}
	}
	return out
}

func (p *Postgres) UpdateState(id, state string) error {
	res, err := p.db.Exec(
		`UPDATE sessions SET state = $2, updated_at = now() WHERE id = $1`, id, state)
	if err != nil {
		return fmt.Errorf("update state: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) Delete(id string) error {
	res, err := p.db.Exec(`DELETE FROM sessions WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) DeleteByOwner(ownerID string) error {
	_, err := p.db.Exec(`DELETE FROM sessions WHERE owner_id = $1`, ownerID)
	return err
}
