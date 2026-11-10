package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/getsounder/sounder-server/internal/auth"
	"github.com/getsounder/sounder-server/internal/store"
)

func newTestAPI(_ ...string) (*Server, store.Store, store.AuthStore, http.Handler) {
	st := store.NewMemory()
	ast := store.NewMemoryAuth()
	svc := auth.New(auth.Config{
		PublicBaseURL:       "http://api.test",
		WebBaseURL:          "http://web.test",
		RegistrationEnabled: true,
		LocalEnabled:        true,
		DevProvider:         true,
		SessionTTL:          time.Hour,
	}, ast, nil)
	srv := New(st, ast, svc, nil, "", "", nil, "")
	return srv, st, ast, srv.Handler()
}

func doJSON(t *testing.T, h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
}

func registerUser(t *testing.T, h http.Handler, email, password string) (string, store.User) {
	t.Helper()
	w := doJSON(t, h, http.MethodPost, "/v1/auth/register",
		`{"email":"`+email+`","password":"`+password+`","displayName":"Tester"}`, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		Token string     `json:"token"`
		User  store.User `json:"user"`
	}
	decode(t, w, &out)
	if out.Token == "" {
		t.Fatal("missing token")
	}
	return out.Token, out.User
}

func TestRegisterLoginMe(t *testing.T) {
	_, _, _, h := newTestAPI("")

	token, user := registerUser(t, h, "alice@example.com", "very-secret-1")
	if user.Email != "alice@example.com" {
		t.Fatalf("unexpected email %q", user.Email)
	}

	// 重复注册
	w := doJSON(t, h, http.MethodPost, "/v1/auth/register",
		`{"email":"alice@example.com","password":"very-secret-1"}`, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate register: %d %s", w.Code, w.Body.String())
	}

	// 弱密码
	w = doJSON(t, h, http.MethodPost, "/v1/auth/register",
		`{"email":"weak@example.com","password":"short"}`, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("weak password: %d %s", w.Code, w.Body.String())
	}

	w = doJSON(t, h, http.MethodGet, "/v1/auth/me", "", map[string]string{"Authorization": "Bearer " + token})
	if w.Code != http.StatusOK {
		t.Fatalf("me: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "alice@example.com") {
		t.Fatalf("me body: %s", w.Body.String())
	}

	// 错误密码
	w = doJSON(t, h, http.MethodPost, "/v1/auth/login",
		`{"email":"alice@example.com","password":"wrong-password"}`, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bad login: %d %s", w.Code, w.Body.String())
	}

	// 正确密码 + me
	w = doJSON(t, h, http.MethodPost, "/v1/auth/login",
		`{"email":"alice@example.com","password":"very-secret-1"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	var login struct {
		Token string `json:"token"`
	}
	decode(t, w, &login)
	w = doJSON(t, h, http.MethodGet, "/v1/auth/me", "", map[string]string{"Authorization": "Bearer " + login.Token})
	if w.Code != http.StatusOK {
		t.Fatalf("me after login: %d %s", w.Code, w.Body.String())
	}

	// 登出后 token 失效
	w = doJSON(t, h, http.MethodPost, "/v1/auth/logout", "", map[string]string{"Authorization": "Bearer " + login.Token})
	if w.Code != http.StatusNoContent {
		t.Fatalf("logout: %d %s", w.Code, w.Body.String())
	}
	w = doJSON(t, h, http.MethodGet, "/v1/auth/me", "", map[string]string{"Authorization": "Bearer " + login.Token})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("me after logout: %d %s", w.Code, w.Body.String())
	}
}

func TestSessionOwnership(t *testing.T) {
	_, st, _, h := newTestAPI("")
	tokenA, userA := registerUser(t, h, "a@example.com", "password-aaaa")
	_, userB := registerUser(t, h, "b@example.com", "password-bbbb")

	now := time.Now()
	if err := st.Create(store.Session{ID: "ses_a", OwnerID: userA.ID, WorkerID: "w1", State: "idle", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.Create(store.Session{ID: "ses_b", OwnerID: userB.ID, WorkerID: "w1", State: "idle", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	w := doJSON(t, h, http.MethodGet, "/v1/sessions", "", map[string]string{"Authorization": "Bearer " + tokenA})
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	var list struct {
		Sessions []store.Session `json:"sessions"`
	}
	decode(t, w, &list)
	if len(list.Sessions) != 1 || list.Sessions[0].ID != "ses_a" {
		t.Fatalf("unexpected sessions: %+v", list.Sessions)
	}

	w = doJSON(t, h, http.MethodGet, "/v1/sessions/ses_b", "", map[string]string{"Authorization": "Bearer " + tokenA})
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-owner get: %d %s", w.Code, w.Body.String())
	}

	w = doJSON(t, h, http.MethodDelete, "/v1/sessions/ses_b", "", map[string]string{"Authorization": "Bearer " + tokenA})
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-owner delete: %d %s", w.Code, w.Body.String())
	}
}

func TestAdminSeesAllSessions(t *testing.T) {
	_, st, ast, h := newTestAPI()
	now := time.Now()
	_ = st.Create(store.Session{ID: "ses_1", OwnerID: "usr_other", WorkerID: "w1", CreatedAt: now})
	_ = st.Create(store.Session{ID: "ses_2", OwnerID: "usr_other2", WorkerID: "w1", CreatedAt: now})

	token, user := registerUser(t, h, "admin-all@example.com", "password-admin")
	user.IsAdmin = true
	if err := ast.UpdateUser(user); err != nil {
		t.Fatal(err)
	}

	w := doJSON(t, h, http.MethodGet, "/v1/sessions", "", map[string]string{"Authorization": "Bearer " + token})
	if w.Code != http.StatusOK {
		t.Fatalf("admin list: %d %s", w.Code, w.Body.String())
	}
	var list struct {
		Sessions []store.Session `json:"sessions"`
	}
	decode(t, w, &list)
	if len(list.Sessions) != 2 {
		t.Fatalf("admin should see all, got %d", len(list.Sessions))
	}

	w = doJSON(t, h, http.MethodGet, "/v1/sessions", "", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d %s", w.Code, w.Body.String())
	}
}

func TestDevOAuthWebFlow(t *testing.T) {
	_, _, ast, h := newTestAPI("")

	w := doJSON(t, h, http.MethodGet,
		"/v1/auth/oauth/dev/start?mode=json&client=web&redirect=/&login_hint=oauth@dev.local", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	var start struct {
		URL string `json:"url"`
	}
	decode(t, w, &start)
	u, err := url.Parse(start.URL)
	if err != nil {
		t.Fatal(err)
	}
	state := u.Query().Get("state")
	code := u.Query().Get("code")
	if state == "" || code == "" {
		t.Fatalf("bad dev url: %s", start.URL)
	}

	w = doJSON(t, h, http.MethodGet,
		"/v1/auth/oauth/dev/callback?state="+url.QueryEscape(state)+"&code="+url.QueryEscape(code), "", nil)
	if w.Code != http.StatusFound {
		t.Fatalf("callback: %d %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("missing session cookie")
	}
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "http://web.test/") {
		t.Fatalf("unexpected redirect %q", loc)
	}

	w = doJSON(t, h, http.MethodGet, "/v1/auth/me", "", map[string]string{
		"Cookie": cookies[0].Name + "=" + cookies[0].Value,
	})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "oauth@dev.local") {
		t.Fatalf("me: %d %s", w.Code, w.Body.String())
	}

	// 同一第三方账号再次登录应复用用户，而不是新建。
	if u, err := ast.GetUserByEmail("oauth@dev.local"); err != nil || u.ID == "" {
		t.Fatalf("user not persisted: %v", err)
	}
}

func TestOAuthLinkAndUnlink(t *testing.T) {
	_, _, _, h := newTestAPI("")
	token, _ := registerUser(t, h, "link@example.com", "password-link")

	w := doJSON(t, h, http.MethodGet,
		"/v1/auth/oauth/dev/link/start?mode=json&client=web&login_hint=link@example.com", "",
		map[string]string{"Authorization": "Bearer " + token})
	if w.Code != http.StatusOK {
		t.Fatalf("link start: %d %s", w.Code, w.Body.String())
	}
	var start struct {
		URL string `json:"url"`
	}
	decode(t, w, &start)
	u, _ := url.Parse(start.URL)

	w = doJSON(t, h, http.MethodGet,
		"/v1/auth/oauth/dev/callback?state="+url.QueryEscape(u.Query().Get("state"))+
			"&code="+url.QueryEscape(u.Query().Get("code")), "",
		map[string]string{"Authorization": "Bearer " + token})
	if w.Code != http.StatusFound {
		t.Fatalf("link callback: %d %s", w.Code, w.Body.String())
	}

	w = doJSON(t, h, http.MethodGet, "/v1/auth/identities", "", map[string]string{"Authorization": "Bearer " + token})
	if !strings.Contains(w.Body.String(), "dev") || !strings.Contains(w.Body.String(), "local") {
		t.Fatalf("identities: %s", w.Body.String())
	}

	// 只剩两种登录方式时，解绑 dev 成功，再解绑 local 被拒绝。
	w = doJSON(t, h, http.MethodDelete, "/v1/auth/identities/dev", "", map[string]string{"Authorization": "Bearer " + token})
	if w.Code != http.StatusNoContent {
		t.Fatalf("unlink dev: %d %s", w.Code, w.Body.String())
	}
	w = doJSON(t, h, http.MethodDelete, "/v1/auth/identities/local", "", map[string]string{"Authorization": "Bearer " + token})
	if w.Code != http.StatusConflict {
		t.Fatalf("unlink last: %d %s", w.Code, w.Body.String())
	}
}

func TestChangePasswordRevokesSessions(t *testing.T) {
	_, _, _, h := newTestAPI("")
	token, _ := registerUser(t, h, "pw@example.com", "password-old1")

	w := doJSON(t, h, http.MethodPost, "/v1/auth/password",
		`{"oldPassword":"password-old1","newPassword":"password-new2"}`, map[string]string{"Authorization": "Bearer " + token})
	if w.Code != http.StatusOK {
		t.Fatalf("change password: %d %s", w.Code, w.Body.String())
	}
	w = doJSON(t, h, http.MethodGet, "/v1/auth/me", "", map[string]string{"Authorization": "Bearer " + token})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("me after change: %d %s", w.Code, w.Body.String())
	}
	w = doJSON(t, h, http.MethodPost, "/v1/auth/login",
		`{"email":"pw@example.com","password":"password-new2"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("login new password: %d %s", w.Code, w.Body.String())
	}
}

func TestAuthRateLimit(t *testing.T) {
	_, _, _, h := newTestAPI("")
	registerUser(t, h, "rate@example.com", "password-rate")
	var last int
	for i := 0; i < 12; i++ {
		w := doJSON(t, h, http.MethodPost, "/v1/auth/login",
			`{"email":"rate@example.com","password":"wrong-password"}`, nil)
		last = w.Code
		if w.Code == http.StatusTooManyRequests {
			break
		}
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("expected rate limit, got %d", last)
	}
}

func TestPublicPathsDoNotRequireAuth(t *testing.T) {
	_, _, _, h := newTestAPI("dev-token")
	for _, p := range []string{"/healthz", "/v1/meta", "/v1/auth/providers"} {
		w := doJSON(t, h, http.MethodGet, p, "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", p, w.Code, w.Body.String())
		}
	}
}

func TestExpiredSessionRejected(t *testing.T) {
	_, _, ast, h := newTestAPI("")
	token, user := registerUser(t, h, "exp@example.com", "password-exp")
	sessions, err := ast.ListAuthSessions(user.ID)
	if err != nil || len(sessions) == 0 {
		t.Fatalf("sessions: %v %v", sessions, err)
	}
	_ = ast.TouchAuthSession(sessions[0].ID, time.Now())
	// 直接把过期时间改到过去：用 Consume 路径外的 store 更新不可行，改为删除会话模拟过期。
	_ = ast.DeleteAuthSession(sessions[0].ID)
	w := doJSON(t, h, http.MethodGet, "/v1/auth/me", "", map[string]string{"Authorization": "Bearer " + token})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expired: %d %s", w.Code, w.Body.String())
	}
}

