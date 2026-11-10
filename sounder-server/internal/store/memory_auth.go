package store

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// MemoryAuth 是 AuthStore 的内存实现，用于本地开发与测试。
type MemoryAuth struct {
	mu         sync.RWMutex
	users      map[string]User
	byEmail    map[string]string
	identities map[string]Identity // provider+"\x00"+subject
	passwords  map[string]string
	sessions   map[string]AuthSession
	byHash     map[string]string
	tokens     map[string]AuthToken
	tokenHash  map[string]string
	audits     []AuditEvent
}

func NewMemoryAuth() *MemoryAuth {
	return &MemoryAuth{
		users:      make(map[string]User),
		byEmail:    make(map[string]string),
		identities: make(map[string]Identity),
		passwords:  make(map[string]string),
		sessions:   make(map[string]AuthSession),
		byHash:     make(map[string]string),
		tokens:     make(map[string]AuthToken),
		tokenHash:  make(map[string]string),
	}
}

func identityKey(provider, subject string) string { return provider + "\x00" + subject }
func emailKey(email string) string                { return strings.ToLower(strings.TrimSpace(email)) }

func (m *MemoryAuth) CreateUser(u User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[u.ID]; ok {
		return ErrConflict
	}
	if u.Email != "" {
		if _, ok := m.byEmail[emailKey(u.Email)]; ok {
			return ErrConflict
		}
		m.byEmail[emailKey(u.Email)] = u.ID
	}
	if u.Status == "" {
		u.Status = "active"
	}
	now := time.Now()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now
	}
	u.UpdatedAt = now
	m.users[u.ID] = u
	return nil
}

func (m *MemoryAuth) UpdateUser(u User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.users[u.ID]
	if !ok {
		return ErrNotFound
	}
	if u.Email != "" && emailKey(u.Email) != emailKey(old.Email) {
		if id, exists := m.byEmail[emailKey(u.Email)]; exists && id != u.ID {
			return ErrConflict
		}
		delete(m.byEmail, emailKey(old.Email))
		m.byEmail[emailKey(u.Email)] = u.ID
	}
	u.UpdatedAt = time.Now()
	m.users[u.ID] = u
	return nil
}

func (m *MemoryAuth) GetUser(id string) (User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	u, ok := m.users[id]
	if !ok {
		return User{}, ErrNotFound
	}
	return u, nil
}

func (m *MemoryAuth) GetUserByEmail(email string) (User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.byEmail[emailKey(email)]
	if !ok {
		return User{}, ErrNotFound
	}
	return m.users[id], nil
}

func (m *MemoryAuth) CreateIdentity(i Identity) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.identities[identityKey(i.Provider, i.Subject)]; ok {
		return ErrConflict
	}
	if i.CreatedAt.IsZero() {
		i.CreatedAt = time.Now()
	}
	m.identities[identityKey(i.Provider, i.Subject)] = i
	return nil
}

func (m *MemoryAuth) GetIdentity(provider, subject string) (Identity, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	i, ok := m.identities[identityKey(provider, subject)]
	if !ok {
		return Identity{}, ErrNotFound
	}
	return i, nil
}

func (m *MemoryAuth) ListUsers(limit, offset int) ([]User, int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	all := make([]User, 0, len(m.users))
	for _, u := range m.users {
		all = append(all, u)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt.After(all[j].CreatedAt) })
	total := len(all)
	if offset > 0 {
		if offset >= len(all) {
			return nil, total, nil
		}
		all = all[offset:]
	}
	if limit > 0 && limit < len(all) {
		all = all[:limit]
	}
	return all, total, nil
}

func (m *MemoryAuth) ListIdentities(userID string) ([]Identity, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Identity
	for _, i := range m.identities {
		if i.UserID == userID {
			out = append(out, i)
		}
	}
	return out, nil
}

func (m *MemoryAuth) DeleteIdentity(userID, provider string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, i := range m.identities {
		if i.UserID == userID && i.Provider == provider {
			delete(m.identities, k)
			return nil
		}
	}
	return ErrNotFound
}

func (m *MemoryAuth) SetPassword(userID, hash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[userID]; !ok {
		return ErrNotFound
	}
	m.passwords[userID] = hash
	return nil
}

func (m *MemoryAuth) GetPasswordHash(userID string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	h, ok := m.passwords[userID]
	if !ok {
		return "", ErrNotFound
	}
	return h, nil
}

func (m *MemoryAuth) DeletePassword(userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.passwords[userID]; !ok {
		return ErrNotFound
	}
	delete(m.passwords, userID)
	return nil
}

func (m *MemoryAuth) CreateAuthSession(s AuthSession) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[s.ID] = s
	m.byHash[string(s.TokenHash)] = s.ID
	return nil
}

func (m *MemoryAuth) GetAuthSessionByHash(hash []byte) (AuthSession, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.byHash[string(hash)]
	if !ok {
		return AuthSession{}, ErrNotFound
	}
	return m.sessions[id], nil
}

func (m *MemoryAuth) TouchAuthSession(id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return ErrNotFound
	}
	s.LastSeenAt = at
	m.sessions[id] = s
	return nil
}

func (m *MemoryAuth) ExtendAuthSession(id string, expiresAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return ErrNotFound
	}
	s.ExpiresAt = expiresAt
	m.sessions[id] = s
	return nil
}

func (m *MemoryAuth) DeleteAuthSession(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return ErrNotFound
	}
	delete(m.byHash, string(s.TokenHash))
	delete(m.sessions, id)
	return nil
}

func (m *MemoryAuth) DeleteUserAuthSessions(userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		if s.UserID == userID {
			delete(m.byHash, string(s.TokenHash))
			delete(m.sessions, id)
		}
	}
	return nil
}

func (m *MemoryAuth) ListAuthSessions(userID string) ([]AuthSession, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []AuthSession
	for _, s := range m.sessions {
		if s.UserID == userID {
			out = append(out, s)
		}
	}
	return out, nil
}

func (m *MemoryAuth) CreateAuthToken(t AuthToken) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens[t.ID] = t
	m.tokenHash[string(t.TokenHash)] = t.ID
	return nil
}

func (m *MemoryAuth) ConsumeAuthToken(hash []byte, kind string, now time.Time) (AuthToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.tokenHash[string(hash)]
	if !ok {
		return AuthToken{}, ErrNotFound
	}
	t, ok := m.tokens[id]
	if !ok || t.Kind != kind || t.UsedAt != nil || now.After(t.ExpiresAt) {
		return AuthToken{}, ErrNotFound
	}
	used := now
	t.UsedAt = &used
	m.tokens[id] = t
	return t, nil
}

func (m *MemoryAuth) Audit(e AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	m.audits = append(m.audits, e)
	return nil
}

func (m *MemoryAuth) ListAuditEvents(limit int) ([]AuditEvent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	out := make([]AuditEvent, 0, limit)
	for i := len(m.audits) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, m.audits[i])
	}
	return out, nil
}

func (m *MemoryAuth) DeleteUser(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[id]
	if !ok {
		return ErrNotFound
	}
	delete(m.users, id)
	delete(m.byEmail, emailKey(u.Email))
	for k, ident := range m.identities {
		if ident.UserID == id {
			delete(m.identities, k)
		}
	}
	delete(m.passwords, id)
	for sid, sess := range m.sessions {
		if sess.UserID == id {
			delete(m.sessions, sid)
		}
	}
	for tid, tok := range m.tokens {
		if tok.UserID == id {
			delete(m.tokens, tid)
		}
	}
	return nil
}
