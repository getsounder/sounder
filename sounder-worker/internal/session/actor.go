// Package session 封装一个 PiG 会话的完整生命周期。
//
// 关键约束（详见架构文档 3.2）：
//   - 每会话独立 Services（CWD / AgentDir / SettingsManager / SessionManager）
//   - 每会话一个 Runtime，不要复用
//   - Subscribe 回调是同步的，只做有界投递
//   - Send 阻塞到 AgentSettledEvent；provider 失败时 err 可能为 nil
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"

	"github.com/getsounder/sounder-worker/internal/approval"
	"github.com/getsounder/sounder-worker/internal/policy"
	"github.com/getsounder/sounder-worker/internal/protocol"
)

type Quota struct {
	MaxConcurrentTurns int
	SendTimeout        time.Duration
}

// Approver 由 approval.Gate 实现；工具执行前用它阻塞等待人工确认。
type Actor struct {
	ID      string
	ModelID string

	guard *policy.Guard

	services *coding.AgentSessionServices
	runtime  *coding.Runtime
	session  *coding.Session
	agentDir string

	subs       map[uint64]chan protocol.Envelope
	nextSub    uint64
	history    []protocol.Envelope
	historyMax int
	subMu      sync.Mutex

	done        chan struct{}
	seq         atomic.Int64
	quota       Quota
	sink  func(protocol.Envelope)

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	closed  bool
}

type Options struct {
	ID           string
	CWD          string
	AgentDir     string
	ModelID      string
	SystemPrompt string
	Quota        Quota
	ResumePath   string
}

func New(opts Options) (*Actor, error) {
	if opts.ID == "" {
		opts.ID = newID()
	}
	if opts.Quota.SendTimeout <= 0 {
		opts.Quota.SendTimeout = 30 * time.Minute
	}

	services, err := coding.CreateAgentSessionServices(coding.CreateAgentSessionServicesOptions{
		CWD:      opts.CWD,
		AgentDir: opts.AgentDir,
	})
	if err != nil {
		return nil, fmt.Errorf("create services: %w", err)
	}

	rt, err := coding.NewRuntime(coding.RuntimeOptions{Services: services})
	if err != nil {
		services.Close()
		return nil, fmt.Errorf("new runtime: %w", err)
	}

	model, err := coding.BuildModel(opts.ModelID, services)
	if err != nil {
		rt.Close()
		services.Close()
		return nil, fmt.Errorf("build model %q: %w", opts.ModelID, err)
	}

	a := &Actor{
		ID:         opts.ID,
		ModelID:    opts.ModelID,
		guard:      policy.NewGuard(opts.CWD, nil),
		services:   services,
		runtime:    rt,
		agentDir:   opts.AgentDir,
		subs:       make(map[uint64]chan protocol.Envelope),
		historyMax: 2048,
		done:       make(chan struct{}),
		quota:      opts.Quota,
	}

	// 审批阻塞前先把请求推给控制端，否则前端只会看到"卡住"。
	a.guard.OnAsk = func(req approval.Request) {
		a.emit(protocol.TypeApprovalAsk, map[string]any{
			"approvalId": req.ID,
			"tool":       req.Tool,
			"level":      req.Level,
			"reason":     req.Reason,
			"args":       req.Args,
		})
	}

	sess, err := rt.New(coding.SessionStartOptions{
		Model:        model,
		SystemPrompt: opts.SystemPrompt,
		ResumePath:   opts.ResumePath,
		// 每个工具执行前都走策略与审批；Block 会变成 error tool result 回给模型。
		BeforeToolCall: []agent.BeforeToolCallHook{a.beforeToolCall},
	})
	if err != nil {
		rt.Close()
		services.Close()
		return nil, fmt.Errorf("new session: %w", err)
	}
	a.session = sess

	// 同步回调：只做有界投递，禁止在此写库/发网络。
	sess.Subscribe(func(ev agent.AgentEvent) {
		a.forward(ev)
	})

	return a, nil
}

