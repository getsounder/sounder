// sounder-worker 是唯一执行层：桌面（本地容器/裸进程）与云端（K8s Pod）跑同一份二进制。
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/getsounder/sounder-worker/internal/api"
	"github.com/getsounder/sounder-worker/internal/approval"
	"github.com/getsounder/sounder-worker/internal/buildinfo"
	"github.com/getsounder/sounder-worker/internal/protocol"
	"github.com/getsounder/sounder-worker/internal/session"
	"github.com/getsounder/sounder-worker/internal/tunnel"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "监听地址；0 表示随机端口")
	data := flag.String("data", "/data", "数据目录（workspace / agent / sessions）")
	model := flag.String("model", "", "默认 provider/model")
	max := flag.Int("max", 4, "本 Worker 最大会话数")
	token := flag.String("token", "", "访问令牌；为空则随机生成")
	sysPrompt := flag.String("system-prompt", "", "默认 system prompt")
	meta := flag.String("meta-file", "", "可选：读取 {protocolVersion,pigVersion,pigCommit} 的 JSON")
	gateway := flag.String("gateway", "", "可选：ws(s)://gateway/v1/tunnel，启用反向隧道")
	gatewayToken := flag.String("gateway-token", "", "连接 Gateway 的 Bearer token")
	workerID := flag.String("worker-id", "", "Worker ID；默认取 hostname")
	workerKind := flag.String("worker-kind", "cloud", "desktop | cloud")
	controlPlane := flag.String("control-plane", "", "控制面地址；配合 -device-key 换取短期网关令牌")
	deviceKey := flag.String("device-key", "", "设备密钥（长期），用于定期换取短期网关令牌")
	flag.Parse()

	if *token == "" {
		*token = randomToken()
	}

	// 固定 PiG 的配置根，绝不读宿主 ~/.pig（否则多会话、多租户都会串）。
	if err := os.Setenv("PIG_HOME", *data); err != nil {
		log.Fatalf("set PIG_HOME: %v", err)
	}
	if err := os.MkdirAll(*data, 0o755); err != nil {
		log.Fatalf("mkdir data: %v", err)
	}

	m := session.NewManager(*max, *data, *model, *sysPrompt)
	gate := approval.NewGate(5 * time.Minute)
	m.SetApprover(gate)

	if n := m.Restore(); n > 0 {
		log.Printf("restored %d session(s) from %s", n, *data)
	}

	info := loadMeta(*meta)
	srv := api.New(m, gate, info, *token)

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 设备密钥 > 短期令牌：启动时先换一次，之后定期续期。
	var tokenMu sync.RWMutex
	gwToken := *gatewayToken
	setToken := func(t string) {
		tokenMu.Lock()
		gwToken = t
		tokenMu.Unlock()
	}
	getToken := func() string {
		tokenMu.RLock()
		defer tokenMu.RUnlock()
		return gwToken
	}
	if *deviceKey != "" && *controlPlane != "" {
		if tok, exp, err := refreshWorkerToken(ctx, *controlPlane, *deviceKey); err != nil {
			log.Printf("device key exchange failed: %v (fallback to configured token)", err)
		} else {
			setToken(tok)
			log.Printf("gateway token refreshed, expires %s", exp)
		}
		go func() {
			t := time.NewTicker(20 * time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if tok, exp, err := refreshWorkerToken(ctx, *controlPlane, *deviceKey); err != nil {
						log.Printf("refresh gateway token: %v", err)
					} else {
						setToken(tok)
						log.Printf("gateway token refreshed, expires %s", exp)
					}
				}
			}
		}()
	}

	if *gateway != "" {
		id := *workerID
		if id == "" {
			if h, err := os.Hostname(); err == nil {
				id = h
			} else {
				id = randomToken()[:12]
			}
		}
		tc := tunnel.New(
			*gateway, getToken(), *token, id, *workerKind, buildinfo.String(),
			"http://"+ln.Addr().String(), *max, m.Len,
		)
		tc.SetTokenFunc(getToken)
		m.SetEventSink(tc.Publish)
		go tc.Run(ctx)
		log.Printf("tunnel enabled: %s as %s (%s)", *gateway, id, *workerKind)
	}

	go func() {
		// 启动器（桌面/控制面）靠这一行拿到端口与 token。
		fmt.Printf("READY {\"addr\":%q,\"token\":%q,\"capacity\":%d}\n", ln.Addr().String(), *token, *max)
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down: stop accepting, drain sessions")

	// 1. 停止接新请求
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)

	// 2. 依次关闭会话（内部已保证 session -> runtime -> services）
	m.CloseAll()
	log.Println("bye")
}

func loadMeta(path string) protocol.Meta {
	m := protocol.Meta{
		ProtocolVersion: "v1",
		PigVersion:      os.Getenv("PIG_VERSION"),
		PigCommit:       os.Getenv("PIG_COMMIT"),
		Features:        []string{"sessions", "sse", "multi-session"},
	}
	if path == "" {
		return m
	}
	b, err := os.ReadFile(path)
	if err != nil {
		log.Printf("read meta file: %v", err)
		return m
	}
	if err := json.Unmarshal(b, &m); err != nil {
		log.Printf("parse meta file: %v", err)
	}
	return m
}

func randomToken() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		log.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b[:])
}

// refreshWorkerToken 用设备密钥换一张短期网关令牌。
func refreshWorkerToken(ctx context.Context, server, deviceKey string) (string, time.Time, error) {
	body, _ := json.Marshal(map[string]any{})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(server, "/")+"/v1/worker-tokens/refresh", bytes.NewReader(body))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+deviceKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", time.Time{}, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var out struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expiresAt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", time.Time{}, err
	}
	exp, _ := time.Parse(time.RFC3339, out.ExpiresAt)
	return out.Token, exp, nil
}
