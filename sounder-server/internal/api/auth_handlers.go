package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/getsounder/sounder-server/internal/auth"
	"github.com/getsounder/sounder-server/internal/store"
)

// legacyUserID 是 -token 兼容模式下使用的 owner。
const legacyUserID = "usr_legacy"

// LegacyUserID 暴露给启动代码，用于保证兼容模式下的 owner 记录存在。
const LegacyUserID = legacyUserID

type authContext struct {
	UserID    string
	SessionID string
	Token     string
	Admin     bool
}

type authCtxKey struct{}

func authFrom(r *http.Request) authContext {
	if v, ok := r.Context().Value(authCtxKey{}).(authContext); ok {
		return v
	}
	return authContext{}
}

func (s *Server) tokenFromRequest(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(h[len("Bearer "):])
	}
	if s.auth != nil {
		if c, err := r.Cookie(s.auth.Config().CookieName); err == nil {
			return c.Value
		}
	}
	return ""
}

func (s *Server) authenticate(r *http.Request) (authContext, bool) {
	token := s.tokenFromRequest(r)
	if token != "" && s.auth != nil {
		if u, sess, err := s.auth.Authenticate(token); err == nil {
			return authContext{UserID: u.ID, SessionID: sess.ID, Token: token}, true
		}
	}
	if s.token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) == 1 {
		return authContext{UserID: legacyUserID, Token: token, Admin: true}, true
	}
	return authContext{}, false
}

func publicAuthPath(path string) bool {
	switch path {
	case "/healthz", "/readyz", "/v1/meta",
		"/v1/auth/providers", "/v1/auth/register", "/v1/auth/login",
		"/v1/auth/password/forgot", "/v1/auth/password/reset",
		"/v1/auth/email/verify", "/v1/auth/native/exchange":
		return true
	}
	// OAuth 的 start/callback/device 都是浏览器直达；link 需要已登录。
	if strings.HasPrefix(path, "/v1/auth/oauth/") && !strings.Contains(path, "/link/") {
		return true
	}
	return false
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	// 仅信任来自本机/内网代理的转发头，避免公网客户端伪造 IP
	// 绕过限流或污染审计日志。
	if trustedProxy(host) {
		if v := r.Header.Get("X-Forwarded-For"); v != "" {
			if i := strings.IndexByte(v, ','); i >= 0 {
				v = v[:i]
			}
			return strings.TrimSpace(v)
		}
		if v := r.Header.Get("X-Real-IP"); v != "" {
			return strings.TrimSpace(v)
		}
	}
	return host
}

