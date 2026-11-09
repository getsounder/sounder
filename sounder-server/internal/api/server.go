// Package api 是控制面对外暴露的接口。
// 它不执行 Agent，只做会话编排、鉴权与事件转发。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/getsounder/sounder-server/internal/auth"
	"github.com/getsounder/sounder-server/internal/gateway"
	"github.com/getsounder/sounder-server/internal/store"
)

type Server struct {
	store     store.Store
	authStore store.AuthStore
	auth      *auth.Service
	gw        *gateway.Client
	token     string
}

func New(st store.Store, authStore store.AuthStore, authSvc *auth.Service, gw *gateway.Client, token string) *Server {
	return &Server{store: st, authStore: authStore, auth: authSvc, gw: gw, token: token}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/meta", s.handleMeta)
	mux.HandleFunc("GET /v1/workers", s.handleWorkers)

	mux.HandleFunc("GET /v1/auth/providers", s.handleAuthProviders)
	mux.HandleFunc("POST /v1/auth/register", s.handleRegister)
	mux.HandleFunc("POST /v1/auth/login", s.handleLogin)
	mux.HandleFunc("POST /v1/auth/logout", s.handleLogout)
	mux.HandleFunc("GET /v1/auth/me", s.handleMe)
	mux.HandleFunc("GET /v1/auth/sessions", s.handleAuthSessions)
	mux.HandleFunc("DELETE /v1/auth/sessions/{id}", s.handleRevokeSession)
	mux.HandleFunc("POST /v1/auth/password", s.handleChangePassword)
	mux.HandleFunc("POST /v1/auth/password/forgot", s.handleForgotPassword)
	mux.HandleFunc("POST /v1/auth/password/reset", s.handleResetPassword)
	mux.HandleFunc("GET /v1/auth/email/verify", s.handleVerifyEmail)
	mux.HandleFunc("POST /v1/auth/email/verify", s.handleResendVerify)
	mux.HandleFunc("GET /v1/auth/identities", s.handleIdentities)
	mux.HandleFunc("DELETE /v1/auth/identities/{provider}", s.handleUnlink)
	mux.HandleFunc("GET /v1/auth/oauth/{provider}/start", s.handleOAuthStart)
	mux.HandleFunc("GET /v1/auth/oauth/{provider}/callback", s.handleOAuthCallback)
	mux.HandleFunc("GET /v1/auth/oauth/{provider}/link/start", s.handleOAuthStart)
	mux.HandleFunc("POST /v1/auth/oauth/{provider}/device/start", s.handleDeviceStart)
	mux.HandleFunc("POST /v1/auth/oauth/{provider}/device/poll", s.handleDevicePoll)
	mux.HandleFunc("POST /v1/auth/native/exchange", s.handleNativeExchange)

	mux.HandleFunc("POST /v1/sessions", s.handleCreate)
	mux.HandleFunc("GET /v1/sessions", s.handleList)
	mux.HandleFunc("GET /v1/sessions/{id}", s.handleGet)
	mux.HandleFunc("DELETE /v1/sessions/{id}", s.handleDelete)
	mux.HandleFunc("POST /v1/sessions/{id}/prompts", s.handlePrompt)
	mux.HandleFunc("POST /v1/sessions/{id}/cancel", s.handleCancel)
	mux.HandleFunc("GET /v1/sessions/{id}/events", s.handleEvents)
	mux.HandleFunc("GET /v1/sessions/{id}/history", s.handleHistory)
	mux.HandleFunc("GET /v1/approvals", s.handleApprovals)
	mux.HandleFunc("POST /v1/approvals/{id}", s.handleApproval)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return s.cors(s.authMiddleware(mux))
}

// allowedOrigin 判断浏览器 Origin 是否属于配置的前端/服务地址。
// 只有白名单 Origin 才能带 Cookie（Allow-Credentials），其余一律用 * 且不带凭证，
// 避免任意站点复用用户 Cookie 读取接口响应。
func (s *Server) allowedOrigin(origin string) bool {
	if s.auth == nil || origin == "" {
		return false
	}
	for _, base := range []string{s.auth.Config().WebBaseURL, s.auth.Config().PublicBaseURL} {
		u, err := url.Parse(base)
		if err != nil || u.Scheme == "" || u.Host == "" {
			continue
		}
		if origin == u.Scheme+"://"+u.Host {
			return true
		}
	}
	return false
}

