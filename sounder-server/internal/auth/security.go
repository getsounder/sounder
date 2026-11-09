package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"log"
	"net/smtp"
	"strings"
	"sync"
	"time"
)

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败属于不可恢复的环境问题。
		panic("auth: crypto/rand: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func randomID(prefix string) string {
	return prefix + "_" + randomToken(12)
}

// limiter 是进程内的滑动窗口限流器。多实例部署时应换成 Redis。
type limiter struct {
	mu sync.Mutex
	m  map[string][]time.Time
}

func newLimiter() *limiter { return &limiter{m: make(map[string][]time.Time)} }

func (l *limiter) allow(key string, limit int, window time.Duration, now time.Time) bool {
	if limit <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-window)
	times := l.m[key]
	kept := times[:0]
	for _, t := range times {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= limit {
		l.m[key] = kept
		return false
	}
	l.m[key] = append(kept, now)
	return true
}

// Mailer 发送验证/重置邮件。
type Mailer interface {
	Send(ctx context.Context, to, subject, body string) error
}

type logMailer struct{}

func (logMailer) Send(_ context.Context, to, subject, body string) error {
	log.Printf("[mail] to=%s subject=%q\n%s", to, subject, body)
	return nil
}

type smtpMailer struct {
	addr, user, pass, from string
}

// NewSMTPMailer 构造 SMTP 发信器；addr 形如 smtp.example.com:587。
func NewSMTPMailer(addr, user, pass, from string) Mailer {
	return smtpMailer{addr: addr, user: user, pass: pass, from: from}
}

func (m smtpMailer) Send(_ context.Context, to, subject, body string) error {
	var auth smtp.Auth
	if m.user != "" {
		auth = smtp.PlainAuth("", m.user, m.pass, strings.Split(m.addr, ":")[0])
	}
	msg := "From: " + m.from + "\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=UTF-8\r\n\r\n" + body
	return smtp.SendMail(m.addr, auth, m.from, []string{to}, []byte(msg))
}
