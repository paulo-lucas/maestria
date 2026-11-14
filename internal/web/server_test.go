package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maestria-dev/maestria/internal/audit"
	"github.com/maestria-dev/maestria/internal/auth"
	"github.com/maestria-dev/maestria/internal/auth/authtest"
	"github.com/maestria-dev/maestria/internal/config"
	"github.com/maestria-dev/maestria/internal/store"
	"github.com/maestria-dev/maestria/internal/tunnel"
)

const (
	testPassword = "correct horse battery"
	testPort     = 4777
	testHost     = "127.0.0.1:4777"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type harness struct {
	t        *testing.T
	srv      *Server
	db       *sql.DB
	clock    *fakeClock
	secret   string
	stopRuns *[][]string
}

type opts struct {
	mutate func(*config.Config)
	access *auth.AccessVerifier
}

func newHarness(t *testing.T, o ...opts) *harness {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	cfg, err := config.Defaults(t.TempDir(), config.Env{Getenv: func(string) string { return "" }, GOOS: "linux", Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Port = testPort
	cfg.CookieSecure = false
	cfg.Tunnel.StopCommand = []string{"stop-tunnel"}
	cfg.Tunnel.StartCommand = []string{"start-tunnel"}
	var access *auth.AccessVerifier
	if len(o) > 0 {
		if o[0].mutate != nil {
			o[0].mutate(cfg)
		}
		access = o[0].access
	}

	secret, _ := auth.GenerateTOTPSecret()
	hash, err := auth.HashPassword(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{t: time.UnixMilli(1_700_000_000_000)}
	aud := audit.New(db, clock.now)
	sessions := auth.NewSessionStore(db, time.Duration(cfg.Session.IdleMinutes)*time.Minute, time.Duration(cfg.Session.AbsoluteHours)*time.Hour, clock.now)
	limiter := auth.NewLoginLimiter(auth.LimiterOptions{
		MaxFailuresPerIP: cfg.Login.MaxFailuresPerIP, MaxFailuresGlobal: cfg.Login.MaxFailuresGlobal,
		Window: time.Duration(cfg.Login.WindowSeconds) * time.Second, Lockout: time.Duration(cfg.Login.LockoutSeconds) * time.Second,
	}, clock.now)
	runs := &[][]string{}
	tun := &tunnel.Controller{
		DB: db, Audit: aud, Sessions: sessions, Config: cfg.Tunnel, Now: clock.now,
		Runner: func(_ context.Context, argv []string) tunnel.CommandResult {
			*runs = append(*runs, argv)
			return tunnel.CommandResult{OK: true}
		},
	}
	srv, err := New(Deps{
		Config: cfg, DB: db, Secrets: &auth.Secrets{PasswordHash: hash, TOTPSecret: secret, CreatedAt: "now"},
		Audit: aud, Sessions: sessions, Limiter: limiter, Tunnel: tun, Access: access, Now: clock.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, srv: srv, db: db, clock: clock, secret: secret, stopRuns: runs}
}

// client is a cookie-keeping browser stand-in that talks to the handler directly.
type client struct {
	h    *harness
	jar  *cookiejar.Jar
	host string
	ip   string
}

func (h *harness) client() *client {
	jar, _ := cookiejar.New(nil)
	return &client{h: h, jar: jar, host: testHost, ip: "198.51.100.7:4000"}
}

type resp struct {
	*httptest.ResponseRecorder
	body string
}

func (c *client) do(method, path string, body io.Reader, hdr map[string]string) resp {
	c.h.t.Helper()
	req := httptest.NewRequest(method, "http://"+c.host+path, body)
	req.RemoteAddr = c.ip
	req.Host = c.host
	u := &url.URL{Scheme: "http", Host: c.host, Path: "/"}
	for _, ck := range c.jar.Cookies(u) {
		req.AddCookie(ck)
	}
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("Origin", "http://"+c.host)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	c.h.srv.ServeHTTP(rec, req)
	res := rec.Result()
	c.jar.SetCookies(u, res.Cookies())
	b, _ := io.ReadAll(res.Body)
	return resp{ResponseRecorder: rec, body: string(b)}
}

func (c *client) cookie(name string) string {
	for _, ck := range c.jar.Cookies(&url.URL{Scheme: "http", Host: c.host, Path: "/"}) {
		if ck.Name == name {
			return ck.Value
		}
	}
	return ""
}

func (c *client) get(path string) resp { return c.do(http.MethodGet, path, nil, nil) }

func (c *client) postForm(path string, form url.Values, hdr map[string]string) resp {
	h := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	for k, v := range hdr {
		h[k] = v
	}
	return c.do(http.MethodPost, path, strings.NewReader(form.Encode()), h)
}

var csrfRE = regexp.MustCompile(`name="_csrf" value="([^"]+)"`)

func csrfFrom(t *testing.T, r resp) string {
	t.Helper()
	m := csrfRE.FindStringSubmatch(r.body)
	if m == nil {
		t.Fatalf("no csrf token in page (status %d): %.200s", r.Code, r.body)
	}
	return m[1]
}

func (h *harness) code(offset time.Duration) string {
	c, _ := auth.TOTP(h.secret, h.clock.now().Add(offset))
	return c
}

// login performs the full password + TOTP flow and returns the session CSRF token.
func (c *client) login() string {
	c.h.t.Helper()
	page := c.get("/login")
	r := c.postForm("/login", url.Values{"_csrf": {csrfFrom(c.h.t, page)}, "password": {testPassword}}, nil)
	if r.Code != http.StatusSeeOther || r.Header().Get("Location") != "/login/2fa" {
		c.h.t.Fatalf("password step: %d %s", r.Code, r.body)
	}
	page = c.get("/login/2fa")
	// A TOTP step can only be used once, so each client logging in needs a fresh one.
	c.h.clock.advance(auth.TOTPStep)
	r = c.postForm("/login/2fa", url.Values{"_csrf": {csrfFrom(c.h.t, page)}, "code": {c.h.code(0)}}, nil)
	if r.Code != http.StatusSeeOther || r.Header().Get("Location") != "/" {
		c.h.t.Fatalf("2fa step: %d %s", r.Code, r.body)
	}
	home := c.get("/security")
	if home.Code != http.StatusOK {
		c.h.t.Fatalf("authenticated page: %d", home.Code)
	}
	return csrfFrom(c.h.t, home)
}

// ---- security headers ----

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	for _, p := range []string{"/login", "/static/app.js", "/nope", "/api/session", "/"} {
		r := c.get(p)
		hd := r.Header()
		if hd.Get("Content-Security-Policy") != CSP {
			t.Errorf("%s: CSP %q", p, hd.Get("Content-Security-Policy"))
		}
		for k, want := range map[string]string{
			"X-Frame-Options": "DENY", "X-Content-Type-Options": "nosniff", "Referrer-Policy": "same-origin",
			"Cross-Origin-Opener-Policy": "same-origin", "Cross-Origin-Resource-Policy": "same-origin",
		} {
			if hd.Get(k) != want {
				t.Errorf("%s: %s = %q", p, k, hd.Get(k))
			}
		}
		if hd.Get("Permissions-Policy") == "" {
			t.Errorf("%s: no Permissions-Policy", p)
		}
		for k := range hd {
			if strings.HasPrefix(strings.ToLower(k), "access-control-") {
				t.Errorf("%s: CORS header %s must never be set", p, k)
			}
		}
		if hd.Get("Strict-Transport-Security") != "" {
			t.Errorf("%s: HSTS without Secure cookies", p)
		}
	}
	if !strings.Contains(CSP, "script-src 'self'") || strings.Contains(CSP, "unsafe-inline") || strings.Contains(CSP, "unsafe-eval") {
		t.Fatalf("CSP too loose: %s", CSP)
	}
}

func TestHSTSOnlyWithSecureCookies(t *testing.T) {
	h := newHarness(t, opts{mutate: func(c *config.Config) { c.CookieSecure = true }})
	if got := h.client().get("/login").Header().Get("Strict-Transport-Security"); got == "" {
		t.Fatal("HSTS expected with cookieSecure")
	}
}

func TestPagesHaveNoInlineScriptOrStyle(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	pages := map[string]string{"/login": c.get("/login").body}
	c.login()
	pages["/"] = c.get("/").body
	pages["/security"] = c.get("/security").body
	for path, body := range pages {
		if body == "" {
			t.Fatalf("%s: empty", path)
		}
		scripts := regexp.MustCompile(`(?is)<script\b[^>]*>`).FindAllString(body, -1)
		for _, s := range scripts {
			if !strings.Contains(s, "src=") {
				t.Errorf("%s: inline <script>: %s", path, s)
			}
		}
		if regexp.MustCompile(`(?i)<style\b`).MatchString(body) {
			t.Errorf("%s: inline <style>", path)
		}
		if regexp.MustCompile(`(?i)\sstyle\s*=`).MatchString(body) {
			t.Errorf("%s: style attribute", path)
		}
		if regexp.MustCompile(`(?i)\son[a-z]+\s*=`).MatchString(body) {
			t.Errorf("%s: inline event handler", path)
		}
	}
}

// ---- Host header ----

func TestHostAllowlist(t *testing.T) {
	h := newHarness(t, opts{mutate: func(c *config.Config) { c.AllowedHosts = []string{"panel.example.com"} }})
	for host, want := range map[string]int{
		"127.0.0.1:4777": 200, "localhost:4777": 200, "LOCALHOST:4777": 200, "panel.example.com": 200,
		"evil.example.com": 421, "127.0.0.1:9999": 421, "127.0.0.1": 421, "localhost": 421, "10.0.0.5:4777": 421, "": 421,
	} {
		c := h.client()
		c.host = host
		req := httptest.NewRequest(http.MethodGet, "/login", nil)
		req.Host = host
		req.RemoteAddr = c.ip
		rec := httptest.NewRecorder()
		h.srv.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Host %q: %d, want %d", host, rec.Code, want)
		}
	}
}

// ---- authentication ----

func TestUnauthenticatedRedirectsAndAPI401(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	if r := c.get("/"); r.Code != http.StatusSeeOther || r.Header().Get("Location") != "/login" {
		t.Fatalf("page: %d %s", r.Code, r.Header().Get("Location"))
	}
	if r := c.get("/security"); r.Code != http.StatusSeeOther {
		t.Fatalf("security: %d", r.Code)
	}
	for _, p := range []string{"/api/session", "/api/audit", "/api/tunnel"} {
		if r := c.get(p); r.Code != http.StatusUnauthorized || !strings.Contains(r.body, "unauthenticated") {
			t.Fatalf("%s: %d %s", p, r.Code, r.body)
		}
	}
	if r := c.postForm("/api/kill-switch", url.Values{}, nil); r.Code != http.StatusUnauthorized {
		t.Fatalf("kill switch unauthenticated: %d", r.Code)
	}
}

func TestLoginSetsHardenedSessionCookie(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	c.login()
	var found *http.Cookie
	for _, ck := range c.jar.Cookies(&url.URL{Scheme: "http", Host: testHost, Path: "/"}) {
		if ck.Name == "maestria_session" {
			found = ck
		}
	}
	if found == nil {
		t.Fatal("no session cookie")
	}
	// Inspect the Set-Cookie attributes directly on a fresh login.
	c2 := h.client()
	page := c2.get("/login")
	c2.postForm("/login", url.Values{"_csrf": {csrfFrom(t, page)}, "password": {testPassword}}, nil)
	page = c2.get("/login/2fa")
	h.clock.advance(auth.TOTPStep)
	r := c2.postForm("/login/2fa", url.Values{"_csrf": {csrfFrom(t, page)}, "code": {h.code(0)}}, nil)
	set := strings.Join(r.Header().Values("Set-Cookie"), "\n")
	for _, want := range []string{"maestria_session=", "HttpOnly", "SameSite=Strict", "Path=/"} {
		if !strings.Contains(set, want) {
			t.Errorf("Set-Cookie missing %q: %s", want, set)
		}
	}
	if strings.Contains(set, "Secure") {
		t.Error("Secure set although cookieSecure is false")
	}
	var token string
	for _, ck := range r.Result().Cookies() {
		if ck.Name == "maestria_session" {
			token = ck.Value
		}
	}
	var stored string
	_ = h.db.QueryRow("SELECT group_concat(id) FROM sessions").Scan(&stored)
	if strings.Contains(stored, token) || !strings.Contains(stored, auth.HashToken(token)) {
		t.Fatal("only the session token hash may be stored")
	}
}

func TestSecureCookieUsesHostPrefix(t *testing.T) {
	h := newHarness(t, opts{mutate: func(c *config.Config) { c.CookieSecure = true }})
	c := h.client()
	r := c.get("/login")
	set := strings.Join(r.Header().Values("Set-Cookie"), "\n")
	if !strings.Contains(set, "__Host-maestria_login_csrf=") || !strings.Contains(set, "Secure") || !strings.Contains(set, "Path=/") {
		t.Fatalf("login csrf cookie: %s", set)
	}
	if strings.Contains(set, "Domain=") {
		t.Fatal("__Host- cookies must not set Domain")
	}
}

func TestWrongPasswordAndWrongCodeAreRejected(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	page := c.get("/login")
	r := c.postForm("/login", url.Values{"_csrf": {csrfFrom(t, page)}, "password": {"not the password"}}, nil)
	if r.Code != http.StatusUnauthorized || !strings.Contains(r.body, "Invalid credentials.") {
		t.Fatalf("wrong password: %d", r.Code)
	}
	page = c.get("/login")
	c.postForm("/login", url.Values{"_csrf": {csrfFrom(t, page)}, "password": {testPassword}}, nil)
	page = c.get("/login/2fa")
	r = c.postForm("/login/2fa", url.Values{"_csrf": {csrfFrom(t, page)}, "code": {"000000"}}, nil)
	if r.Code != http.StatusUnauthorized || !strings.Contains(r.body, "That code didn") || !strings.Contains(r.body, "attempts left") {
		t.Fatalf("wrong code: %d %.300s", r.Code, r.body)
	}
	if r := c.get("/"); r.Code != http.StatusSeeOther {
		t.Fatal("must still be logged out")
	}
}

func TestTwoFARequiresPasswordStep(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	if r := c.get("/login/2fa"); r.Code != http.StatusSeeOther || r.Header().Get("Location") != "/login" {
		t.Fatalf("2fa page without password: %d", r.Code)
	}
	page := c.get("/login")
	r := c.postForm("/login/2fa", url.Values{"_csrf": {csrfFrom(t, page)}, "code": {h.code(0)}}, nil)
	if r.Code != http.StatusSeeOther || r.Header().Get("Location") != "/login" {
		t.Fatalf("2fa without password step must bounce to login: %d %s", r.Code, r.Header().Get("Location"))
	}
	if r := c.get("/"); r.Code != http.StatusSeeOther {
		t.Fatal("a correct code alone must not log in")
	}
}

func TestTOTPReplayRefusedOverHTTP(t *testing.T) {
	h := newHarness(t)
	first := h.client()
	first.login()
	// The same code (same step) for a second login attempt must be refused.
	c := h.client()
	page := c.get("/login")
	c.postForm("/login", url.Values{"_csrf": {csrfFrom(t, page)}, "password": {testPassword}}, nil)
	page = c.get("/login/2fa")
	r := c.postForm("/login/2fa", url.Values{"_csrf": {csrfFrom(t, page)}, "code": {h.code(0)}}, nil)
	if r.Code != http.StatusUnauthorized {
		t.Fatalf("replayed TOTP code: %d", r.Code)
	}
}

func TestRecoveryCodeLoginIsSingleUse(t *testing.T) {
	h := newHarness(t)
	codes, _ := auth.GenerateRecoveryCodes(2)
	if err := auth.ReplaceRecoveryCodes(h.db, codes, h.clock.now()); err != nil {
		t.Fatal(err)
	}
	attempt := func(code string) resp {
		c := h.client()
		page := c.get("/login")
		c.postForm("/login", url.Values{"_csrf": {csrfFrom(t, page)}, "password": {testPassword}}, nil)
		page = c.get("/login/2fa")
		return c.postForm("/login/recovery", url.Values{"_csrf": {csrfFrom(t, page)}, "recovery": {code}}, nil)
	}
	if r := attempt("aaaa-bbbb"); r.Code != http.StatusUnauthorized || !strings.Contains(r.body, "That recovery code didn") {
		t.Fatalf("bad recovery code: %d", r.Code)
	}
	if r := attempt(codes[0]); r.Code != http.StatusSeeOther || r.Header().Get("Location") != "/" {
		t.Fatalf("good recovery code: %d", r.Code)
	}
	if r := attempt(codes[0]); r.Code != http.StatusUnauthorized {
		t.Fatalf("reused recovery code: %d", r.Code)
	}
}

func TestLoginAuditNeverContainsSecrets(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	page := c.get("/login")
	c.postForm("/login", url.Values{"_csrf": {csrfFrom(t, page)}, "password": {"hunter2hunter2"}}, nil)
	c.login()
	entries, _ := h.srv.Audit.Recent(100)
	var all strings.Builder
	events := map[string]bool{}
	for _, e := range entries {
		all.WriteString(e.Event + " " + e.Detail + " " + e.IP + "\n")
		events[e.Event] = true
	}
	for _, e := range []string{"login_fail", "login_password_ok", "login_ok"} {
		if !events[e] {
			t.Errorf("missing audit event %s", e)
		}
	}
	for _, secret := range []string{"hunter2hunter2", testPassword, h.secret, h.code(0)} {
		if strings.Contains(all.String(), secret) {
			t.Fatalf("audit log leaks %q", secret)
		}
	}
}

// ---- rate limit ----

func TestLockoutBlocksEvenCorrectCredentialsThenRecovers(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	for i := 0; i < 5; i++ {
		page := c.get("/login")
		r := c.postForm("/login", url.Values{"_csrf": {csrfFrom(t, page)}, "password": {"wrong password!"}}, nil)
		want := http.StatusUnauthorized
		if i == 4 {
			want = http.StatusTooManyRequests // the fifth failure triggers the lockout
		}
		if r.Code != want {
			t.Fatalf("attempt %d: %d, want %d", i, r.Code, want)
		}
	}
	page := c.get("/login")
	r := c.postForm("/login", url.Values{"_csrf": {csrfFrom(t, page)}, "password": {testPassword}}, nil)
	if r.Code != http.StatusTooManyRequests || r.Header().Get("Retry-After") == "" || !strings.Contains(r.body, "Too many failed attempts") {
		t.Fatalf("locked out: %d retry=%q", r.Code, r.Header().Get("Retry-After"))
	}
	other := h.client()
	other.ip = "203.0.113.9:1234"
	page = other.get("/login")
	r = other.postForm("/login", url.Values{"_csrf": {csrfFrom(t, page)}, "password": {testPassword}}, nil)
	if r.Code != http.StatusSeeOther {
		t.Fatalf("another IP is unaffected by the per-IP lockout: %d", r.Code)
	}
	h.clock.advance(16 * time.Minute)
	page = c.get("/login")
	r = c.postForm("/login", url.Values{"_csrf": {csrfFrom(t, page)}, "password": {testPassword}}, nil)
	if r.Code != http.StatusSeeOther {
		t.Fatalf("recovers after the lockout: %d", r.Code)
	}
}

func TestTrustedCfConnectingIPKeysTheLimiter(t *testing.T) {
	h := newHarness(t, opts{mutate: func(c *config.Config) { c.TrustCfConnectingIP = true }})
	c := h.client()
	for i := 0; i < 5; i++ {
		page := c.get("/login")
		c.postForm("/login", url.Values{"_csrf": {csrfFrom(t, page)}, "password": {"wrong password!"}}, map[string]string{"Cf-Connecting-Ip": "192.0.2.50"})
	}
	page := c.get("/login")
	r := c.postForm("/login", url.Values{"_csrf": {csrfFrom(t, page)}, "password": {testPassword}}, map[string]string{"Cf-Connecting-Ip": "192.0.2.50"})
	if r.Code != http.StatusTooManyRequests {
		t.Fatalf("limiter must key on CF-Connecting-IP when trusted: %d", r.Code)
	}
	page = c.get("/login")
	r = c.postForm("/login", url.Values{"_csrf": {csrfFrom(t, page)}, "password": {testPassword}}, map[string]string{"Cf-Connecting-Ip": "192.0.2.51"})
	if r.Code != http.StatusSeeOther {
		t.Fatalf("a different client IP is unaffected: %d", r.Code)
	}
	// Untrusted by default: the header is ignored.
	h2 := newHarness(t)
	c2 := h2.client()
	for i := 0; i < 5; i++ {
		page := c2.get("/login")
		c2.postForm("/login", url.Values{"_csrf": {csrfFrom(t, page)}, "password": {"wrong password!"}}, map[string]string{"Cf-Connecting-Ip": "192." + string(rune('0'+i)) + ".2.1"})
	}
	page = c2.get("/login")
	if r := c2.postForm("/login", url.Values{"_csrf": {csrfFrom(t, page)}, "password": {testPassword}}, map[string]string{"Cf-Connecting-Ip": "9.9.9.9"}); r.Code != http.StatusTooManyRequests {
		t.Fatalf("CF-Connecting-IP must be ignored unless trusted: %d", r.Code)
	}
}

// ---- CSRF and Origin ----

func TestLoginPostWithoutOrWithWrongCSRFTokenIsRejected(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	page := c.get("/login")
	good := csrfFrom(t, page)

	r := c.postForm("/login", url.Values{"password": {testPassword}}, nil)
	if r.Code != http.StatusForbidden || !strings.Contains(r.body, "security token") {
		t.Fatalf("missing token: %d", r.Code)
	}
	fresh := csrfFrom(t, r)
	if fresh == good {
		t.Fatal("the re-rendered form must carry a fresh token")
	}
	r = c.postForm("/login", url.Values{"_csrf": {strings.Repeat("x", 43)}, "password": {testPassword}}, nil)
	if r.Code != http.StatusForbidden {
		t.Fatalf("wrong token: %d", r.Code)
	}
	r = c.postForm("/login", url.Values{"_csrf": {csrfFrom(t, r)}, "password": {testPassword}}, nil)
	if r.Code != http.StatusSeeOther {
		t.Fatalf("a fresh token must work: %d", r.Code)
	}
}

func TestLoginCSRFCookieIsReusedAcrossDoubleGET(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	first := csrfFrom(t, c.get("/login"))
	second := csrfFrom(t, c.get("/login"))
	if first != second {
		t.Fatal("a second GET /login must not rotate the token (prefetch, two tabs)")
	}
	if r := c.postForm("/login", url.Values{"_csrf": {first}, "password": {testPassword}}, nil); r.Code != http.StatusSeeOther {
		t.Fatalf("first form token must still work: %d", r.Code)
	}
}

// Every non-GET route, public or not, must refuse a request without a CSRF token.
func TestCSRFRequiredOnEveryWrite(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	token := c.login()

	writes := []string{"/logout", "/logout-all", "/kill-switch", "/api/kill-switch", "/login", "/login/2fa", "/login/recovery"}
	for _, p := range writes {
		for name, hdr := range map[string]map[string]string{
			"no token":    nil,
			"wrong token": {"X-CSRF-Token": strings.Repeat("A", 43)},
			"empty token": {"X-CSRF-Token": ""},
		} {
			r := c.postForm(p, url.Values{}, hdr)
			if r.Code != http.StatusForbidden {
				t.Errorf("POST %s (%s): %d, want 403", p, name, r.Code)
			}
		}
		r := c.do(http.MethodPost, p, strings.NewReader(`{}`), map[string]string{"Content-Type": "application/json", "X-CSRF-Token": strings.Repeat("A", 43)})
		if r.Code != http.StatusForbidden {
			t.Errorf("POST %s (JSON, wrong token): %d, want 403", p, r.Code)
		}
	}
	for _, method := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if r := c.do(method, "/api/kill-switch", nil, nil); r.Code != http.StatusForbidden {
			t.Errorf("%s without token: %d", method, r.Code)
		}
	}
	// Still logged in: none of the rejected writes had an effect.
	if r := c.get("/security"); r.Code != http.StatusOK {
		t.Fatal("session must survive rejected writes")
	}
	if h.srv.Tunnel.Killed() || len(*h.stopRuns) != 0 {
		t.Fatal("kill switch must not fire without a valid token")
	}
	// And the session token works in the header and in the form field.
	if r := c.postForm("/logout", url.Values{}, map[string]string{"X-CSRF-Token": token}); r.Code != http.StatusSeeOther {
		t.Fatalf("header token: %d", r.Code)
	}
	c2 := h.client()
	token2 := c2.login()
	if r := c2.postForm("/logout", url.Values{"_csrf": {token2}}, nil); r.Code != http.StatusSeeOther {
		t.Fatalf("form token: %d", r.Code)
	}
}

func TestSessionTokenIsBoundToItsSession(t *testing.T) {
	h := newHarness(t)
	a, b := h.client(), h.client()
	tokenA := a.login()
	b.login()
	if r := b.postForm("/logout", url.Values{}, map[string]string{"X-CSRF-Token": tokenA}); r.Code != http.StatusForbidden {
		t.Fatalf("another session's CSRF token must be refused: %d", r.Code)
	}
	if r := b.postForm("/logout", url.Values{"_csrf": {csrfFrom(t, b.get("/security"))}}, nil); r.Code != http.StatusSeeOther {
		t.Fatalf("own token: %d", r.Code)
	}
}

func TestCrossOriginAndOriginlessWritesRejectedEvenWithValidToken(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	token := c.login()
	cases := map[string]map[string]string{
		"cross-site origin":     {"Origin": "https://evil.example.com", "Sec-Fetch-Site": ""},
		"no origin":             {"Origin": "", "Sec-Fetch-Site": ""},
		"null, cross-site":      {"Origin": "null", "Sec-Fetch-Site": "cross-site"},
		"null, no fetch-site":   {"Origin": "null", "Sec-Fetch-Site": ""},
		"same-site, not origin": {"Origin": "http://" + testHost, "Sec-Fetch-Site": "same-site"},
		"port mismatch":         {"Origin": "http://127.0.0.1:9999", "Sec-Fetch-Site": ""},
		"garbage origin":        {"Origin": "://", "Sec-Fetch-Site": ""},
	}
	for name, hdr := range cases {
		hdr["X-CSRF-Token"] = token
		if r := c.postForm("/logout", url.Values{}, hdr); r.Code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", name, r.Code)
		}
	}
	if r := c.get("/security"); r.Code != http.StatusOK {
		t.Fatal("rejected writes must not end the session")
	}
	// Browsers may send Origin: null for same-origin form posts under strict privacy modes.
	r := c.postForm("/logout", url.Values{}, map[string]string{"Origin": "null", "Sec-Fetch-Site": "same-origin", "X-CSRF-Token": token})
	if r.Code != http.StatusSeeOther {
		t.Fatalf("Origin: null with Sec-Fetch-Site: same-origin must work: %d", r.Code)
	}
}

func TestOPTIONSAndUnknownMethodsAreNotCORSPreflights(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	c.login()
	r := c.do(http.MethodOptions, "/api/session", nil, map[string]string{
		"Origin": "https://evil.example.com", "Access-Control-Request-Method": "GET",
	})
	if r.Code == http.StatusOK || r.Code == http.StatusNoContent {
		t.Fatalf("preflight must not succeed: %d", r.Code)
	}
	if r.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("no CORS headers")
	}
}