// beforeToolCall 是 PiG 的官方钩子：在参数校验之后、工具执行之前运行，可阻塞。
func (a *Actor) beforeToolCall(ctx context.Context, _ string, name string, args json.RawMessage) agent.ToolCallHookResult {
	if err := a.CheckTool(ctx, name, args); err != nil {
		log.Printf("session %s blocked tool %s: %v", a.ID, name, err)
		return agent.ToolCallHookResult{Block: true, Reason: err.Error()}
	}
	return agent.ToolCallHookResult{}
}

// SetApprover 注入审批闸门。Guard 已经封装了分级、路径校验与高危扫描，
// 剩下的唯一挂载点是：包装 agent.AgentTool，在其 Execute 开头调用
//
//	if err := actor.CheckTool(ctx, toolName, rawArgs); err != nil { return denied }
func (a *Actor) SetApprover(x policy.Approver) {
	if a.guard != nil {
		a.guard.SetApprover(x)
	}
}

// CheckTool 供工具包装器调用；返回 ErrDenied 表示拒绝执行。
func (a *Actor) CheckTool(ctx context.Context, tool string, rawArgs []byte) error {
	if a.guard == nil {
		return nil
	}
	return a.guard.Before(ctx, a.ID, tool, rawArgs)
}

// MarkTainted 在读取不可信内容后调用，让后续 L1/L2 升级为审批。
func (a *Actor) MarkTainted(v bool) {
	if a.guard != nil {
		a.guard.MarkTainted(v)
	}
}

// HistoryPath 返回当前会话的 Session JSONL 路径；会话尚未落盘时为空。
func (a *Actor) HistoryPath() string {
	if a.agentDir == "" {
		return ""
	}
	return findSessionFile(a.agentDir)
}

// Subscribe 返回一条独立的事件流，支持多端同时订阅。
// after 用于从内存历史补发缺口；更早的历史要回源 Session JSONL。
func (a *Actor) Subscribe(after int64) (<-chan protocol.Envelope, func()) {
	ch := make(chan protocol.Envelope, 256)

	a.subMu.Lock()
	id := a.nextSub
	a.nextSub++
	for _, env := range a.history {
		if env.Seq > after {
			select {
			case ch <- env:
			default:
			}
		}
	}
	a.subs[id] = ch
	a.subMu.Unlock()

	cancel := func() {
		a.subMu.Lock()
		if c, ok := a.subs[id]; ok {
			delete(a.subs, id)
			close(c)
		}
		a.subMu.Unlock()
	}
	return ch, cancel
}

func (a *Actor) Prompt(ctx context.Context, text string) error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return fmt.Errorf("session closed")
	}
	if a.running {
		a.mu.Unlock()
		return fmt.Errorf("session busy")
	}
	runCtx, cancel := context.WithTimeout(ctx, a.quotaTimeout())
	a.running = true
	a.cancel = cancel
	a.mu.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("session %s panic: %v", a.ID, r)
				a.emit(protocol.TypeError, map[string]any{"message": fmt.Sprint(r)})
			}
			a.mu.Lock()
			a.running = false
			a.cancel = nil
			a.mu.Unlock()
			cancel()
		}()

		msgs, err := a.session.Send(runCtx, text)
		if err != nil {
			a.emit(protocol.TypeError, map[string]any{"message": err.Error()})
			return
		}

		if reason, msg := failureOf(msgs); reason != "" {
			a.emit(protocol.TypeError, map[string]any{
				"message":    msg,
				"stopReason": reason,
			})
		}
		a.emit(protocol.TypeSettled, nil)
	}()

	return nil
}

