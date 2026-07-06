package httpapi

import (
	"container/list"
	"sync"
	"time"
)

// Login rate limits (design §5): a token bucket per (clientIP, username) at
// 5/min with burst 10, PLUS a per-clientIP aggregate at 20/min. Both live in
// one bounded LRU table so a spray of usernames or IPs cannot grow memory
// without bound.
const (
	loginPairRatePerMin = 5
	loginPairBurst      = 10
	loginIPRatePerMin   = 20
	loginIPBurst        = 20
	loginLimiterEntries = 10000
)

type bucketEntry struct {
	key          string
	tokens       float64
	burst        float64
	refillPerSec float64
	last         time.Time
}

// loginLimiter is a bounded-LRU collection of token buckets.
type loginLimiter struct {
	mu      sync.Mutex
	cap     int
	order   *list.List // front = most recently used
	entries map[string]*list.Element
}

func newLoginLimiter(capacity int) *loginLimiter {
	return &loginLimiter{
		cap:     capacity,
		order:   list.New(),
		entries: make(map[string]*list.Element),
	}
}

// Allow reports whether one login attempt from clientIP for username may
// proceed at time now. Both buckets are consulted (and consumed) so a
// rejected attempt still counts against the other bucket.
func (l *loginLimiter) Allow(clientIP, username string, now time.Time) bool {
	ipOK := l.allow("ip\x00"+clientIP, loginIPRatePerMin, loginIPBurst, now)
	pairOK := l.allow("pu\x00"+clientIP+"\x00"+username, loginPairRatePerMin, loginPairBurst, now)
	return ipOK && pairOK
}

// Len reports the number of live bucket entries (tests: the table must not
// grow on requests that are rejected before the limiter runs).
func (l *loginLimiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

func (l *loginLimiter) allow(key string, ratePerMin, burst float64, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	var b *bucketEntry
	if el, ok := l.entries[key]; ok {
		l.order.MoveToFront(el)
		b = el.Value.(*bucketEntry)
		if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
			b.tokens = min(b.burst, b.tokens+elapsed*b.refillPerSec)
			b.last = now
		}
	} else {
		b = &bucketEntry{key: key, tokens: burst, burst: burst, refillPerSec: ratePerMin / 60, last: now}
		l.entries[key] = l.order.PushFront(b)
		for l.order.Len() > l.cap {
			oldest := l.order.Back()
			l.order.Remove(oldest)
			delete(l.entries, oldest.Value.(*bucketEntry).key)
		}
	}

	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}