// ---- sessions over HTTP ----

func TestSessionIdleAndAbsoluteExpiryOverHTTP(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	c.login()
	h.clock.advance(59 * time.Minute)
	if r := c.get("/security"); r.Code != http.StatusOK {
		t.Fatal("within the idle limit")
	}
	h.clock.advance(61 * time.Minute)
	if r := c.get("/security"); r.Code != http.StatusSeeOther {
		t.Fatal("idle session must expire")
	}

	c = h.client()
	c.login()
	for i := 0; i < 23; i++ {
		h.clock.advance(30 * time.Minute)
		if r := c.get("/security"); r.Code != http.StatusOK {
			t.Fatalf("active session expired early at step %d", i)
		}
	}
	h.clock.advance(31 * time.Minute)
	if r := c.get("/security"); r.Code != http.StatusSeeOther {
		t.Fatal("absolute limit must apply even when active")
	}
}

func TestLogoutAllInvalidatesEverySession(t *testing.T) {
	h := newHarness(t)
	a, b := h.client(), h.client()
	tokenA := a.login()
	b.login()
	r := a.postForm("/logout-all", url.Values{"_csrf": {tokenA}}, nil)
	if r.Code != http.StatusSeeOther {
		t.Fatalf("logout-all: %d", r.Code)
	}
	if b.get("/security").Code != http.StatusSeeOther || a.get("/security").Code != http.StatusSeeOther {
		t.Fatal("every session must be invalidated")
	}
	entries, _ := h.srv.Audit.Recent(20)
	seen := false
	for _, e := range entries {
		if e.Event == "logout_all" && strings.Contains(e.Detail, `"sessions":2`) {
			seen = true
		}
	}
	if !seen {
		t.Fatal("logout_all must be audited with the session count")
	}
}

