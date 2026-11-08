// shellcheck：在不启动 GUI 的情况下验证 Shell 的 Worker 生命周期。
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
	if len(os.Args) > 2 {
		binary = os.Args[2]
	}

	svc := shell.New(shell.Config{
		Mode:    mode,
		Binary:  binary,
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
