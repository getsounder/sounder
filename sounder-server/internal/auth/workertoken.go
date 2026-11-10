package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// WorkerToken 是给单个 Worker 运行环境签发的网关凭据。
// 用 HMAC 签名，网关只校验签名，不需要回调服务端。
type WorkerToken struct {
	WorkerID string `json:"wid"`
	UserID   string `json:"uid,omitempty"`
	Kind     string `json:"kind,omitempty"`
	// Use 区分用途：gateway（连网关，短期）| device-key（换短期令牌用，长期）
	Use    string `json:"use,omitempty"`
	Expiry int64  `json:"exp"`
}

const workerTokenPrefix = "wt_"

// SignWorkerToken 生成 `<prefix><payload>.<sig>` 形式的 token。
func SignWorkerToken(secret string, t WorkerToken) (string, error) {
	if secret == "" {
		return "", errors.New("worker secret not configured")
	}
	if t.WorkerID == "" {
		return "", errors.New("workerId required")
	}
	if t.Expiry == 0 {
		t.Expiry = time.Now().Add(24 * time.Hour).Unix()
	}
	payload, err := json.Marshal(t)
	if err != nil {
		return "", err
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return workerTokenPrefix + body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// VerifyWorkerToken 校验签名与过期时间，返回载荷。
func VerifyWorkerToken(secret, token string) (WorkerToken, error) {
	var out WorkerToken
	token = strings.TrimSpace(token)
	if secret == "" || !strings.HasPrefix(token, workerTokenPrefix) {
		return out, errors.New("invalid worker token")
	}
	rest := strings.TrimPrefix(token, workerTokenPrefix)
	dot := strings.LastIndex(rest, ".")
	if dot <= 0 {
		return out, errors.New("invalid worker token")
	}
	body, sig := rest[:dot], rest[dot+1:]
	raw, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return out, errors.New("invalid worker token signature")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	if !hmac.Equal(raw, mac.Sum(nil)) {
		return out, errors.New("invalid worker token signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return out, errors.New("invalid worker token payload")
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return out, fmt.Errorf("invalid worker token payload: %w", err)
	}
	if out.Expiry > 0 && time.Now().Unix() > out.Expiry {
		return out, errors.New("worker token expired")
	}
	return out, nil
}
