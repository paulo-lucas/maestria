package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paulo-lucas/maestria/internal/auth"
	"github.com/paulo-lucas/maestria/internal/config"
)

// Step 1 of the login runs argon2id for anyone who can reach the page, so it is counted and
// limited before it runs. A refusal says nothing about the password.
func TestLoginStepOneIsRateLimitedBeforeHashing(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	tok := csrfFrom(t, c.get("/login"))

	before := auth.VerifyCalls()
	var limited bool
	for i := 0; i < 40; i++ {
		r := c.postForm("/login", url.Values{"_csrf": {tok}, "password": {"wrong-password-123"}}, nil)
		if r.Code == http.StatusTooManyRequests {
			if r.Header().Get("Retry-After") == "" {
				t.Fatal("a throttled sign-in needs a Retry-After")
			}
			if !contains(r.body, "Too many sign-in attempts") || contains(r.body, "failed attempts") {
				t.Fatalf("a throttle is not a lockout and must not look like a wrong password: %s", r.body)
			}
			limited = true
			break
		}
		if r.Code != http.StatusSeeOther {
			t.Fatalf("attempt %d: %d", i, r.Code)
		}
		tok = csrfFrom(t, c.get("/login"))
	}
	if !limited {
		t.Fatal("forty step-1 posts from one address were all accepted")
	}
	if n := auth.VerifyCalls() - before; n > 11 {
		t.Fatalf("%d argon2 checks ran for one address in one window", n)
	}
	// Another address is not affected, and the right password still works for it.
	other := h.client()
	other.ip = "203.0.113.9:1234"
	if _, r := other.attempt(testPassword, h.code(0), false); r.Header().Get("Location") != "/" {
		t.Fatalf("another address: %d %s", r.Code, r.Header().Get("Location"))
	}
	// The window slides.
	h.clock.advance(2 * time.Minute)
	if r := c.postForm("/login", url.Values{"_csrf": {csrfFrom(t, c.get("/login"))}, "password": {"x"}}, nil); r.Code != http.StatusSeeOther {
		t.Fatalf("after the window: %d", r.Code)
	}
}

// A burst of step-1 posts from many addresses never runs more than the allowed number of checks at
// once; the rest are told to come back (429) instead of being queued with their memory.
func TestLoginStepOneBurstIsBoundedByTheSemaphore(t *testing.T) {
	h := newHarness(t)
	const n = 24
	clients := make([]*client, n)
	toks := make([]string, n)
	for i := range clients {
		clients[i] = h.client()
		clients[i].ip = "203.0.113." + strconv.Itoa(i+10) + ":5000"
		toks[i] = csrfFrom(t, clients[i].get("/login"))
	}
	codes := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range clients {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			r := clients[i].postForm("/login", url.Values{"_csrf": {toks[i]}, "password": {"x"}}, nil)
			codes[i] = r.Code
			if r.Code == http.StatusTooManyRequests && r.Header().Get("Retry-After") == "" {
				t.Errorf("client %d: 429 without Retry-After", i)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	ok, busy := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusSeeOther:
			ok++
		case http.StatusTooManyRequests:
			busy++
		default:
			t.Fatalf("unexpected status %d", c)
		}
	}
	if ok == 0 || busy == 0 {
		t.Fatalf("expected some checks to run and some to be refused as busy, got %d ok and %d refused", ok, busy)
	}
}

// Behind a tunnel that reports no addresses, every visitor is 127.0.0.1. One visitor's first
// step must not wipe the owner's half-finished sign-in.
func TestPendingLoginIsNotEvictedByAnotherVisitor(t *testing.T) {
	h := newHarness(t)
	owner, attacker := h.client(), h.client()
	owner.ip, attacker.ip = "127.0.0.1:50001", "127.0.0.1:50002"

	page := owner.get("/login")
	r := owner.postForm("/login", url.Values{"_csrf": {csrfFrom(t, page)}, "password": {testPassword}}, nil)
	if r.Header().Get("Location") != "/login/2fa" {
		t.Fatal("owner step 1 failed")
	}
	twofa := owner.get("/login/2fa")

	for i := 0; i < 5; i++ { // the other visitor tries a few times
		ap := attacker.get("/login")
		attacker.postForm("/login", url.Values{"_csrf": {csrfFrom(t, ap)}, "password": {"anything-at-all"}}, nil)
	}

	h.clock.advance(auth.TOTPStep)
	r = owner.postForm("/login/2fa", url.Values{"_csrf": {csrfFrom(t, twofa)}, "code": {h.code(0)}}, nil)
	if r.Header().Get("Location") != "/" {
		t.Fatalf("the owner's correct code after another visitor's step 1: %d -> %s", r.Code, r.Header().Get("Location"))
	}
}

