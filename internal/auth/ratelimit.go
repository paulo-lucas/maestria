package auth

import (
	"math"
	"sync"
	"time"
)

// LimiterOptions configure the failed-login limiter.
type LimiterOptions struct {
	MaxFailuresPerIP  int
	MaxFailuresGlobal int
	Window            time.Duration
	Lockout           time.Duration
}

// LimitDecision says whether a login attempt may proceed.
type LimitDecision struct {
	Allowed    bool
	RetryAfter time.Duration
	Scope      string // "ip" or "global" when refused
}

type bucket struct {
	failures    []time.Time
	lockedUntil time.Time
}

const maxTrackedIPs = 10000

// LoginLimiter is the failed-login limiter (V1 semantics): one bucket per client IP plus a global
// bucket against distributed guessing. Reaching the maximum within the window locks the bucket;
// while locked, even correct credentials are refused. Success clears only the IP's bucket.
type LoginLimiter struct {
	opts LimiterOptions
	now  func() time.Time

	mu     sync.Mutex
	ips    map[string]*bucket
	global bucket
}

// NewLoginLimiter returns a limiter. now may be nil.
func NewLoginLimiter(opts LimiterOptions, now func() time.Time) *LoginLimiter {
	if now == nil {
		now = time.Now
	}
	return &LoginLimiter{opts: opts, now: now, ips: map[string]*bucket{}}
}

func (l *LoginLimiter) prune(b *bucket, t time.Time) {
	kept := b.failures[:0]
	for _, f := range b.failures {
		if t.Sub(f) < l.opts.Window {
			kept = append(kept, f)
		}
	}
	b.failures = kept
}

func retryAfter(until, t time.Time) time.Duration {
	secs := math.Ceil(until.Sub(t).Seconds())
	return time.Duration(secs) * time.Second
}

// Check reports whether ip may attempt a login now.
func (l *LoginLimiter) Check(ip string) LimitDecision {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.now()
	if l.global.lockedUntil.After(t) {
		return LimitDecision{RetryAfter: retryAfter(l.global.lockedUntil, t), Scope: "global"}
	}
	if b := l.ips[ip]; b != nil && b.lockedUntil.After(t) {
		return LimitDecision{RetryAfter: retryAfter(b.lockedUntil, t), Scope: "ip"}
	}
	return LimitDecision{Allowed: true}
}

// Fail records a failed attempt and reports whether it triggered a lockout.
func (l *LoginLimiter) Fail(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.now()
	b := l.ips[ip]
	if b == nil {
		if len(l.ips) >= maxTrackedIPs {
			l.evict(t)
		}
		b = &bucket{}
		l.ips[ip] = b
	}
	locked := false
	l.prune(b, t)
	b.failures = append(b.failures, t)
	if len(b.failures) >= l.opts.MaxFailuresPerIP {
		b.lockedUntil = t.Add(l.opts.Lockout)
		b.failures = nil
		locked = true
	}
	l.prune(&l.global, t)
	l.global.failures = append(l.global.failures, t)
	if len(l.global.failures) >= l.opts.MaxFailuresGlobal {
		l.global.lockedUntil = t.Add(l.opts.Lockout)
		l.global.failures = nil
		locked = true
	}
	return locked
}

// Remaining returns how many failures ip has left before its bucket locks.
func (l *LoginLimiter) Remaining(ip string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.ips[ip]
	if b == nil {
		return l.opts.MaxFailuresPerIP
	}
	l.prune(b, l.now())
	return l.opts.MaxFailuresPerIP - len(b.failures)
}

// Success clears the IP's bucket.
func (l *LoginLimiter) Success(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.ips, ip)
}

func (l *LoginLimiter) evict(t time.Time) {
	for ip, b := range l.ips {
		l.prune(b, t)
		if len(b.failures) == 0 && !b.lockedUntil.After(t) {
			delete(l.ips, ip)
		}
	}
	if len(l.ips) >= maxTrackedIPs {
		l.ips = map[string]*bucket{}
	}
}
