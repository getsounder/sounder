// Package api 暴露 Worker 的唯一 HTTP 契约。控制端（Web/Desktop/Mobile）只依赖它。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/getsounder/sounder-worker/internal/approval"
	"github.com/getsounder/sounder-worker/internal/protocol"
	"github.com/getsounder/sounder-worker/internal/session"
)

type Server struct {
	mgr   *session.Manager
	gate  *approval.Gate
	meta  protocol.Meta
	token string
}

func New(mgr *session.Manager, gate *approval.Gate, meta protocol.Meta, token string) *Server {
	return &Server{mgr: mgr, gate: gate, meta: meta, token: token}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/meta", s.handleMeta)
	mux.HandleFunc("POST /v1/sessions", s.handleCreate)
	mux.HandleFunc("GET /v1/sessions", s.handleList)
	mux.HandleFunc("DELETE /v1/sessions/{id}", s.handleDelete)
	mux.HandleFunc("POST /v1/sessions/{id}/prompts", s.handlePrompt)
	mux.HandleFunc("POST /v1/sessions/{id}/cancel", s.handleCancel)
	mux.HandleFunc("GET /v1/sessions/{id}/events", s.handleEvents)
	mux.HandleFunc("GET /v1/sessions/{id}/history", s.handleHistory)
	mux.HandleFunc("GET /v1/approvals", s.handleApprovalList)
	mux.HandleFunc("POST /v1/approvals/{id}", s.handleApprovalResolve)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	return s.cors(s.auth(mux))
}

// cors 允许桌面/移动 WebView 以自定义 scheme（例如 wails://wails）跨源访问本地 Worker。
// Worker 用 Bearer token 鉴权，不依赖 Cookie，因此可以回显任意 Origin；预检请求直接放行。
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Last-Event-ID")
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) auth(next http.Handler) http.Handler {
	if s.token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 探针不应需要凭证。
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+s.token {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleMeta(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.meta)
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req protocol.CreateSessionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}

	actor, err := s.mgr.Create(req)
	if err != nil {
		if errors.Is(err, session.ErrFull) {
			writeErr(w, http.StatusTooManyRequests, "worker at capacity")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, protocol.CreateSessionResponse{
		ID:        actor.ID,
		Model:     actor.ModelID,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"sessions": s.mgr.Views(),
		"capacity": s.mgr.Capacity(),
		"used":     s.mgr.Len(),
	})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	s.mgr.Delete(r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.mgr.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}

	var req protocol.PromptRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if req.Message == "" {
		writeErr(w, http.StatusBadRequest, "message required")
		return
	}

	// 不能用 r.Context()：HTTP 202 返回后它就被取消，Agent 会被立刻中断。
	if err := actor.Prompt(context.WithoutCancel(r.Context()), req.Message); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	// 立即返回，结果通过 SSE 推送。与 PiG RPC 的 "started" 语义一致。
	writeJSON(w, http.StatusAccepted, map[string]string{"disposition": "started"})
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.mgr.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	actor.Cancel()
	w.WriteHeader(http.StatusNoContent)
}

// handleEvents 是 SSE。多端可同时订阅，各自带 after 游标。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.mgr.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "session not found")
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

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, cancel := actor.Subscribe(after)
	defer cancel()

	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case env, ok := <-ch:
			if !ok {
				return
			}
			b, err := json.Marshal(env)
			if err != nil {
				log.Printf("marshal event: %v", err)
				continue
			}
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", env.Seq, b)
			flusher.Flush()
		}
	}
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.mgr.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	path := actor.HistoryPath()
	if path == "" {
		writeJSON(w, http.StatusOK, map[string]any{"messages": []any{}})
		return
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	msgs, err := session.ReadHistory(path, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs})
}

func (s *Server) handleApprovalList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"approvals": s.gate.List()})
}

func (s *Server) handleApprovalResolve(w http.ResponseWriter, r *http.Request) {
	var res approval.Result
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&res); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	out, err := s.gate.Resolve(r.PathValue("id"), res)
	if err != nil {
		if errors.Is(err, approval.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "approval not found or already resolved")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	if s.mgr.Len() >= s.mgr.Capacity() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ready": false, "reason": "at capacity",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ready": true})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
