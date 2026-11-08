// sounder-gateway：Worker 反向隧道的接入点与事件扇出层。
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/getsounder/sounder-gateway/internal/api"
	"github.com/getsounder/sounder-gateway/internal/registry"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9090", "监听地址")
	token := flag.String("token", "", "访问令牌；为空则不校验")
	stale := flag.Duration("stale", 90*time.Second, "心跳超时，超时后从注册表移除")
	flag.Parse()

	reg := registry.New()

	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for range t.C {
			for _, id := range reg.Cleanup(*stale) {
				log.Printf("worker evicted (stale): %s", id)
			}
		}
	}()

	srv := api.New(reg, *token)
	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("sounder-gateway listening on %s", ln.Addr())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
	}()

	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdown)
	log.Println("bye")
}