func TestLogoutEndsOnlyThisSession(t *testing.T) {
	h := newHarness(t)
	a, b := h.client(), h.client()
	tokenA := a.login()
	b.login()
	if r := a.postForm("/logout", url.Values{"_csrf": {tokenA}}, nil); r.Code != http.StatusSeeOther {
		t.Fatalf("logout: %d", r.Code)
	}
	if a.get("/security").Code != http.StatusSeeOther {
		t.Fatal("logged out")
	}
	if b.get("/security").Code != http.StatusOK {
		t.Fatal("the other session must survive")
	}
}

// ---- kill switch ----

func TestKillSwitchStopsTunnelInvalidatesSessionsAndCannotBeUndoneOverHTTP(t *testing.T) {
	h := newHarness(t)
	a, b := h.client(), h.client()
	tokenA := a.login()
	b.login()

	r := a.postForm("/kill-switch", url.Values{"_csrf": {tokenA}}, nil)
	if r.Code != http.StatusSeeOther || r.Header().Get("Location") != "/login" {
		t.Fatalf("kill switch: %d", r.Code)
	}
	if len(*h.stopRuns) != 1 || (*h.stopRuns)[0][0] != "stop-tunnel" {
		t.Fatalf("stop command must run once: %v", *h.stopRuns)
	}
	if a.get("/security").Code != http.StatusSeeOther || b.get("/security").Code != http.StatusSeeOther {
		t.Fatal("every session must be invalidated")
	}
	if n, _ := h.srv.Sessions.Count(); n != 0 {
		t.Fatalf("sessions left: %d", n)
	}

	// Logins are refused, even with correct credentials and a valid CSRF token.
	c := h.client()
	page := c.get("/login")
	if !strings.Contains(page.body, "Kill switch is on") || strings.Contains(page.body, `name="password"`) {
		t.Fatal("login page must say the kill switch is on and offer no form")
	}
	// The page offers no form, but the pre-login cookie still exists: a script could post with it.
	r = c.postForm("/login", url.Values{"_csrf": {c.cookie("maestria_login_csrf")}, "password": {testPassword}}, nil)
	if r.Code != http.StatusForbidden || !strings.Contains(r.body, "Kill switch is on") {
		t.Fatalf("login while killed: %d", r.Code)
	}
	if r.Header().Get("Set-Cookie") != "" && strings.Contains(r.Header().Get("Set-Cookie"), "maestria_pending=") {
		t.Fatal("no pending login while killed")
	}

	// No HTTP route can turn it off: every plausible path and method is refused.
	for _, p := range []string{"/tunnel/on", "/api/tunnel/on", "/api/tunnel", "/kill-switch/off", "/api/kill-switch/off", "/enable", "/api/enable"} {
		for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
			rr := c.do(m, p, strings.NewReader("{}"), map[string]string{"Content-Type": "application/json"})
			if rr.Code == http.StatusOK {
				t.Errorf("%s %s answered 200 while killed", m, p)
			}
		}
	}
	if !h.srv.Tunnel.Killed() {
		t.Fatal("kill switch must still be on")
	}

	// Only the local path (the CLI's EnableLocal) turns it off.
	en, err := h.srv.Tunnel.EnableLocal(context.Background())
	if err != nil || !en.WasKilled {
		t.Fatalf("local enable: %+v %v", en, err)
	}
	if len(*h.stopRuns) != 2 || (*h.stopRuns)[1][0] != "start-tunnel" {
		t.Fatalf("start command: %v", *h.stopRuns)
	}
	c.login()
}

