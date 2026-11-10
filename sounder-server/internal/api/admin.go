package api

import (
	"net/http"
	"strconv"

	"github.com/getsounder/sounder-server/internal/gateway"
	"github.com/getsounder/sounder-server/internal/store"
)

// adminOnly 统一拦截非管理员。
func adminOnly(w http.ResponseWriter, r *http.Request) bool {
	if !authFrom(r).Admin {
		writeErr(w, http.StatusForbidden, "admin only")
		return false
	}
	return true
}

type adminDeviceView struct {
	store.Device
	Status     string `json:"status"`
	Used       int    `json:"used"`
	Capacity   int    `json:"capacity"`
	OwnerEmail string `json:"ownerEmail,omitempty"`
}

// handleAdminDevices 列出所有用户的设备（含在线状态与归属）。
func (s *Server) handleAdminDevices(w http.ResponseWriter, r *http.Request) {
	if !adminOnly(w, r) {
		return
	}
	devices, err := s.store.ListAllDevices()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	workers, _ := s.gw.ListWorkers(r.Context())
	online := make(map[string]gateway.Worker, len(workers))
	for _, wk := range workers {
		online[wk.ID] = wk
	}

	emails := map[string]string{}
	if users, _, err := s.authStore.ListUsers(1000, 0); err == nil {
		for _, u := range users {
			emails[u.ID] = u.Email
		}
	}

	out := make([]adminDeviceView, 0, len(devices))
	for _, d := range devices {
		v := adminDeviceView{Device: d, Status: "offline", OwnerEmail: emails[d.UserID]}
		if wk, ok := online[d.WorkerID]; ok {
			v.Status = "online"
			v.Used = wk.Used
			v.Capacity = wk.Capacity
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": out})
}

type adminUserView struct {
	store.User
	Quota        store.Quota `json:"quota"`
	DeviceCount  int         `json:"deviceCount"`
	CloudDevices int         `json:"cloudDevices"`
}

// handleAdminUsers 分页列出用户，附带配额与设备数。
func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	if !adminOnly(w, r) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	users, total, err := s.authStore.ListUsers(limit, offset)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	allDevices, _ := s.store.ListAllDevices()
	byUser := map[string][]store.Device{}
	for _, d := range allDevices {
		byUser[d.UserID] = append(byUser[d.UserID], d)
	}

	out := make([]adminUserView, 0, len(users))
	for _, u := range users {
		q, _ := s.store.GetQuota(u.ID)
		devices := byUser[u.ID]
		out = append(out, adminUserView{
			User:         u,
			Quota:        q,
			DeviceCount:  len(devices),
			CloudDevices: countCloud(devices),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out, "total": total})
}

// handleAdminStats 给后台首屏用的汇总。
func (s *Server) handleAdminStats(w http.ResponseWriter, r *http.Request) {
	if !adminOnly(w, r) {
		return
	}
	_, totalUsers, _ := s.authStore.ListUsers(1, 0)
	devices, _ := s.store.ListAllDevices()
	workers, _ := s.gw.ListWorkers(r.Context())
	sessions := s.store.List()

	online := 0
	cloud := 0
	for _, d := range devices {
		if d.Kind == "cloud" {
			cloud++
		}
		for _, wk := range workers {
			if wk.ID == d.WorkerID {
				online++
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"users":         totalUsers,
		"devices":       len(devices),
		"onlineDevices": online,
		"cloudDevices":  cloud,
		"sessions":      len(sessions),
		"workers":       len(workers),
	})
}

// handleAdminSessions 列出所有会话（带归属邮箱）。
func (s *Server) handleAdminSessions(w http.ResponseWriter, r *http.Request) {
	if !adminOnly(w, r) {
		return
	}
	sessions := s.store.List()
	emails := map[string]string{}
	if users, _, err := s.authStore.ListUsers(1000, 0); err == nil {
		for _, u := range users {
			emails[u.ID] = u.Email
		}
	}
	type view struct {
		store.Session
		OwnerEmail string `json:"ownerEmail,omitempty"`
	}
	out := make([]view, 0, len(sessions))
	for _, sess := range sessions {
		out = append(out, view{Session: sess, OwnerEmail: emails[sess.OwnerID]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}
