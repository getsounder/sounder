package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/getsounder/sounder-worker/internal/policy"
	"github.com/getsounder/sounder-worker/internal/protocol"
)

var ErrFull = errors.New("worker at capacity")

type Manager struct {
	mu       sync.RWMutex
	actors   map[string]*Actor
	max      int
	dataDir  string
	modelID  string
	prompt   string
	approver policy.Approver
	sink     func(protocol.Envelope)
	// modelsFile 是可选的 PiG models.json；为空时按 dataDir/models.json 查找。
	modelsFile string
}

func NewManager(max int, dataDir, modelID, systemPrompt string) *Manager {
	if max <= 0 {
		max = 4
	}
	return &Manager{
		actors:     make(map[string]*Actor),
		max:        max,
		dataDir:    dataDir,
		modelID:    modelID,
		prompt:     systemPrompt,
		modelsFile: filepath.Join(dataDir, "models.json"),
	}
}

func (m *Manager) Create(req protocol.CreateSessionRequest) (*Actor, error) {
	m.mu.Lock()
	if len(m.actors) >= m.max {
		m.mu.Unlock()
		return nil, ErrFull
	}
	m.mu.Unlock()

	id := NewID()
	cwd := req.Workspace
	if cwd == "" {
		cwd = filepath.Join(m.dataDir, "sessions", id, "workspace")
	}
	agentDir := filepath.Join(m.dataDir, "sessions", id, "agent")
	for _, d := range []string{cwd, agentDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	modelID := req.Model
	if modelID == "" {
		modelID = m.modelID
	}
	m.seedAgentDir(agentDir, modelID)
	sysPrompt := req.SystemPrompt
	if sysPrompt == "" {
		sysPrompt = m.prompt
	}

	actor, err := New(Options{
		ID:           id,
		CWD:          cwd,
		AgentDir:     agentDir,
		ModelID:      modelID,
		SystemPrompt: sysPrompt,
		Quota:        Quota{MaxConcurrentTurns: 1, SendTimeout: 30 * time.Minute},
	})
	if err != nil {
		return nil, err
	}

	m.mu.RLock()
	approver := m.approver
	sink := m.sink
	m.mu.RUnlock()
	if approver != nil {
		actor.SetApprover(approver)
	}
	if sink != nil {
		actor.SetEventSink(sink)
	}

	m.mu.Lock()
	m.actors[id] = actor
	m.mu.Unlock()
	return actor, nil
}

func (m *Manager) Get(id string) (*Actor, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a, ok := m.actors[id]
	return a, ok
}

func (m *Manager) List() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.actors))
	for id := range m.actors {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Views 返回带标题/模型/状态的会话列表，按创建时间倒序。
func (m *Manager) Views() []protocol.SessionView {
	m.mu.RLock()
	actors := make([]*Actor, 0, len(m.actors))
	for _, a := range m.actors {
		actors = append(actors, a)
	}
	m.mu.RUnlock()

	views := make([]protocol.SessionView, 0, len(actors))
	for _, a := range actors {
		views = append(views, a.View())
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].CreatedAt == views[j].CreatedAt {
			return views[i].ID > views[j].ID
		}
		return views[i].CreatedAt > views[j].CreatedAt
	})
	return views
}

func (m *Manager) Delete(id string) {
	m.mu.Lock()
	a, ok := m.actors[id]
	delete(m.actors, id)
	m.mu.Unlock()
	if ok {
		a.Close()
	}
	// 目录也要删掉：否则下次启动 Restore 会把已删除的会话又恢复出来。
	if strings.HasPrefix(id, "ses_") {
		if err := os.RemoveAll(filepath.Join(m.dataDir, "sessions", id)); err != nil {
			log.Printf("delete session dir %s: %v", id, err)
		}
	}
}

func (m *Manager) CloseAll() {
	m.mu.Lock()
	actors := make([]*Actor, 0, len(m.actors))
	for _, a := range m.actors {
		actors = append(actors, a)
	}
	m.actors = map[string]*Actor{}
	m.mu.Unlock()

	for _, a := range actors {
		a.Close()
	}
}

