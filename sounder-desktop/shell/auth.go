package shell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// AuthService 为桌面/移动端提供系统浏览器 OAuth 登录。
//
// 流程：本机监听 127.0.0.1 随机端口 -> 系统浏览器打开服务端 start 地址 ->
// provider 回调服务端 -> 服务端带一次性 ticket 重定向回本机 -> 用 ticket 换 token。
type AuthService struct {
	mu sync.Mutex
}

func NewAuth() *AuthService { return &AuthService{} }

type exchangeResult struct {
	Token string `json:"token"`
	Error string `json:"error"`
}

// StartOAuth 打开系统浏览器完成登录，返回访问 token。
// link=true 时把第三方账号绑定到 bearer 对应的已登录用户。
func (a *AuthService) StartOAuth(provider string, link bool, apiBase, bearer string) (string, error) {
	apiBase = strings.TrimRight(strings.TrimSpace(apiBase), "/")
	if apiBase == "" {
		return "", errors.New("请先填写云端 API 地址")
	}
	if provider == "" || strings.ContainsAny(provider, "/?#") {
		return "", errors.New("invalid provider")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("listen: %w", err)
	}
	defer ln.Close()

	redirect := "http://" + ln.Addr().String() + "/callback"
	path := "/v1/auth/oauth/" + url.PathEscape(provider) + "/start"
	if link {
		path = "/v1/auth/oauth/" + url.PathEscape(provider) + "/link/start"
	}
	startURL := apiBase + path + "?client=native&mode=json&native_redirect=" + url.QueryEscape(redirect)

	req, err := http.NewRequest(http.MethodGet, startURL, nil)
	if err != nil {
		return "", err
	}
	if link && bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求登录地址失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return "", errors.New(e.Error)
	}
	var started struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&started); err != nil {
		return "", fmt.Errorf("解析登录地址失败: %w", err)
	}

	result := make(chan exchangeResult, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if e := q.Get("error"); e != "" {
			fmt.Fprintf(w, "<h3>登录失败</h3><p>%s</p><p>可以关闭此页面。</p>", html.EscapeString(e))
			result <- exchangeResult{Error: e}
			return
		}
		ticket := q.Get("ticket")
		if ticket == "" {
			fmt.Fprint(w, "<h3>登录失败</h3><p>缺少 ticket。</p>")
			result <- exchangeResult{Error: "missing ticket"}
			return
		}
		token, err := a.exchange(apiBase, ticket)
		if err != nil {
			fmt.Fprintf(w, "<h3>登录失败</h3><p>%s</p>", html.EscapeString(err.Error()))
			result <- exchangeResult{Error: err.Error()}
			return
		}
		fmt.Fprint(w, "<h3>登录成功</h3><p>可以关闭此页面并返回 Sounder。</p>")
		result <- exchangeResult{Token: token}
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	if runtime.GOOS == "android" || runtime.GOOS == "ios" {
		application.Mobile.OpenURL(started.URL)
	} else if err := application.Get().Browser.OpenURL(started.URL); err != nil {
		return "", fmt.Errorf("打开浏览器失败: %w", err)
	}

	select {
	case res := <-result:
		if res.Error != "" {
			return "", errors.New(res.Error)
		}
		return res.Token, nil
	case <-time.After(5 * time.Minute):
		return "", errors.New("登录超时，请重试")
	}
}

func (a *AuthService) exchange(apiBase, ticket string) (string, error) {
	body, _ := json.Marshal(map[string]string{"ticket": ticket})
	req, err := http.NewRequest(http.MethodPost, apiBase+"/v1/auth/native/exchange", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out exchangeResult
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusOK {
		if out.Error == "" {
			out.Error = resp.Status
		}
		return "", errors.New(out.Error)
	}
	if out.Token == "" {
		return "", errors.New("服务端未返回 token")
	}
	return out.Token, nil
}
