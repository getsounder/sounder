// shellcheck：在不启动 GUI 的情况下验证 Shell 的 Worker 生命周期与设备管理。
//
//	go run ./cmd/shellcheck                      # process 模式，跑一次 start/stop
//	go run ./cmd/shellcheck container            # container 模式
//	go run ./cmd/shellcheck devices [image]      # 设备管理：列表 / 新增 / 启动 / 删除
package main

import (
	"fmt"
	"os"

	"github.com/getsounder/sounder-desktop/shell"
)

func main() {
	mode := "process"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	binary := "/tmp/sounder-bin/worker"
	if len(os.Args) > 2 && mode != "devices" {
		binary = os.Args[2]
	}

	svc := shell.New(shell.Config{
		Mode:    "process",
		Binary:  binary,
		DataDir: "/tmp/sounder-desktop-data",
		Model:   "minimax-cn/MiniMax-M3",
	})

	if mode == "devices" {
		image := "ghcr.io/getsounder/sounder-worker:dev"
		if len(os.Args) > 2 {
			image = os.Args[2]
		}
		runDevices(image)
		return
	}

	svc = shell.New(shell.Config{
		Mode:    mode,
		Binary:  binary,
		Image:   "ghcr.io/getsounder/sounder-worker:dev",
		DataDir: "/tmp/sounder-desktop-data",
		Model:   "minimax-cn/MiniMax-M3",
	})

	ep, err := svc.StartWorker("/tmp/sounder-desktop-ws")
	if err != nil {
		fmt.Println("start failed:", err)
		os.Exit(1)
	}
	fmt.Printf("endpoint: %+v\n", ep)

	if err := svc.StopWorker(); err != nil {
		fmt.Println("stop failed:", err)
		os.Exit(1)
	}
	fmt.Println("stopped ok")
}

func runDevices(image string) {
	svc := shell.New(shell.Config{
		Mode:    "container",
		Image:   image,
		DataDir: "/tmp/sounder-devices-test",
		Model:   "minimax-cn/MiniMax-M3",
	})

	list, err := svc.ListDevices()
	if err != nil {
		fmt.Println("list failed:", err)
		os.Exit(1)
	}
	fmt.Printf("devices: %d\n", len(list))
	for _, d := range list {
		fmt.Printf("  - %s %s %s\n", d.ID, d.Name, d.Status)
	}

	first, err := svc.StartWorker("/tmp/sounder-desktop-ws")
	if err != nil {
		fmt.Println("start default failed:", err)
		os.Exit(1)
	}
	fmt.Printf("default endpoint: %+v\n", first)

	added, err := svc.AddDevice("测试设备 B")
	if err != nil {
		fmt.Println("add failed:", err)
		os.Exit(1)
	}
	fmt.Printf("added: %s %s %s %s\n", added.ID, added.Name, added.Status, added.BaseURL)

	list, _ = svc.ListDevices()
	for _, d := range list {
		fmt.Printf("  - %s %s %s active=%v\n", d.ID, d.Name, d.Status, d.Active)
	}

	if err := svc.RemoveDevice(added.ID); err != nil {
		fmt.Println("remove failed:", err)
		os.Exit(1)
	}
	list, _ = svc.ListDevices()
	fmt.Printf("after remove: %d devices\n", len(list))

	if err := svc.StopWorker(); err != nil {
		fmt.Println("stop failed:", err)
		os.Exit(1)
	}
	fmt.Println("devices ok")
}
