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

// Kind 供 /v1/meta 展示当前存储实现。
func (p *Postgres) Kind() string { return "postgres" }

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

func (p *Postgres) DeleteByWorker(workerID, ownerID string) error {
	_, err := p.db.Exec(
		`DELETE FROM sessions WHERE worker_id = $1 AND ($2 = '' OR owner_id = $2)`, workerID, ownerID)
	if err != nil {
		return fmt.Errorf("delete sessions by worker: %w", err)
	}
	return nil
}

func (p *Postgres) DeleteByOwner(ownerID string) error {
	_, err := p.db.Exec(`DELETE FROM sessions WHERE owner_id = $1`, ownerID)
	return err
}

func (p *Postgres) ListByState(state string) []Session {
	rows, err := p.db.Query(
		`SELECT id, owner_id, worker_id, model, state, created_at, updated_at FROM sessions WHERE state = $1 ORDER BY created_at`, state)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var s Session
		var owner sql.NullString
		if err := rows.Scan(&s.ID, &owner, &s.WorkerID, &s.Model, &s.State, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return out
		}
		s.OwnerID = owner.String
		out = append(out, s)
	}
	return out
}
