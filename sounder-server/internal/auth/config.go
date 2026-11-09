// Package auth 实现 Sounder 的用户体系：本地账号、Google/GitHub 登录、
// 登录态、账号绑定与审计。它只依赖 store.AuthStore，便于单测。
package auth

import (
	"errors"
	"time"
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrEmailTaken         = errors.New("email already registered")
	ErrDisabled           = errors.New("account disabled")
	ErrEmailNotVerified   = errors.New("email not verified")
	ErrInvalidToken       = errors.New("invalid or expired token")
	ErrRateLimited        = errors.New("too many attempts")
	ErrProviderDisabled   = errors.New("provider disabled")
	ErrIdentityTaken      = errors.New("identity already linked to another account")
	ErrLastIdentity       = errors.New("cannot unlink the last login method")
	ErrWeakPassword       = errors.New("password must be at least 10 characters")
	ErrInvalidEmail       = errors.New("invalid email")
	ErrRegistrationClosed = errors.New("registration is closed")
)

// ProviderConfig 描述一个 OAuth/OIDC provider。
type ProviderConfig struct {
	Enabled       bool
	ClientID      string
	ClientSecret  string
	AuthURL       string
	TokenURL      string
	UserURL       string
	EmailsURL     string
	DeviceCodeURL string
	JWKSURL       string
	Issuer        string
	Scopes        []string
}

type Config struct {
	PublicBaseURL       string
	WebBaseURL          string
	SessionTTL          time.Duration
	CookieName          string
	CookieSecure        bool
	CookieDomain        string
	LocalEnabled        bool
	RegistrationEnabled bool
	RequireEmailVerify  bool
	NativeRedirect      string
	Google              ProviderConfig
	GitHub              ProviderConfig
	DevProvider         bool

	Now func() time.Time
}

func (c Config) normalize() Config {
	if c.SessionTTL <= 0 {
		c.SessionTTL = 30 * 24 * time.Hour
	}
	if c.CookieName == "" {
		c.CookieName = "sounder_session"
	}
	if c.NativeRedirect == "" {
		c.NativeRedirect = "sounder://auth/callback"
	}
	if c.PublicBaseURL == "" {
		c.PublicBaseURL = "http://localhost:8000"
	}
	if c.WebBaseURL == "" {
		c.WebBaseURL = c.PublicBaseURL
	}
	if c.Google.AuthURL == "" {
		c.Google.AuthURL = "https://accounts.google.com/o/oauth2/v2/auth"
	}
	if c.Google.TokenURL == "" {
		c.Google.TokenURL = "https://oauth2.googleapis.com/token"
	}
	if c.Google.JWKSURL == "" {
		c.Google.JWKSURL = "https://www.googleapis.com/oauth2/v3/certs"
	}
	if c.Google.Issuer == "" {
		c.Google.Issuer = "https://accounts.google.com"
	}
	if len(c.Google.Scopes) == 0 {
		c.Google.Scopes = []string{"openid", "email", "profile"}
	}
	if c.GitHub.AuthURL == "" {
		c.GitHub.AuthURL = "https://github.com/login/oauth/authorize"
	}
	if c.GitHub.TokenURL == "" {
		c.GitHub.TokenURL = "https://github.com/login/oauth/access_token"
	}
	if c.GitHub.UserURL == "" {
		c.GitHub.UserURL = "https://api.github.com/user"
	}
	if c.GitHub.EmailsURL == "" {
		c.GitHub.EmailsURL = "https://api.github.com/user/emails"
	}
	if len(c.GitHub.Scopes) == 0 {
		c.GitHub.Scopes = []string{"read:user", "user:email"}
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}
