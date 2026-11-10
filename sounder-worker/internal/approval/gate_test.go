package approval

import (
	"context"
	"testing"
	"time"
)

func TestPendingForCountsBySession(t *testing.T) {
	g := NewGate(time.Minute)
	go func() {
		_, _ = g.Ask(context.Background(), Request{ID: "a1", SessionID: "s1", Tool: "bash", Level: "l2"})
	}()
	go func() {
		_, _ = g.Ask(context.Background(), Request{ID: "a2", SessionID: "s1", Tool: "ask_user", Level: "question"})
	}()
	go func() {
		_, _ = g.Ask(context.Background(), Request{ID: "a3", SessionID: "s2", Tool: "bash", Level: "l1"})
	}()

	deadline := time.Now().Add(2 * time.Second)
	var total, questions int
	for time.Now().Before(deadline) {
		total, questions = g.PendingFor("s1")
		if total == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if total != 2 {
		t.Fatalf("s1 total = %d, want 2", total)
	}
	if questions != 1 {
		t.Fatalf("s1 questions = %d, want 1", questions)
	}

	if other, _ := g.PendingFor("s2"); other != 1 {
		t.Fatalf("s2 total = %d, want 1", other)
	}
	if none, _ := g.PendingFor("s3"); none != 0 {
		t.Fatalf("s3 total = %d, want 0", none)
	}

	// 处理掉一个后计数下降
	if _, err := g.Resolve("a2", Result{Allow: true, Reason: "ok"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	total, questions = g.PendingFor("s1")
	if total != 1 || questions != 0 {
		t.Fatalf("after resolve: total=%d questions=%d", total, questions)
	}
}
