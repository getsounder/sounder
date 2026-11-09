package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/getsounder/sounder-server/internal/store"
)

type Service struct {
	cfg       Config
	st        store.AuthStore
	mail      Mailer
	lim       *limiter
	providers map[string]Provider

	mu      sync.Mutex
	states  map[string]stateEntry
	devices map[string]*deviceState

	dummyOnce sync.Once
	dummyHash string
}

type StartOptions struct {
	Client         string // web | native
	Redirect       string // web: 站内路径
	NativeRedirect string // native: loopback 或自定义 scheme
	LinkUserID     string
	LoginHint      string
}

type OAuthResult struct {
	User           store.User
	Token          string
	Ticket         string
	Redirect       string
	NativeRedirect string
}

func New(cfg Config, st store.AuthStore, mail Mailer) *Service {
	cfg = cfg.normalize()
	if mail == nil {
		mail = logMailer{}
	}
	s := &Service{
		cfg:       cfg,
		st:        st,
		mail:      mail,
		lim:       newLimiter(),
		providers: make(map[string]Provider),
		states:    make(map[string]stateEntry),
		devices:   make(map[string]*deviceState),
	}
	if cfg.Google.Enabled && cfg.Google.ClientID != "" {
		s.providers["google"] = newGoogleProvider(cfg.Google)
	}
	if cfg.GitHub.Enabled && cfg.GitHub.ClientID != "" {
		s.providers["github"] = newGitHubProvider(cfg.GitHub)
	}
	if cfg.DevProvider {
		s.providers["dev"] = &devProvider{baseURL: cfg.PublicBaseURL}
	}
	return s
}

func (s *Service) Config() Config { return s.cfg }

// ProviderNames 返回已启用的第三方登录方式。
func (s *Service) ProviderNames() []string {
	out := make([]string, 0, len(s.providers))
	for name := range s.providers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (s *Service) localUser(email, password, name, ip, ua string) (store.User, string, error) {
	if !s.cfg.LocalEnabled {
		return store.User{}, "", ErrProviderDisabled
	}
	if !s.cfg.RegistrationEnabled {
		return store.User{}, "", ErrRegistrationClosed
	}
	if !s.lim.allow("register:"+ip, 10, time.Hour, s.cfg.Now()) {
		return store.User{}, "", ErrRateLimited
	}
	email = normalizeEmail(email)
	if !validEmail(email) {
		return store.User{}, "", ErrInvalidEmail
	}
	if len(password) < 10 {
		return store.User{}, "", ErrWeakPassword
	}
	if len(password) > 1024 {
		return store.User{}, "", ErrWeakPassword
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = strings.Split(email, "@")[0]
	}
	if _, err := s.st.GetUserByEmail(email); err == nil {
		return store.User{}, "", ErrEmailTaken
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.User{}, "", err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return store.User{}, "", err
	}
	now := s.cfg.Now()
	u := store.User{
		ID:          randomID("usr"),
		Email:       email,
		DisplayName: name,
		Status:      "active",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.st.CreateUser(u); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return store.User{}, "", ErrEmailTaken
		}
		return store.User{}, "", err
	}
	if err := s.st.CreateIdentity(store.Identity{
		ID: randomID("idn"), UserID: u.ID, Provider: "local", Subject: u.ID,
		Email: email, CreatedAt: now,
	}); err != nil {
		return store.User{}, "", err
	}
	if err := s.st.SetPassword(u.ID, hash); err != nil {
		return store.User{}, "", err
	}
	s.audit(u.ID, "register", ip, ua, map[string]any{"provider": "local"})
	s.sendVerifyEmail(u)

	if s.cfg.RequireEmailVerify {
		return u, "", nil
	}
	sess, token, err := s.CreateSession(u.ID, ip, ua)
	if err != nil {
		return store.User{}, "", err
	}
	_ = sess
	return u, token, nil
}

// Register 创建本地账号。RequireEmailVerify 为 true 时不会直接返回登录态。
func (s *Service) Register(ctx context.Context, email, password, name, ip, ua string) (store.User, string, error) {
	return s.localUser(email, password, name, ip, ua)
}

var errDummyHash = errors.New("dummy")

func (s *Service) dummy() string {
	s.dummyOnce.Do(func() {
		h, err := hashPassword("sounder-dummy-password")
		if err == nil {
			s.dummyHash = h
		}
	})
	return s.dummyHash
}

