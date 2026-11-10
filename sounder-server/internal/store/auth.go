package store

import (
	"encoding/json"
	"errors"
	"time"
)

// ErrConflict 表示唯一键冲突（邮箱、provider+subject 等）。
var ErrConflict = errors.New("conflict")

type User struct {
	ID            string    `json:"id"`
	Email         string    `json:"email,omitempty"`
	EmailVerified bool      `json:"emailVerified"`
	DisplayName   string    `json:"displayName"`
	AvatarURL     string    `json:"avatarUrl,omitempty"`
	IsAdmin       bool      `json:"isAdmin"`
	Status        string    `json:"status"` // active | disabled
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type Identity struct {
	ID            string     `json:"id"`
	UserID        string     `json:"userId"`
	Provider      string     `json:"provider"` // local | google | github
	Subject       string     `json:"subject"`
	Email         string     `json:"email,omitempty"`
	EmailVerified bool       `json:"emailVerified"`
	CreatedAt     time.Time  `json:"createdAt"`
	LastLoginAt   *time.Time `json:"lastLoginAt,omitempty"`
}

type AuthSession struct {
	ID         string    `json:"id"`
	UserID     string    `json:"userId"`
	TokenHash  []byte    `json:"-"`
	UserAgent  string    `json:"userAgent,omitempty"`
	IP         string    `json:"ip,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	LastSeenAt time.Time `json:"lastSeenAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

type AuthToken struct {
	ID        string     `json:"id"`
	UserID    string     `json:"userId"`
	Kind      string     `json:"kind"` // verify_email | reset_password | native_ticket
	TokenHash []byte     `json:"-"`
	ExpiresAt time.Time  `json:"expiresAt"`
	UsedAt    *time.Time `json:"usedAt,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
}

type AuditEvent struct {
	UserID    string          `json:"userId,omitempty"`
	Action    string          `json:"action"`
	IP        string          `json:"ip,omitempty"`
	UserAgent string          `json:"userAgent,omitempty"`
	Detail    json.RawMessage `json:"detail,omitempty"`
	CreatedAt time.Time       `json:"createdAt"`
}

// AuthStore 是用户体系持久化接口。Memory 与 Postgres 都实现它。
type AuthStore interface {
	CreateUser(u User) error
	UpdateUser(u User) error
	GetUser(id string) (User, error)
	GetUserByEmail(email string) (User, error)
	ListUsers(limit, offset int) ([]User, int, error)

	CreateIdentity(i Identity) error
	GetIdentity(provider, subject string) (Identity, error)
	ListIdentities(userID string) ([]Identity, error)
	DeleteIdentity(userID, provider string) error

	SetPassword(userID, hash string) error
	GetPasswordHash(userID string) (string, error)
	DeletePassword(userID string) error

	CreateAuthSession(s AuthSession) error
	GetAuthSessionByHash(hash []byte) (AuthSession, error)
	TouchAuthSession(id string, at time.Time) error
	ExtendAuthSession(id string, expiresAt time.Time) error
	DeleteAuthSession(id string) error
	DeleteUserAuthSessions(userID string) error
	ListAuthSessions(userID string) ([]AuthSession, error)

	CreateAuthToken(t AuthToken) error
	// ConsumeAuthToken 以原子方式取出未使用且未过期的 token，并标记为已使用。
	ConsumeAuthToken(hash []byte, kind string, now time.Time) (AuthToken, error)

	Audit(e AuditEvent) error
	ListAuditEvents(limit int) ([]AuditEvent, error)

	// DeleteUser 删除用户（身份、凭据、会话由外键级联）。
	DeleteUser(id string) error
}
