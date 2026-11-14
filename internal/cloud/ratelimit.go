package cloud

import (
	"sync"
	"time"
)

// rateLimiter is a small sliding-window counter keyed by string, for endpoints that are not
// logins (sign-up, reset mail, link redemption, pairing).
type rateLimiter struct {
	mu   sync.Mutex
	now  func() time.Time
	hits map[string][]time.Time
}

func newRateLimiter(now func() time.Time) *rateLimiter {
	if now == nil {
		now = time.Now
	}
	return &rateLimiter{now: now, hits: map[string][]time.Time{}}
}

// Allow records one hit for key and reports whether it is within max hits per window.
func (r *rateLimiter) Allow(key string, max int, window time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.now()
	kept := r.hits[key][:0]
	for _, h := range r.hits[key] {
		if t.Sub(h) < window {
			kept = append(kept, h)
		}
	}
	if len(kept) >= max {
		r.hits[key] = kept
		return false
	}
	if len(r.hits) > 50000 {
		r.hits = map[string][]time.Time{}
	}
	r.hits[key] = append(kept, t)
	return true
}

// Prune drops idle keys.
func (r *rateLimiter) Prune() {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.now()
	for k, hs := range r.hits {
		if len(hs) == 0 || t.Sub(hs[len(hs)-1]) > 2*time.Hour {
			delete(r.hits, k)
		}
	}
}
