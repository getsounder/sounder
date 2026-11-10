package shell

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Device 是本机的一个 Worker 运行环境（一个容器 / 一个进程）。
// 每个设备有独立的数据目录，因此会话、历史互不干扰。
type Device struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"` // desktop-container | desktop-process
	Container string    `json:"container,omitempty"`
	DataDir   string    `json:"dataDir"`
	CreatedAt time.Time `json:"createdAt"`

	// 运行时字段（不落盘）
	Status  string `json:"status"` // running | stopped
	BaseURL string `json:"baseUrl,omitempty"`
	Token   string `json:"token,omitempty"`
	Active  bool   `json:"active,omitempty"`
}

type deviceRuntime struct {
	Device
	runner Runner
}

type devicesFile struct {
	Devices []Device `json:"devices"`
}

func newDeviceID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("dev_%d", time.Now().UnixNano()%1e8)
	}
	return "dev_" + hex.EncodeToString(b[:])
}

func (s *Service) devicesPath() string {
	return filepath.Join(s.cfg.DataDir, "devices.json")
}

// loadDevices 读取落盘的设备列表；首次运行会迁移旧的单容器数据目录。
func (s *Service) loadDevices() error {
	if s.loaded {
		return nil
	}
	s.loaded = true
	s.devices = make(map[string]*deviceRuntime)

	raw, err := os.ReadFile(s.devicesPath())
	if err == nil {
		var f devicesFile
		if err := json.Unmarshal(raw, &f); err == nil {
			for _, d := range f.Devices {
				dc := d
				if dc.Status == "" {
					dc.Status = "stopped"
				}
				s.devices[dc.ID] = &deviceRuntime{Device: dc}
				s.order = append(s.order, dc.ID)
			}
		}
	}

	// 旧版本只有一个 DataDir 作为 /data，这里把它当成默认设备，避免会话“消失”。
	if len(s.devices) == 0 {
		id := newDeviceID()
		def := Device{
			ID:        id,
			Name:      "本机设备",
			Kind:      s.deviceKind(),
			Container: s.containerName(id),
			DataDir:   s.cfg.DataDir,
			CreatedAt: time.Now(),
			Status:    "stopped",
		}
		s.devices[id] = &deviceRuntime{Device: def}
		s.order = append(s.order, id)
		_ = s.saveDevicesLocked()
	}

	for _, id := range s.order {
		d := s.devices[id]
		if d.Kind == "desktop-container" && d.Container != "" {
			if containerRunning(d.Container) {
				d.Status = "running"
				if ep, ok := s.containerEndpoint(d.Container); ok {
					d.BaseURL = ep
				}
			}
		}
	}
	return nil
}

func (s *Service) saveDevicesLocked() error {
	if err := os.MkdirAll(s.cfg.DataDir, 0o755); err != nil {
		return err
	}
	f := devicesFile{Devices: make([]Device, 0, len(s.order))}
	for _, id := range s.order {
		d := s.devices[id].Device
		// 运行时字段不落盘
		d.Status = ""
		d.BaseURL = ""
		d.Token = ""
		d.Active = false
		f.Devices = append(f.Devices, d)
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.devicesPath(), b, 0o644)
}

func (s *Service) deviceKind() string {
	if s.cfg.Mode == "process" {
		return "desktop-process"
	}
	return "desktop-container"
}

func (s *Service) containerName(id string) string {
	if s.cfg.Mode == "process" {
		return ""
	}
	return "sounder-desktop-" + strings.TrimPrefix(id, "dev_")
}

