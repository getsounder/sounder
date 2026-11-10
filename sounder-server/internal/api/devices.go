package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/getsounder/sounder-server/internal/gateway"
	"github.com/getsounder/sounder-server/internal/store"
)

// deviceView 在设备元数据上叠加网关的实时在线状态与占用情况。
type deviceView struct {
	store.Device
	Status   string `json:"status"` // online | offline
	Used     int    `json:"used"`
	Capacity int    `json:"capacity"`
}

func countCloud(devices []store.Device) int {
	n := 0
	for _, d := range devices {
		if d.Kind == "cloud" {
			n++
		}
	}
	return n
}

// handleDevices 列出当前用户的设备，并把在线状态与配额一起返回。
func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	ctx := authFrom(r)
	devices, err := s.store.ListDevices(ctx.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	workers, _ := s.gw.ListWorkers(r.Context())
	online := make(map[string]gateway.Worker, len(workers))
	for _, wk := range workers {
		online[wk.ID] = wk
	}

	views := make([]deviceView, 0, len(devices))
	for _, d := range devices {
		v := deviceView{Device: d, Status: "offline"}
		if wk, ok := online[d.WorkerID]; ok {
			v.Status = "online"
			v.Used = wk.Used
			v.Capacity = wk.Capacity
			if v.Kind == "" {
				v.Kind = wk.Kind
			}
		}
		views = append(views, v)
	}

	quota, err := s.store.GetQuota(ctx.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"devices":      views,
		"quota":        quota,
		"cloudDevices": countCloud(devices),
	})
}

type createDeviceReq struct {
	Kind     string `json:"kind,omitempty"` // cloud | desktop-container | desktop-process
	WorkerID string `json:"workerId,omitempty"`
	Name     string `json:"name,omitempty"`
	Platform string `json:"platform,omitempty"`
	Version  string `json:"version,omitempty"`
}

// handleCreateDevice 注册设备。
//   - 桌面端：控制端自己拉起容器/进程后带着 workerId 来登记，数量不限。
//   - 云端：服务端按配额从网关空闲 Worker 里分配一个，默认每用户 1 个。
func (s *Server) handleCreateDevice(w http.ResponseWriter, r *http.Request) {
	ctx := authFrom(r)
	var req createDeviceReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	kind := req.Kind
	if kind == "" {
		kind = "cloud"
	}

	devices, err := s.store.ListDevices(ctx.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	quota, err := s.store.GetQuota(ctx.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	if kind == "cloud" {
		if countCloud(devices) >= quota.MaxDevices {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":        "云设备数量已达配额上限",
				"quota":        quota,
				"cloudDevices": countCloud(devices),
			})
			return
		}
		workers, err := s.gw.ListWorkers(r.Context())
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		claimed := make(map[string]bool, len(devices))
		for _, d := range devices {
			claimed[d.WorkerID] = true
		}
		var pick *gateway.Worker
		for i := range workers {
			wk := &workers[i]
			if wk.Kind != "cloud" || wk.Used >= wk.Capacity || claimed[wk.ID] {
				continue
			}
			pick = wk
			break
		}
		if pick == nil {
			writeErr(w, http.StatusServiceUnavailable, "没有可用的云端容量，请稍后再试")
			return
		}
		req.WorkerID = pick.ID
		if req.Version == "" {
			req.Version = pick.Version
		}
	}

	if req.WorkerID == "" {
		writeErr(w, http.StatusBadRequest, "workerId required")
		return
	}

	dev, err := s.store.EnsureDevice(store.Device{
		UserID:   ctx.UserID,
		WorkerID: req.WorkerID,
		Name:     req.Name,
		Kind:     kind,
		Platform: req.Platform,
		Version:  req.Version,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	view := deviceView{Device: dev, Status: "online"}
	if workers, err := s.gw.ListWorkers(r.Context()); err == nil {
		for _, wk := range workers {
			if wk.ID == dev.WorkerID {
				view.Used = wk.Used
				view.Capacity = wk.Capacity
				break
			}
		}
	}
	writeJSON(w, http.StatusCreated, view)
}

func (s *Server) handleRenameDevice(w http.ResponseWriter, r *http.Request) {
	ctx := authFrom(r)
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if err := s.store.RenameDevice(r.PathValue("id"), ctx.UserID, req.Name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "device not found")
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteDevice(w http.ResponseWriter, r *http.Request) {
	ctx := authFrom(r)
	if err := s.store.DeleteDevice(r.PathValue("id"), ctx.UserID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "device not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// touchDevice 在用户真正用到某个 Worker 时补一条设备记录（首次使用时自动登记）。
func (s *Server) touchDevice(r *http.Request, userID, workerID string) {
	if userID == "" || workerID == "" {
		return
	}
	kind := ""
	if ws, err := s.gw.ListWorkers(r.Context()); err == nil {
		for _, wk := range ws {
			if wk.ID == workerID {
				kind = wk.Kind
				break
			}
		}
	}
	_, _ = s.store.EnsureDevice(store.Device{
		UserID:   userID,
		WorkerID: workerID,
		Kind:     kind,
	})
}
