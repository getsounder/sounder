// Package shell 负责启动并守护本地 Worker。
// 它不依赖 Wails，可以单独测试；Wails 只负责把方法暴露给前端。
package shell

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type Endpoint struct {
	BaseURL  string `json:"baseUrl"`
	Token    string `json:"token"`
	Mode     string `json:"mode"` // container | process
	DeviceID string `json:"deviceId,omitempty"`
}

type Runner interface {
	Start(ctx context.Context, workspace string) (Endpoint, error)
	Stop(ctx context.Context) error
}

type Config struct {
	Mode      string // container | process
	Image     string // 云端同 digest
	Binary    string // process 模式的 sounder-worker 路径
	DataDir   string
	Model     string
	Container string

	// 可选：接入控制面。设置后本机设备会连上网关并登记到服务端，
	// Web/移动端就能看到并切到这台桌面设备。
	Server       string // 控制面地址，如 https://sounder.example.com
	AccessToken  string // 控制面访问 token（用户登录后拿到的）
	Gateway      string // 网关隧道地址，如 ws://127.0.0.1:19090/v1/tunnel
	GatewayToken string // 网关 Bearer token
}

// Service 是 Wails 的 service 实现，也是 CLI 调试入口。
// 一个 Service 管理多个本机设备（每个设备一个容器/进程）。
type Service struct {
	mu        sync.Mutex
	cfg       Config
	runner    Runner
	ep        Endpoint
	workspace string

	devices map[string]*deviceRuntime
	order   []string
	active  string
	loaded  bool
}

func New(cfg Config) *Service {
	if cfg.Mode == "" {
		cfg.Mode = "container"
	}
	if cfg.Image == "" {
		cfg.Image = "ghcr.io/getsounder/sounder-worker:dev"
	}
	if cfg.Container == "" {
		cfg.Container = "sounder-desktop"
	}
	if cfg.DataDir == "" {
		home, _ := os.UserHomeDir()
		cfg.DataDir = filepath.Join(home, "Library", "Application Support", "Sounder")
	}
	return &Service{cfg: cfg, devices: make(map[string]*deviceRuntime)}
}

func (s *Service) WorkerEndpoint() Endpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ep
}

// OpenPath 在系统文件管理器里定位一个文件（界面里的文件路径可点击）。
// 只接受已存在的路径，避免把任意字符串交给 shell。
func (s *Service) OpenPath(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("empty path")
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", "-R", path)
	case "windows":
		cmd = exec.Command("explorer", "/select,"+path)
	default:
		cmd = exec.Command("xdg-open", filepath.Dir(path))
	}
	return cmd.Start()
}

// StartWorker 启动（必要时创建）默认设备，返回它的访问地址。
// 兼容旧调用方：不指定设备时总是用列表里的第一台。
func (s *Service) StartWorker(workspace string) (Endpoint, error) {
	s.mu.Lock()
	if s.ep.BaseURL != "" {
		ep := s.ep
		s.mu.Unlock()
		return ep, nil
	}
	if workspace == "" {
		workspace, _ = os.UserHomeDir()
	}
	s.workspace = workspace
	if err := os.MkdirAll(s.cfg.DataDir, 0o755); err != nil {
		s.mu.Unlock()
		return Endpoint{}, err
	}
	if err := s.loadDevices(); err != nil {
		s.mu.Unlock()
		return Endpoint{}, err
	}
	first := s.order[0]
	d := s.devices[first]
	dev, err := s.startDeviceLocked(d)
	if err != nil {
		s.mu.Unlock()
		return Endpoint{}, err
	}
	ep := Endpoint{BaseURL: dev.BaseURL, Token: dev.Token, Mode: s.cfg.Mode, DeviceID: dev.ID}
	s.mu.Unlock()
	return ep, nil
}

// StopWorker 停止所有本机设备（退出时调用）。
func (s *Service) StopWorker() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstErr error
	for _, id := range s.order {
		d := s.devices[id]
		if d == nil || d.runner == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := d.runner.Stop(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
		cancel()
		d.runner = nil
		d.Status = "stopped"
		d.BaseURL = ""
		d.Token = ""
	}
	s.active = ""
	s.ep = Endpoint{}
	return firstErr
}

// ---------- container ----------

type ContainerRunner struct {
	Image         string
	Container     string
	DataDir       string
	Model         string
	Extra         []string // 追加给 worker 的参数（隧道、worker-id 等）
	NoBind        bool     // computer 形态：数据在容器里，不挂载 /data
	TokenOverride string   // computer 原地重建时复用原来的 token
}

