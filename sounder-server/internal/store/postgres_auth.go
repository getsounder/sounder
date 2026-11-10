package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func (p *Postgres) CreateUser(u User) error {
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now()
	}
	u.UpdatedAt = u.CreatedAt
	if u.Status == "" {
		u.Status = "active"
	}
	_, err := p.db.Exec(
		`INSERT INTO users (id, email, email_verified, display_name, avatar_url, is_admin, status, created_at, updated_at)
		 VALUES ($1, NULLIF($2, ''), $3, $4, $5, $6, $7, $8, $9)`,
		u.ID, u.Email, u.EmailVerified, u.DisplayName, u.AvatarURL, u.IsAdmin, u.Status, u.CreatedAt, u.UpdatedAt)
	if isUniqueViolation(err) {
		return ErrConflict
	}
	if err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	return nil
}

func (p *Postgres) UpdateUser(u User) error {
	u.UpdatedAt = time.Now()
	res, err := p.db.Exec(
		`UPDATE users SET email = NULLIF($2, ''), email_verified = $3, display_name = $4,
		        avatar_url = $5, is_admin = $6, status = $7, updated_at = $8
		 WHERE id = $1`,
		u.ID, u.Email, u.EmailVerified, u.DisplayName, u.AvatarURL, u.IsAdmin, u.Status, u.UpdatedAt)
	if isUniqueViolation(err) {
		return ErrConflict
	}
	if err != nil {
		return fmt.Errorf("update user: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	var email sql.NullString
	err := row.Scan(&u.ID, &email, &u.EmailVerified, &u.DisplayName, &u.AvatarURL,
		&u.IsAdmin, &u.Status, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return User{}, ErrNotFound
		}
		return User{}, err
	}
	u.Email = email.String
	return u, nil
}

func (p *Postgres) GetUser(id string) (User, error) {
	u, err := scanUser(p.db.QueryRow(
		`SELECT id, email, email_verified, display_name, avatar_url, is_admin, status, created_at, updated_at
		 FROM users WHERE id = $1`, id))
	if err != nil {
		return User{}, err
	}
	return u, nil
}

func (p *Postgres) GetUserByEmail(email string) (User, error) {
	u, err := scanUser(p.db.QueryRow(
		`SELECT id, email, email_verified, display_name, avatar_url, is_admin, status, created_at, updated_at
		 FROM users WHERE lower(email) = lower($1)`, email))
	if err != nil {
		return User{}, err
	}
	return u, nil
}

func (p *Postgres) CreateIdentity(i Identity) error {
	if i.CreatedAt.IsZero() {
		i.CreatedAt = time.Now()
	}
	_, err := p.db.Exec(
		`INSERT INTO identities (id, user_id, provider, subject, email, email_verified, created_at, last_login_at)
		 VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $7, $8)`,
		i.ID, i.UserID, i.Provider, i.Subject, i.Email, i.EmailVerified, i.CreatedAt, i.LastLoginAt)
	if isUniqueViolation(err) {
		return ErrConflict
	}
	if err != nil {
		return fmt.Errorf("create identity: %w", err)
	}
	return nil
}

func scanIdentity(row interface{ Scan(...any) error }) (Identity, error) {
	var i Identity
	var email sql.NullString
	var last sql.NullTime
	err := row.Scan(&i.ID, &i.UserID, &i.Provider, &i.Subject, &email, &i.EmailVerified, &i.CreatedAt, &last)
	if err != nil {
		if err == sql.ErrNoRows {
			return Identity{}, ErrNotFound
		}
		return Identity{}, err
	}
	i.Email = email.String
	if last.Valid {
		t := last.Time
		i.LastLoginAt = &t
	}
	return i, nil
}

func (p *Postgres) GetIdentity(provider, subject string) (Identity, error) {
	i, err := scanIdentity(p.db.QueryRow(
		`SELECT id, user_id, provider, subject, email, email_verified, created_at, last_login_at
		 FROM identities WHERE provider = $1 AND subject = $2`, provider, subject))
	if err != nil {
		return Identity{}, err
	}
	return i, nil
}

func (p *Postgres) ListIdentities(userID string) ([]Identity, error) {
	rows, err := p.db.Query(
		`SELECT id, user_id, provider, subject, email, email_verified, created_at, last_login_at
		 FROM identities WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, fmt.Errorf("list identities: %w", err)
	}
	defer rows.Close()
	var out []Identity
	for rows.Next() {
		i, err := scanIdentity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

func (p *Postgres) DeleteIdentity(userID, provider string) error {
	res, err := p.db.Exec(`DELETE FROM identities WHERE user_id = $1 AND provider = $2`, userID, provider)
	if err != nil {
		return fmt.Errorf("delete identity: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) SetPassword(userID, hash string) error {
	_, err := p.db.Exec(
		`INSERT INTO local_credentials (user_id, password_hash, password_updated_at)
		 VALUES ($1, $2, now())
		 ON CONFLICT (user_id) DO UPDATE SET password_hash = EXCLUDED.password_hash,
		        password_updated_at = now()`, userID, hash)
	if err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	return nil
}

func (p *Postgres) GetPasswordHash(userID string) (string, error) {
	var hash string
	err := p.db.QueryRow(`SELECT password_hash FROM local_credentials WHERE user_id = $1`, userID).Scan(&hash)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get password: %w", err)
	}
	return hash, nil
}

func (p *Postgres) DeletePassword(userID string) error {
	_, err := p.db.Exec(`DELETE FROM local_credentials WHERE user_id = $1`, userID)
	if err != nil {
		return fmt.Errorf("delete password: %w", err)
	}
	return nil
}

func (p *Postgres) CreateAuthSession(s AuthSession) error {
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now()
	}
	if s.LastSeenAt.IsZero() {
		s.LastSeenAt = s.CreatedAt
	}
	_, err := p.db.Exec(
		`INSERT INTO auth_sessions (id, user_id, token_hash, user_agent, ip, created_at, last_seen_at, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		s.ID, s.UserID, s.TokenHash, s.UserAgent, s.IP, s.CreatedAt, s.LastSeenAt, s.ExpiresAt)
	if err != nil {
		return fmt.Errorf("create auth session: %w", err)
	}
	return nil
}

func scanAuthSession(row interface{ Scan(...any) error }) (AuthSession, error) {
	var s AuthSession
	err := row.Scan(&s.ID, &s.UserID, &s.TokenHash, &s.UserAgent, &s.IP,
		&s.CreatedAt, &s.LastSeenAt, &s.ExpiresAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return AuthSession{}, ErrNotFound
		}
		return AuthSession{}, err
	}
	return s, nil
}

func (p *Postgres) GetAuthSessionByHash(hash []byte) (AuthSession, error) {
	return scanAuthSession(p.db.QueryRow(
		`SELECT id, user_id, token_hash, user_agent, ip, created_at, last_seen_at, expires_at
		 FROM auth_sessions WHERE token_hash = $1`, hash))
}

func (p *Postgres) TouchAuthSession(id string, at time.Time) error {
	res, err := p.db.Exec(`UPDATE auth_sessions SET last_seen_at = $2 WHERE id = $1`, id, at)
	if err != nil {
		return fmt.Errorf("touch auth session: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) ExtendAuthSession(id string, expiresAt time.Time) error {
	res, err := p.db.Exec(`UPDATE auth_sessions SET expires_at = $2 WHERE id = $1`, id, expiresAt)
	if err != nil {
		return fmt.Errorf("extend auth session: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) DeleteAuthSession(id string) error {
	res, err := p.db.Exec(`DELETE FROM auth_sessions WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete auth session: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) DeleteUserAuthSessions(userID string) error {
	_, err := p.db.Exec(`DELETE FROM auth_sessions WHERE user_id = $1`, userID)
	if err != nil {
		return fmt.Errorf("delete user sessions: %w", err)
	}
	return nil
}

func (p *Postgres) ListAuthSessions(userID string) ([]AuthSession, error) {
	rows, err := p.db.Query(
		`SELECT id, user_id, token_hash, user_agent, ip, created_at, last_seen_at, expires_at
		 FROM auth_sessions WHERE user_id = $1 ORDER BY last_seen_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list auth sessions: %w", err)
	}
	defer rows.Close()
	var out []AuthSession
	for rows.Next() {
		s, err := scanAuthSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (p *Postgres) CreateAuthToken(t AuthToken) error {
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	_, err := p.db.Exec(
		`INSERT INTO auth_tokens (id, user_id, kind, token_hash, expires_at, used_at, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		t.ID, t.UserID, t.Kind, t.TokenHash, t.ExpiresAt, t.UsedAt, t.CreatedAt)
	if err != nil {
		return fmt.Errorf("create auth token: %w", err)
	}
	return nil
}

func (p *Postgres) ConsumeAuthToken(hash []byte, kind string, now time.Time) (AuthToken, error) {
	var t AuthToken
	var used sql.NullTime
	err := p.db.QueryRow(
		`UPDATE auth_tokens SET used_at = $3
		 WHERE token_hash = $1 AND kind = $2 AND used_at IS NULL AND expires_at > $3
		 RETURNING id, user_id, kind, token_hash, expires_at, used_at, created_at`,
		hash, kind, now).Scan(&t.ID, &t.UserID, &t.Kind, &t.TokenHash, &t.ExpiresAt, &used, &t.CreatedAt)
	if err == sql.ErrNoRows {
		return AuthToken{}, ErrNotFound
	}
	if err != nil {
		return AuthToken{}, fmt.Errorf("consume auth token: %w", err)
	}
	if used.Valid {
		u := used.Time
		t.UsedAt = &u
	}
	return t, nil
}

func (p *Postgres) Audit(e AuditEvent) error {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	detail := e.Detail
	if len(detail) == 0 {
		detail = []byte("{}")
	}
	_, err := p.db.Exec(
		`INSERT INTO audit_events (user_id, action, ip, user_agent, detail, created_at)
		 VALUES (NULLIF($1, ''), $2, $3, $4, $5, $6)`,
		e.UserID, e.Action, e.IP, e.UserAgent, detail, e.CreatedAt)
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	return nil
}

// ListUsers 供管理后台分页列出用户。
func (p *Postgres) ListUsers(limit, offset int) ([]User, int, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	var total int
	if err := p.db.QueryRow(`SELECT count(*) FROM users`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := p.db.Query(
		`SELECT id, email, email_verified, display_name, avatar_url, is_admin, status, created_at, updated_at
		 FROM users ORDER BY created_at DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, u)
	}
	return out, total, rows.Err()
}