func (a *Actor) Cancel() {
	a.mu.Lock()
	cancel := a.cancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (a *Actor) Close() {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.closed = true
	cancel := a.cancel
	a.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	close(a.done)

	a.subMu.Lock()
	for id, ch := range a.subs {
		delete(a.subs, id)
		close(ch)
	}
	a.subMu.Unlock()

	// 顺序不能反：session -> runtime -> services
	if a.session != nil {
		a.session.Close()
	}
	if a.runtime != nil {
		a.runtime.Close()
	}
	if a.services != nil {
		a.services.Close()
	}
}

func (a *Actor) quotaTimeout() time.Duration {
	if a.quota.SendTimeout > 0 {
		return a.quota.SendTimeout
	}
	return 30 * time.Minute
}

// forward 把 PiG 的 AgentEvent 归一化为稳定 Envelope。
// 不把 AgentEvent 直接透给前端：它的结构会随 PiG 版本变化。
func (a *Actor) forward(ev agent.AgentEvent) {
	switch ev.(type) {
	// PiG 的事件可能是值也可能是指针，两种都要匹配。
	case agent.MessageStartEvent, *agent.MessageStartEvent:
		a.emit(protocol.TypeMessageStart, nil)
	case agent.MessageUpdateEvent, *agent.MessageUpdateEvent:
		// PiG 的 Partial 是"当前完整内容"，不是增量；前端按前缀判断替换或追加。
		a.emit(protocol.TypeMessageDelta, textPayload(ev))
	case agent.MessageEndEvent, *agent.MessageEndEvent:
		a.emit(protocol.TypeMessageEnd, textPayload(ev))
	case agent.TurnEndEvent, *agent.TurnEndEvent:
		a.emit(protocol.TypeTurnEnd, nil)
	case agent.AgentSettledEvent, *agent.AgentSettledEvent:
		// settled 统一由 Prompt 在 Send 返回后发一次，这里丢弃，避免重复。
		return
	case agent.ToolExecutionStartEvent, *agent.ToolExecutionStartEvent:
		a.emit(protocol.TypeToolStart, map[string]any{
			"toolCallId": eventField(ev, "ToolCallID"),
			"tool":       eventField(ev, "ToolName"),
			"args":       eventField(ev, "Args"),
		})
	case agent.ToolExecutionUpdateEvent, *agent.ToolExecutionUpdateEvent:
		// 流式进度太密，控制端只关心 start/end。
		return
	case agent.ToolExecutionEndEvent, *agent.ToolExecutionEndEvent:
		payload := map[string]any{
			"toolCallId": eventField(ev, "ToolCallID"),
			"tool":       eventField(ev, "ToolName"),
			"isError":    eventField(ev, "IsError"),
		}
		if d := eventField(ev, "DurationMs"); d != nil {
			payload["durationMs"] = d
		}
		if r, ok := eventField(ev, "Result").(agent.AgentToolResult); ok {
			payload["output"] = toolOutput(r)
		}
		a.emit(protocol.TypeToolEnd, payload)
	case agent.AgentStartEvent, *agent.AgentStartEvent,
		agent.AgentEndEvent, *agent.AgentEndEvent,
		agent.TurnStartEvent, *agent.TurnStartEvent:
		// 内部生命周期事件，不对控制端暴露。
		return
	default:
		// 未知事件保留类型名，便于升级 PiG 后发现新增类型。
		a.emit("raw."+fmt.Sprintf("%T", ev), nil)
	}
}

func (a *Actor) emit(t string, payload map[string]any) {
	select {
	case <-a.done:
		return
	default:
	}

	env := protocol.Envelope{
		Seq:       a.seq.Add(1),
		SessionID: a.ID,
		Type:      t,
		TS:        time.Now().UTC(),
		Payload:   payload,
	}

	a.subMu.Lock()
	a.history = append(a.history, env)
	if len(a.history) > a.historyMax {
		a.history = a.history[len(a.history)-a.historyMax:]
	}
	for _, ch := range a.subs {
		select {
		case ch <- env:
		default:
			// 慢消费者：丢弃并计数，绝不阻塞 Agent。
			log.Printf("session %s event dropped (type=%s, seq=%d)", a.ID, t, env.Seq)
		}
	}
	a.subMu.Unlock()

	if a.sink != nil {
		a.sink(env)
	}
}

// SetEventSink 把事件同时送到隧道（云端或远程控制端）；sink 必须非阻塞。
func (a *Actor) SetEventSink(fn func(protocol.Envelope)) { a.sink = fn }

// NewID 生成会话 ID，供 Manager 使用。
func NewID() string { return newID() }

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("ses_%d", time.Now().UnixNano())
	}
	return "ses_" + hex.EncodeToString(b[:])
}

