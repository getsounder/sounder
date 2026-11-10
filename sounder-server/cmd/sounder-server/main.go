// sounder-server：控制面。负责会话编排、鉴权与事件转发，不执行 Agent。
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/getsounder/sounder-server/internal/api"
	"github.com/getsounder/sounder-server/internal/auth"
	"github.com/getsounder/sounder-server/internal/gateway"
	"github.com/getsounder/sounder-server/internal/provision"
	"github.com/getsounder/sounder-server/internal/store"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func main() {
	addr := flag.String("addr", "0.0.0.0:8000", "监听地址")
	gwURL := flag.String("gateway", "http://127.0.0.1:9090", "sounder-gateway 地址")
	gwToken := flag.String("gateway-token", "", "访问 Gateway 的 token")
	publicGateway := flag.String("public-gateway", env("SOUNDER_PUBLIC_GATEWAY", ""), "下发给客户端的网关地址（如 wss://gateway.example.com/v1/tunnel）")
	workerSecret := flag.String("worker-secret", env("SOUNDER_WORKER_SECRET", ""), "设备 worker token 的 HMAC 密钥（与网关一致）")
	provisioner := flag.String("provisioner", env("SOUNDER_PROVISIONER", "none"), "云端设备 provisioner：none | docker")
	workerImage := flag.String("worker-image", env("SOUNDER_WORKER_IMAGE", "ghcr.io/getsounder/sounder-worker:dev"), "provisioner 创建的 Worker 镜像")
	workerModel := flag.String("worker-model", env("SOUNDER_WORKER_MODEL", ""), "新 Worker 的默认模型")
	dockerHost := flag.String("docker-host", env("SOUNDER_DOCKER_HOST", "unix:///var/run/docker.sock"), "Docker socket（-provisioner=docker）")
	k8sNamespace := flag.String("k8s-namespace", env("SOUNDER_K8S_NAMESPACE", "sounder"), "K8s 命名空间（-provisioner=k8s）")
	k8sStorageClass := flag.String("k8s-storage-class", env("SOUNDER_K8S_STORAGE_CLASS", ""), "PVC StorageClass；留空用集群默认")
	k8sServiceAccount := flag.String("k8s-service-account", env("SOUNDER_K8S_SERVICE_ACCOUNT", "default"), "Worker Pod 的 ServiceAccount")
	provisionerGateway := flag.String("provisioner-gateway", env("SOUNDER_PROVISIONER_GATEWAY", ""), "provisioner 创建的 Worker 该连的网关地址；留空则由 public-gateway 推导（localhost→host.docker.internal）")
	provisionerCP := flag.String("provisioner-control-plane", env("SOUNDER_PROVISIONER_CONTROL_PLANE", ""), "Worker 换短期令牌用的控制面地址；留空由 public-url 推导（localhost→host.docker.internal）")
	storeKind := flag.String("store", "memory", "memory | postgres")
	dsn := flag.String("dsn", "", "PostgreSQL DSN（-store=postgres 时必填）")

	authEnabled := flag.Bool("auth", envBool("SOUNDER_AUTH", true), "启用用户体系（本地账号 + Google/GitHub）")
	publicURL := flag.String("public-url", env("SOUNDER_PUBLIC_URL", "http://localhost:8000"), "本服务对外地址（OAuth 回调前缀）")
	webURL := flag.String("web-url", env("SOUNDER_WEB_URL", "http://localhost:3000"), "Web 前端地址（邮件链接前缀）")
	nativeRedirect := flag.String("native-redirect", env("SOUNDER_NATIVE_REDIRECT", "sounder://auth/callback"), "桌面/移动端 OAuth 回调")
	cookieName := flag.String("cookie-name", env("SOUNDER_COOKIE_NAME", "sounder_session"), "Web 登录 Cookie 名")
	cookieSecure := flag.Bool("cookie-secure", envBool("SOUNDER_COOKIE_SECURE", false), "Cookie 只走 HTTPS")
	cookieDomain := flag.String("cookie-domain", env("SOUNDER_COOKIE_DOMAIN", ""), "Cookie Domain（留空为当前主机）")
	localAuth := flag.Bool("local-auth", envBool("SOUNDER_LOCAL_AUTH", true), "启用邮箱密码登录")
	registration := flag.Bool("registration", envBool("SOUNDER_REGISTRATION", true), "开放注册")
	requireVerify := flag.Bool("require-email-verify", envBool("SOUNDER_REQUIRE_EMAIL_VERIFY", false), "登录前必须验证邮箱")
	googleID := flag.String("google-client-id", env("SOUNDER_GOOGLE_CLIENT_ID", ""), "Google OAuth Client ID")
	googleSecret := flag.String("google-client-secret", env("SOUNDER_GOOGLE_CLIENT_SECRET", ""), "Google OAuth Client Secret")
	githubID := flag.String("github-client-id", env("SOUNDER_GITHUB_CLIENT_ID", ""), "GitHub OAuth Client ID")
	githubSecret := flag.String("github-client-secret", env("SOUNDER_GITHUB_CLIENT_SECRET", ""), "GitHub OAuth Client Secret")
	devProvider := flag.Bool("dev-provider", envBool("SOUNDER_DEV_PROVIDER", false), "启用本地联调登录（code=邮箱）")
	smtpAddr := flag.String("smtp-addr", env("SOUNDER_SMTP_ADDR", ""), "SMTP 地址，如 smtp.example.com:587")
	smtpUser := flag.String("smtp-user", env("SOUNDER_SMTP_USER", ""), "SMTP 用户名")
	smtpPass := flag.String("smtp-pass", env("SOUNDER_SMTP_PASS", ""), "SMTP 密码")
	smtpFrom := flag.String("smtp-from", env("SOUNDER_SMTP_FROM", "no-reply@sounder.local"), "发件人")
	flag.Parse()

	var st store.Store
	var ast store.AuthStore
	switch *storeKind {
	case "memory":
		st = store.NewMemory()
		ast = store.NewMemoryAuth()
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
		ast = pg
	default:
		log.Fatalf("unsupported store: %s", *storeKind)
	}

	var authSvc *auth.Service
	if *authEnabled {
		var mailer auth.Mailer
		if *smtpAddr != "" {
			mailer = auth.NewSMTPMailer(*smtpAddr, *smtpUser, *smtpPass, *smtpFrom)
		}
		authSvc = auth.New(auth.Config{
			PublicBaseURL:       *publicURL,
			WebBaseURL:          *webURL,
			CookieName:          *cookieName,
			CookieSecure:        *cookieSecure,
			CookieDomain:        *cookieDomain,
			LocalEnabled:        *localAuth,
			RegistrationEnabled: *registration,
			RequireEmailVerify:  *requireVerify,
			NativeRedirect:      *nativeRedirect,
			DevProvider:         *devProvider,
			Google: auth.ProviderConfig{
				Enabled:      *googleID != "",
				ClientID:     *googleID,
				ClientSecret: *googleSecret,
			},
			GitHub: auth.ProviderConfig{
				Enabled:      *githubID != "",
				ClientID:     *githubID,
				ClientSecret: *githubSecret,
			},
		}, ast, mailer)
		log.Printf("auth enabled: local=%v providers=%v web=%s", *localAuth, authSvc.ProviderNames(), *webURL)
	}

	gw := gateway.New(*gwURL, *gwToken)
	controlPlane := *provisionerCP
	if controlPlane == "" {
		controlPlane = hostReachable(*publicURL)
	}

	var prov provision.Provisioner = provision.Disabled{}
	switch *provisioner {
	case "docker":
		tunnelURL := *provisionerGateway
		if tunnelURL == "" {
			tunnelURL = hostReachable(*publicGateway)
		}
		if tunnelURL == "" {
			tunnelURL = internalGatewayWS(*gwURL)
		}
		prov = provision.NewDocker(*dockerHost, *workerImage, tunnelURL, "cloud", *workerModel, 4)
		log.Printf("provisioner: docker (image=%s, gateway=%s, control-plane=%s)", *workerImage, tunnelURL, controlPlane)
	case "k8s":
		tunnelURL := *provisionerGateway
		if tunnelURL == "" {
			tunnelURL = hostReachable(*publicGateway)
		}
		if tunnelURL == "" {
			tunnelURL = internalGatewayWS(*gwURL)
		}
		k8sProv, err := provision.NewKubernetes(*k8sNamespace, *workerImage, tunnelURL, controlPlane, "cloud", *workerModel, *k8sStorageClass, *k8sServiceAccount, 4)
		if err != nil {
			log.Fatalf("k8s provisioner: %v", err)
		}
		prov = k8sProv
		log.Printf("provisioner: k8s (ns=%s, image=%s, gateway=%s)", *k8sNamespace, *workerImage, tunnelURL)
	}
	srv := api.New(st, ast, authSvc, gw, *publicGateway, *workerSecret, prov, controlPlane)

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

	// 后台巡检：补删离线会话、清理长期离线的空设备。
	go srv.Maintain(ctx)

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

// internalGatewayWS 把服务端用的网关 HTTP 地址换成容器内可达的隧道地址。
func internalGatewayWS(base string) string {
	base = strings.TrimRight(base, "/")
	base = strings.TrimPrefix(base, "http://")
	base = strings.TrimPrefix(base, "https://")
	return "ws://" + base + "/v1/tunnel"
}

// hostReachable 把客户端用的 localhost 地址换成容器内可达的宿主机地址。
func hostReachable(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	raw = strings.ReplaceAll(raw, "//localhost", "//host.docker.internal")
	raw = strings.ReplaceAll(raw, "//127.0.0.1", "//host.docker.internal")
	return raw
}