func (s *Service) Login(ctx context.Context, email, password, ip, ua string) (store.User, string, error) {
	if !s.cfg.LocalEnabled {
		return store.User{}, "", ErrProviderDisabled
	}
	email = normalizeEmail(email)
	now := s.cfg.Now()
	if !s.lim.allow("login:ip:"+ip, 30, 5*time.Minute, now) ||
		!s.lim.allow("login:email:"+email, 10, 5*time.Minute, now) {
		return store.User{}, "", ErrRateLimited
	}
	u, err := s.st.GetUserByEmail(email)
	if err != nil {
		// 不存在的用户也做一次哈希校验，避免时间侧信道。
		if h := s.dummy(); h != "" {
			verifyPassword(h, password)
		}
		s.audit("", "login.failed", ip, ua, map[string]any{"email": email})
		return store.User{}, "", ErrInvalidCredentials
	}
	if u.Status != "active" {
		return store.User{}, "", ErrDisabled
	}
	hash, err := s.st.GetPasswordHash(u.ID)
	if err != nil {
		return store.User{}, "", ErrInvalidCredentials
	}
	if !verifyPassword(hash, password) {
		s.audit(u.ID, "login.failed", ip, ua, map[string]any{"provider": "local"})
		return store.User{}, "", ErrInvalidCredentials
	}
	if s.cfg.RequireEmailVerify && !u.EmailVerified {
		return store.User{}, "", ErrEmailNotVerified
	}
	_, token, err := s.CreateSession(u.ID, ip, ua)
	if err != nil {
		return store.User{}, "", err
	}
	s.audit(u.ID, "login.success", ip, ua, map[string]any{"provider": "local"})
	return u, token, nil
}

func (s *Service) CreateSession(userID, ip, ua string) (store.AuthSession, string, error) {
	now := s.cfg.Now()
	token := randomToken(32)
	sess := store.AuthSession{
		ID:         randomID("ase"),
		UserID:     userID,
		TokenHash:  hashToken(token),
		UserAgent:  truncate(ua, 300),
		IP:         ip,
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  now.Add(s.cfg.SessionTTL),
	}
	if err := s.st.CreateAuthSession(sess); err != nil {
		return store.AuthSession{}, "", err
	}
	return sess, token, nil
}

func (s *Service) Authenticate(token string) (store.User, store.AuthSession, error) {
	if token == "" {
		return store.User{}, store.AuthSession{}, ErrInvalidToken
	}
	sess, err := s.st.GetAuthSessionByHash(hashToken(token))
	if err != nil {
		return store.User{}, store.AuthSession{}, ErrInvalidToken
	}
	now := s.cfg.Now()
	if now.After(sess.ExpiresAt) {
		_ = s.st.DeleteAuthSession(sess.ID)
		return store.User{}, store.AuthSession{}, ErrInvalidToken
	}
	// 滑动续期：剩余不足一半时延长到完整 TTL。
	if now.Add(s.cfg.SessionTTL / 2).After(sess.ExpiresAt) {
		extended := now.Add(s.cfg.SessionTTL)
		if err := s.st.ExtendAuthSession(sess.ID, extended); err == nil {
			sess.ExpiresAt = extended
		}
	}
	_ = s.st.TouchAuthSession(sess.ID, now)
	u, err := s.st.GetUser(sess.UserID)
	if err != nil {
		return store.User{}, store.AuthSession{}, ErrInvalidToken
	}
	if u.Status != "active" {
		return store.User{}, store.AuthSession{}, ErrDisabled
	}
	return u, sess, nil
}

func (s *Service) Logout(token string) error {
	if token == "" {
		return nil
	}
	sess, err := s.st.GetAuthSessionByHash(hashToken(token))
	if err != nil {
		return nil
	}
	return s.st.DeleteAuthSession(sess.ID)
}

func (s *Service) ListSessions(userID string) ([]store.AuthSession, error) {
	return s.st.ListAuthSessions(userID)
}

func (s *Service) RevokeSession(userID, sessionID string) error {
	list, err := s.st.ListAuthSessions(userID)
	if err != nil {
		return err
	}
	for _, sess := range list {
		if sess.ID == sessionID {
			return s.st.DeleteAuthSession(sessionID)
		}
	}
	return store.ErrNotFound
}

func (s *Service) ChangePassword(userID, oldPassword, newPassword, ip, ua string) error {
	hash, err := s.st.GetPasswordHash(userID)
	if err != nil || !verifyPassword(hash, oldPassword) {
		return ErrInvalidCredentials
	}
	if err := s.validPassword(newPassword); err != nil {
		return err
	}
	newHash, err := hashPassword(newPassword)
	if err != nil {
		return err
	}
	if err := s.st.SetPassword(userID, newHash); err != nil {
		return err
	}
	_ = s.st.DeleteUserAuthSessions(userID)
	s.audit(userID, "password.changed", ip, ua, nil)
	return nil
}

func (s *Service) validPassword(pw string) error {
	if len(pw) < 10 || len(pw) > 1024 {
		return ErrWeakPassword
	}
	return nil
}

func (s *Service) ForgotPassword(email, ip, ua string) error {
	email = normalizeEmail(email)
	now := s.cfg.Now()
	if !s.lim.allow("forgot:"+email, 5, 15*time.Minute, now) ||
		!s.lim.allow("forgot:ip:"+ip, 20, 15*time.Minute, now) {
		return nil // 静默限流，避免暴露邮箱是否存在
	}
	u, err := s.st.GetUserByEmail(email)
	if err != nil || u.Status != "active" {
		return nil
	}
	raw, err := s.newAuthToken(u.ID, "reset_password", time.Hour)
	if err != nil {
		return err
	}
	link := s.cfg.WebBaseURL + "/?reset=" + url.QueryEscape(raw)
	s.send(u.Email, "重置你的 Sounder 密码",
		"点击链接重置密码（1 小时内有效）：\n\n"+link+"\n\n如果不是你本人操作，请忽略这封邮件。\n")
	s.audit(u.ID, "password.forgot", ip, ua, nil)
	return nil
}