// textPayload 反射提取 message 的文本块。PiG 的 AgentEvent 结构随 0.x 变化，
// 这里不绑定具体字段名：找不到就返回 nil，前端只显示"生成中"。
func textPayload(ev agent.AgentEvent) map[string]any {
	text := textOf(messageOf(ev))
	if text == "" {
		return nil
	}
	return map[string]any{"text": text}
}

func messageOf(ev any) any {
	rv := reflect.ValueOf(ev)
	for rv.Kind() == reflect.Ptr || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil
	}
	for _, name := range []string{"Message", "Partial", "AssistantMessage"} {
		if f := rv.FieldByName(name); f.IsValid() && f.CanInterface() {
			return f.Interface()
		}
	}
	return nil
}

// textOf 递归查找消息里的文本块：
// AgentMessage -> Assistant -> Content[] -> {Type:"text", Text}。
func textOf(v any) string {
	if v == nil {
		return ""
	}
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Ptr || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return ""
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return ""
	}

	if f := rv.FieldByName("Content"); f.IsValid() && (f.Kind() == reflect.Slice || f.Kind() == reflect.Array) {
		var b strings.Builder
		for i := 0; i < f.Len(); i++ {
			blk := f.Index(i)
			for blk.Kind() == reflect.Ptr || blk.Kind() == reflect.Interface {
				if blk.IsNil() {
					break
				}
				blk = blk.Elem()
			}
			if blk.Kind() != reflect.Struct {
				continue
			}
			// ai.TextContent 没有 Type 字段（类型由接口决定），直接读 Text。
			if s := blk.FieldByName("Text"); s.IsValid() && s.Kind() == reflect.String {
				b.WriteString(s.String())
			}
		}
		if b.Len() > 0 {
			return b.String()
		}
	}

	for _, name := range []string{"Assistant", "Message", "Partial"} {
		f := rv.FieldByName(name)
		if f.IsValid() && f.CanInterface() {
			if s := textOf(f.Interface()); s != "" {
				return s
			}
		}
	}
	return ""
}

// failureOf 检查 provider 失败：Send 的 err 为 nil，错误在 assistant message 里。
func failureOf(msgs []agent.AgentMessage) (string, string) {
	for i := len(msgs) - 1; i >= 0; i-- {
		a := msgs[i].Assistant
		if a == nil {
			continue
		}
		if a.StopReason == "error" || a.StopReason == "aborted" {
			return string(a.StopReason), a.ErrorMessage
		}
		break
	}
	return "", ""
}

// eventField 反射读取事件字段，避免绑定具体结构。
func eventField(ev any, name string) any {
	rv := reflect.ValueOf(ev)
	for rv.Kind() == reflect.Ptr || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil
	}
	f := rv.FieldByName(name)
	if !f.IsValid() || !f.CanInterface() {
		return nil
	}
	return f.Interface()
}

// toolOutput 把工具结果里的文本块拼起来，并截断到 2KB。
func toolOutput(r agent.AgentToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		if t, ok := c.(ai.TextContent); ok {
			b.WriteString(t.Text)
		}
	}
	s := b.String()
	if len(s) > 2048 {
		s = s[:2048] + "…(truncated)"
	}
	return s
}
