package tunnel

import (
	"sync"
	"time"
)

const (
	defaultVerifyConcurrency = 4
	defaultVerifyBackoff     = 500 * time.Millisecond
	defaultVerifyMaxBackoff  = 30 * time.Second
	maxVerifyConcurrency     = 64
	maxVerifyFailureEntries  = 4096
)

type verificationFailure struct {
	count       uint8
	retryAt     time.Time
	lastFailure time.Time
}

type verificationLimiter struct {
	slots       chan struct{}
	baseBackoff time.Duration
	maxBackoff  time.Duration

	mu       sync.Mutex
	failures map[string]verificationFailure
}

func newVerificationLimiter(concurrency int, baseBackoff, maxBackoff time.Duration) *verificationLimiter {
	return &verificationLimiter{
		slots:       make(chan struct{}, concurrency),
		baseBackoff: baseBackoff,
		maxBackoff:  maxBackoff,
		failures:    make(map[string]verificationFailure),
	}
}

func (l *verificationLimiter) tryAcquire() bool {
	select {
	case l.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (l *verificationLimiter) release() {
	<-l.slots
}

func (l *verificationLimiter) retryAfter(key string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	failure, ok := l.failures[key]
	if !ok || !now.Before(failure.retryAt) {
		return 0
	}
	return failure.retryAt.Sub(now)
}

func (l *verificationLimiter) recordFailure(key string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	failure := l.failures[key]
	if now.Sub(failure.lastFailure) > l.maxBackoff {
		failure.count = 0
	}
	if failure.count < 63 {
		failure.count++
	}

	backoff := l.baseBackoff
	for remaining := failure.count - 1; remaining > 0 && backoff < l.maxBackoff; remaining-- {
		if backoff > l.maxBackoff/2 {
			backoff = l.maxBackoff
			break
		}
		backoff *= 2
	}
	if backoff > l.maxBackoff {
		backoff = l.maxBackoff
	}

	if _, exists := l.failures[key]; !exists && len(l.failures) >= maxVerifyFailureEntries {
		l.evictOldest()
	}
	failure.retryAt = now.Add(backoff)
	failure.lastFailure = now
	l.failures[key] = failure
	return backoff
}

func (l *verificationLimiter) clear(key string) {
	l.mu.Lock()
	delete(l.failures, key)
	l.mu.Unlock()
}

func (l *verificationLimiter) evictOldest() {
	var (
		oldestKey  string
		oldestTime time.Time
	)
	for key, failure := range l.failures {
		if oldestKey == "" || failure.lastFailure.Before(oldestTime) {
			oldestKey = key
			oldestTime = failure.lastFailure
		}
	}
	if oldestKey != "" {
		delete(l.failures, oldestKey)
	}
}
