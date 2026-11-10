package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/getsounder/sounder-server/internal/auth"
	"github.com/getsounder/sounder-server/internal/gateway"
	"github.com/getsounder/sounder-server/internal/provision"
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
	Form     string `json:"form,omitempty"` // computer | sandbox（云端设备形态）
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
		// 配置了 provisioner 就按配额真正开一台，否则退回"认领空闲 Worker"。
		if s.prov.Enabled() {
			deviceID := store.NewDeviceID()
			var workerTok string
			if s.workerSecret != "" {
				tok, err := auth.SignWorkerToken(s.workerSecret, auth.WorkerToken{
					WorkerID: deviceID,
					UserID:   ctx.UserID,
					Kind:     "cloud",
					Expiry:   time.Now().Add(24 * time.Hour).Unix(),
				})
				if err != nil {
					writeErr(w, http.StatusInternalServerError, err.Error())
					return
				}
				workerTok = tok
			}
			res, err := s.prov.Create(r.Context(), provision.Request{
				DeviceID:    deviceID,
				UserID:      ctx.UserID,
				WorkerToken: workerTok,
				Name:        req.Name,
				Form:        provision.Form(req.Form).Normalize(),
				CPU:         quota.DeviceCPU,
				MemMB:       quota.DeviceMemMB,
				DiskGB:      quota.DeviceDiskGB,
				Env:         workerEnv(),
			})
			if err != nil {
				writeErr(w, http.StatusBadGateway, "创建云端设备失败: "+err.Error())
				return
			}
			dev, err := s.store.EnsureDevice(store.Device{
				ID:       res.WorkerID,
				UserID:   ctx.UserID,
				WorkerID: res.WorkerID,
				Name:     req.Name,
				Kind:     "cloud",
				Form:     string(provision.Form(req.Form).Normalize()),
			})
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, http.StatusCreated, deviceView{Device: dev, Status: "starting"})
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
	id := r.PathValue("id")

	// 云端设备连同它的 Worker 一起回收。
	if devices, err := s.store.ListDevices(ctx.UserID); err == nil && s.prov.Enabled() {
		for _, d := range devices {
			if d.ID == id && d.Kind == "cloud" {
				if err := s.prov.Delete(r.Context(), d.WorkerID); err != nil {
					writeErr(w, http.StatusBadGateway, "销毁 Worker 失败: "+err.Error())
					return
				}
			}
		}
	}

	if err := s.store.DeleteDevice(id, ctx.UserID); err != nil {
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

// handleWorkerToken 给一台设备签发网关凭据（HMAC，24h 有效）。
// 网关用同一密钥离线校验，不需要回调本服务。
func (s *Server) handleWorkerToken(w http.ResponseWriter, r *http.Request) {
	ctx := authFrom(r)
	var req struct {
		WorkerID string `json:"workerId"`
		Kind     string `json:"kind,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if req.WorkerID == "" {
		writeErr(w, http.StatusBadRequest, "workerId required")
		return
	}
	if s.workerSecret == "" {
		writeErr(w, http.StatusServiceUnavailable, "worker secret not configured")
		return
	}

	// 只能给已登记在自己名下的设备签发。
	devices, err := s.store.ListDevices(ctx.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	owned := ctx.Admin
	for _, d := range devices {
		if d.WorkerID == req.WorkerID {
			owned = true
			break
		}
	}
	if !owned {
		writeErr(w, http.StatusForbidden, "device not registered")
		return
	}

	exp := time.Now().Add(24 * time.Hour)
	tok, err := auth.SignWorkerToken(s.workerSecret, auth.WorkerToken{
		WorkerID: req.WorkerID,
		UserID:   ctx.UserID,
		Kind:     req.Kind,
		Expiry:   exp.Unix(),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":     tok,
		"workerId":  req.WorkerID,
		"expiresAt": exp.UTC().Format(time.RFC3339),
	})
}

// workerEnv 把模型 key 之类的环境变量透传给新开的 Worker。
func workerEnv() map[string]string {
	out := map[string]string{}
	for _, k := range []string{
		"MINIMAX_API_KEY", "MINIMAX_CN_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY",
	} {
		if v := os.Getenv(k); v != "" {
			out[k] = v
		}
	}
	return out
}

// deviceByID 在当前用户的设备里按 id 查一条。
func (s *Server) deviceByID(userID, id string) (store.Device, bool) {
	devices, err := s.store.ListDevices(userID)
	if err != nil {
		return store.Device{}, false
	}
	for _, d := range devices {
		if d.ID == id {
			return d, true
		}
	}
	return store.Device{}, false
}

// handleStartDevice 开机：computer 原地启动，sandbox 新建容器。
func (s *Server) handleStartDevice(w http.ResponseWriter, r *http.Request) {
	ctx := authFrom(r)
	dev, ok := s.deviceByID(ctx.UserID, r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "device not found")
		return
	}
	if dev.Kind != "cloud" || !s.prov.Enabled() {
		writeErr(w, http.StatusBadRequest, "该设备由客户端管理，请在其所在端开机")
		return
	}
	quota, _ := s.store.GetQuota(ctx.UserID)
	var workerTok string
	if s.workerSecret != "" {
		if tok, err := auth.SignWorkerToken(s.workerSecret, auth.WorkerToken{
			WorkerID: dev.WorkerID,
			UserID:   ctx.UserID,
			Kind:     "cloud",
			Expiry:   time.Now().Add(30 * 24 * time.Hour).Unix(),
		}); err == nil {
			workerTok = tok
		}
	}
	err := s.prov.Start(r.Context(), provision.Request{
		DeviceID:    dev.WorkerID,
		UserID:      ctx.UserID,
		WorkerToken: workerTok,
		Name:        dev.Name,
		Form:        provision.Form(dev.Form).Normalize(),
		CPU:         quota.DeviceCPU,
		MemMB:       quota.DeviceMemMB,
		DiskGB:      quota.DeviceDiskGB,
		Env:         workerEnv(),
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, "开机失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "starting"})
}

// handleStopDevice 关机：computer 提交镜像后停机，sandbox 直接删容器。
func (s *Server) handleStopDevice(w http.ResponseWriter, r *http.Request) {
	ctx := authFrom(r)
	dev, ok := s.deviceByID(ctx.UserID, r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "device not found")
		return
	}
	if dev.Kind != "cloud" || !s.prov.Enabled() {
		writeErr(w, http.StatusBadRequest, "该设备由客户端管理，请在其所在端关机")
		return
	}
	if err := s.prov.Stop(r.Context(), dev.WorkerID, provision.Form(dev.Form).Normalize()); err != nil {
		writeErr(w, http.StatusBadGateway, "关机失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "stopped"})
}
