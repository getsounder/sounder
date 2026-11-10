package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getsounder/sounder-server/internal/store"
)

func TestNormalizeRoute(t *testing.T) {
	cases := map[string]string{
		"/v1/sessions/ses_9f2c1a4b8e70": "/v1/sessions/ses_*",
		"/v1/devices/dev_abc123":        "/v1/devices/dev_*",
		"/v1/admin/users/usr_123":       "/v1/admin/users/usr_*",
		"/v1/sessions":                  "/v1/sessions",
		"/v1/approvals/apr_notmatched":  "/v1/approvals/apr_notmatched",
	}
	for in, want := range cases {
		if got := normalizeRoute(in); got != want {
			t.Errorf("normalizeRoute(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMetricsEndpoint(t *testing.T) {
	st := store.NewMemory()
	now := time.Now()
	_ = st.Create(store.Session{ID: "ses_1", OwnerID: "usr_1", WorkerID: "w1", State: "idle", CreatedAt: now})
	_ = st.Create(store.Session{ID: "ses_2", OwnerID: "usr_1", WorkerID: "w1", State: "running", CreatedAt: now})
	_, _ = st.EnsureDevice(store.Device{UserID: "usr_1", WorkerID: "w1", Kind: "cloud"})

	srv := New(st, store.NewMemoryAuth(), nil, nil, "", "", nil, "")
	// 先制造一次请求，让计数器有数据
	rec := httptest.NewRecorder()
	srv.metricsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions/ses_1", nil))

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	srv.handleMetrics(w, req)
	body := w.Body.String()

	for _, want := range []string{
		"sounder_http_requests_total",
		`route="/v1/sessions/ses_*"`,
		`sounder_sessions{state="idle"} 1`,
		`sounder_sessions{state="running"} 1`,
		`sounder_devices{status="offline"} 1`,
		"sounder_workers 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics 缺少 %q\n%s", want, body)
		}
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("content-type = %q", ct)
	}
}

func TestMetricsMiddlewarePreservesFlusher(t *testing.T) {
	st := store.NewMemory()
	srv := New(st, store.NewMemoryAuth(), nil, nil, "", "", nil, "")

	var sawFlusher bool
	h := srv.metricsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("包装后的 ResponseWriter 丢失了 Flusher，SSE 会退化成一次性响应")
			return
		}
		f.Flush()
		sawFlusher = true
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/sessions/ses_1/events", nil))
	if !sawFlusher {
		t.Fatal("handler 未被调用")
	}
}
