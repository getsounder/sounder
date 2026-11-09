package auth

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/getsounder/sounder-server/internal/store"
)

type stateEntry struct {
	Provider       string
	Client         string
	Redirect       string
	NativeRedirect string
	Verifier       string
	Nonce          string
	LinkUserID     string
	Expires        time.Time
}

func (s *Service) putState(e stateEntry) string {
	state := randomToken(32)
	now := s.cfg.Now()
	e.Expires = now.Add(10 * time.Minute)
	s.mu.Lock()
	for k, v := range s.states {
		if now.After(v.Expires) {
			delete(s.states, k)
		}
	}
	s.states[state] = e
	s.mu.Unlock()
	return state
}

func (s *Service) takeState(state string) (stateEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.states[state]
	delete(s.states, state)
	if !ok || s.cfg.Now().After(e.Expires) {
		return stateEntry{}, false
	}
	return e, true
}

func (s *Service) Provider(name string) (Provider, bool) {
	p, ok := s.providers[name]
	return p, ok
}

// StartOAuth 生成 state/PKCE 并返回 provider 的授权地址。
func (s *Service) StartOAuth(provider string, opt StartOptions) (string, error) {
	p, ok := s.providers[provider]
	if !ok {
		return "", ErrProviderDisabled
	}
	client := opt.Client
	if client == "" {
		client = "web"
	}
	if client != "web" && client != "native" {
		return "", errors.New("invalid client")
	}
	redirect := opt.Redirect
	if client == "web" {
		if redirect == "" {
			redirect = "/"
		}
		if !strings.HasPrefix(redirect, "/") || strings.HasPrefix(redirect, "//") {
			return "", errors.New("invalid redirect")
		}
	} else {
		redirect = opt.NativeRedirect
		if redirect == "" {
			redirect = s.cfg.NativeRedirect
		}
		if !s.validNativeRedirect(redirect) {
			return "", errors.New("invalid native redirect")
		}
	}
	verifier := randomToken(32)
	nonce := randomToken(24)
	state := s.putState(stateEntry{
		Provider: provider, Client: client, Redirect: redirect,
		NativeRedirect: redirect, Verifier: verifier, Nonce: nonce,
		LinkUserID: opt.LinkUserID,
	})
	return p.AuthCodeURL(state, verifier, nonce, opt.LoginHint), nil
}

