package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
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
			var workerTok, deviceKey string
			if s.workerSecret != "" {
				if tok, err := auth.SignWorkerToken(s.workerSecret, auth.WorkerToken{
					WorkerID: deviceID, UserID: ctx.UserID, Kind: "cloud", Use: "gateway",
					Expiry: time.Now().Add(time.Hour).Unix(),
				}); err == nil {
					workerTok = tok
				}
				if key, err := auth.SignWorkerToken(s.workerSecret, auth.WorkerToken{
					WorkerID: deviceID, UserID: ctx.UserID, Kind: "cloud", Use: "device-key",
					Expiry: time.Now().Add(90 * 24 * time.Hour).Unix(),
				}); err == nil {
					deviceKey = key
				}
			}
			res, err := s.prov.Create(r.Context(), provision.Request{
				DeviceID:     deviceID,
				UserID:       ctx.UserID,
				WorkerToken:  workerTok,
				DeviceKey:    deviceKey,
				ControlPlane: s.provisionerControlPlane,
				Name:         req.Name,
				Form:         provision.Form(req.Form).Normalize(),
				CPU:          quota.DeviceCPU,
				MemMB:        quota.DeviceMemMB,
				DiskGB:       quota.DeviceDiskGB,
				Env:          workerEnv(),
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
	s.audit(r, "device.create", map[string]any{
		"deviceId": dev.ID, "workerId": dev.WorkerID, "kind": dev.Kind, "form": dev.Form,
	})
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
	s.audit(r, "device.rename", map[string]any{"deviceId": r.PathValue("id"), "name": req.Name})
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
	s.audit(r, "device.delete", map[string]any{"deviceId": id})
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
		Use      string `json:"use,omitempty"` // gateway（默认，短期）| device-key（长期，用于换短期令牌）
		TTLHours int    `json:"ttlHours,omitempty"`
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

	use := req.Use
	if use == "" {
		use = "gateway"
	}
	ttl := 24 * time.Hour
	if use == "device-key" {
		ttl = 90 * 24 * time.Hour
	}
	if req.TTLHours > 0 && req.TTLHours < 24*90 {
		ttl = time.Duration(req.TTLHours) * time.Hour
	}
	exp := time.Now().Add(ttl)
	tok, err := auth.SignWorkerToken(s.workerSecret, auth.WorkerToken{
		WorkerID: req.WorkerID,
		UserID:   ctx.UserID,
		Kind:     req.Kind,
		Use:      use,
		Expiry:   exp.Unix(),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":     tok,
		"workerId":  req.WorkerID,
		"use":       use,
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
	var workerTok, deviceKey string
	if s.workerSecret != "" {
		if tok, err := auth.SignWorkerToken(s.workerSecret, auth.WorkerToken{
			WorkerID: dev.WorkerID, UserID: ctx.UserID, Kind: "cloud", Use: "gateway",
			Expiry: time.Now().Add(time.Hour).Unix(),
		}); err == nil {
			workerTok = tok
		}
		if key, err := auth.SignWorkerToken(s.workerSecret, auth.WorkerToken{
			WorkerID: dev.WorkerID, UserID: ctx.UserID, Kind: "cloud", Use: "device-key",
			Expiry: time.Now().Add(90 * 24 * time.Hour).Unix(),
		}); err == nil {
			deviceKey = key
		}
	}
	err := s.prov.Start(r.Context(), provision.Request{
		DeviceID:     dev.WorkerID,
		UserID:       ctx.UserID,
		WorkerToken:  workerTok,
		DeviceKey:    deviceKey,
		ControlPlane: s.provisionerControlPlane,
		Name:         dev.Name,
		Form:         provision.Form(dev.Form).Normalize(),
		CPU:          quota.DeviceCPU,
		MemMB:        quota.DeviceMemMB,
		DiskGB:       quota.DeviceDiskGB,
		Env:          workerEnv(),
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
	s.audit(r, "device.stop", map[string]any{"deviceId": dev.ID, "workerId": dev.WorkerID})
	if err := s.prov.Stop(r.Context(), dev.WorkerID, provision.Form(dev.Form).Normalize()); err != nil {
		writeErr(w, http.StatusBadGateway, "关机失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "stopped"})
}

// 管理员可以用 ?userId= 或 ?email= 查看别人的配额。
func (s *Server) resolveTargetUser(r *http.Request, ctx authContext) (string, bool) {
	id := r.URL.Query().Get("userId")
	email := r.URL.Query().Get("email")
	if id == "" && email == "" {
		return ctx.UserID, true
	}
	if !ctx.Admin {
		return "", false
	}
	if id != "" {
		return id, true
	}
	if u, err := s.authStore.GetUserByEmail(email); err == nil {
		return u.ID, true
	}
	return "", false
}

// handleGetQuota 返回配额与用量（默认自己；管理员可查任意用户）。
func (s *Server) handleGetQuota(w http.ResponseWriter, r *http.Request) {
	ctx := authFrom(r)
	userID, ok := s.resolveTargetUser(r, ctx)
	if !ok {
		writeErr(w, http.StatusForbidden, "admin only")
		return
	}
	q, err := s.store.GetQuota(userID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	devices, _ := s.store.ListDevices(userID)
	writeJSON(w, http.StatusOK, map[string]any{
		"quota": q,
		"usage": map[string]any{
			"devices":      len(devices),
			"cloudDevices": countCloud(devices),
		},
	})
}

// handleSetQuota 调整某个用户的配额（仅管理员）。
func (s *Server) handleSetQuota(w http.ResponseWriter, r *http.Request) {
	ctx := authFrom(r)
	if !ctx.Admin {
		writeErr(w, http.StatusForbidden, "admin only")
		return
	}
	var req struct {
		UserID       string `json:"userId"`
		Email        string `json:"email"`
		MaxDevices   int    `json:"maxDevices"`
		DeviceCPU    int    `json:"deviceCpu"`
		DeviceMemMB  int    `json:"deviceMemMb"`
		DeviceDiskGB int    `json:"deviceDiskGb"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if req.UserID == "" && req.Email != "" {
		if u, err := s.authStore.GetUserByEmail(req.Email); err == nil {
			req.UserID = u.ID
		}
	}
	if req.UserID == "" {
		writeErr(w, http.StatusBadRequest, "userId or email required")
		return
	}
	q, err := s.store.GetQuota(req.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if req.MaxDevices > 0 {
		q.MaxDevices = req.MaxDevices
	}
	if req.DeviceCPU > 0 {
		q.DeviceCPU = req.DeviceCPU
	}
	if req.DeviceMemMB > 0 {
		q.DeviceMemMB = req.DeviceMemMB
	}
	if req.DeviceDiskGB > 0 {
		q.DeviceDiskGB = req.DeviceDiskGB
	}
	if err := s.store.SetQuota(q); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, q)
}

// handleRefreshWorkerToken 用长期设备密钥换短期网关令牌（Worker 自己调用，不需要用户会话）。
func (s *Server) handleRefreshWorkerToken(w http.ResponseWriter, r *http.Request) {
	if s.workerSecret == "" {
		writeErr(w, http.StatusServiceUnavailable, "worker secret not configured")
		return
	}
	token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	key, err := auth.VerifyWorkerToken(s.workerSecret, token)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "invalid device key")
		return
	}
	if key.Use != "device-key" {
		writeErr(w, http.StatusForbidden, "device key required")
		return
	}
	exp := time.Now().Add(time.Hour)
	gwToken, err := auth.SignWorkerToken(s.workerSecret, auth.WorkerToken{
		WorkerID: key.WorkerID,
		UserID:   key.UserID,
		Kind:     key.Kind,
		Use:      "gateway",
		Expiry:   exp.Unix(),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":     gwToken,
		"workerId":  key.WorkerID,
		"expiresAt": exp.UTC().Format(time.RFC3339),
	})
}