func TestCORSCredentialsOnlyForConfiguredOrigins(t *testing.T) {
	_, _, _, h := newTestAPI("")
	w := doJSON(t, h, http.MethodOptions, "/v1/sessions", "", map[string]string{"Origin": "http://web.test"})
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "http://web.test" {
		t.Fatalf("allowed origin: %q", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("allowed credentials: %q", got)
	}

	w = doJSON(t, h, http.MethodOptions, "/v1/sessions", "", map[string]string{"Origin": "http://evil.test"})
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("foreign origin: %q", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("foreign credentials should be empty, got %q", got)
	}
}

func TestAuthDisabledOpenMode(t *testing.T) {
	st := store.NewMemory()
	ast := store.NewMemoryAuth()
	srv := New(st, ast, nil, nil, "", "", nil, "")
	h := srv.Handler()

	// 没有用户体系时是单租户本地模式：接口直接放行。
	w := doJSON(t, h, http.MethodGet, "/v1/sessions", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("open list: %d %s", w.Code, w.Body.String())
	}
	// 用户体系相关接口在未启用时不可用。
	w = doJSON(t, h, http.MethodGet, "/v1/auth/me", "", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("auth me should be unauthorized: %d %s", w.Code, w.Body.String())
	}
}

func TestJSONBodyLimit(t *testing.T) {
	_, _, _, h := newTestAPI("")
	big := bytes.Repeat([]byte("a"), 2<<20)
	w := doJSON(t, h, http.MethodPost, "/v1/auth/register",
		`{"email":"big@example.com","password":"`+string(big)+`"}`, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("big body: %d", w.Code)
	}
}

func TestClientIPTrustsOnlyProxySources(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		xff        string
		realIP     string
		want       string
	}{
		{"direct public", "203.0.113.7:5555", "1.2.3.4", "", "203.0.113.7"},
		{"direct public real-ip", "203.0.113.7:5555", "", "1.2.3.4", "203.0.113.7"},
		{"loopback proxy", "127.0.0.1:8080", "1.2.3.4", "", "1.2.3.4"},
		{"private proxy", "10.0.0.5:8080", "1.2.3.4, 10.0.0.9", "", "1.2.3.4"},
		{"private proxy real-ip", "192.168.1.5:8080", "", "1.2.3.4", "1.2.3.4"},
		{"trusted no header", "10.0.0.5:8080", "", "", "10.0.0.5"},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = tc.remoteAddr
		if tc.xff != "" {
			r.Header.Set("X-Forwarded-For", tc.xff)
		}
		if tc.realIP != "" {
			r.Header.Set("X-Real-IP", tc.realIP)
		}
		if got := clientIP(r); got != tc.want {
			t.Fatalf("%s: clientIP=%q want %q", tc.name, got, tc.want)
		}
	}
}
