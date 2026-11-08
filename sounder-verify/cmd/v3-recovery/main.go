// V3 验证：Worker 被 kill -9 后，Session 日志是否完整、能否继续。
//
//	-mode=create   建会话、跑两轮、打印 session 文件路径后挂起（等待被 kill）
//	-mode=inspect  纯文件层面检查 JSONL 完整性与树结构（不需要模型）
//	-mode=resume   重新打开日志并追加一轮（需要模型）
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/MichaelKinsy/PiG/coding"
)

type entry struct {
	ID       string  `json:"id"`
	ParentID *string `json:"parentId"`
	Type     string  `json:"type"`
}

func main() {
	mode := flag.String("mode", "inspect", "create | inspect | resume")
	out := flag.String("out", "/tmp/pig-v3", "工作目录（create 模式）")
	session := flag.String("session", "", "session.jsonl 路径（inspect / resume 模式）")
	modelID := flag.String("model", "", "provider/model（create / resume 需要）")
	flag.Parse()

	switch *mode {
	case "create":
		create(*out, *modelID)
	case "inspect":
		os.Exit(inspect(resolveSession(*out, *session)))
	case "resume":
		resume(resolveSession(*out, *session), *out, *modelID)
	default:
		fatal("未知 mode: %s", *mode)
	}
}

func create(out, modelID string) {
	if modelID == "" {
		fatal("create 需要 -model")
	}
	cwd, agentDir := prepare(out)

	services, err := coding.CreateAgentSessionServices(coding.CreateAgentSessionServicesOptions{
		CWD:      cwd,
		AgentDir: agentDir,
	})
	if err != nil {
		fatal("CreateAgentSessionServices: %v", err)
	}
	defer services.Close()

	rt, err := coding.NewRuntime(coding.RuntimeOptions{Services: services})
	if err != nil {
		fatal("NewRuntime: %v", err)
	}
	defer rt.Close()

	model, err := coding.BuildModel(modelID, services)
	if err != nil {
		fatal("BuildModel: %v", err)
	}

	sess, err := rt.New(coding.SessionStartOptions{
		Model:        model,
		SystemPrompt: "你是执行器，只执行指令，不解释。",
	})
	if err != nil {
		fatal("runtime.New: %v", err)
	}
	defer sess.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for i := 1; i <= 2; i++ {
		if _, err := sess.Send(ctx, fmt.Sprintf("请只回复：ROUND-%d", i)); err != nil {
			fatal("Send round %d: %v", i, err)
		}
	}

	path := findSessionFile(agentDir)
	if path == "" {
		fatal("未找到 session 文件，检查 AgentDir: %s", agentDir)
	}
	if err := os.WriteFile(filepath.Join(out, "session-path.txt"), []byte(path), 0o644); err != nil {
		fatal("write session-path.txt: %v", err)
	}

	fmt.Println("SESSION_FILE=" + path)
	fmt.Println("READY_TO_KILL")
	for { // 等待外部 kill -9；不要用 select{}，会触发 deadlock panic
		time.Sleep(time.Hour)
	}
}

// inspect 只依赖文件格式，不需要 PiG 运行时。
func inspect(path string) int {
	if path == "" {
		fatal("inspect 需要 -session 或 -out")
	}
	entries, err := readEntries(path)
	if err != nil {
		fmt.Println("FAIL  无法解析 session 文件:", err)
		return 1
	}

	// 第一行是 session header（type=session），不计入树统计。
	var body []entry
	for _, e := range entries {
		if e.Type != "session" {
			body = append(body, e)
		}
	}
	entries = body

	ids := map[string]int{}
	roots := 0
	for _, e := range entries {
		if e.ID != "" {
			ids[e.ID]++
		}
		if e.ParentID == nil || *e.ParentID == "" {
			roots++
		}
	}

	dups := 0
	for _, n := range ids {
		if n > 1 {
			dups++
		}
	}
	broken := 0
	for _, e := range entries {
		if e.ParentID != nil && *e.ParentID != "" && ids[*e.ParentID] == 0 {
			broken++
		}
	}

	fmt.Printf("文件      %s\n", path)
	fmt.Printf("条目数    %d\n", len(entries))
	fmt.Printf("root 数   %d\n", roots)
	fmt.Printf("重复 id   %d\n", dups)
	fmt.Printf("断链      %d\n", broken)

	failed := false
	if len(entries) == 0 {
		fmt.Println("FAIL  日志为空，kill 前可能未落盘")
		failed = true
	}
	if roots != 1 {
		fmt.Printf("WARN  root 数为 %d（期望 1），检查是否发生 fork\n", roots)
	}
	if dups > 0 {
		fmt.Println("FAIL  存在重复 id，append 非原子")
		failed = true
	}
	if broken > 0 {
		fmt.Println("FAIL  存在断链，恢复会丢上下文")
		failed = true
	}
	if failed {
		return 1
	}
	fmt.Println("PASS")
	return 0
}

func resume(path, out, modelID string) {
	if path == "" {
		fatal("resume 需要 -session 或 -out")
	}
	if modelID == "" {
		fatal("resume 需要 -model")
	}
	cwd, agentDir := prepare(out)

	services, err := coding.CreateAgentSessionServices(coding.CreateAgentSessionServicesOptions{
		CWD:      cwd,
		AgentDir: agentDir,
	})
	if err != nil {
		fatal("CreateAgentSessionServices: %v", err)
	}
	defer services.Close()

	rt, err := coding.NewRuntime(coding.RuntimeOptions{Services: services})
	if err != nil {
		fatal("NewRuntime: %v", err)
	}
	defer rt.Close()

	// API-CHECK: 0.7.0 的签名是 Open(path, SessionStartOptions)。
	sess, err := rt.Open(path, coding.SessionStartOptions{})
	if err != nil {
		fatal("Runtime.Open: %v", err)
	}
	defer sess.Close()

	before := countLines(path)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if _, err := sess.Send(ctx, "请只回复：RESUMED"); err != nil {
		fatal("Send after resume: %v", err)
	}
	after := countLines(path)

	fmt.Printf("恢复前条目 %d，恢复后条目 %d\n", before, after)
	if after <= before {
		fmt.Println("FAIL  追加未生效，恢复链路不可用")
		os.Exit(1)
	}
	fmt.Println("PASS")
}

func prepare(out string) (cwd, agentDir string) {
	cwd = filepath.Join(out, "workspace")
	agentDir = filepath.Join(out, "agent")
	for _, d := range []string{cwd, agentDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			fatal("mkdir %s: %v", d, err)
		}
	}
	return cwd, agentDir
}

// findSessionFile 扫描 AgentDir 下最新的 jsonl，避免依赖未导出的路径 API。
func findSessionFile(agentDir string) string {
	var newest string
	var newestMod time.Time
	_ = filepath.Walk(agentDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() || filepath.Ext(p) != ".jsonl" {
			return nil
		}
		if info.ModTime().After(newestMod) {
			newest, newestMod = p, info.ModTime()
		}
		return nil
	})
	return newest
}

func resolveSession(out, explicit string) string {
	if explicit != "" {
		return explicit
	}
	b, err := os.ReadFile(filepath.Join(out, "session-path.txt"))
	if err != nil {
		return ""
	}
	return string(b)
}

func readEntries(path string) ([]entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var entries []entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e entry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("解析行失败: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, sc.Err()
}

func countLines(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		n++
	}
	return n
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "FATAL "+format+"\n", args...)
	os.Exit(2)
}
