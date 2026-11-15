package auth

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHashGuardLimitsPerClientAndRecovers(t *testing.T) {
	c := newClock()
	g := NewHashGuard(GuardOptions{PerKeyMax: 3, Slots: 8}, c.now)
	for i := 0; i < 3; i++ {
		rel, d := g.Acquire("203.0.113.5")
		if !d.Allowed {
			t.Fatalf("check %d refused: %+v", i, d)
		}
		rel()
	}
	if rel, d := g.Acquire("203.0.113.5"); d.Allowed || d.Reason != "client" || d.RetryAfter < time.Second {
		t.Fatalf("the fourth check from one client must be refused with a Retry-After: %+v", d)
	} else if rel != nil {
		t.Fatal("a refusal must not hand out a slot")
	}
	if _, d := g.Acquire("203.0.113.6"); !d.Allowed {
		t.Fatalf("another client is unaffected: %+v", d)
	}
	c.advance(time.Minute + time.Second)
	if _, d := g.Acquire("203.0.113.5"); !d.Allowed {
		t.Fatalf("the window slides: %+v", d)
	}
}

func TestHashGuardGlobalBudgetAndLoopbackOwner(t *testing.T) {
	c := newClock()
	g := NewHashGuard(GuardOptions{PerKeyMax: 100, GlobalMax: 5, LocalMax: 100, Slots: 100, LocalOwner: true}, c.now)
	for i := 0; i < 5; i++ {
		keyIP := "198.51.100." + string(rune('1'+i))
		if rel, d := g.Acquire(keyIP); !d.Allowed {
			t.Fatalf("check %d: %+v", i, d)
		} else {
			rel()
		}
	}
	if _, d := g.Acquire("198.51.100.9"); d.Allowed || d.Reason != "global" {
		t.Fatalf("the global budget must refuse a sixth client: %+v", d)
	}
	// The owner on this machine is outside the global budget: strangers cannot spend it.
	if rel, d := g.Acquire("127.0.0.1"); !d.Allowed {
		t.Fatalf("a direct local client was refused by the global budget: %+v", d)
	} else {
		rel()
	}
	// A server behind a proxy does not know who 127.0.0.1 is, so it gets no exemption there.
	proxied := NewHashGuard(GuardOptions{PerKeyMax: 100, GlobalMax: 1, Slots: 100}, c.now)
	rel, _ := proxied.Acquire("127.0.0.1")
	rel()
	if _, d := proxied.Acquire("127.0.0.1"); d.Allowed {
		t.Fatal("without LocalOwner, loopback is a normal client")
	}
}

// Many requests at once never run more than Slots checks together, and the rest are refused at
// once ("busy") instead of waiting with their memory allocated.
func TestHashGuardBoundsConcurrency(t *testing.T) {
	g := NewHashGuard(GuardOptions{Slots: 2, PerKeyMax: 1000, GlobalMax: 1000}, nil)
	var running, peak, refused atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			rel, d := g.Acquire("203.0.113." + string(rune('a'+i%26)))
			if !d.Allowed {
				if d.Reason != "busy" || d.RetryAfter <= 0 {
					t.Errorf("refusal: %+v", d)
				}
				refused.Add(1)
				return
			}
			n := running.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(30 * time.Millisecond)
			running.Add(-1)
			rel()
		}(i)
	}
	close(start)
	wg.Wait()
	if peak.Load() > 2 {
		t.Fatalf("%d checks ran at once, the limit is 2", peak.Load())
	}
	if refused.Load() == 0 {
		t.Fatal("nothing was refused although 64 requests arrived together")
	}
}

func TestHashGuardReleaseIsIdempotent(t *testing.T) {
	g := NewHashGuard(GuardOptions{Slots: 1, PerKeyMax: 100, GlobalMax: 100}, nil)
	rel, d := g.Acquire("a")
	if !d.Allowed {
		t.Fatal("first")
	}
	rel()
	rel() // a second release must not free a slot that belongs to someone else
	r2, d := g.Acquire("b")
	if !d.Allowed {
		t.Fatal("second")
	}
	if _, d := g.Acquire("c"); d.Allowed {
		t.Fatal("a double release let a third check in")
	}
	r2()
}

// Failures from strangers never count against the owner on this machine, and a global lock set
// by a distributed attack does not refuse the owner either.
func TestLimiterNeverLocksOutDirectLocalAccess(t *testing.T) {
	c := newClock()
	l := NewLoginLimiter(LimiterOptions{MaxFailuresPerIP: 3, MaxFailuresGlobal: 5, Window: time.Minute, Lockout: 10 * time.Minute, LocalOwner: true}, c.now)
	for i := 0; i < 5; i++ { // five different strangers fail
		l.Fail("203.0.113." + string(rune('1'+i)))
	}
	if l.Check("203.0.113.99").Allowed {
		t.Fatal("the global lock must stop other strangers")
	}
	if !l.Check("127.0.0.1").Allowed {
		t.Fatal("the global lock must not lock the owner out on this machine")
	}
	// The owner's own failures stay in the owner's bucket: they cannot lock out the rest of the world.
	l2 := NewLoginLimiter(LimiterOptions{MaxFailuresPerIP: 3, MaxFailuresGlobal: 5, Window: time.Minute, Lockout: 10 * time.Minute, LocalOwner: true}, c.now)
	for i := 0; i < 20; i++ {
		l2.Fail("127.0.0.1")
	}
	if !l2.Check("203.0.113.1").Allowed {
		t.Fatal("local failures reached the global bucket")
	}
	// A server behind a reverse proxy has no such exemption.
	l3 := NewLoginLimiter(LimiterOptions{MaxFailuresPerIP: 3, MaxFailuresGlobal: 2, Window: time.Minute, Lockout: 10 * time.Minute}, c.now)
	l3.Fail("203.0.113.1")
	l3.Fail("203.0.113.2")
	if l3.Check("127.0.0.1").Allowed {
		t.Fatal("without LocalOwner the global lock applies to everyone")
	}
}
