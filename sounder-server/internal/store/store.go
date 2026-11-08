// Package store 是控制面的会话元数据存储。
// M0 用内存实现；接 PostgreSQL 时只需换一个实现。
package store

import (
	"errors"
	"sort"
	"sync"
	"time"
)

var ErrNotFound = errors.New("session not found")

type Session struct {
	ID        string    `json:"id"`
	WorkerID  string    `json:"workerId"`
	Model     string    `json:"model"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type Store interface {
	Create(s Session) error
	Get(id string) (Session, error)
	List() []Session
	UpdateState(id, state string) error
	Delete(id string) error
}

type Memory struct {
	mu   sync.RWMutex
	data map[string]Session
}

func NewMemory() *Memory {
	return &Memory{data: make(map[string]Session)}
}

func (m *Memory) Create(s Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.data[s.ID]; ok {
		return errors.New("session already exists")
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now()
	}
	s.UpdatedAt = s.CreatedAt
	if s.State == "" {
		s.State = "created"
	}
	m.data[s.ID] = s
	return nil
}

func (m *Memory) Get(id string) (Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.data[id]
	if !ok {
		return Session{}, ErrNotFound
	}
	return s, nil
}

func (m *Memory) List() []Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Session, 0, len(m.data))
	for _, s := range m.data {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (m *Memory) UpdateState(id, state string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.data[id]
	if !ok {
		return ErrNotFound
	}
	s.State = state
	s.UpdatedAt = time.Now()
	m.data[id] = s
	return nil
}

func (m *Memory) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.data[id]; !ok {
		return ErrNotFound
	}
	delete(m.data, id)
	return nil
}
