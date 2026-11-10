package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSeedAgentDirOllama(t *testing.T) {
	dir := t.TempDir()
	agentDir := filepath.Join(dir, "agent")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OLLAMA_HOST", "127.0.0.1:11434")

	m := NewManager(1, dir, "ollama/qwen2.5:3b", "")
	m.seedAgentDir(agentDir, "ollama/qwen2.5:3b")

	body := readFile(t, filepath.Join(agentDir, "models.json"))
	for _, want := range []string{`"baseUrl": "http://127.0.0.1:11434/v1"`, `"api": "openai-completions"`, `"ollama"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("models.json 缺少 %s:\n%s", want, body)
		}
	}
}

func TestSeedAgentDirKeepsExisting(t *testing.T) {
	dir := t.TempDir()
	agentDir := filepath.Join(dir, "agent")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	custom := `{"providers":{"ollama":{"baseUrl":"http://example:1/v1","api":"openai-completions"}}}`
	path := filepath.Join(agentDir, "models.json")
	if err := os.WriteFile(path, []byte(custom), 0o600); err != nil {
		t.Fatal(err)
	}

	m := NewManager(1, dir, "ollama/llama2", "")
	m.seedAgentDir(agentDir, "ollama/llama2")

	if got := readFile(t, path); got != custom {
		t.Fatalf("已有 models.json 被覆盖:\n%s", got)
	}
}

func TestSeedAgentDirCustomModelsFile(t *testing.T) {
	dir := t.TempDir()
	agentDir := filepath.Join(dir, "agent")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	customPath := filepath.Join(dir, "custom-models.json")
	custom := `{"providers":{"local":{"baseUrl":"http://127.0.0.1:1234/v1","api":"openai-completions"}}}`
	if err := os.WriteFile(customPath, []byte(custom), 0o600); err != nil {
		t.Fatal(err)
	}

	m := NewManager(1, dir, "local/model", "")
	m.SetModelsFile(customPath)
	m.seedAgentDir(agentDir, "local/model")

	if got := readFile(t, filepath.Join(agentDir, "models.json")); got != custom {
		t.Fatalf("自定义 models.json 未复制:\n%s", got)
	}
}

func TestSeedAgentDirSkipsOtherProviders(t *testing.T) {
	dir := t.TempDir()
	agentDir := filepath.Join(dir, "agent")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}

	m := NewManager(1, dir, "openai/gpt-5", "")
	m.seedAgentDir(agentDir, "openai/gpt-5")

	if _, err := os.Stat(filepath.Join(agentDir, "models.json")); !os.IsNotExist(err) {
		t.Fatalf("非 ollama 模型不应生成 models.json: %v", err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
