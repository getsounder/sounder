package session

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseAskUserSingle(t *testing.T) {
	args, _ := json.Marshal(map[string]string{"command": `sounder-ask "迁移现在跑还是等低峰？" --options "现在|22:00|dry-run"`})
	qs, ok := parseAskUser(args)
	if !ok || len(qs) != 1 {
		t.Fatalf("expect 1 question, got %+v ok=%v", qs, ok)
	}
	if qs[0].Question != "迁移现在跑还是等低峰？" {
		t.Fatalf("question = %q", qs[0].Question)
	}
	if len(qs[0].Options) != 3 || qs[0].Options[1] != "22:00" {
		t.Fatalf("options = %+v", qs[0].Options)
	}
}

func TestParseAskUserMultiple(t *testing.T) {
	payload := `{"questions":[{"question":"要不要备份？","options":["要","不要"]},{"question":"窗口选哪个？"}]}`
	args, _ := json.Marshal(map[string]string{"command": "sounder-ask --json '" + payload + "'"})
	qs, ok := parseAskUser(args)
	if !ok || len(qs) != 2 {
		t.Fatalf("expect 2 questions, got %+v ok=%v", qs, ok)
	}
	if qs[0].Question != "要不要备份？" || len(qs[0].Options) != 2 {
		t.Fatalf("q0 = %+v", qs[0])
	}
	if qs[1].Question != "窗口选哪个？" {
		t.Fatalf("q1 = %+v", qs[1])
	}
}

func TestParseAskUserIgnoresPlainBash(t *testing.T) {
	args, _ := json.Marshal(map[string]string{"command": "ls -la"})
	if _, ok := parseAskUser(args); ok {
		t.Fatal("普通命令不应被识别为提问")
	}
}

func TestFormatAskAnswers(t *testing.T) {
	qs := []AskQuestion{{Question: "要不要备份？"}, {Question: "窗口？"}}
	raw := `{"answers":[{"question":"要不要备份？","answer":"要"},{"question":"","answer":"22:00"}]}`
	out := formatAskAnswers(qs, raw)
	if !strings.Contains(out, "1) 要不要备份？ → 要") {
		t.Fatalf("missing first answer: %s", out)
	}
	if !strings.Contains(out, "2) 窗口？ → 22:00") {
		t.Fatalf("missing second answer: %s", out)
	}

	if got := formatAskAnswers(qs, "直接回答"); got != "用户回答：直接回答" {
		t.Fatalf("plain answer = %q", got)
	}
}
