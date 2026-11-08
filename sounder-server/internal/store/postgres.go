package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Postgres 是控制面的持久化实现。会话元数据量很小，单表足够。
type Postgres struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
    id         TEXT PRIMARY KEY,
    worker_id  TEXT NOT NULL,
    model      TEXT NOT NULL DEFAULT '',
    state      TEXT NOT NULL DEFAULT 'created',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS sessions_created_at_idx ON sessions (created_at DESC);
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
		`INSERT INTO sessions (id, worker_id, model, state, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		s.ID, s.WorkerID, s.Model, s.State, s.CreatedAt, s.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

func (p *Postgres) Get(id string) (Session, error) {
	var s Session
	err := p.db.QueryRow(
		`SELECT id, worker_id, model, state, created_at, updated_at FROM sessions WHERE id = $1`,
		id,
	).Scan(&s.ID, &s.WorkerID, &s.Model, &s.State, &s.CreatedAt, &s.UpdatedAt)
	if err == sql.ErrNoRows {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("get session: %w", err)
	}
	return s, nil
}

func (p *Postgres) List() []Session {
	rows, err := p.db.Query(
		`SELECT id, worker_id, model, state, created_at, updated_at
		 FROM sessions ORDER BY created_at DESC LIMIT 500`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		var s Session
		if rows.Scan(&s.ID, &s.WorkerID, &s.Model, &s.State, &s.CreatedAt, &s.UpdatedAt) == nil {
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
