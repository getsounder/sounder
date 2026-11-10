package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// ErrNotConfigured 表示当前没有配置 provisioner。
var ErrNotConfigured = errors.New("provisioning not configured")

// Docker 通过 Docker Engine API（unix socket）创建 Worker 容器。
// 不依赖 docker CLI，因此服务端镜像里不需要装 docker。
type Docker struct {
	Host         string // unix:///var/run/docker.sock
	Image        string
	GatewayURL   string // 容器内可达的网关隧道地址
	WorkerKind   string
	MaxSessions  int
	DefaultModel string
	client       *http.Client
}

// NewDocker 构造 provisioner；host 形如 unix:///var/run/docker.sock。
func NewDocker(host, image, gatewayURL, workerKind, model string, maxSessions int) *Docker {
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	if image == "" {
		image = "ghcr.io/getsounder/sounder-worker:dev"
	}
	if workerKind == "" {
		workerKind = "cloud"
	}
	if maxSessions <= 0 {
		maxSessions = 4
	}
	path := strings.TrimPrefix(host, "unix://")
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}
	return &Docker{
		Host:         host,
		Image:        image,
		GatewayURL:   gatewayURL,
		WorkerKind:   workerKind,
		MaxSessions:  maxSessions,
		DefaultModel: model,
		client:       &http.Client{Transport: tr, Timeout: 120 * time.Second},
	}
}

func (d *Docker) Enabled() bool { return d != nil && d.Image != "" }

func containerName(workerID string) string {
	return "sounder-worker-" + strings.TrimPrefix(workerID, "dev_")
}

func volumeName(workerID string) string {
	return "sounder-dev-" + strings.TrimPrefix(workerID, "dev_")
}

// committedImage 是 computer 形态关机时提交出来的镜像。
func committedImage(workerID string) string {
	return "sounder-device-" + strings.TrimPrefix(workerID, "dev_")
}

type dockerContainerConfig struct {
	Image      string   `json:"Image"`
	Cmd        []string `json:"Cmd"`
	Env        []string `json:"Env"`
	HostConfig struct {
		Binds         []string `json:"Binds"`
		NanoCPUs      int64    `json:"NanoCpus"`
		Memory        int64    `json:"Memory"`
		RestartPolicy struct {
			Name string `json:"Name"`
		} `json:"RestartPolicy"`
	} `json:"HostConfig"`
}

func (d *Docker) workerArgs(req Request) []string {
	args := []string{
		"-addr=0.0.0.0:8080", "-data=/data", "-max=" + fmt.Sprint(d.MaxSessions),
		"-worker-id=" + req.DeviceID, "-worker-kind=" + d.WorkerKind,
	}
	model := req.Model
	if model == "" {
		model = d.DefaultModel
	}
	if model != "" {
		args = append(args, "-model="+model)
	}
	if d.GatewayURL != "" {
		args = append(args, "-gateway="+d.GatewayURL)
	}
	// 优先用设备密钥自行换令牌，这样令牌可以很短命。
	if req.DeviceKey != "" && req.ControlPlane != "" {
		args = append(args, "-control-plane="+req.ControlPlane, "-device-key="+req.DeviceKey)
	} else if req.WorkerToken != "" {
		args = append(args, "-gateway-token="+req.WorkerToken)
	}
	return args
}

func (d *Docker) Create(ctx context.Context, req Request) (Result, error) {
	if !d.Enabled() {
		return Result{}, ErrNotConfigured
	}
	id := req.DeviceID
	if id == "" {
		return Result{}, errors.New("deviceId required")
	}
	form := req.Form.Normalize()

	// computer 形态如果之前提交过镜像，就从那个镜像起来，状态原地恢复。
	image := d.Image
	if form == FormComputer {
		if exists, _ := d.imageExists(ctx, committedImage(id)); exists {
			image = committedImage(id) + ":latest"
		}
	}

	cfg := dockerContainerConfig{Image: image, Cmd: d.workerArgs(req)}
	cfg.Env = append(cfg.Env, "PIG_HOME=/data", "HOME=/home/pig")
	for k, v := range req.Env {
		if v != "" {
			cfg.Env = append(cfg.Env, k+"="+v)
		}
	}
	// sandbox 靠挂载目录持久化；computer 的状态在容器文件系统里，提交成镜像。
	if form == FormSandbox {
		cfg.HostConfig.Binds = []string{volumeName(id) + ":/data"}
	}
	if req.CPU > 0 {
		cfg.HostConfig.NanoCPUs = int64(req.CPU) * 1_000_000_000
	}
	if req.MemMB > 0 {
		cfg.HostConfig.Memory = int64(req.MemMB) * 1024 * 1024
	}
	cfg.HostConfig.RestartPolicy.Name = "unless-stopped"

	// 同名容器先清掉（sandbox 重建、computer 从镜像重建）。
	_ = d.removeContainer(ctx, id, false)

	body, err := json.Marshal(cfg)
	if err != nil {
		return Result{}, err
	}
	var created struct {
		ID string `json:"Id"`
	}
	name := containerName(id)
	if err := d.do(ctx, http.MethodPost, "/v1.41/containers/create?name="+name, body, &created); err != nil {
		return Result{}, err
	}
	if err := d.do(ctx, http.MethodPost, "/v1.41/containers/"+created.ID+"/start", nil, nil); err != nil {
		_ = d.removeContainer(ctx, id, false)
		return Result{}, err
	}
	return Result{WorkerID: id, ContainerID: created.ID}, nil
}