func (s *Service) validNativeRedirect(uri string) bool {
	if uri == s.cfg.NativeRedirect {
		return true
	}
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "http" || u.Port() == "" {
		return false
	}
	switch u.Hostname() {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

// CompleteOAuth 处理 provider 回调：校验 state、换 token、创建/绑定用户。
func (s *Service) CompleteOAuth(ctx context.Context, provider, state, code, ip, ua string) (OAuthResult, error) {
	e, ok := s.takeState(state)
	if !ok || e.Provider != provider {
		return OAuthResult{}, errors.New("invalid oauth state")
	}
	p, ok := s.providers[provider]
	if !ok {
		return OAuthResult{}, ErrProviderDisabled
	}
	ext, err := p.Exchange(ctx, code, e.Verifier, e.Nonce)
	if err != nil {
		return OAuthResult{}, err
	}
	if ext.Provider == "" {
		ext.Provider = provider
	}

	// 已登录用户主动绑定新登录方式。
	if e.LinkUserID != "" {
		u, err := s.st.GetUser(e.LinkUserID)
		if err != nil {
			return OAuthResult{}, ErrInvalidToken
		}
		existing, err := s.st.GetIdentity(ext.Provider, ext.Subject)
		switch {
		case err == nil && existing.UserID != u.ID:
			return OAuthResult{}, ErrIdentityTaken
		case errors.Is(err, store.ErrNotFound):
			if err := s.st.CreateIdentity(store.Identity{
				ID: randomID("idn"), UserID: u.ID, Provider: ext.Provider, Subject: ext.Subject,
				Email: ext.Email, EmailVerified: ext.EmailVerified, CreatedAt: s.cfg.Now(),
			}); err != nil && !errors.Is(err, store.ErrConflict) {
				return OAuthResult{}, err
			}
		}
		s.audit(u.ID, "identity.linked", ip, ua, map[string]any{"provider": ext.Provider})
		res := OAuthResult{User: u, Redirect: e.Redirect}
		if e.Client == "native" {
			ticket, err := s.newAuthToken(u.ID, "native_ticket", time.Minute)
			if err != nil {
				return OAuthResult{}, err
			}
			res.Ticket = ticket
			res.NativeRedirect = e.NativeRedirect
		}
		return res, nil
	}

	u, err := s.resolveExternal(ext)
	if err != nil {
		return OAuthResult{}, err
	}
	if u.Status != "active" {
		return OAuthResult{}, ErrDisabled
	}
	s.audit(u.ID, "login.success", ip, ua, map[string]any{"provider": ext.Provider})

	res := OAuthResult{User: u}
	if e.Client == "native" {
		ticket, err := s.newAuthToken(u.ID, "native_ticket", time.Minute)
		if err != nil {
			return OAuthResult{}, err
		}
		res.Ticket = ticket
		res.NativeRedirect = e.NativeRedirect
		return res, nil
	}
	_, token, err := s.CreateSession(u.ID, ip, ua)
	if err != nil {
		return OAuthResult{}, err
	}
	res.Token = token
	res.Redirect = e.Redirect
	return res, nil
}

func (s *Service) resolveExternal(ext ExternalIdentity) (store.User, error) {
	if idn, err := s.st.GetIdentity(ext.Provider, ext.Subject); err == nil {
		return s.st.GetUser(idn.UserID)
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.User{}, err
	}

	if ext.Email != "" && ext.EmailVerified {
		if u, err := s.st.GetUserByEmail(ext.Email); err == nil {
			if !u.EmailVerified {
				// 本地账号还没验证邮箱，不能静默合并。
				return store.User{}, ErrEmailTaken
			}
			if err := s.st.CreateIdentity(store.Identity{
				ID: randomID("idn"), UserID: u.ID, Provider: ext.Provider, Subject: ext.Subject,
				Email: ext.Email, EmailVerified: true, CreatedAt: s.cfg.Now(),
			}); err != nil && !errors.Is(err, store.ErrConflict) {
				return store.User{}, err
			}
			return u, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return store.User{}, err
		}
	}

	now := s.cfg.Now()
	u := store.User{
		ID:            randomID("usr"),
		Email:         normalizeEmail(ext.Email),
		EmailVerified: ext.EmailVerified,
		DisplayName:   strings.TrimSpace(ext.DisplayName),
		AvatarURL:     ext.AvatarURL,
		Status:        "active",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if u.DisplayName == "" {
		u.DisplayName = strings.Split(u.Email, "@")[0]
	}
	if err := s.st.CreateUser(u); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return store.User{}, ErrEmailTaken
		}
		return store.User{}, err
	}
	if err := s.st.CreateIdentity(store.Identity{
		ID: randomID("idn"), UserID: u.ID, Provider: ext.Provider, Subject: ext.Subject,
		Email: u.Email, EmailVerified: u.EmailVerified, CreatedAt: now,
	}); err != nil {
		return store.User{}, err
	}
	return u, nil
}

// ExchangeNativeTicket 把浏览器回调带回的一次性 ticket 换成登录态。
func (s *Service) ExchangeNativeTicket(ticket, ip, ua string) (store.User, string, error) {
	t, err := s.st.ConsumeAuthToken(hashToken(ticket), "native_ticket", s.cfg.Now())
	if err != nil {
		return store.User{}, "", ErrInvalidToken
	}
	u, err := s.st.GetUser(t.UserID)
	if err != nil {
		return store.User{}, "", ErrInvalidToken
	}
	if u.Status != "active" {
		return store.User{}, "", ErrDisabled
	}
	_, token, err := s.CreateSession(u.ID, ip, ua)
	if err != nil {
		return store.User{}, "", err
	}
	s.audit(u.ID, "login.native", ip, ua, nil)
	return u, token, nil
}
