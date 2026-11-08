// Package policy 做工具分级、路径校验与高危命令识别。
// 它不依赖 PiG API，可以单独测试。
package policy

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

type Level int

const (
	L0Read Level = iota // read / grep / find / ls
	L1Write             // write / edit
	L2Exec              // bash
	L3Network           // curl / git push / npm install / 外部 MCP
)

func (l Level) String() string {
	return [...]string{"L0-read", "L1-write", "L2-exec", "L3-network"}[l]
}

type Decision int

const (
	Allow Decision = iota
	Ask
	Deny
)

// Classify 把工具名映射到风险级别。未知工具按 L3 处理（保守）。
func Classify(tool string) Level {
	switch strings.ToLower(tool) {
	case "read", "grep", "find", "ls", "glob":
		return L0Read
	case "write", "edit", "apply_patch", "multiedit":
		return L1Write
	case "bash", "shell", "run_command", "execute_bash":
		return L2Exec
	default:
		return L3Network
	}
}

// Decide 结合污点状态给出默认策略。
// tainted: 本会话读过不可信内容（issue / 网页 / 依赖 README / 日志）。
func Decide(lv Level, tainted bool) Decision {
	switch lv {
	case L0Read:
		return Allow
	case L1Write:
		if tainted {
			return Ask
		}
		return Allow
	case L2Exec:
		if tainted {
			return Ask
		}
		return Ask // 首次执行一律确认，由上层缓存"本会话已批准"
	case L3Network:
		return Ask
	default:
		return Deny
	}
}

// CheckPath 确保目标路径落在 workspace 内。symbolic link 需在调用方先 EvalSymlinks。
func CheckPath(workspace, target string) error {
	if workspace == "" {
		return fmt.Errorf("workspace is empty")
	}
	absWS, err := filepath.Abs(workspace)
	if err != nil {
		return err
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(absWS, absTarget)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path escapes workspace: %s", target)
	}
	return nil
}

// 高危模式：命中即强制审批，与 taint 无关。
var highRisk = []struct {
	name string
	re   *regexp.Regexp
}{
	{"curl-pipe-shell", regexp.MustCompile(`(?i)curl[^|]*\|\s*(ba)?sh`)},
	{"wget-pipe-shell", regexp.MustCompile(`(?i)wget[^|]*\|\s*(ba)?sh`)},
	{"base64-exec", regexp.MustCompile(`(?i)base64\s+-d.*\|\s*(ba)?sh`)},
	{"write-ci", regexp.MustCompile(`(?i)\.github/workflows/`)},
	{"write-git-hook", regexp.MustCompile(`(?i)\.git/hooks/`)},
	{"read-ssh", regexp.MustCompile(`(?i)(~|\$HOME)/\.ssh|/etc/passwd|/etc/shadow`)},
	{"force-push", regexp.MustCompile(`(?i)git\s+push\s+(--force|-f)`)},
	{"rm-root", regexp.MustCompile(`(?i)rm\s+-rf\s+/(\s|$)`)},
	{"env-exfil", regexp.MustCompile(`(?i)(env|printenv)\s*\|`)},
	{"chmod-suid", regexp.MustCompile(`(?i)chmod\s+[0-7]*[4-7][0-7]{2}\s`)},
}

// ScanCommand 返回命中的高危规则名。空切片表示未命中。
func ScanCommand(command string) []string {
	var hits []string
	for _, h := range highRisk {
		if h.re.MatchString(command) {
			hits = append(hits, h.name)
		}
	}
	return hits
}

// Args 从工具参数里提取命令或路径，供审批展示与校验。
func Args(raw json.RawMessage) map[string]any {
	var m map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

// CommandOf 兼容不同工具的参数字段名。
func CommandOf(args map[string]any) string {
	for _, k := range []string{"command", "cmd", "script"} {
		if v, ok := args[k].(string); ok {
			return v
		}
	}
	return ""
}

// PathOf 兼容 write/edit 的 path 字段。
func PathOf(args map[string]any) string {
	for _, k := range []string{"path", "file_path", "file"} {
		if v, ok := args[k].(string); ok {
			return v
		}
	}
	return ""
}