// Start 开机：
//   - computer：容器还在就原地启动，不在就从提交的镜像重建
//   - sandbox：删除旧容器后新建（数据在挂载卷里）
func (d *Docker) Start(ctx context.Context, req Request) error {
	if !d.Enabled() {
		return ErrNotConfigured
	}
	form := req.Form.Normalize()
	if form == FormComputer {
		// computer 关机后容器还在，直接原地启动（状态在容器里）。
		if exists, running := d.containerState(ctx, containerName(req.DeviceID)); exists {
			if running {
				return nil
			}
			return d.do(ctx, http.MethodPost, "/v1.41/containers/"+containerName(req.DeviceID)+"/start", nil, nil)
		}
	}
	// sandbox 常态 & computer 容器丢失：按参数（重新）创建，computer 会自动用已提交镜像。
	_, err := d.Create(ctx, req)
	return err
}

// Stop 关机：
//   - computer：提交镜像后停止容器（容器保留，下次原地启动）
//   - sandbox：直接删除容器（数据在挂载卷里保留）
func (d *Docker) Stop(ctx context.Context, workerID string, form Form) error {
	if !d.Enabled() {
		return ErrNotConfigured
	}
	name := containerName(workerID)
	if form.Normalize() == FormComputer {
		// 先提交，保证镜像里有最新状态；提交失败不影响关机。
		repo := committedImage(workerID)
		_ = d.do(ctx, http.MethodPost,
			"/v1.41/commit?container="+name+"&repo="+repo+"&tag=latest", nil, nil)
		if exists, running := d.containerState(ctx, name); exists && running {
			return d.do(ctx, http.MethodPost, "/v1.41/containers/"+name+"/stop?t=15", nil, nil)
		}
		return nil
	}
	return d.removeContainer(ctx, workerID, true)
}

// Delete 彻底删除：容器、挂载卷、提交过的镜像。
func (d *Docker) Delete(ctx context.Context, workerID string) error {
	if !d.Enabled() {
		return nil
	}
	_ = d.removeContainer(ctx, workerID, true)
	_ = d.do(ctx, http.MethodDelete, "/v1.41/volumes/"+volumeName(workerID)+"?force=1", nil, nil)
	_ = d.do(ctx, http.MethodDelete, "/v1.41/images/"+committedImage(workerID)+":latest?force=1", nil, nil)
	return nil
}

func (d *Docker) removeContainer(ctx context.Context, workerID string, removeVolume bool) error {
	vol := "0"
	if removeVolume {
		vol = "1"
	}
	err := d.do(ctx, http.MethodDelete,
		"/v1.41/containers/"+containerName(workerID)+"?force=1&v="+vol, nil, nil)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "no such container") {
		return nil
	}
	return err
}

func (d *Docker) containerState(ctx context.Context, name string) (exists, running bool) {
	var info struct {
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
	}
	if err := d.do(ctx, http.MethodGet, "/v1.41/containers/"+name+"/json", nil, &info); err != nil {
		return false, false
	}
	return true, info.State.Running
}

func (d *Docker) imageExists(ctx context.Context, repo string) (bool, error) {
	err := d.do(ctx, http.MethodGet, "/v1.41/images/"+repo+":latest/json", nil, nil)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such image") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (d *Docker) do(ctx context.Context, method, path string, body []byte, out any) error {
	var rdr *bytes.Reader
	if body == nil {
		rdr = bytes.NewReader(nil)
	} else {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("docker api %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return fmt.Errorf("docker api %s %s: %s %s", method, path, resp.Status, strings.TrimSpace(buf.String()))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
