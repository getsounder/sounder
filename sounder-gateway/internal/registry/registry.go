package registry

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/getsounder/sounder-gateway/internal/tunnel"
)

type Event struct {
	Seq       int64
	SessionID string
	Payload   json.RawMessage
}

type Worker struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	Version     string    `json:"version"`
	Pool        string    `json:"pool,omitempty"`
	Capacity    int       `json:"capacity"`
	Used        int       `json:"used"`
	RemoteAddr  string    `json:"remoteAddr"`
	ConnectedAt time.Time `json:"connectedAt"`
	LastSeen    time.Time `json:"lastSeen"`

	events  []Event
	nextSeq int64
	subs    map[uint64]chan Event
	nextSub uint64
}

const eventRing = 512

type Registry struct {
	mu      sync.RWMutex
	workers map[string]*Worker
	conns   map[string]*tunnel.Conn
}

func New() *Registry {
	return &Registry{
		workers: make(map[string]*Worker),
		conns:   make(map[string]*tunnel.Conn),
	}
}

func (r *Registry) Register(w *Worker, c *tunnel.Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	w.ConnectedAt = now
	w.LastSeen = now
	w.subs = make(map[uint64]chan Event)
	r.workers[w.ID] = w
	r.conns[w.ID] = c
}

func (r *Registry) Unregister(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if w, ok := r.workers[id]; ok {
		for sid, ch := range w.subs {
			delete(w.subs, sid)
			close(ch)
		}
	}
	delete(r.workers, id)
	delete(r.conns, id)
}

func (r *Registry) Touch(id string, used int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if w, ok := r.workers[id]; ok {
		w.LastSeen = time.Now()
		w.Used = used
	}
}

func (r *Registry) Conn(id string) (*tunnel.Conn, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.conns[id]
	return c, ok
}

func (r *Registry) List() []Worker {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Worker, 0, len(r.workers))
	for _, w := range r.workers {
		out = append(out, *w)
	}
	return out
}

// Publish 由隧道的 onEvent 调用；seq 在 Gateway 侧单调递增，供 SSE 重放。
func (r *Registry) Publish(workerID, sessionID string, payload json.RawMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.workers[workerID]
	if !ok {
		return
	}
	w.nextSeq++
	ev := Event{Seq: w.nextSeq, SessionID: sessionID, Payload: payload}
	w.events = append(w.events, ev)
	if len(w.events) > eventRing {
		w.events = w.events[len(w.events)-eventRing:]
	}
	for _, ch := range w.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (r *Registry) Subscribe(workerID string, after int64) (<-chan Event, func(), bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.workers[workerID]
	if !ok {
		return nil, nil, false
	}
	ch := make(chan Event, 256)
	id := w.nextSub
	w.nextSub++
	for _, ev := range w.events {
		if ev.Seq > after {
			select {
			case ch <- ev:
			default:
			}
		}
	}
	w.subs[id] = ch
	cancel := func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if cur, ok := r.workers[workerID]; ok {
			if c, ok := cur.subs[id]; ok {
				delete(cur.subs, id)
				close(c)
			}
		}
	}
	return ch, cancel, true
}

// Cleanup 清理超过 timeout 没有心跳的连接，返回被清理的 workerID。
func (r *Registry) Cleanup(timeout time.Duration) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var dead []string
	for id, w := range r.workers {
		if time.Since(w.LastSeen) > timeout {
			dead = append(dead, id)
			for sid, ch := range w.subs {
				delete(w.subs, sid)
				close(ch)
			}
			delete(r.workers, id)
			delete(r.conns, id)
		}
	}
	return dead
}