func (m *Manager) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.actors)
}

func (m *Manager) Capacity() int { return m.max }

// SetApprover 把审批闸门注入后续创建的会话。
func (m *Manager) SetApprover(a policy.Approver) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.approver = a
}

// SetEventSink 把事件上行通道注入后续创建的会话（隧道/消息队列）。
func (m *Manager) SetEventSink(fn func(protocol.Envelope)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sink = fn
}

// Restore 扫描 dataDir 下已有的会话目录，用 ResumePath 重新打开。
// 返回恢复成功的会话数。损坏的会话会被跳过，不影响其他会话。
func (m *Manager) Restore() int {
	root := filepath.Join(m.dataDir, "sessions")
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}

	restored := 0
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "ses_") {
			continue
		}
		id := e.Name()
		agentDir := filepath.Join(root, id, "agent")
		path := findSessionFile(agentDir)
		if path == "" {
			continue
		}
		cwd := filepath.Join(root, id, "workspace")
		m.seedAgentDir(agentDir, m.modelID)
		actor, err := New(Options{
			ID:           id,
			CWD:          cwd,
			AgentDir:     agentDir,
			ModelID:      m.modelID,
			SystemPrompt: m.prompt,
			ResumePath:   path,
			Quota:        Quota{MaxConcurrentTurns: 1, SendTimeout: 30 * time.Minute},
		})
		if err != nil {
			log.Printf("restore %s: %v (skipped)", id, err)
			continue
		}
		// 恢复出来的会话标题取自历史里的第一条用户消息。
		if msgs, err := ReadHistory(path, 50); err == nil {
			for _, m := range msgs {
				if m.Role == "user" && m.Text != "" {
					actor.SetTitle(m.Text)
					break
				}
			}
		}
		m.mu.Lock()
		m.actors[id] = actor
		m.mu.Unlock()
		restored++
	}
	return restored
}

// SetModelsFile 指定自定义的 PiG models.json（留空则用 <data>/models.json）。
func (m *Manager) SetModelsFile(path string) {
	m.mu.Lock()
	m.modelsFile = path
	m.mu.Unlock()
}

// seedAgentDir 在会话的 AgentDir 里准备 models.json。
// PiG 每个会话有独立 AgentDir，所以 provider 配置必须随会话一起落地；
// 否则 ollama 会退回到 api.openai.com。已有的 models.json 不会被覆盖。
func (m *Manager) seedAgentDir(agentDir, modelID string) {
	target := filepath.Join(agentDir, "models.json")
	if _, err := os.Stat(target); err == nil {
		return
	}

	m.mu.RLock()
	modelsFile := m.modelsFile
	m.mu.RUnlock()
	if modelsFile != "" {
		if b, err := os.ReadFile(modelsFile); err == nil {
			if err := os.WriteFile(target, b, 0o600); err != nil {
				log.Printf("seed models.json: %v", err)
			}
			return
		}
	}

	provider := modelID
	if i := strings.Index(provider, "/"); i > 0 {
		provider = provider[:i]
	}
	if provider != "ollama" {
		return
	}

	host := strings.TrimSpace(os.Getenv("OLLAMA_HOST"))
	if host == "" {
		host = "http://127.0.0.1:11434/v1"
	}
	if !strings.Contains(host, "://") {
		host = "http://" + host
	}
	host = strings.TrimRight(host, "/")
	if !strings.HasSuffix(host, "/v1") {
		host += "/v1"
	}

	cfg := map[string]any{
		"providers": map[string]any{
			"ollama": map[string]any{
				"name":    "Ollama (local)",
				"baseUrl": host,
				"api":     "openai-completions",
				"apiKey":  "ollama",
			},
		},
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(target, b, 0o600); err != nil {
		log.Printf("seed ollama models.json: %v", err)
	}
}
