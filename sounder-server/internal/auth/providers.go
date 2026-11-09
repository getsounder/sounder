package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

type ExternalIdentity struct {
	Provider      string
	Subject       string
	Email         string
	EmailVerified bool
	DisplayName   string
	AvatarURL     string
}

type Provider interface {
	Name() string
	AuthCodeURL(state, verifier, nonce, loginHint string) string
	Exchange(ctx context.Context, code, verifier, nonce string) (ExternalIdentity, error)
}

type googleProvider struct {
	cfg   ProviderConfig
	oauth oauth2.Config

	once     sync.Once
	verifier *oidc.IDTokenVerifier
	err      error
}

func newGoogleProvider(cfg ProviderConfig) *googleProvider {
	return &googleProvider{
		cfg: cfg,
		oauth: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     oauth2.Endpoint{AuthURL: cfg.AuthURL, TokenURL: cfg.TokenURL},
			Scopes:       cfg.Scopes,
		},
	}
}

func (g *googleProvider) Name() string { return "google" }

func (g *googleProvider) idVerifier() (*oidc.IDTokenVerifier, error) {
	g.once.Do(func() {
		if g.cfg.JWKSURL == "" || g.cfg.Issuer == "" {
			g.err = errors.New("google: jwks/issuer not configured")
			return
		}
		keys := oidc.NewRemoteKeySet(context.Background(), g.cfg.JWKSURL)
		g.verifier = oidc.NewVerifier(g.cfg.Issuer, keys, &oidc.Config{ClientID: g.cfg.ClientID})
	})
	return g.verifier, g.err
}

func (g *googleProvider) AuthCodeURL(state, verifier, nonce, loginHint string) string {
	opts := []oauth2.AuthCodeOption{
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("nonce", nonce),
		oauth2.AccessTypeOnline,
		oauth2.SetAuthURLParam("prompt", "select_account"),
	}
	if loginHint != "" {
		opts = append(opts, oauth2.SetAuthURLParam("login_hint", loginHint))
	}
	return g.oauth.AuthCodeURL(state, opts...)
}

func (g *googleProvider) Exchange(ctx context.Context, code, verifier, nonce string) (ExternalIdentity, error) {
	tok, err := g.oauth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return ExternalIdentity{}, fmt.Errorf("google exchange: %w", err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return ExternalIdentity{}, errors.New("google: missing id_token")
	}
	idtVerifier, err := g.idVerifier()
	if err != nil {
		return ExternalIdentity{}, err
	}
	idt, err := idtVerifier.Verify(ctx, raw)
	if err != nil {
		return ExternalIdentity{}, fmt.Errorf("google: verify id_token: %w", err)
	}
	var claims struct {
		Sub           string `json:"sub"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
		Picture       string `json:"picture"`
		Nonce         string `json:"nonce"`
	}
	if err := idt.Claims(&claims); err != nil {
		return ExternalIdentity{}, fmt.Errorf("google: claims: %w", err)
	}
	if nonce != "" && claims.Nonce != nonce {
		return ExternalIdentity{}, errors.New("google: nonce mismatch")
	}
	if claims.Sub == "" {
		return ExternalIdentity{}, errors.New("google: empty subject")
	}
	return ExternalIdentity{
		Provider:      "google",
		Subject:       claims.Sub,
		Email:         claims.Email,
		EmailVerified: claims.EmailVerified,
		DisplayName:   claims.Name,
		AvatarURL:     claims.Picture,
	}, nil
}

type githubProvider struct {
	cfg   ProviderConfig
	oauth oauth2.Config
}

func newGitHubProvider(cfg ProviderConfig) *githubProvider {
	return &githubProvider{
		cfg: cfg,
		oauth: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     oauth2.Endpoint{AuthURL: cfg.AuthURL, TokenURL: cfg.TokenURL},
			Scopes:       cfg.Scopes,
		},
	}
}

func (g *githubProvider) Name() string { return "github" }

func (g *githubProvider) AuthCodeURL(state, verifier, _, _ string) string {
	return g.oauth.AuthCodeURL(state,
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("allow_signup", "true"))
}

func (g *githubProvider) Exchange(ctx context.Context, code, verifier, _ string) (ExternalIdentity, error) {
	tok, err := g.oauth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return ExternalIdentity{}, fmt.Errorf("github exchange: %w", err)
	}
	return g.UserFromToken(ctx, tok.AccessToken)
}

// UserFromToken 用 GitHub access token 取用户信息，供 Device Flow 复用。
func (g *githubProvider) UserFromToken(ctx context.Context, accessToken string) (ExternalIdentity, error) {
	client := oauth2.NewClient(ctx, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: accessToken}))

	var user struct {
		ID        int64  `json:"id"`
		Login     string `json:"login"`
		Name      string `json:"name"`
		Email     string `json:"email"`
		AvatarURL string `json:"avatar_url"`
	}
	if err := getJSON(ctx, client, g.cfg.UserURL, &user); err != nil {
		return ExternalIdentity{}, fmt.Errorf("github user: %w", err)
	}
	if user.ID == 0 {
		return ExternalIdentity{}, errors.New("github: empty user id")
	}
	// /user 的 email 是公开邮箱，未必已验证；始终用 /user/emails 找 primary+verified。
	email, verified := "", false
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := getJSON(ctx, client, g.cfg.EmailsURL, &emails); err == nil {
		for _, e := range emails {
			if e.Primary && e.Verified {
				email, verified = e.Email, true
				break
			}
		}
	}
	if email == "" && user.Email != "" {
		email, verified = user.Email, false
	}
	name := user.Name
	if name == "" {
		name = user.Login
	}
	return ExternalIdentity{
		Provider:      "github",
		Subject:       strconv.FormatInt(user.ID, 10),
		Email:         email,
		EmailVerified: verified,
		DisplayName:   name,
		AvatarURL:     user.AvatarURL,
	}, nil
}

func getJSON(ctx context.Context, client *http.Client, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// devProvider 仅用于本地联调：code 就是邮箱，直接登录。默认关闭。
type devProvider struct {
	baseURL string
}

func (d *devProvider) Name() string { return "dev" }

func (d *devProvider) AuthCodeURL(state, _, _, loginHint string) string {
	email := loginHint
	if email == "" {
		email = "dev@example.com"
	}
	q := url.Values{}
	q.Set("code", email)
	q.Set("state", state)
	return d.baseURL + "/v1/auth/oauth/dev/callback?" + q.Encode()
}

func (d *devProvider) Exchange(_ context.Context, code, _, _ string) (ExternalIdentity, error) {
	email := normalizeEmail(code)
	if email == "" {
		return ExternalIdentity{}, errors.New("dev: empty email")
	}
	if !strings.Contains(email, "@") {
		email += "@dev.local"
	}
	return ExternalIdentity{
		Provider:      "dev",
		Subject:       email,
		Email:         email,
		EmailVerified: true,
		DisplayName:   strings.Split(email, "@")[0],
	}, nil
}
