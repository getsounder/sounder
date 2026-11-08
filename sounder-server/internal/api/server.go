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
	"strconv"
	"time"

	"github.com/getsounder/sounder-server/internal/gateway"
	"github.com/getsounder/sounder-server/internal/store"
)

type Server struct {
	store store.Store
	gw    *gateway.Client
	token string
}

func New(st store.Store, gw *gateway.Client, token string) *Server {
	return &Server{store: st, gw: gw, token: token}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/meta", s.handleMeta)
	mux.HandleFunc("GET /v1/workers", s.handleWorkers)
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
	return s.auth(mux)
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
	sess := store.Session{
		ID:        created.ID,
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

func (s *Server) handleList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"sessions": s.store.List()})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	sess, err := s.store.Get(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	sess, err := s.store.Get(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	_, _, _ = s.gw.RPC(r.Context(), sess.WorkerID, http.MethodDelete, "/v1/sessions/"+sess.ID, nil)
	_ = s.store.Delete(sess.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	sess, err := s.store.Get(r.PathValue("id"))
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
	sess, err := s.store.Get(r.PathValue("id"))
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
	sess, err := s.store.Get(r.PathValue("id"))
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
	sess, err := s.store.Get(r.PathValue("id"))
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
	body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	status, resp, err := s.gw.RPC(r.Context(), workerID, http.MethodPost,
		"/v1/approvals/"+r.PathValue("id"), json.RawMessage(body))
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
