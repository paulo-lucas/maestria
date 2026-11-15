package auth

import (
	"math"
	"net"
	"sync"
	"time"
)

// A password check is deliberately expensive (argon2id, 64 MiB). On a page anyone can reach, that
// cost is a weapon: a few hundred requests in flight would use gigabytes. HashGuard puts three
// limits in front of every argon2 call that an unauthenticated client can trigger:
//
//   - a number of checks per client address per window, counted before the work starts;
//   - a number of checks for the whole server per window;
//   - a small number of checks running at the same time. When they are all busy the request is
//     refused (the caller answers 429 with Retry-After), never queued, so memory stays bounded.
//
// A refusal says nothing about the password, so it is no oracle. Direct local requests (the panel
// opened on the same machine, no proxy header) are only bound by their own, larger, allowance and
// by the concurrency limit: a stranger on the internet cannot spend the owner's budget.

// GuardOptions configure a HashGuard. Zero fields take the defaults below.
type GuardOptions struct {
	Slots      int           // checks running at once (default 2)
	PerKeyMax  int           // checks per client per Window (default 10)
	LocalMax   int           // checks per Window for a loopback client (default 30)
	GlobalMax  int           // checks per Window for everyone (default 300)
	Window     time.Duration // default one minute
	BusyRetry  time.Duration // Retry-After when every slot is busy (default 2s)
	MaxTracked int           // client keys remembered (default 10000)
	// LocalOwner is set by the local panel: a loopback client is then a direct request on this
	// machine, gets LocalMax and is outside the global budget. A server behind a reverse proxy
	// leaves it off (there every visitor can look like the proxy).
	LocalOwner bool
}

func (o *GuardOptions) fill() {
	if o.Slots <= 0 {
		o.Slots = 2
	}
	if o.PerKeyMax <= 0 {
		o.PerKeyMax = 10
	}
	if o.LocalMax <= 0 {
		o.LocalMax = 30
	}
	if o.GlobalMax <= 0 {
		o.GlobalMax = 300
	}
	if o.Window <= 0 {
		o.Window = time.Minute
	}
	if o.BusyRetry <= 0 {
		o.BusyRetry = 2 * time.Second
	}
	if o.MaxTracked <= 0 {
		o.MaxTracked = 10000
	}
}

// GuardDecision is the outcome of Acquire.
type GuardDecision struct {
	Allowed    bool
	RetryAfter time.Duration
	Reason     string // "client", "global" or "busy" when refused
}

// HashGuard limits unauthenticated argon2 work. The zero value is not usable; use NewHashGuard.
type HashGuard struct {
	opts GuardOptions
	now  func() time.Time
	sem  chan struct{}

	mu     sync.Mutex
	keys   map[string][]time.Time
	global []time.Time
}

// NewHashGuard returns a guard. now may be nil.
func NewHashGuard(opts GuardOptions, now func() time.Time) *HashGuard {
	opts.fill()
	if now == nil {
		now = time.Now
	}
	return &HashGuard{opts: opts, now: now, sem: make(chan struct{}, opts.Slots), keys: map[string][]time.Time{}}
}

// IsLoopback reports whether a client key is a loopback address.
func IsLoopback(key string) bool {
	ip := net.ParseIP(key)
	return ip != nil && ip.IsLoopback()
}

func (g *HashGuard) trim(ts []time.Time, t time.Time) []time.Time {
	kept := ts[:0]
	for _, x := range ts {
		if t.Sub(x) < g.opts.Window {
			kept = append(kept, x)
		}
	}
	return kept
}

func (g *HashGuard) retry(ts []time.Time, t time.Time) time.Duration {
	if len(ts) == 0 {
		return time.Second
	}
	d := ts[0].Add(g.opts.Window).Sub(t)
	if d < time.Second {
		d = time.Second
	}
	return time.Duration(math.Ceil(d.Seconds())) * time.Second
}

// Acquire is called right before an argon2 check for the client key (its address). When Allowed,
// the caller must call release when the check is done. The check is counted even if the
// password turns out wrong; a refusal does not run it.
func (g *HashGuard) Acquire(key string) (release func(), d GuardDecision) {
	g.mu.Lock()
	t := g.now()
	limit := g.opts.PerKeyMax
	local := g.opts.LocalOwner && IsLoopback(key)
	if local {
		limit = g.opts.LocalMax
	}
	hits := g.trim(g.keys[key], t)
	if len(hits) >= limit {
		g.keys[key] = hits
		g.mu.Unlock()
		return nil, GuardDecision{Reason: "client", RetryAfter: g.retry(hits, t)}
	}
	exempt := local
	if !exempt {
		g.global = g.trim(g.global, t)
		if len(g.global) >= g.opts.GlobalMax {
			g.mu.Unlock()
			return nil, GuardDecision{Reason: "global", RetryAfter: g.retry(g.global, t)}
		}
	}
	select {
	case g.sem <- struct{}{}:
	default:
		g.mu.Unlock()
		return nil, GuardDecision{Reason: "busy", RetryAfter: g.opts.BusyRetry}
	}
	if len(g.keys) >= g.opts.MaxTracked {
		g.evict(t)
	}
	g.keys[key] = append(hits, t)
	if !exempt {
		g.global = append(g.global, t)
	}
	g.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { <-g.sem }) }, GuardDecision{Allowed: true}
}

func (g *HashGuard) evict(t time.Time) {
	for k, ts := range g.keys {
		if ts = g.trim(ts, t); len(ts) == 0 {
			delete(g.keys, k)
		} else {
			g.keys[k] = ts
		}
	}
	if len(g.keys) >= g.opts.MaxTracked {
		g.keys = map[string][]time.Time{}
	}
}