// cors 允许 WebView / 独立 Web 客户端（Origin 与本服务不同）直接调用。
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if s.allowedOrigin(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Vary", "Origin")
		} else {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Max-Age", "600")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	open := s.auth == nil && s.token == ""
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 探针不应需要凭证。
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		if ctx, ok := s.authenticate(r); ok {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authCtxKey{}, ctx)))
			return
		}
		if open {
			ctx := authContext{UserID: legacyUserID, Admin: true}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authCtxKey{}, ctx)))
			return
		}
		if publicAuthPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		writeErr(w, http.StatusUnauthorized, "unauthorized")
	})
}

func (s *Server) handleMeta(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":            "sounder",
		"protocolVersion": "v1",
		"store":           "memory",
	})
}

func (s *Server) handleWorkers(w http.ResponseWriter, r *http.Request) {
	ws, err := s.gw.ListWorkers(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workers": ws})
}

type createReq struct {
	WorkerID     string `json:"workerId,omitempty"`
	Model        string `json:"model,omitempty"`
	SystemPrompt string `json:"systemPrompt,omitempty"`
	ProjectID    string `json:"projectId,omitempty"`
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}

	workerID := req.WorkerID
	if workerID == "" {
		var err error
		workerID, err = s.pickWorker(r.Context())
		if err != nil {
			writeErr(w, http.StatusServiceUnavailable, err.Error())
			return
		}
	}

	status, body, err := s.gw.RPC(r.Context(), workerID, http.MethodPost, "/v1/sessions", map[string]any{
		"model":        req.Model,
		"systemPrompt": req.SystemPrompt,
		"projectId":    req.ProjectID,
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if status != http.StatusCreated {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
		return
	}

	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.ID == "" {
		writeErr(w, http.StatusBadGateway, "worker returned invalid session")
		return
	}

	now := time.Now()
	ctx := authFrom(r)
	owner := ctx.UserID
	if owner == "" {
		owner = legacyUserID
	}
	sess := store.Session{
		ID:        created.ID,
		OwnerID:   owner,
		WorkerID:  workerID,
		Model:     req.Model,
		State:     "running",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.store.Create(sess); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sess)
}

func (s *Server) pickWorker(ctx context.Context) (string, error) {
	ws, err := s.gw.ListWorkers(ctx)
	if err != nil {
		return "", err
	}
	for _, w := range ws {
		if w.Kind == "cloud" && w.Used < w.Capacity {
			return w.ID, nil
		}
	}
	for _, w := range ws {
		if w.Used < w.Capacity {
			return w.ID, nil
		}
	}
	return "", errors.New("no worker available")
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	ctx := authFrom(r)
	if ctx.Admin {
		writeJSON(w, http.StatusOK, map[string]any{"sessions": s.store.List()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": s.store.ListByOwner(ctx.UserID)})
}

// ownedSession 读取当前登录用户拥有的会话；管理员（-token 兼容模式）可见全部。
func (s *Server) ownedSession(r *http.Request, id string) (store.Session, error) {
	ctx := authFrom(r)
	if ctx.Admin {
		return s.store.Get(id)
	}
	return s.store.GetOwned(id, ctx.UserID)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	sess, err := s.ownedSession(r, r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	sess, err := s.ownedSession(r, r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	_, _, _ = s.gw.RPC(r.Context(), sess.WorkerID, http.MethodDelete, "/v1/sessions/"+sess.ID, nil)
	_ = s.store.Delete(sess.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	sess, err := s.ownedSession(r, r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}

	var req struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if req.Message == "" {
		writeErr(w, http.StatusBadRequest, "message required")
		return
	}

	status, body, err := s.gw.RPC(r.Context(), sess.WorkerID, http.MethodPost,
		"/v1/sessions/"+sess.ID+"/prompts", map[string]string{"message": req.Message})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if status != http.StatusAccepted {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
		return
	}

	_ = s.store.UpdateState(sess.ID, "running")
	writeJSON(w, http.StatusAccepted, map[string]string{"disposition": "started"})
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	sess, err := s.ownedSession(r, r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	_, _, _ = s.gw.RPC(r.Context(), sess.WorkerID, http.MethodPost,
		"/v1/sessions/"+sess.ID+"/cancel", nil)
	w.WriteHeader(http.StatusNoContent)
}

// handleEvents 把某个 Worker 的事件流按 sessionId 过滤后转发给控制端。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	sess, err := s.ownedSession(r, r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			after = n
		}
	}

	ch, err := s.gw.Events(r.Context(), sess.WorkerID, after)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.SessionID != sess.ID {
				continue
			}
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ev.Seq, ev.Payload)
			flusher.Flush()

			if isSettled(ev.Payload) {
				_ = s.store.UpdateState(sess.ID, "idle")
			}
		}
	}
}

// handleHistory 从 Worker 的 Session JSONL 投影历史消息，供 UI 刷新后回填。
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	sess, err := s.ownedSession(r, r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	status, body, err := s.gw.RPC(r.Context(), sess.WorkerID, http.MethodGet,
		"/v1/sessions/"+sess.ID+"/history", nil)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (s *Server) ownedSessionSet(r *http.Request) map[string]bool {
	ctx := authFrom(r)
	if ctx.Admin {
		return nil
	}
	owned := make(map[string]bool)
	for _, sess := range s.store.ListByOwner(ctx.UserID) {
		owned[sess.ID] = true
	}
	return owned
}

// filterApprovals 只保留属于当前用户会话的审批请求。
func filterApprovals(r *http.Request, owned map[string]bool, body []byte) []byte {
	if owned == nil {
		return body
	}
	var payload struct {
		Approvals []json.RawMessage `json:"approvals"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	kept := make([]json.RawMessage, 0, len(payload.Approvals))
	for _, raw := range payload.Approvals {
		var probe struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(raw, &probe) == nil && owned[probe.SessionID] {
			kept = append(kept, raw)
		}
	}
	out, err := json.Marshal(map[string]any{"approvals": kept})
	if err != nil {
		return body
	}
	return out
}

func (s *Server) handleApprovals(w http.ResponseWriter, r *http.Request) {
	workerID := r.URL.Query().Get("workerId")
	if workerID == "" {
		writeErr(w, http.StatusBadRequest, "workerId required")
		return
	}
	status, body, err := s.gw.RPC(r.Context(), workerID, http.MethodGet, "/v1/approvals", nil)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	body = filterApprovals(r, s.ownedSessionSet(r), body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (s *Server) handleApproval(w http.ResponseWriter, r *http.Request) {
	workerID := r.URL.Query().Get("workerId")
	if workerID == "" {
		writeErr(w, http.StatusBadRequest, "workerId required")
		return
	}
	approvalID := r.PathValue("id")
	// 确认审批属于当前用户的会话，避免越权批准他人的危险操作。
	status, body, err := s.gw.RPC(r.Context(), workerID, http.MethodGet, "/v1/approvals", nil)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if status != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
		return
	}
	var payload struct {
		Approvals []struct {
			ID        string `json:"id"`
			SessionID string `json:"sessionId"`
		} `json:"approvals"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		writeErr(w, http.StatusBadGateway, "invalid worker approval list")
		return
	}
	owned := s.ownedSessionSet(r)
	found := false
	for _, a := range payload.Approvals {
		if a.ID == approvalID && (owned == nil || owned[a.SessionID]) {
			found = true
			break
		}
	}
	if !found {
		writeErr(w, http.StatusNotFound, "approval not found")
		return
	}

	reqBody, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	status, resp, err := s.gw.RPC(r.Context(), workerID, http.MethodPost,
		"/v1/approvals/"+approvalID, json.RawMessage(reqBody))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(resp)
}

func isSettled(payload json.RawMessage) bool {
	var probe struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(payload, &probe) != nil {
		return false
	}
	return probe.Type == "session.settled"
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode: %v", err)
	}
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
