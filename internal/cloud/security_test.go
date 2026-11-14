package cloud

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestHostHeaderIsChecked(t *testing.T) {
	b := newBed(t)
	for _, host := range []string{"evil.example", "evil.example:80", b.ts.Listener.Addr().String() + ".evil.example"} {
		req, _ := http.NewRequest("GET", b.ts.URL+"/login", nil)
		req.Host = host
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			continue // an empty Host is refused by net/http itself
		}
		res.Body.Close()
		if res.StatusCode != http.StatusMisdirectedRequest {
			t.Errorf("Host %q: %d", host, res.StatusCode)
		}
	}
	res, _ := http.Get(b.ts.URL + "/login")
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("own host: %d", res.StatusCode)
	}
	// Health checks come from proxies with their own Host.
	req, _ := http.NewRequest("GET", b.ts.URL+"/healthz", nil)
	req.Host = "internal"
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != 200 {
		t.Fatalf("healthz: %d", res.StatusCode)
	}
}

func TestSecurityHeadersAndNoCORS(t *testing.T) {
	b := newBed(t)
	for _, p := range []string{"/login", "/signup", "/static/tokens.css", "/nope", "/api/me"} {
		res, err := http.Get(b.ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		csp := res.Header.Get("Content-Security-Policy")
		for _, must := range []string{"default-src 'none'", "script-src 'self'", "style-src 'self'", "frame-ancestors 'none'", "base-uri 'none'"} {
			if !strings.Contains(csp, must) {
				t.Errorf("%s: CSP lacks %q: %s", p, must, csp)
			}
		}
		if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
			t.Errorf("%s: CSP allows inline code: %s", p, csp)
		}
		for k := range res.Header {
			if strings.HasPrefix(strings.ToLower(k), "access-control-") {
				t.Errorf("%s: CORS header %s", p, k)
			}
		}
		if res.Header.Get("X-Frame-Options") != "DENY" || res.Header.Get("X-Content-Type-Options") != "nosniff" || res.Header.Get("Referrer-Policy") != "same-origin" {
			t.Errorf("%s: missing headers %v", p, res.Header)
		}
	}
	// A cross-origin preflight is not answered with permission.
	req, _ := http.NewRequest("OPTIONS", b.ts.URL+"/api/me", nil)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Access-Control-Request-Method", "GET")
	res, _ := http.DefaultClient.Do(req)
	res.Body.Close()
	if res.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("no CORS, ever")
	}
}

func TestPagesHaveNoInlineScriptOrStyle(t *testing.T) {
	b := newBed(t)
	br := b.browser("10.0.0.1")
	br.signup("ann@example.com", testPassword)
	for _, p := range []string{"/login", "/signup", "/reset", "/signup/continue", "/", "/connectors", "/connectors/link", "/security", "/pair"} {
		r := br.get(p)
		if r.StatusCode != 200 && r.StatusCode != 303 {
			t.Errorf("%s: %d", p, r.StatusCode)
			continue
		}
		low := strings.ToLower(r.body)
		for _, bad := range []string{"<script>", "style=\"", "onclick=", "onload=", "href=\"javascript:"} {
			if strings.Contains(low, bad) {
				t.Errorf("%s contains %q", p, bad)
			}
		}
	}
}

