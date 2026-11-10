// Package api 暴露 Gateway 的对外接口：
//   - Worker 侧：GET /v1/tunnel（WebSocket）
//   - 控制面侧：GET /v1/workers、POST /v1/workers/{id}/rpc、GET /v1/workers/{id}/events
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/getsounder/sounder-gateway/internal/auth"
	"github.com/getsounder/sounder-gateway/internal/protocol"
	"github.com/getsounder/sounder-gateway/internal/registry"
	"github.com/getsounder/sounder-gateway/internal/tunnel"
)

type Server struct {
	reg          *registry.Registry
	token        string
	workerSecret string
	up           websocket.Upgrader
}

func New(reg *registry.Registry, token, workerSecret string) *Server {
	return &Server{
		reg:          reg,
		token:        token,
		workerSecret: workerSecret,
		up: websocket.Upgrader{
			ReadBufferSize:  64 * 1024,
			WriteBufferSize: 64 * 1024,
			// 生产环境要校验 Origin / 使用 mTLS；这里只做 token。
			CheckOrigin: func(*http.Request) bool { return true },
		},
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/tunnel", s.handleTunnel)
	mux.HandleFunc("GET /v1/workers", s.handleList)
	mux.HandleFunc("GET /v1/workers/{id}", s.handleGet)
	mux.HandleFunc("POST /v1/workers/{id}/rpc", s.handleRPC)
	mux.HandleFunc("GET /v1/workers/{id}/events", s.handleEvents)
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
		// 隧道有自己的凭证体系：管理 token 或按设备签发的 worker token。
		if r.URL.Path == "/v1/tunnel" {
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

func (s *Server) handleTunnel(w http.ResponseWriter, r *http.Request) {
	// 凭证：管理 token（兼容）或某台设备专属的 worker token。
	token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	admin := s.token != "" && token == s.token
	var device auth.WorkerToken
	deviceOK := false
	if !admin && s.workerSecret != "" && token != "" {
		if dt, err := auth.VerifyWorkerToken(s.workerSecret, token); err == nil && dt.Use != "device-key" {
			device, deviceOK = dt, true
		}
	}
	if !admin && !deviceOK {
		writeErr(w, http.StatusUnauthorized, "invalid worker token")
		return
	}

	ws, err := s.up.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade: %v", err)
		return
	}

	// 心跳：收到 pong 就续读超时，避免空闲隧道被 90s 读超时踢掉。
	_ = ws.SetReadDeadline(time.Now().Add(90 * time.Second))
	ws.SetPongHandler(func(string) error {
		return ws.SetReadDeadline(time.Now().Add(90 * time.Second))
	})

	conn := tunnel.New(ws, s.reg.Publish, func(id string) {
		log.Printf("worker disconnected: %s", id)
		s.reg.Unregister(id)
	})

	// 第一帧必须是 register。
	_ = ws.SetReadDeadline(time.Now().Add(15 * time.Second))
	var hello protocol.Frame
	if err := ws.ReadJSON(&hello); err != nil || hello.Kind != protocol.KindRegister || hello.WorkerID == "" {
		_ = ws.WriteJSON(protocol.Frame{Kind: protocol.KindError, Error: "first frame must be register with workerId"})
		_ = ws.Close()
		return
	}
	// 设备 token 只能注册自己的 workerId，防止冒用别人的设备身份。
	if deviceOK && device.WorkerID != "" && hello.WorkerID != device.WorkerID {
		_ = ws.WriteJSON(protocol.Frame{Kind: protocol.KindError, Error: "worker token does not match workerId"})
		_ = ws.Close()
		return
	}

	// 心跳续期：Worker 每 25s 发 ping；收到就回 pong 并续读超时，
	// 空闲隧道不会因为 90s 没数据被判超时，也不会被 Cleanup 当 stale 摘掉。
	ws.SetPingHandler(func(appData string) error {
		s.reg.Touch(hello.WorkerID, -1)
		_ = ws.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(10*time.Second))
		return ws.SetReadDeadline(time.Now().Add(90 * time.Second))
	})
	ws.SetPongHandler(func(string) error {
		s.reg.Touch(hello.WorkerID, -1)
		return ws.SetReadDeadline(time.Now().Add(90 * time.Second))
	})

	conn.SetWorkerID(hello.WorkerID)
	s.reg.Register(&registry.Worker{
		ID:         hello.WorkerID,
		Kind:       hello.Kind2,
		Version:    hello.Version,
		Pool:       hello.Pool,
		Capacity:   hello.Capacity,
		Used:       hello.Used,
		RemoteAddr: r.RemoteAddr,
	}, conn)

	_ = conn.Write(protocol.Frame{Kind: protocol.KindAck, WorkerID: hello.WorkerID})
	log.Printf("worker connected: %s (%s, cap=%d)", hello.WorkerID, hello.Kind2, hello.Capacity)

	conn.ReadLoop()
}

func (s *Server) handleList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"workers": s.reg.List()})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	list := s.reg.List()
	for _, wk := range list {
		if wk.ID == r.PathValue("id") {
			writeJSON(w, http.StatusOK, wk)
			return
		}
	}
	writeErr(w, http.StatusNotFound, "worker not found")
}

type rpcRequest struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body,omitempty"`
}

func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	conn, ok := s.reg.Conn(id)
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, "worker offline")
		return
	}

	var req rpcRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	resp, err := conn.Call(ctx, protocol.Frame{
		Kind:   protocol.KindRPC,
		ID:     newID(),
		Method: req.Method,
		Path:   req.Path,
		Body:   req.Body,
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

// handleEvents 把 Worker 上行事件扇出给任意数量的控制端。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
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

	ch, cancel, ok := s.reg.Subscribe(r.PathValue("id"), after)
	if !ok {
		writeErr(w, http.StatusNotFound, "worker not found")
		return
	}
	defer cancel()

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
			fmt.Fprintf(w, "id: %d\ndata: {\"sessionId\":%q,\"event\":%s}\n\n", ev.Seq, ev.SessionID, ev.Payload)
			flusher.Flush()
		}
	}
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
