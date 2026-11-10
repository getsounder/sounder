// Package store 是控制面的会话元数据与用户身份存储。
// M0 用内存实现；接 PostgreSQL 时只需换一个实现。
package store

import (
	"errors"
	"sort"
	"sync"
	"time"
)

var ErrNotFound = errors.New("not found")

type Session struct {
	ID        string    `json:"id"`
	OwnerID   string    `json:"ownerId,omitempty"`
	WorkerID  string    `json:"workerId"`
	Model     string    `json:"model"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type Store interface {
	Create(s Session) error
	Get(id string) (Session, error)
	// GetOwned 只在会话属于 ownerID 时返回；否则返回 ErrNotFound。
	GetOwned(id, ownerID string) (Session, error)
	List() []Session
	ListByOwner(ownerID string) []Session
	UpdateState(id, state string) error
	Delete(id string) error

	// 设备 = 一个 Worker 运行环境（桌面 container / 桌面 process / 云端容器）。
	EnsureDevice(d Device) (Device, error)
	ListDevices(userID string) ([]Device, error)
	RenameDevice(id, userID, name string) error
	DeleteDevice(id, userID string) error

	// 配额：云端设备数量与单设备规格。桌面本地设备不受 max_devices 限制。
	GetQuota(userID string) (Quota, error)
	SetQuota(q Quota) error
}

// Quota 是单个用户的资源配额。
type Quota struct {
	UserID       string `json:"userId,omitempty"`
	MaxDevices   int    `json:"maxDevices"`   // 云端设备上限
	DeviceCPU    int    `json:"deviceCpu"`    // 单设备 vCPU
	DeviceMemMB  int    `json:"deviceMemMb"`  // 单设备内存
	DeviceDiskGB int    `json:"deviceDiskGb"` // 单设备磁盘
}

// DefaultQuota 是新用户的默认配额：默认只允许一个云端设备。
func DefaultQuota(userID string) Quota {
	return Quota{
		UserID:       userID,
		MaxDevices:   1,
		DeviceCPU:    2,
		DeviceMemMB:  4096,
		DeviceDiskGB: 20,
	}
}

// Device 是用户与 Worker 运行环境的绑定关系。
// 在线状态不在库里，读取时用网关的 worker 列表实时合并。
type Device struct {
	ID         string    `json:"id"`
	UserID     string    `json:"userId"`
	WorkerID   string    `json:"workerId"`
	Name       string    `json:"name"`
	Kind       string    `json:"kind"` // desktop-container | desktop-process | cloud
	Platform   string    `json:"platform"`
	Version    string    `json:"version"`
	LastSeenAt time.Time `json:"lastSeenAt"`
	CreatedAt  time.Time `json:"createdAt"`
}

type Memory struct {
	mu      sync.RWMutex
	data    map[string]Session
	devices map[string]Device
	quotas  map[string]Quota
}

func NewMemory() *Memory {
	return &Memory{
		data:    make(map[string]Session),
		devices: make(map[string]Device),
		quotas:  make(map[string]Quota),
	}
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

func (m *Memory) GetOwned(id, ownerID string) (Session, error) {
	s, err := m.Get(id)
	if err != nil {
		return Session{}, err
	}
	if s.OwnerID != ownerID {
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

func (m *Memory) ListByOwner(ownerID string) []Session {
	all := m.List()
	out := make([]Session, 0, len(all))
	for _, s := range all {
		if s.OwnerID == ownerID {
			out = append(out, s)
		}
	}
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
