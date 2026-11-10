// Package approval 实现"危险工具执行前阻塞等待人工确认"。
//
// 语义（与架构文档 12.8.8 对齐）：
//   - 超时默认 deny，可配置
//   - approvalId 幂等：重复提交返回首次结果，绝不执行两次
//   - 支持 approve / deny / approve_with_edits
package approval

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	ErrNotFound = errors.New("approval not found")
	ErrInvalid  = errors.New("invalid approval request")
)

type Request struct {
	ID        string         `json:"id"`
	SessionID string         `json:"sessionId"`
	Tool      string         `json:"tool"`
	Level     string         `json:"level"`
	Reason    string         `json:"reason,omitempty"`
	Args      map[string]any `json:"args,omitempty"`
	CreatedAt time.Time      `json:"createdAt"`
	ExpiresAt time.Time      `json:"expiresAt"`
}

type Result struct {
	Allow     bool           `json:"allow"`
	Reason    string         `json:"reason,omitempty"`
	Patched   map[string]any `json:"patched,omitempty"`
	DecidedAt time.Time      `json:"decidedAt"`
}

type pending struct {
	req Request
	ch  chan Result
}

type Gate struct {
	mu      sync.Mutex
	pending map[string]*pending
	decided map[string]Result
	ttl     time.Duration
}

func NewGate(ttl time.Duration) *Gate {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &Gate{
		pending: make(map[string]*pending),
		decided: make(map[string]Result),
		ttl:     ttl,
	}
}

// Ask 阻塞直到有人审批、超时或 ctx 取消。返回值永远不是 error 决定是否执行。
func (g *Gate) Ask(ctx context.Context, req Request) (Result, error) {
	if req.ID == "" {
		return Result{}, ErrInvalid
	}
	now := time.Now()
	if req.CreatedAt.IsZero() {
		req.CreatedAt = now
	}
	req.ExpiresAt = req.CreatedAt.Add(g.ttl)

	g.mu.Lock()
	if r, ok := g.decided[req.ID]; ok { // 幂等：重复请求直接返回首次结果
		g.mu.Unlock()
		return r, nil
	}
	p := &pending{req: req, ch: make(chan Result, 1)}
	g.pending[req.ID] = p
	g.mu.Unlock()

	timer := time.NewTimer(g.ttl)
	defer timer.Stop()

	select {
	case r := <-p.ch:
		return r, nil
	case <-timer.C:
		return g.settle(req.ID, Result{Allow: false, Reason: "approval timeout"}), nil
	case <-ctx.Done():
		return g.settle(req.ID, Result{Allow: false, Reason: "canceled"}), ctx.Err()
	}
}

// Resolve 由 HTTP handler 调用；重复提交是幂等的。
func (g *Gate) Resolve(id string, r Result) (Result, error) {
	g.mu.Lock()
	if prev, ok := g.decided[id]; ok {
		g.mu.Unlock()
		return prev, nil
	}
	p, ok := g.pending[id]
	if !ok {
		g.mu.Unlock()
		return Result{}, ErrNotFound
	}
	if r.DecidedAt.IsZero() {
		r.DecidedAt = time.Now()
	}
	delete(g.pending, id)
	g.decided[id] = r
	g.mu.Unlock()

	p.ch <- r
	return r, nil
}

func (g *Gate) List() []Request {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Request, 0, len(g.pending))
	for _, p := range g.pending {
		out = append(out, p.req)
	}
	return out
}

// PendingFor 返回某会话当前的待处理数量，以及其中"提问"的数量。
func (g *Gate) PendingFor(sessionID string) (total int, questions int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range g.pending {
		if p.req.SessionID != sessionID {
			continue
		}
		total++
		if p.req.Level == "question" || p.req.Tool == "ask_user" {
			questions++
		}
	}
	return total, questions
}

// Cleanup 清掉过期的已决记录，避免长期运行内存增长。
func (g *Gate) Cleanup(before time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for id, r := range g.decided {
		if r.DecidedAt.Before(before) {
			delete(g.decided, id)
		}
	}
}

func (g *Gate) settle(id string, r Result) Result {
	r.DecidedAt = time.Now()
	g.mu.Lock()
	delete(g.pending, id)
	g.decided[id] = r
	g.mu.Unlock()
	return r
}
