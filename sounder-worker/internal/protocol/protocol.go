// Package protocol 定义控制端与 Worker 之间的唯一契约。
// 控制端不消费 PiG 的 AgentEvent，只消费这里的 Envelope。
package protocol

import "time"

const (
	TypeMessageStart = "message.start"
	TypeMessageDelta = "message.delta"
	TypeMessageEnd   = "message.end"
	TypeToolStart    = "tool.start"
	TypeToolEnd      = "tool.end"
	TypeTurnEnd      = "turn.end"
	TypeSettled      = "session.settled"
	TypeError        = "error"
	TypeApprovalAsk  = "approval.requested"
	TypeApprovalDone = "approval.resolved"
)

// Envelope 是 SSE 的唯一帧类型。seq 单调递增，断线重连按 after 重放。
type Envelope struct {
	Seq       int64          `json:"seq"`
	SessionID string         `json:"sessionId"`
	Type      string         `json:"type"`
	TS        time.Time      `json:"ts"`
	Payload   map[string]any `json:"payload,omitempty"`
}

type CreateSessionRequest struct {
	ProjectID    string `json:"projectId,omitempty"`
	Model        string `json:"model,omitempty"`
	SystemPrompt string `json:"systemPrompt,omitempty"`
	Piglet       string `json:"piglet,omitempty"`
	Workspace    string `json:"workspace,omitempty"` // 为空则由 Worker 分配
}

type CreateSessionResponse struct {
	ID        string `json:"id"`
	Model     string `json:"model"`
	CreatedAt string `json:"createdAt"`
}

// SessionView 是列表接口的展示模型：控制端不用再逐个拉历史就能渲染侧栏。
type SessionView struct {
	ID        string `json:"id"`
	Model     string `json:"model,omitempty"`
	State     string `json:"state"`
	Title     string `json:"title,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	CreatedAt string `json:"createdAt,omitempty"`
}

type PromptRequest struct {
	Message string `json:"message"`
}

type ApprovalRequest struct {
	ID      string `json:"id"`
	Allow   bool   `json:"allow"`
	Reason  string `json:"reason,omitempty"`
	Patched map[string]any `json:"patched,omitempty"`
}

type Meta struct {
	ProtocolVersion string   `json:"protocolVersion"`
	PigVersion      string   `json:"pigVersion"`
	PigCommit       string   `json:"pigCommit"`
	Features        []string `json:"features"`
}