func (s *Service) ResetPassword(token, newPassword, ip, ua string) error {
	if err := s.validPassword(newPassword); err != nil {
		return err
	}
	t, err := s.st.ConsumeAuthToken(hashToken(token), "reset_password", s.cfg.Now())
	if err != nil {
		return ErrInvalidToken
	}
	hash, err := hashPassword(newPassword)
	if err != nil {
		return err
	}
	u, err := s.st.GetUser(t.UserID)
	if err != nil {
		return ErrInvalidToken
	}
	if err := s.st.SetPassword(u.ID, hash); err != nil {
		return err
	}
	if _, err := s.st.GetIdentity("local", u.ID); errors.Is(err, store.ErrNotFound) {
		_ = s.st.CreateIdentity(store.Identity{
			ID: randomID("idn"), UserID: u.ID, Provider: "local", Subject: u.ID,
			Email: u.Email, EmailVerified: u.EmailVerified, CreatedAt: s.cfg.Now(),
		})
	}
	_ = s.st.DeleteUserAuthSessions(u.ID)
	s.audit(u.ID, "password.reset", ip, ua, nil)
	return nil
}

func (s *Service) SendVerification(userID string) error {
	u, err := s.st.GetUser(userID)
	if err != nil {
		return err
	}
	s.sendVerifyEmail(u)
	return nil
}

func (s *Service) VerifyEmail(token string) (store.User, error) {
	t, err := s.st.ConsumeAuthToken(hashToken(token), "verify_email", s.cfg.Now())
	if err != nil {
		return store.User{}, ErrInvalidToken
	}
	u, err := s.st.GetUser(t.UserID)
	if err != nil {
		return store.User{}, ErrInvalidToken
	}
	if !u.EmailVerified {
		u.EmailVerified = true
		u.UpdatedAt = s.cfg.Now()
		if err := s.st.UpdateUser(u); err != nil {
			return store.User{}, err
		}
	}
	s.audit(u.ID, "email.verified", "", "", nil)
	return u, nil
}

func (s *Service) sendVerifyEmail(u store.User) {
	if u.Email == "" {
		return
	}
	raw, err := s.newAuthToken(u.ID, "verify_email", 24*time.Hour)
	if err != nil {
		return
	}
	link := s.cfg.PublicBaseURL + "/v1/auth/email/verify?token=" + url.QueryEscape(raw)
	s.send(u.Email, "验证你的 Sounder 邮箱",
		"点击链接完成验证（24 小时内有效）：\n\n"+link+"\n\n如果不是你本人注册，请忽略这封邮件。\n")
}

func (s *Service) send(to, subject, body string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := s.mail.Send(ctx, to, subject, body); err != nil {
			// 邮件失败不阻塞主流程；日志里只记地址，不记正文。
			fmt.Printf("auth: send mail to %s: %v\n", to, err)
		}
	}()
}

func (s *Service) newAuthToken(userID, kind string, ttl time.Duration) (string, error) {
	now := s.cfg.Now()
	raw := randomToken(32)
	err := s.st.CreateAuthToken(store.AuthToken{
		ID: randomID("atk"), UserID: userID, Kind: kind,
		TokenHash: hashToken(raw), ExpiresAt: now.Add(ttl), CreatedAt: now,
	})
	if err != nil {
		return "", err
	}
	return raw, nil
}

func (s *Service) ListIdentities(userID string) ([]store.Identity, error) {
	return s.st.ListIdentities(userID)
}

// HasPassword 表示该账号是否设置了本地密码。
func (s *Service) HasPassword(userID string) bool {
	_, err := s.st.GetPasswordHash(userID)
	return err == nil
}

func (s *Service) Unlink(userID, provider string) error {
	list, err := s.st.ListIdentities(userID)
	if err != nil {
		return err
	}
	if len(list) <= 1 {
		return ErrLastIdentity
	}
	found := false
	for _, i := range list {
		if i.Provider == provider {
			found = true
			break
		}
	}
	if !found {
		return store.ErrNotFound
	}
	if err := s.st.DeleteIdentity(userID, provider); err != nil {
		return err
	}
	if provider == "local" {
		_ = s.st.DeletePassword(userID)
	}
	s.audit(userID, "identity.unlinked", "", "", map[string]any{"provider": provider})
	return nil
}

func (s *Service) audit(userID, action, ip, ua string, detail map[string]any) {
	var raw json.RawMessage
	if detail != nil {
		if b, err := json.Marshal(detail); err == nil {
			raw = b
		}
	}
	_ = s.st.Audit(store.AuditEvent{
		UserID: userID, Action: action, IP: ip, UserAgent: truncate(ua, 300),
		Detail: raw, CreatedAt: s.cfg.Now(),
	})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
