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
	"strings"
	"sync"
	"time"
)

type Endpoint struct {
	BaseURL string `json:"baseUrl"`
	Token   string `json:"token"`
	Mode    string `json:"mode"` // container | process
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
}

// Service 是 Wails 的 service 实现，也是 CLI 调试入口。
type Service struct {
	mu     sync.Mutex
	cfg    Config
	runner Runner
	ep     Endpoint
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
	return &Service{cfg: cfg}
}

func (s *Service) WorkerEndpoint() Endpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ep
}

func (s *Service) StartWorker(workspace string) (Endpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.ep.BaseURL != "" {
		return s.ep, nil
	}
	if workspace == "" {
		workspace, _ = os.UserHomeDir()
	}
	if err := os.MkdirAll(s.cfg.DataDir, 0o755); err != nil {
		return Endpoint{}, err
	}

	runner := s.runner
	if runner == nil {
		switch s.cfg.Mode {
		case "process":
			runner = &ProcessRunner{Binary: s.cfg.Binary, DataDir: s.cfg.DataDir, Model: s.cfg.Model}
		default:
			runner = &ContainerRunner{
				Image: s.cfg.Image, Container: s.cfg.Container,
				DataDir: s.cfg.DataDir, Model: s.cfg.Model,
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	ep, err := runner.Start(ctx, workspace)
	if err != nil {
		return Endpoint{}, err
	}
	s.runner = runner
	s.ep = ep
	return ep, nil
}

func (s *Service) StopWorker() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runner == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := s.runner.Stop(ctx)
	s.runner = nil
	s.ep = Endpoint{}
	return err
}

// ---------- container ----------

type ContainerRunner struct {
	Image     string
	Container string
	DataDir   string
	Model     string
}

func (c *ContainerRunner) Start(ctx context.Context, workspace string) (Endpoint, error) {
	token := randomToken()

	_ = exec.CommandContext(ctx, "docker", "rm", "-f", c.Container).Run()

	args := []string{
		"run", "-d", "--name", c.Container,
		"-p", "127.0.0.1::8080",
		"-v", workspace + ":/workspace",
		"-v", c.DataDir + ":/data",
		"-e", "PIG_HOME=/data",
	}
	if c.Model != "" {
		args = append(args, "-e", "SOUNDER_MODEL="+c.Model)
	}
	// 用 -e KEY（不带值）让 docker 从当前环境继承，避免 key 出现在 ps 参数里。
	for _, k := range []string{"MINIMAX_API_KEY", "MINIMAX_CN_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY"} {
		if os.Getenv(k) != "" {
			args = append(args, "-e", k)
		}
	}
	args = append(args, c.Image,
		"-addr=0.0.0.0:8080", "-data=/data", "-token="+token, "-max=2")

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
	cmd     *exec.Cmd
}

func (p *ProcessRunner) Start(ctx context.Context, workspace string) (Endpoint, error) {
	if p.Binary == "" {
		return Endpoint{}, fmt.Errorf("process 模式需要 -worker 二进制路径")
	}

	args := []string{"-addr=127.0.0.1:0", "-data=" + p.DataDir, "-max=2"}
	if p.Model != "" {
		args = append(args, "-model="+p.Model)
	}
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