func TestKillSwitchJSONAPI(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	token := c.login()
	r := c.do(http.MethodPost, "/api/kill-switch", strings.NewReader("{}"), map[string]string{"Content-Type": "application/json", "X-CSRF-Token": token})
	if r.Code != http.StatusOK {
		t.Fatalf("api kill: %d %s", r.Code, r.body)
	}
	var out struct {
		OK                  bool `json:"ok"`
		SessionsInvalidated int  `json:"sessionsInvalidated"`
	}
	if err := json.Unmarshal([]byte(r.body), &out); err != nil || !out.OK || out.SessionsInvalidated != 1 {
		t.Fatalf("response: %s", r.body)
	}
	if !h.srv.Tunnel.Killed() || len(*h.stopRuns) != 1 {
		t.Fatal("tunnel must be stopped")
	}
}

func TestKillSwitchAndEveryAuthenticatedWriteAreAudited(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	token := c.login()
	c.postForm("/logout-all", url.Values{"_csrf": {token}}, nil)
	c2 := h.client()
	token2 := c2.login()
	c2.postForm("/kill-switch", url.Values{"_csrf": {token2}}, nil)

	entries, _ := h.srv.Audit.Recent(100)
	counts := map[string]int{}
	var kill string
	for _, e := range entries {
		counts[e.Event]++
		if e.Event == "kill_switch" {
			kill = e.Detail
		}
	}
	for _, ev := range []string{"login_password_ok", "login_ok", "logout_all", "kill_switch", "write"} {
		if counts[ev] == 0 {
			t.Errorf("missing audit event %q (have %v)", ev, counts)
		}
	}
	if counts["write"] != 2 {
		t.Errorf("every authenticated write must be audited: %d", counts["write"])
	}
	if !strings.Contains(kill, `"via":"web"`) || !strings.Contains(kill, `"sessionsInvalidated":1`) || !strings.Contains(kill, `"stopExecuted":true`) {
		t.Fatalf("kill switch audit detail: %s", kill)
	}
}

