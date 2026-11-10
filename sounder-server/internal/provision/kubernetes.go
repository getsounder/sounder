package provision

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Kubernetes 在集群里按设备创建 Pod + PVC。
// 只用 REST API + ServiceAccount，不引入 client-go。
//
// 形态说明：K8s 没有"提交容器成镜像"的能力，所以这里只支持 sandbox
// （数据落在 PVC 上，关机删 Pod、开机重建 Pod）；computer 会返回明确错误。
type Kubernetes struct {
	APIServer      string // 默认 https://kubernetes.default.svc
	Namespace      string
	Image          string
	GatewayURL     string
	ControlPlane   string
	WorkerKind     string
	MaxSessions    int
	DefaultModel   string
	StorageClass   string
	ServiceAccount string
	CPU            string // 默认 "2"
	Mem            string // 默认 "4Gi"
	bearerToken    string // 集群外调试：来自 kubeconfig
	client         *http.Client
}

// NewKubernetes 读取集群内 ServiceAccount 凭据。
func NewKubernetes(namespace, image, gateway, controlPlane, workerKind, model, storageClass, serviceAccount string, maxSessions int) (*Kubernetes, error) {
	if namespace == "" {
		namespace = envOr("POD_NAMESPACE", "sounder")
	}
	if serviceAccount == "" {
		serviceAccount = "default"
	}
	if workerKind == "" {
		workerKind = "cloud"
	}
	if maxSessions <= 0 {
		maxSessions = 4
	}

	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	api := "https://kubernetes.default.svc"
	bearer := ""
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}

	saTokenPath := "/var/run/secrets/kubernetes.io/serviceaccount/token"
	if _, statErr := os.Stat(saTokenPath); statErr == nil {
		// 集群内：ServiceAccount
		if host != "" {
			if port == "" {
				port = "443"
			}
			api = "https://" + host + ":" + port
		}
		if ca, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"); err == nil {
			pool := x509.NewCertPool()
			if pool.AppendCertsFromPEM(ca) {
				transport.TLSClientConfig.RootCAs = pool
			}
		}
	} else if kc, err := loadKubeconfig(); err == nil {
		// 集群外调试：读 KUBECONFIG
		api = kc.server
		transport.TLSClientConfig = kc.tlsConf
		bearer = kc.token
	} else if envOr("K8S_INSECURE", "") == "1" {
		transport.TLSClientConfig.InsecureSkipVerify = true
	}

	return &Kubernetes{
		APIServer:      api,
		Namespace:      namespace,
		Image:          image,
		GatewayURL:     gateway,
		ControlPlane:   controlPlane,
		WorkerKind:     workerKind,
		MaxSessions:    maxSessions,
		DefaultModel:   model,
		StorageClass:   storageClass,
		ServiceAccount: serviceAccount,
		CPU:            envOr("K8S_WORKER_CPU", "2"),
		Mem:            envOr("K8S_WORKER_MEM", "4Gi"),
		bearerToken:    bearer,
		client:         &http.Client{Transport: transport, Timeout: 60 * time.Second},
	}, nil
}

func (k *Kubernetes) Enabled() bool { return k != nil && k.Image != "" }

func podName(workerID string) string { return "sounder-worker-" + strings.TrimPrefix(workerID, "dev_") }
func pvcName(workerID string) string { return "sounder-dev-" + strings.TrimPrefix(workerID, "dev_") }

func (k *Kubernetes) workerArgs(req Request) []string {
	args := []string{
		"-addr=0.0.0.0:8080", "-data=/data", "-max=" + strconv.Itoa(k.MaxSessions),
		"-worker-id=" + req.DeviceID, "-worker-kind=" + k.WorkerKind,
	}
	model := req.Model
	if model == "" {
		model = k.DefaultModel
	}
	if model != "" {
		args = append(args, "-model="+model)
	}
	if k.GatewayURL != "" {
		args = append(args, "-gateway="+k.GatewayURL)
	}
	if req.DeviceKey != "" && k.ControlPlane != "" {
		args = append(args, "-control-plane="+k.ControlPlane, "-device-key="+req.DeviceKey)
	} else if req.WorkerToken != "" {
		args = append(args, "-gateway-token="+req.WorkerToken)
	}
	return args
}

// pvcManifest 生成设备的磁盘声明。
func (k *Kubernetes) pvcManifest(req Request) map[string]any {
	disk := req.DiskGB
	if disk <= 0 {
		disk = 20
	}
	spec := map[string]any{
		"accessModes": []string{"ReadWriteOnce"},
		"resources": map[string]any{
			"requests": map[string]string{"storage": fmt.Sprintf("%dGi", disk)},
		},
	}
	if k.StorageClass != "" {
		spec["storageClassName"] = k.StorageClass
	}
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]any{
			"name":      pvcName(req.DeviceID),
			"namespace": k.Namespace,
			"labels":    map[string]string{"app": "sounder-worker", "device": req.DeviceID},
		},
		"spec": spec,
	}
}

