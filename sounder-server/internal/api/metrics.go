package api

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"
)

// 极简 Prometheus 指标：请求计数/耗时 + 会话与设备状态。
type metricsState struct {
	mu       sync.Mutex
	requests map[string]int64 // route|status -> count
	seconds  map[string]float64
}

var metrics = &metricsState{requests: map[string]int64{}, seconds: map[string]float64{}}

var idPattern = regexp.MustCompile(`/(ses|dev|usr)_[A-Za-z0-9]+`)

func normalizeRoute(path string) string {
	return idPattern.ReplaceAllString(path, "/${1}_*")
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		if r.URL.Path == "/metrics" {
			return
		}
		route := normalizeRoute(r.URL.Path)
		key := route
		metrics.mu.Lock()
		metrics.requests[key+"|"+strconv.Itoa(rec.status)]++
		metrics.seconds[key] += time.Since(start).Seconds()
		metrics.mu.Unlock()
	})
}

// handleMetrics 输出 Prometheus 文本格式。
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	metrics.mu.Lock()
	reqs := make(map[string]int64, len(metrics.requests))
	secs := make(map[string]float64, len(metrics.seconds))
	for k, v := range metrics.requests {
		reqs[k] = v
	}
	for k, v := range metrics.seconds {
		secs[k] = v
	}
	metrics.mu.Unlock()

	fmt.Fprintln(w, "# HELP sounder_http_requests_total 控制面请求总数")
	fmt.Fprintln(w, "# TYPE sounder_http_requests_total counter")
	keys := make([]string, 0, len(reqs))
	for k := range reqs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		var route, status string
		for i := len(k) - 1; i >= 0; i-- {
			if k[i] == '|' {
				route, status = k[:i], k[i+1:]
				break
			}
		}
		fmt.Fprintf(w, "sounder_http_requests_total{route=%q,status=%q} %d\n", route, status, reqs[k])
	}

	fmt.Fprintln(w, "# HELP sounder_http_request_seconds_sum 请求耗时累计（秒）")
	fmt.Fprintln(w, "# TYPE sounder_http_request_seconds_sum counter")
	names := make([]string, 0, len(secs))
	for k := range secs {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, route := range names {
		fmt.Fprintf(w, "sounder_http_request_seconds_sum{route=%q} %.3f\n", route, secs[route])
	}

	// 会话按状态
	sessions := s.store.List()
	byState := map[string]int{}
	for _, sess := range sessions {
		byState[sess.State]++
	}
	fmt.Fprintln(w, "# HELP sounder_sessions 会话数（按状态）")
	fmt.Fprintln(w, "# TYPE sounder_sessions gauge")
	for k, v := range byState {
		fmt.Fprintf(w, "sounder_sessions{state=%q} %d\n", k, v)
	}

	// 设备在线/离线
	online := map[string]bool{}
	if s.gw != nil {
		if workers, err := s.gw.ListWorkers(r.Context()); err == nil {
			for _, wk := range workers {
				online[wk.ID] = true
			}
		}
	}
	devices, _ := s.store.ListAllDevices()
	devOnline, devOffline := 0, 0
	for _, d := range devices {
		if online[d.WorkerID] {
			devOnline++
		} else {
			devOffline++
		}
	}
	fmt.Fprintln(w, "# HELP sounder_devices 设备总数（按在线状态）")
	fmt.Fprintln(w, "# TYPE sounder_devices gauge")
	fmt.Fprintf(w, "sounder_devices{status=\"online\"} %d\n", devOnline)
	fmt.Fprintf(w, "sounder_devices{status=\"offline\"} %d\n", devOffline)

	fmt.Fprintln(w, "# HELP sounder_workers 网关在线 Worker 数")
	fmt.Fprintln(w, "# TYPE sounder_workers gauge")
	fmt.Fprintf(w, "sounder_workers %d\n", len(online))
}
