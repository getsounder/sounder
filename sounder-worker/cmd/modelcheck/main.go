// modelcheck 是一个诊断工具：验证 provider 认证与模型是否能真正返回内容。
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/MichaelKinsy/PiG/coding"
)

func main() {
	modelID := "minimax-cn/MiniMax-M3"
	if len(os.Args) > 1 {
		modelID = os.Args[1]
	}

	dir, err := os.MkdirTemp("", "modelcheck-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	services, err := coding.CreateAgentSessionServices(coding.CreateAgentSessionServicesOptions{
		CWD:      dir,
		AgentDir: dir + "/agent",
	})
	if err != nil {
		log.Fatal(err)
	}
	defer services.Close()

	rt, err := coding.NewRuntime(coding.RuntimeOptions{Services: services})
	if err != nil {
		log.Fatal(err)
	}
	defer rt.Close()

	model, err := coding.BuildModel(modelID, services)
	fmt.Printf("build model: %+v\nerr: %v\n", model, err)

	sess, err := rt.New(coding.SessionStartOptions{Model: model})
	if err != nil {
		log.Fatal(err)
	}
	defer sess.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	msgs, sendErr := sess.Send(ctx, "只回复：OK")
	fmt.Printf("send err: %v, messages: %d\n", sendErr, len(msgs))
	for i, m := range msgs {
		fmt.Printf("[%d]", i)
		switch {
		case m.Assistant != nil:
			a := m.Assistant
			fmt.Printf(" assistant stop=%q err=%q\n", a.StopReason, a.ErrorMessage)
			for j, b := range a.Content {
				fmt.Printf("    block[%d] %T %+v\n", j, b, b)
			}
		case m.User != nil:
			fmt.Printf(" user content=%T\n", m.User.Content)
		case m.System != nil:
			fmt.Printf(" system\n")
		default:
			fmt.Printf(" %T\n", m)
		}
	}
}