// ---- Cloudflare Access gate ----

func TestCloudflareAccessGate(t *testing.T) {
	const team, aud = "team.cloudflareaccess.com", "abcdef0123456789abcdef0123456789"
	key := authtest.NewKey(t, "k1")
	clock := time.UnixMilli(1_700_000_000_000)
	verifier := auth.NewAccessVerifier(team, aud, key.Fetcher(), func() time.Time { return clock })
	h := newHarness(t, opts{access: verifier, mutate: func(c *config.Config) {
		c.CloudflareAccess = &config.CloudflareAccess{TeamDomain: team, Audience: aud}
	}})
	c := h.client()

	for _, p := range []string{"/login", "/static/app.js", "/api/session"} {
		if r := c.get(p); r.Code != http.StatusForbidden {
			t.Errorf("%s without a JWT: %d", p, r.Code)
		}
	}
	if r := c.do(http.MethodGet, "/login", nil, map[string]string{"Cf-Access-Jwt-Assertion": "a.b.c"}); r.Code != http.StatusForbidden {
		t.Fatalf("garbage JWT: %d", r.Code)
	}
	if r := c.do(http.MethodPost, "/login", strings.NewReader("password=x"), map[string]string{"Content-Type": "application/x-www-form-urlencoded"}); r.Code != http.StatusForbidden {
		t.Fatalf("POST without a JWT: %d", r.Code)
	}
	good := key.Token(t, team, aud, clock.Add(5*time.Minute))
	if r := c.do(http.MethodGet, "/login", nil, map[string]string{"Cf-Access-Jwt-Assertion": good}); r.Code != http.StatusOK {
		t.Fatalf("valid JWT: %d", r.Code)
	}
	wrongAud := key.Token(t, team, "ffff0000ffff0000", clock.Add(5*time.Minute))
	if r := c.do(http.MethodGet, "/login", nil, map[string]string{"Cf-Access-Jwt-Assertion": wrongAud}); r.Code != http.StatusForbidden {
		t.Fatalf("wrong audience: %d", r.Code)
	}
	expired := key.Token(t, team, aud, clock.Add(-time.Minute))
	if r := c.do(http.MethodGet, "/login", nil, map[string]string{"Cf-Access-Jwt-Assertion": expired}); r.Code != http.StatusForbidden {
		t.Fatalf("expired: %d", r.Code)
	}
	entries, _ := h.srv.Audit.Recent(20)
	seen := false
	for _, e := range entries {
		seen = seen || e.Event == "access_jwt_reject"
	}
	if !seen {
		t.Fatal("rejections must be audited")
	}
}

