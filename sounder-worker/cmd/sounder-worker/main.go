// sounder-worker 是唯一执行层：桌面（本地容器/裸进程）与云端（K8s Pod）跑同一份二进制。
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/getsounder/sounder-worker/internal/api"
	"github.com/getsounder/sounder-worker/internal/approval"
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
			*gateway, *gatewayToken, *token, id, *workerKind, "0.1.0",
			"http://"+ln.Addr().String(), *max, m.Len,
		)
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