// Strangers (with trusted visitor addresses) can fill the global failure bucket, and the lock then
// refuses other strangers. It must never refuse the owner opening the panel directly.
func TestGlobalLockoutNeverLocksOutDirectLocalAccess(t *testing.T) {
	h := newHarness(t, opts{mutate: trustCfHeader})
	for i := 0; i < 20; i++ { // MaxFailuresGlobal in the harness
		stranger := h.client()
		stranger.ip = "127.0.0.1:6000" // the tunnel's address; the header carries the visitor
		hdr := map[string]string{"Cf-Connecting-Ip": "192.0.2." + strconv.Itoa(i+1)}
		page := stranger.do(http.MethodGet, "/login", nil, hdr)
		stranger.postForm("/login", url.Values{"_csrf": {csrfFrom(t, page)}, "password": {"wrong password!"}}, hdr)
		page = stranger.do(http.MethodGet, "/login/2fa", nil, hdr)
		stranger.postForm("/login/2fa", url.Values{"_csrf": {csrfFrom(t, page)}, "code": {"000000"}}, hdr)
	}
	// Another stranger is now refused by the global lock...
	late := h.client()
	late.ip = "127.0.0.1:6001"
	hdr := map[string]string{"Cf-Connecting-Ip": "192.0.2.200"}
	page := late.do(http.MethodGet, "/login", nil, hdr)
	if r := late.postForm("/login", url.Values{"_csrf": {csrfFrom(t, page)}, "password": {testPassword}}, hdr); r.Code != http.StatusTooManyRequests {
		t.Fatalf("the global lock must stop other strangers: %d", r.Code)
	}
	// ...but the owner on this machine, with no proxy header, is not.
	owner := h.client()
	owner.ip = "127.0.0.1:7000"
	if _, r := owner.attempt(testPassword, h.code(0), false); r.Header().Get("Location") != "/" {
		t.Fatalf("direct local access was locked out: %d %s", r.Code, r.Header().Get("Location"))
	}
}

// A loopback value in CF-Connecting-IP is never believed: it would be a way to look like a direct
// local request and skip the global lock.
func TestLoopbackInCfHeaderIsIgnored(t *testing.T) {
	h := newHarness(t, opts{mutate: trustCfHeader})
	for _, v := range []string{"127.0.0.1", "::1", "0.0.0.0", "::ffff:127.0.0.1"} {
		c := h.client()
		c.ip = "127.0.0.1:6000"
		r := c.do(http.MethodGet, "/login", nil, map[string]string{"Cf-Connecting-Ip": v})
		if r.Code != http.StatusOK {
			t.Fatalf("%s: %d", v, r.Code)
		}
		if got := h.srv.clientIP(requestWith("127.0.0.1:6000", v)); got != "127.0.0.1" {
			t.Fatalf("header %q was believed: client address %q", v, got)
		}
	}
	if got := h.srv.clientIP(requestWith("127.0.0.1:6000", "192.0.2.77")); got != "192.0.2.77" {
		t.Fatalf("a real visitor address must be used: %q", got)
	}
}

// With a public host allowed and nothing else configured, the header is trusted: otherwise every
// visitor would share one address.
func TestPublicHostTurnsOnCfConnectingIPByDefault(t *testing.T) {
	h := newHarness(t, opts{mutate: func(c *configT) { c.AllowedHosts = append(c.AllowedHosts, "panel.example.com") }})
	if got := h.srv.clientIP(requestWith("127.0.0.1:6000", "192.0.2.77")); got != "192.0.2.77" {
		t.Fatalf("a tunnel is in use (public host allowed): %q", got)
	}
	h2 := newHarness(t)
	if got := h2.srv.clientIP(requestWith("127.0.0.1:6000", "192.0.2.77")); got != "127.0.0.1" {
		t.Fatalf("no public host: the header is ignored: %q", got)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

type configT = config.Config

func trustCfHeader(c *config.Config) { c.TrustCfConnectingIP, c.TrustCfConnectingIPSet = true, true }

func requestWith(remote, cfIP string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/login", nil)
	r.RemoteAddr = remote
	r.Header.Set("Cf-Connecting-Ip", cfIP)
	return r
}

// An unauthenticated post with a bad CSRF token (and a matching Origin, which a non-browser client
// sets freely) must not write one audit row per request.
func TestBadCSRFPostsDoNotFloodTheAuditLog(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	var before int
	_ = h.db.QueryRow("SELECT COUNT(*) FROM audit").Scan(&before)
	for i := 0; i < 2000; i++ {
		c.postForm("/login", url.Values{"_csrf": {"nope"}, "password": {"x"}}, nil)
	}
	var after int
	_ = h.db.QueryRow("SELECT COUNT(*) FROM audit").Scan(&after)
	if after-before > 3 {
		t.Fatalf("2000 bad-token posts from one address added %d audit rows", after-before)
	}
	// And rotating the claimed address (a sender can choose it behind a trusted proxy header) is
	// bounded too by the server-wide throttle.
	h2 := newHarness(t, opts{mutate: trustCfHeader})
	c2 := h2.client()
	_ = h2.db.QueryRow("SELECT COUNT(*) FROM audit").Scan(&before)
	for i := 0; i < 600; i++ {
		c2.postForm("/login", url.Values{"_csrf": {"nope"}, "password": {"x"}}, map[string]string{"Cf-Connecting-Ip": "192.0.2." + strconv.Itoa(i%250+1)})
	}
	_ = h2.db.QueryRow("SELECT COUNT(*) FROM audit").Scan(&after)
	if after-before > 3 {
		t.Fatalf("600 bad-token posts from rotating addresses added %d audit rows", after-before)
	}
}

// The Security page lists the last significant events: a flood of refusals cannot push a sign-in
// out of it.
func TestSecurityPageKeepsSignificantEventsVisible(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 80; i++ {
		_ = h.srv.Audit.Write("csrf_reject", "6.6.6.6", nil)
	}
	c := h.client()
	if _, r := c.attempt(testPassword, h.code(0), false); r.Header().Get("Location") != "/" {
		t.Fatal("sign-in failed")
	}
	page := c.get("/security")
	if !strings.Contains(page.body, "Signed in") {
		t.Fatalf("the sign-in is not on the Security page:\n%.600s", page.body)
	}
}