// podManifest 生成 Worker Pod。
func (k *Kubernetes) podManifest(req Request) map[string]any {
	cpu := k.CPU
	mem := k.Mem
	env := []map[string]string{
		{"name": "PIG_HOME", "value": "/data"},
		{"name": "HOME", "value": "/home/pig"},
	}
	keys := make([]string, 0, len(req.Env))
	for name := range req.Env {
		keys = append(keys, name)
	}
	// 稳定顺序，便于测试与 diff。
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	for _, name := range keys {
		env = append(env, map[string]string{"name": name, "value": req.Env[name]})
	}

	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":      podName(req.DeviceID),
			"namespace": k.Namespace,
			"labels":    map[string]string{"app": "sounder-worker", "device": req.DeviceID},
		},
		"spec": map[string]any{
			"restartPolicy":      "Never",
			"serviceAccountName": k.ServiceAccount,
			"containers": []map[string]any{{
				"name":            "worker",
				"image":           k.Image,
				"imagePullPolicy": envOr("K8S_IMAGE_PULL_POLICY", "IfNotPresent"),
				"args":            k.workerArgs(req),
				"env":             env,
				"ports":           []map[string]any{{"containerPort": 8080, "name": "http"}},
				"volumeMounts":    []map[string]any{{"name": "data", "mountPath": "/data"}},
				"resources": map[string]any{
					"requests": map[string]string{"cpu": cpu, "memory": mem},
					"limits":   map[string]string{"cpu": cpu, "memory": mem},
				},
			}},
			"volumes": []map[string]any{{
				"name": "data",
				"persistentVolumeClaim": map[string]string{
					"claimName": pvcName(req.DeviceID),
				},
			}},
		},
	}
}

func (k *Kubernetes) Create(ctx context.Context, req Request) (Result, error) {
	if !k.Enabled() {
		return Result{}, ErrNotConfigured
	}
	if req.Form.Normalize() == FormComputer {
		return Result{}, errors.New("kubernetes provisioner 暂不支持 computer 形态（无提交镜像能力），请用 sandbox")
	}

	// PVC 幂等：已存在时返回 409，忽略。
	if err := k.apply(ctx, "/api/v1/namespaces/"+k.Namespace+"/persistentvolumeclaims", k.pvcManifest(req)); err != nil &&
		!strings.Contains(err.Error(), "AlreadyExists") {
		return Result{}, fmt.Errorf("创建 PVC 失败: %w", err)
	}
	// Pod 重建前先删掉旧的（sandbox 开机即新建）。
	_ = k.deletePod(ctx, req.DeviceID)
	if err := k.apply(ctx, "/api/v1/namespaces/"+k.Namespace+"/pods", k.podManifest(req)); err != nil {
		return Result{}, fmt.Errorf("创建 Pod 失败: %w", err)
	}
	return Result{WorkerID: req.DeviceID, ContainerID: podName(req.DeviceID)}, nil
}

func (k *Kubernetes) Start(ctx context.Context, req Request) error {
	_, err := k.Create(ctx, req)
	return err
}

func (k *Kubernetes) Stop(ctx context.Context, workerID string, form Form) error {
	if form.Normalize() == FormComputer {
		return errors.New("kubernetes provisioner 暂不支持 computer 形态")
	}
	return k.deletePod(ctx, workerID)
}

func (k *Kubernetes) Delete(ctx context.Context, workerID string) error {
	_ = k.deletePod(ctx, workerID)
	_ = k.do(ctx, http.MethodDelete,
		"/api/v1/namespaces/"+k.Namespace+"/persistentvolumeclaims/"+pvcName(workerID), nil, nil)
	return nil
}

func (k *Kubernetes) deletePod(ctx context.Context, workerID string) error {
	err := k.do(ctx, http.MethodDelete,
		"/api/v1/namespaces/"+k.Namespace+"/pods/"+podName(workerID)+"?gracePeriodSeconds=5", nil, nil)
	if err != nil && strings.Contains(err.Error(), "NotFound") {
		return nil
	}
	return err
}

func (k *Kubernetes) apply(ctx context.Context, path string, manifest any) error {
	body, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	return k.do(ctx, http.MethodPost, path, body, nil)
}

func (k *Kubernetes) do(ctx context.Context, method, path string, body []byte, out any) error {
	var rdr io.Reader = http.NoBody
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.APIServer+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+k.token())
	resp, err := k.client.Do(req)
	if err != nil {
		return fmt.Errorf("k8s api %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("k8s api %s %s: %s %s", method, path, resp.Status, strings.TrimSpace(buf.String()))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (k *Kubernetes) token() string {
	if t := os.Getenv("KUBERNETES_SA_TOKEN"); t != "" {
		return t
	}
	if k.bearerToken != "" {
		return k.bearerToken
	}
	b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
