// Package gateway 是 sounder-gateway 的客户端：列出 Worker、转发 RPC、订阅事件。
package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	base   string
	token  string
	http   *http.Client
	stream *http.Client // 无超时，供 SSE 长连接使用
}

func New(base, token string) *Client {
	return &Client{
		base:   strings.TrimRight(base, "/"),
		token:  token,
		http:   &http.Client{Timeout: 90 * time.Second},
		stream: &http.Client{},
	}
}

type Worker struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Version  string `json:"version"`
	Pool     string `json:"pool"`
	Capacity int    `json:"capacity"`
	Used     int    `json:"used"`
}

type Event struct {
	Seq       int64
	SessionID string
	Payload   json.RawMessage
}

func (c *Client) ListWorkers(ctx context.Context) ([]Worker, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/v1/workers", nil)
	if err != nil {
		return nil, err
	}
	c.setAuth(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("gateway list workers: %s: %s", resp.Status, b)
	}

	var out struct {
		Workers []Worker `json:"workers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Workers, nil
}

// RPC 通过隧道调用 Worker 的 HTTP 接口。
func (c *Client) RPC(ctx context.Context, workerID, method, path string, body any) (int, json.RawMessage, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	payload, err := json.Marshal(map[string]any{
		"method": method,
		"path":   path,
		"body":   json.RawMessage(raw),
	})
	if err != nil {
		return 0, nil, err
	}

	url := fmt.Sprintf("%s/v1/workers/%s/rpc", c.base, workerID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuth(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, b, nil
}

// Events 订阅某个 Worker 的上行事件；返回的 channel 在 ctx 取消或连接关闭时关闭。
func (c *Client) Events(ctx context.Context, workerID string, after int64) (<-chan Event, error) {
	url := fmt.Sprintf("%s/v1/workers/%s/events?after=%d", c.base, workerID, after)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	c.setAuth(req)

	resp, err := c.stream.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("gateway events: %s", resp.Status)
	}

	out := make(chan Event, 256)
	go func() {
		defer close(out)
		defer resp.Body.Close()

		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 4<<20)

		var id int64
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "id:"):
				if n, err := strconv.ParseInt(strings.TrimSpace(line[3:]), 10, 64); err == nil {
					id = n
				}
			case strings.HasPrefix(line, "data:"):
				payload := strings.TrimSpace(line[5:])
				var wrapper struct {
					SessionID string          `json:"sessionId"`
					Event     json.RawMessage `json:"event"`
				}
				if json.Unmarshal([]byte(payload), &wrapper) != nil {
					continue
				}
				select {
				case out <- Event{Seq: id, SessionID: wrapper.SessionID, Payload: wrapper.Event}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

func (c *Client) setAuth(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}
