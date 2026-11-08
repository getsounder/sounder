// sounder-server：控制面。负责会话编排、鉴权与事件转发，不执行 Agent。
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

	"github.com/getsounder/sounder-server/internal/api"
	"github.com/getsounder/sounder-server/internal/gateway"
	"github.com/getsounder/sounder-server/internal/store"
)

func main() {
	addr := flag.String("addr", "0.0.0.0:8000", "监听地址")
	gwURL := flag.String("gateway", "http://127.0.0.1:9090", "sounder-gateway 地址")
	gwToken := flag.String("gateway-token", "", "访问 Gateway 的 token")
	token := flag.String("token", "", "控制端访问本服务的 token；为空则不校验")
	storeKind := flag.String("store", "memory", "memory | postgres")
	dsn := flag.String("dsn", "", "PostgreSQL DSN（-store=postgres 时必填）")
	flag.Parse()

	var st store.Store
	switch *storeKind {
	case "memory":
		st = store.NewMemory()
	case "postgres":
		pg, err := store.NewPostgres(*dsn)
		if err != nil {
			log.Fatalf("postgres: %v", err)
		}
		if err := pg.Migrate(); err != nil {
			log.Fatalf("migrate: %v", err)
		}
		defer pg.Close()
		st = pg
	default:
		log.Fatalf("unsupported store: %s", *storeKind)
	}

	gw := gateway.New(*gwURL, *gwToken)
	srv := api.New(st, gw, *token)

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("sounder-server listening on %s (gateway=%s)", ln.Addr(), *gwURL)

	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// SSE 需要长写超时；不要设置 WriteTimeout。
	}

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
