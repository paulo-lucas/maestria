package web

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
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
		if got := h.srv.clientIP(requestWith("127.0.0.1:6000", v)); got != tunnelClient {
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
	if got := h2.srv.clientIP(requestWith("127.0.0.1:6000", "192.0.2.77")); got != tunnelClient {
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

// An open event stream used to survive signing out, signing out everywhere and the kill switch,
// because the session was only checked when the stream opened.
func TestEventStreamEndsWhenTheSessionDoes(t *testing.T) {
	cases := map[string]func(h *harness, c *client, token string){
		"logout": func(h *harness, c *client, token string) { c.postForm("/logout", url.Values{"_csrf": {token}}, nil) },
		"logout-all": func(h *harness, c *client, token string) {
			c.postForm("/logout-all", url.Values{"_csrf": {token}}, nil)
		},
		"kill switch": func(h *harness, c *client, token string) {
			c.postForm("/kill-switch", url.Values{"_csrf": {token}}, nil)
		},
		"session expired": func(h *harness, c *client, token string) { h.clock.advance(13 * time.Hour) },
	}
	for name, end := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, opts{mutate: func(c *config.Config) { c.MaestriDir = filepath.Join(t.TempDir(), "maestri") }})
			h.srv.ssePollInterval = 20 * time.Millisecond
			h.srv.sseRecheckInterval = 30 * time.Millisecond
			c := h.client()
			token := c.login()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			pr, pw := newPipeResponseWriter()
			req := httptest.NewRequest(http.MethodGet, "http://"+testHost+"/api/stream", nil).WithContext(ctx)
			req.Host, req.RemoteAddr = testHost, c.ip
			for _, ck := range c.jar.Cookies(req.URL) {
				req.AddCookie(ck)
			}
			done := make(chan struct{})
			go func() { defer close(done); h.srv.ServeHTTP(pw, req) }()
			if line, err := bufio.NewReader(pr).ReadString('\n'); err != nil || !strings.Contains(line, "event: ready") {
				t.Fatalf("stream did not open: %q %v", line, err)
			}
			select {
			case <-done:
				t.Fatal("the stream ended by itself")
			case <-time.After(100 * time.Millisecond):
			}

			end(h, c, token)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the stream is still open after the session ended")
			}
			_ = pw.Close()
		})
	}
}

// Behind a tunnel that reports no visitor address (ngrok, Tailscale Funnel, ssh -R, a reverse
// proxy) every visitor connects from 127.0.0.1. Only a request that is really the person at this
// machine (the panel's own loopback Host and no proxy header) is treated as the local owner.
func TestOnlyTheRealLocalOwnerGetsTheLoopbackAddress(t *testing.T) {
	h := newHarness(t, opts{mutate: func(c *config.Config) {
		c.AllowedHosts = append(c.AllowedHosts, "panel.example.com")
		c.TrustCfConnectingIP, c.TrustCfConnectingIPSet = false, true // a tunnel that gives no address
	}})
	req := func(host string, hdr map[string]string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/login", nil)
		r.RemoteAddr, r.Host = "127.0.0.1:50000", host
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		return r
	}
	for _, host := range []string{"127.0.0.1:4777", "localhost:4777", "LocalHost:4777"} {
		if got := h.srv.clientIP(req(host, nil)); got != "127.0.0.1" {
			t.Errorf("the owner at %s must be local, got %q", host, got)
		}
	}
	for name, r := range map[string]*http.Request{
		"public Host":                req("panel.example.com", nil),
		"public Host with a header":  req("panel.example.com", map[string]string{"X-Forwarded-For": "203.0.113.9"}),
		"loopback Host, X-Forwarded": req("127.0.0.1:4777", map[string]string{"X-Forwarded-For": "203.0.113.9"}),
		"loopback Host, Forwarded":   req("127.0.0.1:4777", map[string]string{"Forwarded": "for=203.0.113.9"}),
		"loopback Host, X-Real-Ip":   req("127.0.0.1:4777", map[string]string{"X-Real-Ip": "203.0.113.9"}),
		"loopback Host, Cloudflare":  req("127.0.0.1:4777", map[string]string{"Cf-Ray": "abc"}),
		"loopback Host, Via":         req("127.0.0.1:4777", map[string]string{"Via": "1.1 proxy"}),
		"loopback Host, X-Client-Ip": req("127.0.0.1:4777", map[string]string{"X-Client-Ip": "203.0.113.9"}),
		"loopback Host, X-Cluster":   req("127.0.0.1:4777", map[string]string{"X-Cluster-Client-Ip": "203.0.113.9"}),
		"loopback Host, Fastly":      req("127.0.0.1:4777", map[string]string{"Fastly-Client-Ip": "203.0.113.9"}),
		"loopback Host, X-Original":  req("127.0.0.1:4777", map[string]string{"X-Original-Forwarded-For": "203.0.113.9"}),
		"loopback Host, wrong port":  req("127.0.0.1:9999", nil),
	} {
		if got := h.srv.clientIP(r); got != tunnelClient {
			t.Errorf("%s: client %q, want the shared %q bucket", name, got, tunnelClient)
		}
	}
	// A real visitor address from a trusted header still wins over the shared bucket.
	h2 := newHarness(t, opts{mutate: trustCfHeader})
	if got := h2.srv.clientIP(req("panel.example.com", map[string]string{"Cf-Connecting-Ip": "203.0.113.9"})); got != "203.0.113.9" {
		t.Errorf("visitor address: %q", got)
	}
}

// A stranger's failed sign-ins and password checks through a tunnel that reports no address fill
// the shared "tunnel" bucket. They must not lock out, or 429, the owner at the machine.
func TestTunnelStrangersCannotLockOutTheOwnerAtTheMachine(t *testing.T) {
	h := newHarness(t, opts{mutate: func(c *config.Config) {
		c.AllowedHosts = append(c.AllowedHosts, "panel.example.com")
		c.TrustCfConnectingIP, c.TrustCfConnectingIPSet = false, true
	}})
	fromTunnel := func() *client {
		c := h.client()
		c.ip, c.host = "127.0.0.1:50000", "panel.example.com"
		return c
	}
	// Lock the shared bucket with failed sign-ins (5 per address), then burn the password-check
	// budget as well.
	stranger := fromTunnel()
	locked := false
	for i := 0; i < 8 && !locked; i++ {
		_, r := stranger.attempt("wrong password!", "000000", false)
		locked = r.Code == http.StatusTooManyRequests
	}
	if !locked {
		t.Fatal("the shared tunnel bucket never locked")
	}
	for i := 0; i < 40; i++ {
		c := fromTunnel()
		c.postForm("/login", url.Values{"_csrf": {csrfFrom(t, c.get("/login"))}, "password": {"x"}}, nil)
	}
	// Other tunnel visitors (the owner on their phone, say) share the stranger's fate...
	phone := fromTunnel()
	if r := phone.postForm("/login", url.Values{"_csrf": {csrfFrom(t, phone.get("/login"))}, "password": {testPassword}}, nil); r.Code != http.StatusTooManyRequests {
		t.Fatalf("tunnel visitors share one bucket: %d", r.Code)
	}
	// ...but the owner at the machine does not.
	owner := h.client()
	owner.ip, owner.host = "127.0.0.1:50001", testHost
	if _, r := owner.attempt(testPassword, h.code(0), false); r.Header().Get("Location") != "/" {
		t.Fatalf("the owner at the machine was refused: %d %s", r.Code, r.Header().Get("Location"))
	}
}
