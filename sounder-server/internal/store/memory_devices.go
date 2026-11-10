package store

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// NewDeviceID 生成控制面的设备 ID（dev_ 前缀）。
func NewDeviceID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("dev_%d", time.Now().UnixNano())
	}
	return "dev_" + hex.EncodeToString(b[:])
}

// DefaultDeviceName 在没有显式名字时给出一个可读的默认名。
func DefaultDeviceName(kind, workerID string) string {
	name := workerID
	if len(name) > 12 {
		name = name[:12]
	}
	switch kind {
	case "desktop-container":
		return "桌面容器 " + name
	case "desktop-process":
		return "桌面进程 " + name
	case "cloud":
		return "云端 Worker " + name
	case "mobile":
		return "移动端 " + name
	default:
		if kind == "" {
			return workerID
		}
		return kind + " " + name
	}
}

// ---------- 内存实现 ----------

func (m *Memory) EnsureDevice(d Device) (Device, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for id, dev := range m.devices {
		if dev.UserID != d.UserID || dev.WorkerID != d.WorkerID {
			continue
		}
		if d.Name != "" {
			dev.Name = d.Name
		}
		if d.Kind != "" {
			dev.Kind = d.Kind
		}
		if d.Form != "" {
			dev.Form = d.Form
		}
		if d.Platform != "" {
			dev.Platform = d.Platform
		}
		if d.Version != "" {
			dev.Version = d.Version
		}
		dev.LastSeenAt = now
		m.devices[id] = dev
		return dev, nil
	}

	if d.ID == "" {
		d.ID = NewDeviceID()
	}
	if d.Name == "" {
		d.Name = DefaultDeviceName(d.Kind, d.WorkerID)
	}
	if d.Kind == "" {
		d.Kind = "cloud"
	}
	if d.Form == "" {
		d.Form = "sandbox"
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = now
	}
	d.LastSeenAt = now
	m.devices[d.ID] = d
	return d, nil
}

func (m *Memory) ListDevices(userID string) ([]Device, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Device, 0, len(m.devices))
	for _, d := range m.devices {
		if d.UserID == userID {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeenAt.After(out[j].LastSeenAt) })
	return out, nil
}

func (m *Memory) ListAllDevices() ([]Device, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Device, 0, len(m.devices))
	for _, d := range m.devices {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeenAt.After(out[j].LastSeenAt) })
	return out, nil
}

func (m *Memory) RenameDevice(id, userID, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("name required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.devices[id]
	if !ok || d.UserID != userID {
		return ErrNotFound
	}
	d.Name = name
	m.devices[id] = d
	return nil
}

func (m *Memory) GetDevice(id, userID string) (Device, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	d, ok := m.devices[id]
	if !ok || d.UserID != userID {
		return Device{}, ErrNotFound
	}
	return d, nil
}

func (m *Memory) DeleteDevice(id, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.devices[id]
	if !ok || d.UserID != userID {
		return ErrNotFound
	}
	delete(m.devices, id)
	return nil
}

// ---------- 内存配额 ----------

func (m *Memory) GetQuota(userID string) (Quota, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if q, ok := m.quotas[userID]; ok {
		return q, nil
	}
	return DefaultQuota(userID), nil
}

func (m *Memory) SetQuota(q Quota) error {
	if q.UserID == "" {
		return fmt.Errorf("userId required")
	}
	if q.MaxDevices <= 0 {
		q.MaxDevices = 1
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.quotas[q.UserID] = q
	return nil
}
