package provision

import (
	"encoding/json"
	"strings"
	"testing"
)

func testK8s() *Kubernetes {
	return &Kubernetes{
		APIServer:      "https://k8s.test",
		Namespace:      "sounder",
		Image:          "ghcr.io/getsounder/sounder-worker:dev",
		GatewayURL:     "wss://gateway.example.com/v1/tunnel",
		ControlPlane:   "https://api.example.com",
		WorkerKind:     "cloud",
		MaxSessions:    4,
		DefaultModel:   "minimax-cn/MiniMax-M3",
		StorageClass:   "fast",
		ServiceAccount: "sounder-worker",
		CPU:            "2",
		Mem:            "4Gi",
	}
}

func TestPodManifest(t *testing.T) {
	k := testK8s()
	pod := k.podManifest(Request{
		DeviceID:  "dev_abc123",
		DeviceKey: "dk_test",
		Env:       map[string]string{"OPENAI_API_KEY": "sk-x"},
	})

	b, err := json.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)

	for _, want := range []string{
		`sounder-worker-abc123`,
		`"image":"ghcr.io/getsounder/sounder-worker:dev"`,
		`-worker-id=dev_abc123`,
		`-gateway=wss://gateway.example.com/v1/tunnel`,
		`-device-key=dk_test`,
		`-control-plane=https://api.example.com`,
		`-model=minimax-cn/MiniMax-M3`,
		`"claimName":"sounder-dev-abc123"`,
		`"cpu":"2"`,
		`"memory":"4Gi"`,
		`"OPENAI_API_KEY"`,
		`"restartPolicy":"Never"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("pod manifest 缺少 %s\n%s", want, s)
		}
	}
}

func TestPVCManifest(t *testing.T) {
	k := testK8s()
	pvc := k.pvcManifest(Request{DeviceID: "dev_abc123", DiskGB: 40})
	b, _ := json.Marshal(pvc)
	s := string(b)
	for _, want := range []string{`"name":"sounder-dev-abc123"`, `"storage":"40Gi"`, `"storageClassName":"fast"`} {
		if !strings.Contains(s, want) {
			t.Errorf("pvc manifest 缺少 %s\n%s", want, s)
		}
	}
}

func TestComputerFormRejectedOnK8s(t *testing.T) {
	k := testK8s()
	if _, err := k.Create(t.Context(), Request{DeviceID: "dev_x", Form: FormComputer}); err == nil {
		t.Fatal("期望 computer 形态在 k8s 下报错")
	}
}