func trustedProxy(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	if s.auth == nil || token == "" {
		return
	}
	cfg := s.auth.Config()
	http.SetCookie(w, &http.Cookie{
		Name:     cfg.CookieName,
		Value:    token,
		Path:     "/",
		Domain:   cfg.CookieDomain,
		HttpOnly: true,
		Secure:   cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(cfg.SessionTTL.Seconds()),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	if s.auth == nil {
		return
	}
	cfg := s.auth.Config()
	http.SetCookie(w, &http.Cookie{
		Name:     cfg.CookieName,
		Value:    "",
		Path:     "/",
		Domain:   cfg.CookieDomain,
		HttpOnly: true,
		Secure:   cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func (s *Server) webURL(path string) string {
	if s.auth == nil {
		return path
	}
	base := strings.TrimRight(s.auth.Config().WebBaseURL, "/")
	return base + path
}

func appendQuery(raw, key, val string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	q.Set(key, val)
	u.RawQuery = q.Encode()
	return u.String()
}

func (s *Server) handleAuthProviders(w http.ResponseWriter, _ *http.Request) {
	if s.auth == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"local": false, "registration": false, "providers": []string{},
		})
		return
	}
	cfg := s.auth.Config()
	names := s.auth.ProviderNames()
	providers := make([]map[string]string, 0, len(names))
	for _, n := range names {
		label := n
		switch n {
		case "google":
			label = "Google"
		case "github":
			label = "GitHub"
		}
		providers = append(providers, map[string]string{"name": n, "label": label})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"local":              cfg.LocalEnabled,
		"registration":       cfg.RegistrationEnabled,
		"requireEmailVerify": cfg.RequireEmailVerify,
		"providers":          providers,
		"githubDeviceFlow":   cfg.GitHub.Enabled && cfg.GitHub.ClientID != "",
	})
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeErr(w, http.StatusNotFound, "auth disabled")
		return
	}
	var req struct {
		Email       string `json:"email"`
		Password    string `json:"password"`
		DisplayName string `json:"displayName"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	u, token, err := s.auth.Register(r.Context(), req.Email, req.Password, req.DisplayName, clientIP(r), r.UserAgent())
	if err != nil {
		writeAuthErr(w, err)
		return
	}
	if token == "" {
		writeJSON(w, http.StatusAccepted, map[string]any{"user": u, "verificationRequired": true})
		return
	}
	s.setSessionCookie(w, token)
	writeJSON(w, http.StatusCreated, map[string]any{"token": token, "user": u})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeErr(w, http.StatusNotFound, "auth disabled")
		return
	}
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	u, token, err := s.auth.Login(r.Context(), req.Email, req.Password, clientIP(r), r.UserAgent())
	if err != nil {
		writeAuthErr(w, err)
		return
	}
	s.setSessionCookie(w, token)
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "user": u})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	ctx := authFrom(r)
	if s.auth != nil {
		_ = s.auth.Logout(ctx.Token)
	}
	s.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	ctx := authFrom(r)
	if s.authStore == nil || ctx.UserID == "" {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	u, err := s.authStore.GetUser(ctx.UserID)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	out := map[string]any{"user": u, "sessionId": ctx.SessionID, "admin": ctx.Admin}
	if s.auth != nil {
		if ids, err := s.auth.ListIdentities(ctx.UserID); err == nil {
			out["identities"] = ids
		}
		out["hasPassword"] = s.auth.HasPassword(ctx.UserID)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAuthSessions(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeErr(w, http.StatusNotFound, "auth disabled")
		return
	}
	ctx := authFrom(r)
	sessions, err := s.auth.ListSessions(ctx.UserID)
	if err != nil {
		writeAuthErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions, "current": ctx.SessionID})
}

func (s *Server) handleRevokeSession(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeErr(w, http.StatusNotFound, "auth disabled")
		return
	}
	ctx := authFrom(r)
	if err := s.auth.RevokeSession(ctx.UserID, r.PathValue("id")); err != nil {
		writeAuthErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeErr(w, http.StatusNotFound, "auth disabled")
		return
	}
	ctx := authFrom(r)
	var req struct {
		OldPassword string `json:"oldPassword"`
		NewPassword string `json:"newPassword"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if err := s.auth.ChangePassword(ctx.UserID, req.OldPassword, req.NewPassword, clientIP(r), r.UserAgent()); err != nil {
		writeAuthErr(w, err)
		return
	}
	s.clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]string{"status": "password changed, please sign in again"})
}

func (s *Server) handleForgotPassword(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeErr(w, http.StatusNotFound, "auth disabled")
		return
	}
	var req struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	_ = s.auth.ForgotPassword(req.Email, clientIP(r), r.UserAgent())
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "if the address exists, a reset link was sent"})
}

func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeErr(w, http.StatusNotFound, "auth disabled")
		return
	}
	var req struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if err := s.auth.ResetPassword(req.Token, req.Password, clientIP(r), r.UserAgent()); err != nil {
		writeAuthErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "password reset"})
}

func (s *Server) handleVerifyEmail(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if s.auth == nil {
		http.Redirect(w, r, s.webURL("/?verify=error"), http.StatusFound)
		return
	}
	if _, err := s.auth.VerifyEmail(token); err != nil {
		http.Redirect(w, r, s.webURL("/?verify=error"), http.StatusFound)
		return
	}
	http.Redirect(w, r, s.webURL("/?verify=ok"), http.StatusFound)
}

func (s *Server) handleResendVerify(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeErr(w, http.StatusNotFound, "auth disabled")
		return
	}
	ctx := authFrom(r)
	if err := s.auth.SendVerification(ctx.UserID); err != nil {
		writeAuthErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "sent"})
}

func (s *Server) handleIdentities(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeErr(w, http.StatusNotFound, "auth disabled")
		return
	}
	ctx := authFrom(r)
	ids, err := s.auth.ListIdentities(ctx.UserID)
	if err != nil {
		writeAuthErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"identities": ids, "hasPassword": s.auth.HasPassword(ctx.UserID),
	})
}

