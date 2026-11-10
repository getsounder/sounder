package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/getsounder/sounder-desktop/shell"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	mode := flag.String("mode", "container", "container | process")
	image := flag.String("image", "ghcr.io/getsounder/sounder-worker:dev", "worker 镜像（与云端同 digest）")
	workerBin := flag.String("worker", "", "process 模式下的 sounder-worker 路径")
	model := flag.String("model", "minimax-cn/MiniMax-M3", "默认模型")
	workspace := flag.String("workspace", "", "工作区目录（默认 home）")
	apiBase := flag.String("api", "", "移动端：云端 sounder-server 地址，如 https://sounder.example.com")
	apiToken := flag.String("token", "", "访问 token：移动端直连控制面，或桌面端登记设备用")
	server := flag.String("server", os.Getenv("SOUNDER_SERVER"), "控制面地址；设置后窗口直接连服务端并登记本机设备")
	gateway := flag.String("gateway", os.Getenv("SOUNDER_GATEWAY"), "网关隧道地址；留空则从控制面的 /v1/meta 读取")
	gatewayToken := flag.String("gateway-token", os.Getenv("SOUNDER_GATEWAY_TOKEN"), "网关 Bearer token")
	flag.Parse()

	// 没显式给网关地址时，先问一下控制面（/v1/meta.gatewayUrl），再创建 Service。
	if *gateway == "" && *server != "" {
		if gw := fetchGatewayURL(*server); gw != "" {
			*gateway = gw
			log.Printf("gateway from %s: %s", *server, gw)
		}
	}

	// 移动端不跑本地 Worker：iOS 禁止 fork 子进程，Android 也不适合常驻。
	mobile := runtime.GOOS == "ios" || runtime.GOOS == "android"

	svc := shell.New(shell.Config{
		Mode:         *mode,
		Image:        *image,
		Binary:       *workerBin,
		Model:        *model,
		Server:       *server,
		AccessToken:  *apiToken,
		Gateway:      *gateway,
		GatewayToken: *gatewayToken,
	})
	authSvc := shell.NewAuth()

	target := "/"
	if mobile {
		if *apiBase == "" {
			log.Println("mobile 需要 -api 指向云端 sounder-server；否则页面无法访问任何 Worker")
		}
		target = fmt.Sprintf("/?api=%s&token=%s&mode=mobile",
			url.QueryEscape(*apiBase), url.QueryEscape(*apiToken))
	} else {
		// 本机设备始终启动：即使连了控制面，它也是这台机器上的执行环境。
		ep, err := svc.StartWorker(*workspace)
		if err != nil {
			// 启动失败不退出：界面会显示原因，用户可以切换模式重试。
			log.Printf("worker start failed: %v", err)
		}
		switch {
		case *server != "":
			// 连控制面：窗口走服务端，登录后由界面把本机设备登记上去。
			target = fmt.Sprintf("/?api=%s&mode=remote", url.QueryEscape(*server))
			if *apiToken != "" {
				target += "&token=" + url.QueryEscape(*apiToken)
			}
		case ep.BaseURL != "":
			target = fmt.Sprintf("/?api=%s&token=%s&mode=%s&device=%s",
				url.QueryEscape(ep.BaseURL), url.QueryEscape(ep.Token), url.QueryEscape(ep.Mode), url.QueryEscape(ep.DeviceID))
		}
	}

	app := application.New(application.Options{
		Name:        "Sounder",
		Description: "Sounder desktop agent",
		Services: []application.Service{
			application.NewService(svc),
			application.NewService(authSvc),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
	})

	// Cmd+Q / 关闭窗口时 app.Run 不一定返回，用 OnShutdown 兜底清理 Worker，避免残留容器占端口。
	app.OnShutdown(func() {
		if err := svc.StopWorker(); err != nil {
			log.Printf("stop worker: %v", err)
		}
	})

	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            "Sounder",
		URL:              target,
		Width:            1080,
		Height:           720,
		MinWidth:         820,
		MinHeight:        560,
		BackgroundColour: application.NewRGBA(11, 13, 18, 255),
	})

	runErr := app.Run()
	_ = svc.StopWorker()
	if runErr != nil {
		log.Fatal(runErr)
	}
}

// fetchGatewayURL 从控制面的 /v1/meta 取网关隧道地址，省得客户端再配一遍。
func fetchGatewayURL(server string) string {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(strings.TrimRight(server, "/") + "/v1/meta")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var meta struct {
		GatewayURL string `json:"gatewayUrl"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&meta); err != nil {
		return ""
	}
	return meta.GatewayURL
}
