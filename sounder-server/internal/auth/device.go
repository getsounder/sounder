package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type deviceState struct {
	DeviceCode string
	Expires    time.Time
	Interval   time.Duration
	NextPoll   time.Time
}

type DeviceStart struct {
	FlowID          string `json:"flowId"`
	UserCode        string `json:"userCode"`
	VerificationURI string `json:"verificationUri"`
	ExpiresIn       int    `json:"expiresIn"`
	Interval        int    `json:"interval"`
}

func (s *Service) StartDeviceFlow(ctx context.Context, provider string) (DeviceStart, error) {
	if provider != "github" {
		return DeviceStart{}, ErrProviderDisabled
	}
	cfg := s.cfg.GitHub
	if !cfg.Enabled || cfg.ClientID == "" {
		return DeviceStart{}, ErrProviderDisabled
	}
	endpoint := cfg.DeviceCodeURL
	if endpoint == "" {
		endpoint = "https://github.com/login/device/code"
	}
	form := url.Values{}
	form.Set("client_id", cfg.ClientID)
	form.Set("scope", strings.Join(cfg.Scopes, " "))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return DeviceStart{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return DeviceStart{}, fmt.Errorf("github device: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
		Error           string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return DeviceStart{}, fmt.Errorf("github device decode: %w", err)
	}
	if out.Error != "" || out.DeviceCode == "" {
		return DeviceStart{}, fmt.Errorf("github device: %s", out.Error)
	}
	now := s.cfg.Now()
	interval := time.Duration(out.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	flowID := randomID("flow")
	s.mu.Lock()
	s.devices[flowID] = &deviceState{
		DeviceCode: out.DeviceCode,
		Expires:    now.Add(time.Duration(out.ExpiresIn) * time.Second),
		Interval:   interval,
		NextPoll:   now,
	}
	s.mu.Unlock()
	return DeviceStart{
		FlowID: flowID, UserCode: out.UserCode, VerificationURI: out.VerificationURI,
		ExpiresIn: out.ExpiresIn, Interval: out.Interval,
	}, nil
}

// PollDeviceFlow 返回 status=pending 或 status=ok（带 token）。
func (s *Service) PollDeviceFlow(ctx context.Context, flowID, ip, ua string) (string, OAuthResult, error) {
	now := s.cfg.Now()
	s.mu.Lock()
	flow, ok := s.devices[flowID]
	if !ok {
		s.mu.Unlock()
		return "", OAuthResult{}, ErrInvalidToken
	}
	if now.After(flow.Expires) {
		delete(s.devices, flowID)
		s.mu.Unlock()
		return "", OAuthResult{}, ErrInvalidToken
	}
	if now.Before(flow.NextPoll) {
		s.mu.Unlock()
		return "pending", OAuthResult{}, nil
	}
	flow.NextPoll = now.Add(flow.Interval)
	deviceCode := flow.DeviceCode
	s.mu.Unlock()

	cfg := s.cfg.GitHub
	form := url.Values{}
	form.Set("client_id", cfg.ClientID)
	form.Set("device_code", deviceCode)
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", OAuthResult{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", OAuthResult{}, fmt.Errorf("github device poll: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Interval    int    `json:"interval"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", OAuthResult{}, fmt.Errorf("github device poll decode: %w", err)
	}
	switch out.Error {
	case "authorization_pending":
		return "pending", OAuthResult{}, nil
	case "slow_down":
		s.mu.Lock()
		if f, ok := s.devices[flowID]; ok {
			f.Interval += 5 * time.Second
			f.NextPoll = now.Add(f.Interval)
		}
		s.mu.Unlock()
		return "pending", OAuthResult{}, nil
	case "expired_token", "access_denied", "incorrect_client_credentials":
		s.mu.Lock()
		delete(s.devices, flowID)
		s.mu.Unlock()
		return "", OAuthResult{}, errors.New("device flow: " + out.Error)
	case "":
		// continue
	default:
		return "", OAuthResult{}, errors.New("device flow: " + out.Error)
	}
	if out.AccessToken == "" {
		return "pending", OAuthResult{}, nil
	}
	s.mu.Lock()
	delete(s.devices, flowID)
	s.mu.Unlock()

	gp, ok := s.providers["github"].(*githubProvider)
	if !ok {
		return "", OAuthResult{}, ErrProviderDisabled
	}
	ext, err := gp.UserFromToken(ctx, out.AccessToken)
	if err != nil {
		return "", OAuthResult{}, err
	}
	u, err := s.resolveExternal(ext)
	if err != nil {
		return "", OAuthResult{}, err
	}
	if u.Status != "active" {
		return "", OAuthResult{}, ErrDisabled
	}
	_, token, err := s.CreateSession(u.ID, ip, ua)
	if err != nil {
		return "", OAuthResult{}, err
	}
	s.audit(u.ID, "login.success", ip, ua, map[string]any{"provider": "github", "flow": "device"})
	return "ok", OAuthResult{User: u, Token: token}, nil
}