func (c *ContainerRunner) Start(ctx context.Context, workspace string) (Endpoint, error) {
	token := c.TokenOverride
	if token == "" {
		token = randomToken()
	}

	_ = exec.CommandContext(ctx, "docker", "rm", "-f", c.Container).Run()

	args := []string{
		"run", "-d", "--name", c.Container,
		"-p", "127.0.0.1::8080",
		"-v", workspace + ":/workspace",
		"-e", "PIG_HOME=/data",
	}
	if !c.NoBind {
		args = append(args, "-v", c.DataDir+":/data")
	}
	// 用 -e KEY（不带值）让 docker 从当前环境继承，避免 key 出现在 ps 参数里。
	for _, k := range []string{"MINIMAX_API_KEY", "MINIMAX_CN_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY"} {
		if os.Getenv(k) != "" {
			args = append(args, "-e", k)
		}
	}
	args = append(args, c.Image,
		"-addr=0.0.0.0:8080", "-data=/data", "-token="+token, "-max=4")
	if c.Model != "" {
		// Worker 只认 -model flag；仅在 docker run 的 -e 里设置环境变量不会生效。
		args = append(args, "-model="+c.Model)
	}
	args = append(args, c.Extra...)

	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return Endpoint{}, fmt.Errorf("docker run: %v: %s", err, strings.TrimSpace(string(out)))
	}

	portOut, err := exec.CommandContext(ctx, "docker", "port", c.Container, "8080/tcp").Output()
	if err != nil {
		return Endpoint{}, fmt.Errorf("docker port: %w", err)
	}
	addr := strings.TrimSpace(strings.Split(string(portOut), "\n")[0])
	idx := strings.LastIndex(addr, ":")
	if idx < 0 {
		return Endpoint{}, fmt.Errorf("unexpected docker port output: %q", addr)
	}
	base := "http://127.0.0.1:" + addr[idx+1:]

	ep := Endpoint{BaseURL: base, Token: token, Mode: "container"}
	if err := waitHealthy(ctx, ep); err != nil {
		return Endpoint{}, err
	}
	return ep, nil
}

func (c *ContainerRunner) Stop(ctx context.Context) error {
	return exec.CommandContext(ctx, "docker", "rm", "-f", c.Container).Run()
}

// ---------- process ----------

type ProcessRunner struct {
	Binary  string
	DataDir string
	Model   string
	Extra   []string
	cmd     *exec.Cmd
}

func (p *ProcessRunner) Start(ctx context.Context, workspace string) (Endpoint, error) {
	if p.Binary == "" {
		return Endpoint{}, fmt.Errorf("process 模式需要 -worker 二进制路径")
	}

	args := []string{"-addr=127.0.0.1:0", "-data=" + p.DataDir, "-max=4"}
	if p.Model != "" {
		args = append(args, "-model="+p.Model)
	}
	args = append(args, p.Extra...)
	cmd := exec.CommandContext(ctx, p.Binary, args...)
	cmd.Dir = workspace
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Endpoint{}, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return Endpoint{}, err
	}
	p.cmd = cmd

	// 等 READY {"addr":..., "token":...} 这一行。
	type ready struct {
		Addr  string `json:"addr"`
		Token string `json:"token"`
	}
	line, err := readReady(stdout)
	if err != nil {
		_ = cmd.Process.Kill()
		return Endpoint{}, err
	}
	var r ready
	if err := json.Unmarshal([]byte(line), &r); err != nil {
		return Endpoint{}, fmt.Errorf("解析 READY 失败: %w (%s)", err, line)
	}

	ep := Endpoint{BaseURL: "http://" + r.Addr, Token: r.Token, Mode: "process"}
	if err := waitHealthy(ctx, ep); err != nil {
		return Endpoint{}, err
	}
	return ep, nil
}

func (p *ProcessRunner) Stop(ctx context.Context) error {
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	_ = p.cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() { _ = p.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		_ = p.cmd.Process.Kill()
	}
	return nil
}

// ---------- helpers ----------

func readReady(r io.Reader) (string, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "READY ") {
			return strings.TrimPrefix(line, "READY "), nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("worker 未输出 READY")
}

func waitHealthy(ctx context.Context, ep Endpoint) error {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ep.BaseURL+"/healthz", nil)
		req.Header.Set("Authorization", "Bearer "+ep.Token)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("worker 健康检查超时: %s", ep.BaseURL)
}

func randomToken() string {
	b := make([]byte, 24)
	if _, err := randRead(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// 单独抽出来，方便测试时替换。
var randRead = func(b []byte) (int, error) {
	f, err := os.Open("/dev/urandom")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return io.ReadFull(f, b)
}
