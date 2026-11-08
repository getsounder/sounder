package main

import (
	"embed"
	"flag"
	"fmt"
	"log"
	"net/url"
	"runtime"

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
	apiToken := flag.String("token", "", "移动端：访问 token")
	flag.Parse()

	// 移动端不跑本地 Worker：iOS 禁止 fork 子进程，Android 也不适合常驻。
	mobile := runtime.GOOS == "ios" || runtime.GOOS == "android"

	svc := shell.New(shell.Config{
		Mode:    *mode,
		Image:   *image,
		Binary:  *workerBin,
		Model:   *model,
	})

	target := "/"
	if mobile {
		if *apiBase == "" {
			log.Println("mobile 需要 -api 指向云端 sounder-server；否则页面无法访问任何 Worker")
		}
		target = fmt.Sprintf("/?api=%s&token=%s&mode=mobile",
			url.QueryEscape(*apiBase), url.QueryEscape(*apiToken))
	} else {
		ep, err := svc.StartWorker(*workspace)
		if err != nil {
			// 启动失败不退出：界面会显示原因，用户可以切换模式重试。
			log.Printf("worker start failed: %v", err)
		}
		if ep.BaseURL != "" {
			target = fmt.Sprintf("/?api=%s&token=%s&mode=%s",
				url.QueryEscape(ep.BaseURL), url.QueryEscape(ep.Token), url.QueryEscape(ep.Mode))
		}
	}

	app := application.New(application.Options{
		Name:        "Sounder",
		Description: "Sounder desktop agent",
		Services: []application.Service{
			application.NewService(svc),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
	})

	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title: "Sounder",
		URL:   target,
	})

	runErr := app.Run()
	_ = svc.StopWorker()
	if runErr != nil {
		log.Fatal(runErr)
	}
}