func (s *Server) handleUnlink(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeErr(w, http.StatusNotFound, "auth disabled")
		return
	}
	ctx := authFrom(r)
	if err := s.auth.Unlink(ctx.UserID, r.PathValue("provider")); err != nil {
		writeAuthErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeErr(w, http.StatusNotFound, "auth disabled")
		return
	}
	q := r.URL.Query()
	opt := auth.StartOptions{
		Client:         q.Get("client"),
		Redirect:       q.Get("redirect"),
		NativeRedirect: q.Get("native_redirect"),
		LoginHint:      q.Get("login_hint"),
	}
	linkPath := strings.Contains(r.URL.Path, "/link/")
	if linkPath {
		ctx := authFrom(r)
		if ctx.UserID == "" {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		opt.LinkUserID = ctx.UserID
	}
	target, err := s.auth.StartOAuth(r.PathValue("provider"), opt)
	if err != nil {
		writeAuthErr(w, err)
		return
	}
	if q.Get("mode") == "json" {
		writeJSON(w, http.StatusOK, map[string]string{"url": target})
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}

func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		http.Redirect(w, r, s.webURL("/?authError=auth+disabled"), http.StatusFound)
		return
	}
	provider := r.PathValue("provider")
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		http.Redirect(w, r, s.webURL("/?authError="+url.QueryEscape(e)), http.StatusFound)
		return
	}
	res, err := s.auth.CompleteOAuth(r.Context(), provider, q.Get("state"), q.Get("code"), clientIP(r), r.UserAgent())
	if err != nil {
		http.Redirect(w, r, s.webURL("/?authError="+url.QueryEscape(authErrText(err))), http.StatusFound)
		return
	}
	if res.NativeRedirect != "" {
		target := appendQuery(res.NativeRedirect, "provider", provider)
		if res.Ticket != "" {
			target = appendQuery(target, "ticket", res.Ticket)
		}
		http.Redirect(w, r, target, http.StatusFound)
		return
	}
	if res.Token != "" {
		s.setSessionCookie(w, res.Token)
		http.Redirect(w, r, s.webURL(appendQuery(res.Redirect, "login", "ok")), http.StatusFound)
		return
	}
	http.Redirect(w, r, s.webURL(appendQuery(res.Redirect, "linked", provider)), http.StatusFound)
}

func (s *Server) handleDeviceStart(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeErr(w, http.StatusNotFound, "auth disabled")
		return
	}
	out, err := s.auth.StartDeviceFlow(r.Context(), r.PathValue("provider"))
	if err != nil {
		writeAuthErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDevicePoll(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeErr(w, http.StatusNotFound, "auth disabled")
		return
	}
	var req struct {
		FlowID string `json:"flowId"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	status, res, err := s.auth.PollDeviceFlow(r.Context(), req.FlowID, clientIP(r), r.UserAgent())
	if err != nil {
		writeAuthErr(w, err)
		return
	}
	if status == "pending" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "pending"})
		return
	}
	s.setSessionCookie(w, res.Token)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "token": res.Token, "user": res.User})
}

func (s *Server) handleNativeExchange(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeErr(w, http.StatusNotFound, "auth disabled")
		return
	}
	var req struct {
		Ticket string `json:"ticket"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	u, token, err := s.auth.ExchangeNativeTicket(req.Ticket, clientIP(r), r.UserAgent())
	if err != nil {
		writeAuthErr(w, err)
		return
	}
	s.setSessionCookie(w, token)
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "user": u})
}

func authErrText(err error) string {
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		return "邮箱或密码错误"
	case errors.Is(err, auth.ErrEmailTaken):
		return "邮箱已注册"
	case errors.Is(err, auth.ErrDisabled):
		return "账号已停用"
	case errors.Is(err, auth.ErrEmailNotVerified):
		return "邮箱尚未验证"
	case errors.Is(err, auth.ErrInvalidToken):
		return "链接已失效，请重新发起"
	case errors.Is(err, auth.ErrRateLimited):
		return "操作过于频繁，请稍后再试"
	case errors.Is(err, auth.ErrWeakPassword):
		return "密码至少 10 位"
	case errors.Is(err, auth.ErrInvalidEmail):
		return "邮箱格式不正确"
	case errors.Is(err, auth.ErrRegistrationClosed):
		return "当前未开放注册"
	case errors.Is(err, auth.ErrProviderDisabled):
		return "该登录方式未启用"
	case errors.Is(err, auth.ErrIdentityTaken):
		return "该第三方账号已绑定到其他用户"
	case errors.Is(err, auth.ErrLastIdentity):
		return "至少保留一种登录方式"
	default:
		return "登录失败，请重试"
	}
}

func writeAuthErr(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials), errors.Is(err, auth.ErrInvalidToken):
		code = http.StatusUnauthorized
	case errors.Is(err, auth.ErrDisabled), errors.Is(err, auth.ErrEmailNotVerified),
		errors.Is(err, auth.ErrRegistrationClosed):
		code = http.StatusForbidden
	case errors.Is(err, auth.ErrEmailTaken), errors.Is(err, auth.ErrIdentityTaken),
		errors.Is(err, auth.ErrLastIdentity):
		code = http.StatusConflict
	case errors.Is(err, auth.ErrRateLimited):
		code = http.StatusTooManyRequests
	case errors.Is(err, auth.ErrProviderDisabled), errors.Is(err, store.ErrNotFound):
		code = http.StatusNotFound
	}
	writeJSON(w, code, map[string]string{"error": authErrText(err)})
}