// ListDevices 返回本机设备（含运行状态）；不含 token。
func (s *Service) ListDevices() ([]Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadDevices(); err != nil {
		return nil, err
	}
	out := make([]Device, 0, len(s.order))
	for _, id := range s.order {
		d := s.devices[id]
		dev := d.Device
		if d.runner != nil {
			dev.Status = "running"
		} else if dev.Status == "" {
			dev.Status = "stopped"
		}
		if dev.Kind == "desktop-container" && dev.Container != "" && containerRunning(dev.Container) {
			dev.Status = "running"
		}
		dev.Active = id == s.active
		dev.Token = ""
		dev.BaseURL = ""
		out = append(out, dev)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// AddDevice 新建一个本机设备（容器/进程），并立即启动。
func (s *Service) AddDevice(name string) (Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadDevices(); err != nil {
		return Device{}, err
	}

	id := newDeviceID()
	name = strings.TrimSpace(name)
	if name == "" {
		name = fmt.Sprintf("本机设备 %d", len(s.order)+1)
	}
	dev := Device{
		ID:        id,
		Name:      name,
		Kind:      s.deviceKind(),
		Container: s.containerName(id),
		DataDir:   filepath.Join(s.cfg.DataDir, "devices", id),
		CreatedAt: time.Now(),
		Status:    "stopped",
	}
	d := &deviceRuntime{Device: dev}
	s.devices[id] = d
	s.order = append(s.order, id)
	if err := s.saveDevicesLocked(); err != nil {
		return Device{}, err
	}

	started, err := s.startDeviceLocked(d)
	if err != nil {
		// 起不来就把这条登记删掉，避免列表里留一个坏设备。
		delete(s.devices, id)
		s.order = removeString(s.order, id)
		_ = s.saveDevicesLocked()
		return Device{}, err
	}
	return started, nil
}

// StartDevice 启动指定设备；已经运行时直接返回其地址。
func (s *Service) StartDevice(id string) (Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadDevices(); err != nil {
		return Device{}, err
	}
	d, ok := s.devices[id]
	if !ok {
		return Device{}, fmt.Errorf("device not found")
	}
	return s.startDeviceLocked(d)
}

func (s *Service) startDeviceLocked(d *deviceRuntime) (Device, error) {
	if d.runner != nil && d.BaseURL != "" {
		d.Status = "running"
		return d.Device, nil
	}
	if err := os.MkdirAll(d.DataDir, 0o755); err != nil {
		return Device{}, err
	}
	extra := s.workerExtraArgs(d)

	var runner Runner
	if d.Kind == "desktop-process" {
		runner = &ProcessRunner{Binary: s.cfg.Binary, DataDir: d.DataDir, Model: s.cfg.Model, Extra: extra}
	} else {
		runner = &ContainerRunner{
			Image:     s.cfg.Image,
			Container: d.Container,
			DataDir:   d.DataDir,
			Model:     s.cfg.Model,
			Extra:     extra,
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	ep, err := runner.Start(ctx, s.workspace)
	if err != nil {
		return Device{}, err
	}
	d.runner = runner
	d.BaseURL = ep.BaseURL
	d.Token = ep.Token
	d.Status = "running"
	s.active = d.ID
	s.ep = ep
	s.ep.DeviceID = d.ID

	// 登记到控制面；失败不影响本地使用，只是别的端暂时看不到这台设备。
	go s.registerDevice(d.Device)
	return d.Device, nil
}

// StopDevice 停止设备但保留数据目录，下次启动还是同一台设备。
func (s *Service) StopDevice(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[id]
	if !ok {
		return fmt.Errorf("device not found")
	}
	if d.runner == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := d.runner.Stop(ctx)
	d.runner = nil
	d.Status = "stopped"
	d.BaseURL = ""
	d.Token = ""
	if s.active == id {
		s.ep = Endpoint{}
	}
	return err
}

// RemoveDevice 停止并删除设备（连同它的数据目录）。
func (s *Service) RemoveDevice(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadDevices(); err != nil {
		return err
	}
	d, ok := s.devices[id]
	if !ok {
		return fmt.Errorf("device not found")
	}
	if len(s.devices) <= 1 {
		return fmt.Errorf("至少保留一个设备")
	}
	if d.runner != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = d.runner.Stop(ctx)
		cancel()
	}
	// 默认设备的数据目录就是 DataDir 根，不能整目录删掉。
	if filepath.Clean(d.DataDir) != filepath.Clean(s.cfg.DataDir) {
		_ = os.RemoveAll(d.DataDir)
	}
	delete(s.devices, id)
	s.order = removeString(s.order, id)
	if s.active == id {
		s.active = ""
		s.ep = Endpoint{}
	}
	return s.saveDevicesLocked()
}

// RenameDevice 修改设备显示名。
func (s *Service) RenameDevice(id, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadDevices(); err != nil {
		return err
	}
	d, ok := s.devices[id]
	if !ok {
		return fmt.Errorf("device not found")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("name required")
	}
	d.Name = name
	return s.saveDevicesLocked()
}

func removeString(list []string, v string) []string {
	out := list[:0]
	for _, item := range list {
		if item != v {
			out = append(out, item)
		}
	}
	return out
}

func containerRunning(name string) bool {
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", name).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

func (s *Service) containerEndpoint(name string) (string, bool) {
	out, err := exec.Command("docker", "port", name, "8080/tcp").Output()
	if err != nil {
		return "", false
	}
	addr := strings.TrimSpace(strings.Split(string(out), "\n")[0])
	idx := strings.LastIndex(addr, ":")
	if idx < 0 {
		return "", false
	}
	return "http://127.0.0.1:" + addr[idx+1:], true
}

// workerExtraArgs 给 Worker 加上反向隧道与设备身份参数，让它对控制面可见。
func (s *Service) workerExtraArgs(d *deviceRuntime) []string {
	if s.cfg.Gateway == "" {
		return nil
	}
	gw := s.cfg.Gateway
	// 容器里的 127.0.0.1 是容器自己；桌面容器要访问宿主机上的网关。
	if d.Kind != "desktop-process" {
		gw = strings.ReplaceAll(gw, "127.0.0.1", "host.docker.internal")
		gw = strings.ReplaceAll(gw, "localhost", "host.docker.internal")
	}
	kind := "desktop"
	args := []string{"-gateway=" + gw, "-worker-id=" + d.ID, "-worker-kind=" + kind}
	if s.cfg.GatewayToken != "" {
		args = append(args, "-gateway-token="+s.cfg.GatewayToken)
	}
	return args
}

// registerDevice 把本机设备登记到控制面（需要 -server 与访问 token）。
func (s *Service) registerDevice(d Device) {
	server := strings.TrimRight(s.cfg.Server, "/")
	if server == "" || s.cfg.AccessToken == "" {
		return
	}
	body, _ := json.Marshal(map[string]any{
		"kind":     d.Kind,
		"workerId": d.ID,
		"name":     d.Name,
		"platform": runtime.GOOS,
		"version":  "0.1.0",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server+"/v1/devices", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.cfg.AccessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		fmt.Printf("register device %s failed: %s %s\n", d.ID, resp.Status, strings.TrimSpace(string(b)))
	}
}
