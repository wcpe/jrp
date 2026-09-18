package httpapi

import (
	"strings"
	"sync"
	"time"
)

// 登录失败限流的默认阈值与锁定时长（FR-02 规格 §6 待定项：暂按有界限流实现）。
const (
	loginFailureLimit    = 5
	loginFailureWindow   = 15 * time.Minute
	loginFailureTrackMax = 1024
)

// loginFailure 记录一个主体的失败尝试状态。
type loginFailure struct {
	count        int
	firstFail    time.Time
	limitedUntil time.Time
}

// loginLimiter 是有界的登录失败限流器：它防止凭据爆破，又避免条目无限增长。
//
// 阈值与锁定时长按"有界限流"实现，具体取值待规格 §6 定稿（FR-02）。
type loginLimiter struct {
	mu      sync.Mutex
	entries map[string]*loginFailure
	now     func() time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{
		entries: make(map[string]*loginFailure, 16),
		now:     time.Now,
	}
}

// RecordFailure 记录一次失败尝试；达到阈值后进入锁定时长。
func (l *loginLimiter) RecordFailure(subject string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now().UTC()
	entry, ok := l.entries[normalizeSubject(subject)]
	if !ok {
		if len(l.entries) >= loginFailureTrackMax {
			return
		}
		entry = &loginFailure{firstFail: now}
		l.entries[normalizeSubject(subject)] = entry
	}
	if now.Sub(entry.firstFail) > loginFailureWindow {
		entry.count = 0
		entry.firstFail = now
	}
	entry.count++
	if entry.count >= loginFailureLimit {
		entry.limitedUntil = now.Add(loginFailureWindow)
	}
}

// IsLimited 判断主体当前是否处于限流状态。
func (l *loginLimiter) IsLimited(subject string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.entries[normalizeSubject(subject)]
	if !ok {
		return false
	}
	return l.now().UTC().Before(entry.limitedUntil)
}

// Reset 清除主体的失败计数：登录成功后立即解除限流。
func (l *loginLimiter) Reset(subject string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, normalizeSubject(subject))
}

// normalizeSubject 统一比较口径，避免大小写变形绕开限流。
func normalizeSubject(subject string) string {
	return strings.ToLower(strings.TrimSpace(subject))
}