func TestCSRFAndOriginOnEveryWrite(t *testing.T) {
	b := newBed(t)
	br := b.browser("10.0.0.1")
	br.signup("ann@example.com", testPassword)
	br.api("GET", "/api/connectors", nil)
	good := br.csrf

	post := func(path, origin, token, fetchSite string) int {
		req, _ := http.NewRequest("POST", b.ts.URL+path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", br.ip)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if token != "" {
			req.Header.Set("X-CSRF-Token", token)
		}
		if fetchSite != "" {
			req.Header.Set("Sec-Fetch-Site", fetchSite)
		}
		res, err := br.c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	writes := []string{"/api/link/codes", "/api/link/claim", "/api/pair", "/api/connectors/cn_x/rename", "/api/connectors/cn_x/revoke", "/c/cn_x/api/standby/resume", "/logout"}
	for _, p := range writes {
		if c := post(p, b.ts.URL, "", ""); c != 403 {
			t.Errorf("%s without a CSRF token: %d", p, c)
		}
		if c := post(p, b.ts.URL, "x"+good, ""); c != 403 {
			t.Errorf("%s with a wrong CSRF token: %d", p, c)
		}
		if c := post(p, "", good, ""); c != 403 {
			t.Errorf("%s without Origin: %d", p, c)
		}
		if c := post(p, "https://evil.example", good, ""); c != 403 {
			t.Errorf("%s from another origin: %d", p, c)
		}
		if c := post(p, b.ts.URL, good, "cross-site"); c != 403 {
			t.Errorf("%s with Sec-Fetch-Site cross-site: %d", p, c)
		}
		if c := post(p, b.ts.URL, good, ""); c == 403 || c == 401 {
			t.Errorf("%s with everything right was refused by the gate: %d", p, c)
		}
	}
	// Pre-login forms too.
	anon := b.browser("10.0.0.2")
	anon.get("/login")
	req, _ := http.NewRequest("POST", b.ts.URL+"/login", strings.NewReader(url.Values{"email": {"a@b.co"}, "password": {"x"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", b.ts.URL)
	if r := anon.do(req); r.StatusCode != 403 {
		t.Fatalf("login form without its token: %d", r.StatusCode)
	}
	if r := anon.form("/login", url.Values{"email": {"a@b.co"}, "password": {"x"}, "_csrf": {"forged"}}); r.StatusCode != 403 {
		t.Fatalf("login form with a forged token: %d", r.StatusCode)
	}
	// A session cookie alone (the CSRF-style attack) does nothing without the token.
	if c := post("/api/pair", "https://evil.example", "", ""); c != 403 {
		t.Fatalf("cross-site write with only cookies: %d", c)
	}
	// Reads need a session.
	if r := b.browser("10.0.0.3").get("/api/connectors"); r.StatusCode != 401 {
		t.Fatalf("anonymous read: %d", r.StatusCode)
	}
	if r := b.browser("10.0.0.3").get("/connectors"); r.StatusCode != 303 || r.Header.Get("Location") != "/login" {
		t.Fatalf("anonymous page: %d", r.StatusCode)
	}
}

func TestCookiesAreHardened(t *testing.T) {
	b := newBed(t, func(c *Config) {
		c.PublicURL = "https://cloud.example"
		c.AllowedHosts = []string{} // the test client's Host is added below
	})
	b.srv.hosts[strings.ToLower(b.ts.Listener.Addr().String())] = true
	res, _ := http.Get(b.ts.URL + "/login")
	res.Body.Close()
	if res.Header.Get("Strict-Transport-Security") == "" {
		t.Error("HSTS missing on an https deployment")
	}
	found := false
	for _, c := range res.Cookies() {
		found = true
		if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || !strings.HasPrefix(c.Name, "__Host-") || c.Path != "/" || c.Domain != "" {
			t.Errorf("cookie %s: httpOnly=%v secure=%v sameSite=%v path=%q domain=%q", c.Name, c.HttpOnly, c.Secure, c.SameSite, c.Path, c.Domain)
		}
	}
	if !found {
		t.Error("login page set no CSRF cookie")
	}
}

func TestProxyHeadersOnlyWhenTrusted(t *testing.T) {
	s := &Server{cfg: &Config{TrustProxy: ""}}
	req, _ := http.NewRequest("GET", "/", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	req.Header.Set("X-Forwarded-For", "6.6.6.6")
	req.Header.Set("Cf-Connecting-Ip", "7.7.7.7")
	if got := s.clientIP(req); got != "203.0.113.9" {
		t.Fatalf("untrusted: %s", got)
	}
	s.cfg.TrustProxy, s.cfg.ProxyHops = "xff", 1
	req.Header.Set("X-Forwarded-For", "6.6.6.6, 198.51.100.4")
	if got := s.clientIP(req); got != "198.51.100.4" {
		t.Fatalf("xff must read from the right (a client can prepend anything): %s", got)
	}
	s.cfg.ProxyHops = 2
	if got := s.clientIP(req); got != "6.6.6.6" {
		t.Fatalf("two hops: %s", got)
	}
	s.cfg.TrustProxy = "cf"
	if got := s.clientIP(req); got != "7.7.7.7" {
		t.Fatalf("cf: %s", got)
	}
	req.Header.Set("Cf-Connecting-Ip", "not an ip")
	if got := s.clientIP(req); got != "203.0.113.9" {
		t.Fatalf("garbage header: %s", got)
	}
}

func TestStaticIsAWhitelist(t *testing.T) {
	b := newBed(t)
	for p, want := range map[string]int{"/static/tokens.css": 200, "/static/hosted.js": 200, "/static/auth.js": 200, "/static/..%2fcloud.db": 404, "/static/nope.js": 404, "/static/server.key": 404, "/static/config.json": 404} {
		res, err := http.Get(b.ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != want {
			t.Errorf("%s: %d, want %d", p, res.StatusCode, want)
		}
	}
}

func TestRelayRejectsOddPathsBeforeSending(t *testing.T) {
	f := newFixture(t)
	f.dev.Now = f.a.clock.now
	for _, p := range []string{"/c/" + f.a.link.ConnectorID + "/api/../login", "/c/" + f.a.link.ConnectorID + "/api/%2e%2e/login", "/c/" + f.a.link.ConnectorID + "//api/inbox", "/c/" + f.a.link.ConnectorID + "/api/inbox%2f..%2f"} {
		req, _ := http.NewRequest("GET", f.b.ts.URL+p, nil)
		req.URL.Path = p // keep it raw
		req.URL.RawPath = p
		r := f.br.do(req)
		if r.StatusCode == 200 {
			t.Errorf("%s was relayed", p)
		}
	}
	// Methods other than GET and POST never reach a connector.
	for _, m := range []string{"PUT", "DELETE", "PATCH"} {
		req, _ := http.NewRequest(m, f.b.ts.URL+"/c/"+f.a.link.ConnectorID+"/api/workspaces", strings.NewReader("{}"))
		req.Header.Set("Origin", f.b.ts.URL)
		f.br.api("GET", "/api/connectors", nil)
		req.Header.Set("X-CSRF-Token", f.br.csrf)
		if r := f.br.do(req); r.StatusCode != 405 {
			t.Errorf("%s: %d", m, r.StatusCode)
		}
	}
}
