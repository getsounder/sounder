// Package protocol 定义 Gateway 与 Worker 之间的隧道帧。
// 隧道只转发字节与路由信息，不理解 Agent 业务。
package protocol

import "encoding/json"

const (
	KindRegister  = "register"
	KindHeartbeat = "heartbeat"
	KindRPC       = "rpc"
	KindRPCResult = "rpc_result"
	KindEvent     = "event"
	KindAck       = "ack"
	KindError     = "error"
)

type Frame struct {
	Kind string `json:"kind"`

	// RPC / 结果
	ID     string          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Path   string          `json:"path,omitempty"`
	Body   json.RawMessage `json:"body,omitempty"`
	Status int             `json:"status,omitempty"`
	Error  string          `json:"error,omitempty"`

	// 注册与心跳
	WorkerID string `json:"workerId,omitempty"`
	Version  string `json:"version,omitempty"`
	Capacity int    `json:"capacity,omitempty"`
	Used     int    `json:"used,omitempty"`
	Pool     string `json:"pool,omitempty"`
	Kind2    string `json:"workerKind,omitempty"` // desktop | cloud

	// 事件上行
	SessionID string          `json:"sessionId,omitempty"`
	Event     json.RawMessage `json:"event,omitempty"`
}
