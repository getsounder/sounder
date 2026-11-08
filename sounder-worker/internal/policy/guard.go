package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/getsounder/sounder-worker/internal/approval"
)

var ErrDenied = errors.New("tool call denied by policy")

type Approver interface {
	Ask(ctx context.Context, req approval.Request) (approval.Result, error)
}

// Guard 把"分级 → 路径校验 → 高危扫描 → 审批"串成一次判断。
// 它不依赖 PiG：在工具包装器的 Execute 前调用 Before 即可。
type Guard struct {
	workspace string
	approver  Approver
	tainted   atomic.Bool
	seq       atomic.Uint64

	// OnAsk 在阻塞等待审批前触发，用于把 approval.requested 推给控制端。
	OnAsk func(approval.Request)
}

func NewGuard(workspace string, approver Approver) *Guard {
	return &Guard{workspace: workspace, approver: approver}
}

// SetApprover 允许在启动后再注入闸门（Actor 构造时通常还没有）。
func (g *Guard) SetApprover(a Approver) { g.approver = a }

// MarkTainted 在读取不可信内容（issue / 网页 / 依赖 README / 日志）后置位。
func (g *Guard) MarkTainted(v bool) { g.tainted.Store(v) }

func (g *Guard) Tainted() bool { return g.tainted.Load() }

// Before 返回 nil 表示可以执行；ErrDenied 表示拒绝；其他 err 是基础设施错误。
func (g *Guard) Before(ctx context.Context, sessionID, tool string, raw json.RawMessage) error {
	args := Args(raw)
	lv := Classify(tool)

	// L1：写操作必须落在 workspace 内。
	if lv == L1Write {
		if p := PathOf(args); p != "" {
			if err := CheckPath(g.workspace, p); err != nil {
				return err
			}
		}
	}

	// L2：高危命令无条件升级为审批，与污点无关。
	force := false
	reason := ""
	if lv == L2Exec {
		if hits := ScanCommand(CommandOf(args)); len(hits) > 0 {
			force = true
			reason = "high-risk: " + strings.Join(hits, ",")
		}
	}

	d := Decide(lv, g.tainted.Load())
	if force {
		d = Ask
	}

	switch d {
	case Allow:
		return nil
	case Deny:
		return ErrDenied
	}

	if g.approver == nil {
		// 需要审批但没挂闸门：宁可拒绝，不要静默放行。
		return ErrDenied
	}

	req := approval.Request{
		ID:        g.newID(),
		SessionID: sessionID,
		Tool:      tool,
		Level:     lv.String(),
		Reason:    reason,
		Args:      args,
	}
	if g.OnAsk != nil {
		g.OnAsk(req)
	}

	res, err := g.approver.Ask(ctx, req)
	if err != nil {
		return err
	}
	if !res.Allow {
		return ErrDenied
	}
	return nil
}

func (g *Guard) newID() string {
	return fmt.Sprintf("apv_%d_%d", time.Now().UnixNano(), g.seq.Add(1))
}
