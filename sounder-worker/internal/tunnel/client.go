// Package tunnel 让 Worker 主动连到 sounder-gateway，接收转发的命令并上送事件。
// 桌面 Worker 在 NAT 后面时，只能靠这条出站连接被控制。
package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/getsounder/sounder-worker/internal/protocol"
)

type wireFrame struct {
	Kind      string          `json:"kind"`
	ID        string          `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Path      string          `json:"path,omitempty"`
	Body      json.RawMessage `json:"body,omitempty"`
	Status    int             `json:"status,omitempty"`
	Error     string          `json:"error,omitempty"`
	WorkerID  string          `json:"workerId,omitempty"`
	Version   string          `json:"version,omitempty"`
	Capacity  int             `json:"capacity,omitempty"`
	Used      int             `json:"used,omitempty"`
	Kind2     string          `json:"workerKind,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Event     json.RawMessage `json:"event,omitempty"`
}

type Client struct {
	url        string
	token      string
	localToken string
	workerID   string
	kind     string
	version  string
	capacity int
	used     func() int
	local    string

	send   chan []byte
	client *http.Client
}

func New(url, token, localToken, workerID, kind, version, localBase string, capacity int, used func() int) *Client {
	return &Client{
		url:        url,
		token:      token,
		localToken: localToken,
		workerID:   workerID,
		kind:     kind,
		version:  version,
		capacity: capacity,
		used:     used,
		local:    localBase,
		send:     make(chan []byte, 1024),
		client:   &http.Client{Timeout: 60 * time.Second},
	}
}

// Run 维持连接，断线后指数退避重连，直到 ctx 取消。
func (c *Client) Run(ctx context.Context) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		err := c.session(ctx)
		if ctx.Err() != nil {
			return
		}
		log.Printf("tunnel disconnected: %v (retry in %s)", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (c *Client) session(ctx context.Context) error {
	header := http.Header{}
	if c.token != "" {
		header.Set("Authorization", "Bearer "+c.token)
	}

	ws, _, err := websocket.DefaultDialer.DialContext(ctx, c.url, header)
	if err != nil {
		return err
	}
	defer ws.Close()

	var writeMu sync.Mutex
	write := func(f wireFrame) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return ws.WriteJSON(f)
	}

	if err := write(wireFrame{
		Kind:     "register",
		WorkerID: c.workerID,
		Kind2:    c.kind,
		Version:  c.version,
		Capacity: c.capacity,
	}); err != nil {
		return err
	}

	done := make(chan struct{})
	defer close(done)

	// 写循环：事件与心跳共用一条串行通道，避免并发写 WebSocket。
	go func() {
		h := time.NewTicker(20 * time.Second)
		defer h.Stop()
		for {
			select {
			case <-done:
				return
			case <-h.C:
				used := 0
				if c.used != nil {
					used = c.used()
				}
				if err := write(wireFrame{Kind: "heartbeat", Used: used}); err != nil {
					return
				}
			case raw := <-c.send:
				if err := ws.WriteMessage(websocket.TextMessage, raw); err != nil {
					return
				}
			}
		}
	}()

	for {
		_ = ws.SetReadDeadline(time.Now().Add(120 * time.Second))
		var f wireFrame
		if err := ws.ReadJSON(&f); err != nil {
			return err
		}
		if f.Kind != "rpc" {
			continue
		}
		go func(f wireFrame) {
			res := c.forward(ctx, f)
			_ = write(res)
		}(f)
	}
}

// forward 把 Gateway 转来的命令打到本地 Worker HTTP 服务。
func (c *Client) forward(ctx context.Context, f wireFrame) wireFrame {
	req, err := http.NewRequestWithContext(ctx, f.Method, c.local+f.Path, bytes.NewReader(f.Body))
	if err != nil {
		return wireFrame{Kind: "error", ID: f.ID, Error: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	if c.localToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.localToken)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return wireFrame{Kind: "error", ID: f.ID, Error: err.Error()}
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return wireFrame{Kind: "rpc_result", ID: f.ID, Status: resp.StatusCode, Body: body}
}

// Publish 上行一个事件；队列满时丢弃，绝不阻塞 Agent。
func (c *Client) Publish(env protocol.Envelope) {
	raw, err := json.Marshal(env)
	if err != nil {
		return
	}
	f := wireFrame{Kind: "event", SessionID: env.SessionID, Event: raw}
	b, err := json.Marshal(f)
	if err != nil {
		return
	}
	select {
	case c.send <- b:
	default:
		log.Printf("tunnel send queue full, dropping event seq=%d", env.Seq)
	}
}

var _ = fmt.Sprintf
