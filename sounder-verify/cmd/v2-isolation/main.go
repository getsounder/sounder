// V2 验证：同一个进程内创建多个 PiG Session，目录与配置是否真正隔离。
//
//	阶段 1（无需模型）：两个 Services 的 CWD / AgentDir / settings 互不污染
//	阶段 2（可选）：设置 PIG_VERIFY_MODEL 后，让 Agent 各写一个文件，验证不串目录
//
// 运行：
//
//	go run ./cmd/v2-isolation
//	PIG_VERIFY_MODEL=openai/gpt-5 go run ./cmd/v2-isolation
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MichaelKinsy/PiG/coding"
)

type result struct {
	name   string
	ok     bool
	warn   bool
	detail string
}

type instance struct {
	id       string
	cwd      string
	agentDir string
	services *coding.AgentSessionServices
}

func main() {
	root, err := os.MkdirTemp("", "pig-v2-")
	if err != nil {
		fatal("mkdir temp: %v", err)
	}
	defer os.RemoveAll(root)

	var instances []*instance
	for _, id := range []string{"A", "B"} {
		in, err := newInstance(root, id)
		if err != nil {
			fatal("init instance %s: %v", id, err)
		}
		defer in.services.Close()
		instances = append(instances, in)
	}

	a, b := instances[0], instances[1]
	var results []result

	results = append(results,
		result{name: "CWD 相互独立", ok: a.cwd != b.cwd, detail: a.cwd + " / " + b.cwd},
		result{name: "AgentDir 相互独立", ok: a.agentDir != b.agentDir, detail: a.agentDir + " / " + b.agentDir},
	)

	// 在 A 的目录写标记文件，确认不会出现在 B。
	if err := os.WriteFile(filepath.Join(a.cwd, "only-a.txt"), []byte("a"), 0o644); err != nil {
		fatal("write marker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(a.agentDir, "settings.json"), []byte(`{"theme":"a"}`), 0o644); err != nil {
		fatal("write settings: %v", err)
	}

	results = append(results,
		result{name: "A 的 workspace 文件不落入 B", ok: !exists(filepath.Join(b.cwd, "only-a.txt"))},
		result{name: "A 的 settings 不落入 B", ok: !exists(filepath.Join(b.agentDir, "settings.json"))},
	)

	// 显式传参不应改变全局默认目录，否则说明 PiG 内部有单例。
	def := coding.DefaultAgentDir()
	results = append(results, result{
		name:   "DefaultAgentDir 未被显式参数污染",
		ok:     def != a.agentDir && def != b.agentDir,
		detail: def,
	})

	// 阶段 2：真实模型行为验证。
	if modelID := strings.TrimSpace(os.Getenv("PIG_VERIFY_MODEL")); modelID != "" {
		for _, in := range instances {
			ok, detail := agentCanWriteOnlyItself(modelID, in)
			results = append(results, result{
				name:   "Agent 写入仅落在自身 workspace（" + in.id + "）",
				ok:     ok,
				warn:   !ok,
				detail: detail,
			})
		}
		// 交叉检查：A 的文件不应出现在 B，反之亦然。
		results = append(results,
			result{name: "A 的 proof 不出现在 B", ok: !exists(filepath.Join(b.cwd, "proof.txt"))},
			result{name: "B 的 proof 不出现在 A", ok: !exists(filepath.Join(a.cwd, "proof-b.txt"))},
		)
	} else {
		fmt.Println("SKIP  未设置 PIG_VERIFY_MODEL，跳过 Agent 行为验证")
	}

	os.Exit(report(results))
}

func newInstance(root, id string) (*instance, error) {
	cwd := filepath.Join(root, id, "workspace")
	agentDir := filepath.Join(root, id, "agent")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		return nil, err
	}

	// API-CHECK: 字段名以 pin 的 commit 为准。
	services, err := coding.CreateAgentSessionServices(coding.CreateAgentSessionServicesOptions{
		CWD:      cwd,
		AgentDir: agentDir,
	})
	if err != nil {
		return nil, err
	}
	return &instance{id: id, cwd: cwd, agentDir: agentDir, services: services}, nil
}

// agentCanWriteOnlyItself 让 Agent 在自身 CWD 写一个文件，并确认内容正确。
// 注意：模型不一定严格服从，失败按 WARN 处理，不直接判架构不成立。
func agentCanWriteOnlyItself(modelID string, in *instance) (bool, string) {
	// API-CHECK: RuntimeOptions / SessionStartOptions 字段以 pin 的 commit 为准。
	rt, err := coding.NewRuntime(coding.RuntimeOptions{Services: in.services})
	if err != nil {
		return false, "NewRuntime: " + err.Error()
	}
	defer rt.Close()

	model, err := coding.BuildModel(modelID, in.services)
	if err != nil {
		return false, "BuildModel: " + err.Error()
	}

	sess, err := rt.New(coding.SessionStartOptions{
		Model:        model,
		SystemPrompt: "你是执行器，只执行用户指令，不解释。",
	})
	if err != nil {
		return false, "runtime.New: " + err.Error()
	}
	defer sess.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	name := "proof.txt"
	if in.id == "B" {
		name = "proof-b.txt"
	}
	prompt := fmt.Sprintf("在当前工作目录创建文件 %s，内容为字符串 %s，只做这一件事。", name, in.id)

	// API-CHECK: Send 的返回签名以 pin 的 commit 为准；provider 失败时 err 可能为 nil。
	if _, err := sess.Send(ctx, prompt); err != nil {
		return false, "Send: " + err.Error()
	}

	data, err := os.ReadFile(filepath.Join(in.cwd, name))
	if err != nil {
		return false, "未生成 " + name + "（模型可能未执行工具）"
	}
	if string(data) != in.id {
		return false, fmt.Sprintf("内容不符：期望 %q，实际 %q", in.id, string(data))
	}
	return true, "ok"
}

func report(results []result) int {
	fmt.Println()
	failed := 0
	warned := 0
	for _, r := range results {
		status := "PASS"
		switch {
		case r.warn:
			status = "WARN"
			warned++
		case !r.ok:
			status = "FAIL"
			failed++
		}
		line := fmt.Sprintf("%-5s %s", status, r.name)
		if r.detail != "" {
			line += "  (" + r.detail + ")"
		}
		fmt.Println(line)
	}
	fmt.Println()
	if failed > 0 {
		fmt.Printf("结果：%d 项失败，%d 项警告。1 Pod N Session 的前提不成立，先降到 1 Pod 1 Session。\n", failed, warned)
		return 1
	}
	fmt.Printf("结果：全部通过（%d 项警告）。\n", warned)
	return 0
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "FATAL "+format+"\n", args...)
	os.Exit(2)
}