// ---- input limits, static files ----

func TestOversizedBodiesAreRejected(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	page := c.get("/login")
	big := url.Values{"_csrf": {csrfFrom(t, page)}, "password": {strings.Repeat("a", 20*1024)}}
	if r := c.postForm("/login", big, nil); r.Code == http.StatusSeeOther || r.Code == http.StatusOK {
		t.Fatalf("oversized form: %d", r.Code)
	}
	token := c.login()
	huge := strings.NewReader(`{"x":"` + strings.Repeat("a", 64*1024) + `"}`)
	r := c.do(http.MethodPost, "/api/kill-switch", huge, map[string]string{"Content-Type": "application/json", "X-CSRF-Token": token})
	if r.Code != http.StatusOK {
		// The kill switch ignores the body, so it never reads it; an oversized body must not make it misbehave.
		t.Logf("oversized JSON body answered %d", r.Code)
	}
}

func TestStaticFilesAreWhitelisted(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	for _, name := range []string{"tokens.css", "base.css", "components.css", "shell.css", "app.js", "icons.svg"} {
		r := c.get("/static/" + name)
		if r.Code != http.StatusOK || len(r.body) == 0 {
			t.Errorf("%s: %d", name, r.Code)
		}
	}
	if ct := c.get("/static/app.js").Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("app.js content type %q", ct)
	}
	for _, p := range []string{
		"/static/../go.mod", "/static/..%2fgo.mod", "/static/%2e%2e/config.go", "/static/ui.go", "/static/templates/login.html",
		"/static/.env", "/static/", "/static", "/static/login.html", "/static/%00",
	} {
		if r := c.get(p); r.Code == http.StatusOK {
			t.Errorf("%s served: %s", p, r.body[:min(len(r.body), 60)])
		}
	}
	// Versioned URLs are cached; unversioned ones must revalidate.
	page := c.get("/login").body
	m := regexp.MustCompile(`/static/app\.js\?v=[0-9a-f]+`).FindString(page)
	if m == "" {
		t.Fatal("pages must reference cache-busted asset URLs")
	}
	if cc := c.get(m).Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("versioned asset Cache-Control %q", cc)
	}
	if cc := c.get("/static/app.js").Header().Get("Cache-Control"); strings.Contains(cc, "immutable") {
		t.Errorf("unversioned asset must not be immutable: %q", cc)
	}
}

