package session

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// HistoryMessage 是从 Session JSONL 里投影出来的最小展示模型。
// 不还原模型上下文（那条链路必须走 PiG 的 active branch），只服务 UI。
type HistoryMessage struct {
	Role    string `json:"role"`
	Text    string `json:"text,omitempty"`
	Tool    string `json:"tool,omitempty"`
	IsError bool   `json:"isError,omitempty"`
	TS      string `json:"ts,omitempty"`
}

type rawEntry struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Message json.RawMessage `json:"message"`
}

type rawMessage struct {
	Role    string `json:"role"`
	Content []struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Name    string          `json:"name"`
		Content json.RawMessage `json:"content"`
	} `json:"content"`
	ToolName string `json:"toolName"`
	IsError  bool   `json:"isError"`
}

// ReadHistory 读取 JSONL 的最后 limit 条可展示消息。
// 文件可能很大，这里只保留尾部，避免一次性载入内存。
func ReadHistory(path string, limit int) ([]HistoryMessage, error) {
	if limit <= 0 {
		limit = 200
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var ring []HistoryMessage
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)

	for sc.Scan() {
		var e rawEntry
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Type != "message" {
			continue
		}
		var m rawMessage
		if json.Unmarshal(e.Message, &m) != nil {
			continue
		}
		hm := HistoryMessage{Role: m.Role, Tool: m.ToolName, IsError: m.IsError}
		switch m.Role {
		case "assistant":
			hm.Text = joinText(m.Content)
		case "user":
			hm.Text = joinText(m.Content)
		case "toolResult":
			hm.Text = joinText(m.Content)
		default:
			continue
		}
		if hm.Text == "" && hm.Tool == "" {
			continue
		}
		ring = append(ring, hm)
		if len(ring) > limit {
			ring = ring[len(ring)-limit:]
		}
	}
	return ring, sc.Err()
}

func joinText(blocks []struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`
	Content json.RawMessage `json:"content"`
}) string {
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "")
}

// findSessionFile 找 AgentDir 下最新的 session JSONL。
func findSessionFile(agentDir string) string {
	var newest string
	var newestMod int64
	_ = filepath.Walk(agentDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() || filepath.Ext(p) != ".jsonl" {
			return nil
		}
		if info.ModTime().UnixNano() > newestMod {
			newest, newestMod = p, info.ModTime().UnixNano()
		}
		return nil
	})
	return newest
}
