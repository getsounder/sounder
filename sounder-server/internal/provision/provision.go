package provision

import "context"

// Form 是设备的容器形态：
//   - computer：关机时把容器提交成镜像，开机原地启动（状态在镜像里）
//   - sandbox：靠挂载目录持久化，关机删容器，开机新建容器
type Form string

const (
	FormComputer Form = "computer"
	FormSandbox  Form = "sandbox"
)

// Normalize 把空值/未知值收敛成默认形态。
func (f Form) Normalize() Form {
	switch string(f) {
	case "computer":
		return FormComputer
	default:
		return FormSandbox
	}
}

// Request 描述要创建的一台云端设备。
type Request struct {
	DeviceID     string // 同时作为 worker id
	UserID       string
	WorkerToken  string // 网关凭据（HMAC 签发，短期）
	DeviceKey    string // 长期设备密钥：Worker 用它自行换短期网关令牌
	ControlPlane string // 控制面地址（换令牌用）
	Name         string
	Form         Form
	Model        string
	CPU          int // vCPU
	MemMB        int
	DiskGB       int
	Env          map[string]string // 透传给 Worker 的环境变量（模型 key 等）
}

// Result 是创建结果。
type Result struct {
	WorkerID    string
	ContainerID string
}

// Provisioner 抽象"开一台 Worker"这件事：本地用 Docker，生产可换 K8s。
type Provisioner interface {
	Enabled() bool
	Create(ctx context.Context, req Request) (Result, error)
	Start(ctx context.Context, req Request) error
	Stop(ctx context.Context, workerID string, form Form) error
	Delete(ctx context.Context, workerID string) error
}

// Disabled 表示没有配置任何 provisioner：云端设备退回"认领空闲 Worker"。
type Disabled struct{}

func (Disabled) Enabled() bool { return false }

func (Disabled) Create(context.Context, Request) (Result, error) {
	return Result{}, ErrNotConfigured
}

func (Disabled) Start(context.Context, Request) error     { return ErrNotConfigured }
func (Disabled) Stop(context.Context, string, Form) error { return ErrNotConfigured }
func (Disabled) Delete(context.Context, string) error     { return nil }
