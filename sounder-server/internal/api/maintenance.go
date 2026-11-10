package api

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/getsounder/sounder-server/internal/store"
)

// pruneDevices 删除「离线且名下没有会话」的设备。
// olderThan > 0 时只清理 last_seen 早于该时长的设备（定时巡检用），否则清所有离线空设备。
func (s *Server) pruneDevices(ctx context.Context, userID string, olderThan time.Duration) int {
	if s.gw == nil {
		return 0
	}
	devices, err := s.store.ListDevices(userID)
	if err != nil {
		return 0
	}
	workers, err := s.gw.ListWorkers(ctx)
	if err != nil {
		return 0
	}
	online := make(map[string]bool, len(workers))
	for _, w := range workers {
		online[w.ID] = true
	}
	used := map[string]int{}
	for _, sess := range s.store.ListByOwner(userID) {
		used[sess.WorkerID]++
	}

	removed := 0
	for _, d := range devices {
		if online[d.WorkerID] || used[d.WorkerID] > 0 {
			continue
		}
		if olderThan > 0 && time.Since(d.LastSeenAt) < olderThan {
			continue
		}
		if err := s.store.DeleteDevice(d.ID, userID); err == nil {
			removed++
		}
	}
	return removed
}

// handlePruneDevices 手动清理离线空设备（界面「清理离线设备」用）。
func (s *Server) handlePruneDevices(w http.ResponseWriter, r *http.Request) {
	ctx := authFrom(r)
	removed := s.pruneDevices(r.Context(), ctx.UserID, 0)
	_ = s.authStore.Audit(store.AuditEvent{
		UserID: ctx.UserID, Action: "device.prune", IP: clientIP(r), UserAgent: r.UserAgent(),
	})
	writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
}

// retryPendingDeletes 重试"worker 离线时没删掉"的会话。
func (s *Server) retryPendingDeletes(ctx context.Context) int {
	if s.gw == nil {
		return 0
	}
	pending := s.store.ListByState("deleting")
	done := 0
	for _, sess := range pending {
		status, _, err := s.gw.RPC(ctx, sess.WorkerID, http.MethodDelete, "/v1/sessions/"+sess.ID, nil)
		if err == nil || status == http.StatusNotFound {
			_ = s.store.Delete(sess.ID)
			done++
		}
	}
	return done
}

// Maintain 启动后台巡检：每分钟重试待删会话，每小时清理长期离线的空设备。
func (s *Server) Maintain(ctx context.Context) {
	deleteTicker := time.NewTicker(time.Minute)
	pruneTicker := time.NewTicker(time.Hour)
	defer deleteTicker.Stop()
	defer pruneTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-deleteTicker.C:
			if n := s.retryPendingDeletes(ctx); n > 0 {
				log.Printf("maintenance: deleted %d pending sessions", n)
			}
		case <-pruneTicker.C:
			users, _, err := s.authStore.ListUsers(1000, 0)
			if err != nil {
				continue
			}
			total := 0
			for _, u := range users {
				total += s.pruneDevices(ctx, u.ID, 7*24*time.Hour)
			}
			if total > 0 {
				log.Printf("maintenance: pruned %d stale devices", total)
			}
		}
	}
}