// ---- loopback-only bind ----

func TestListenRefusesAnythingButLoopback(t *testing.T) {
	for _, host := range []string{"0.0.0.0", "::", "::1", "", "localhost", "192.168.1.10", "127.0.0.2", "example.com", "127.0.0.1 ", "[::]"} {
		if ln, err := Listen(host, 0); err == nil {
			ln.Close()
			t.Errorf("Listen(%q) must be refused", host)
		}
	}
	ln, err := Listen("127.0.0.1", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	tcp := ln.Addr().(*net.TCPAddr)
	if !tcp.IP.Equal(net.IPv4(127, 0, 0, 1)) || tcp.Port == 0 {
		t.Fatalf("addr: %v", tcp)
	}
	if _, err := Listen("127.0.0.1", 70000); err == nil {
		t.Fatal("invalid port")
	}
}

func TestServerAnswersOverARealSocket(t *testing.T) {
	h := newHarness(t, opts{mutate: func(c *config.Config) { c.Port = 0 }})
	ln, err := Listen("127.0.0.1", 0)
	if err != nil {
		t.Fatal(err)
	}
	// Allow the ephemeral port as a Host.
	h.srv.hosts[ln.Addr().String()] = true
	hs := h.srv.HTTPServer()
	go hs.Serve(ln)
	defer hs.Close()

	res, err := http.Get("http://" + ln.Addr().String() + "/login")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || !strings.Contains(string(body), "Log in") || res.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("real socket: %d", res.StatusCode)
	}
}
