package cloud

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
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

	"github.com/paulo-lucas/maestria/internal/auth"
)

func TestMain(m *testing.M) {
	// Cheap argon2 for tests: the code under test is the flow, not the cost parameters (which
	// internal/auth tests cover).
	auth.PasswordParams = auth.Argon2Params{MemoryKiB: 64, Iterations: 1, Parallelism: 1, KeyLen: 32}
	auth.RecoveryParams = auth.Argon2Params{MemoryKiB: 64, Iterations: 1, Parallelism: 1, KeyLen: 32}
	m.Run()
}

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

type captureMailer struct {
	mu   sync.Mutex
	msgs []Message
}

func (c *captureMailer) Send(m Message) error {
	c.mu.Lock()
	c.msgs = append(c.msgs, m)
	c.mu.Unlock()
	return nil
}

func (c *captureMailer) all() []Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Message(nil), c.msgs...)
}

type bed struct {
	t      *testing.T
	srv    *Server
	ts     *httptest.Server
	cfg    *Config
	clock  *fakeClock
	mail   *captureMailer
	logbuf *syncBuf
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func newBed(t *testing.T, mut ...func(*Config)) *bed {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Defaults()
	cfg.DataDir = t.TempDir()
	cfg.Listen = ln.Addr().String()
	cfg.PublicURL = "http://" + ln.Addr().String()
	cfg.TrustProxy, cfg.ProxyHops = "xff", 1
	cfg.Signup = SignupOpen // the flows under test are not about invitations; invite tests set their own mode
	for _, m := range mut {
		m(cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	db, err := OpenDB(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	b := &bed{t: t, cfg: cfg, mail: &captureMailer{}, logbuf: &syncBuf{}, clock: &fakeClock{t: time.Unix(1_800_000_000, 0)}}
	log := slog.New(slog.NewTextHandler(b.logbuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv, err := New(Deps{Config: cfg, DB: db, Mailer: b.mail, Log: log, Now: b.clock.now})
	if err != nil {
		t.Fatal(err)
	}
	b.srv = srv
	b.ts = &httptest.Server{Listener: ln, Config: &http.Server{Handler: srv}}
	b.ts.Start()
	t.Cleanup(func() { b.ts.Close(); srv.Close(); db.Close() })
	return b
}

// waitMail returns the newest email to addr whose subject contains subject.
func (b *bed) waitMail(to, subject string) Message {
	b.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		msgs := b.mail.all()
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].To == to && strings.Contains(msgs[i].Subject, subject) {
				return msgs[i]
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	b.t.Fatalf("no email to %s about %q (have %d)", to, subject, len(b.mail.all()))
	return Message{}
}

func (b *bed) noMail(to string) {
	b.t.Helper()
	time.Sleep(60 * time.Millisecond)
	for _, m := range b.mail.all() {
		if m.To == to {
			b.t.Fatalf("unexpected email to %s: %s", to, m.Subject)
		}
	}
}

var linkRE = regexp.MustCompile(`https?://[^\s]+/(verify|reset)/([A-Za-z0-9_-]+)`)

func (b *bed) link(m Message) (path string) {
	b.t.Helper()
	mm := linkRE.FindStringSubmatch(m.Body)
	if mm == nil {
		b.t.Fatalf("no link in %q", m.Body)
	}
	return "/" + mm[1] + "/" + mm[2]
}

// browser is a cookie-keeping client that behaves like a browser for same-origin requests.
type browser struct {
	b    *bed
	c    *http.Client
	ip   string
	csrf string
}

func (b *bed) browser(ip string) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{b: b, ip: ip, c: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

type resp struct {
	*http.Response
	body string
}

func (br *browser) do(req *http.Request) *resp {
	br.b.t.Helper()
	req.Header.Set("X-Forwarded-For", br.ip)
	r, err := br.c.Do(req)
	if err != nil {
		br.b.t.Fatal(err)
	}
	defer r.Body.Close()
	data, _ := io.ReadAll(r.Body)
	return &resp{Response: r, body: string(data)}
}

func (br *browser) get(path string) *resp {
	br.b.t.Helper()
	req, _ := http.NewRequest("GET", br.b.ts.URL+path, nil)
	return br.do(req)
}

func (br *browser) cookie(name string) string {
	u, _ := url.Parse(br.b.ts.URL)
	for _, c := range br.c.Jar.Cookies(u) {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

// form posts a pre-login form with the login CSRF cookie's token.
func (br *browser) form(path string, v url.Values) *resp {
	br.b.t.Helper()
	if v.Get("_csrf") == "" {
		v.Set("_csrf", br.cookie("maestria_login_csrf"))
	}
	req, _ := http.NewRequest("POST", br.b.ts.URL+path, strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", br.b.ts.URL)
	return br.do(req)
}

// api calls the JSON API with the session CSRF token.
func (br *browser) api(method, path string, body any) *resp {
	br.b.t.Helper()
	if br.csrf == "" && path != "/api/me" {
		var me struct {
			CSRF string `json:"csrfToken"`
		}
		_ = json.Unmarshal([]byte(br.get("/api/me").body), &me)
		br.csrf = me.CSRF
	}
	var rd io.Reader
	if body != nil {
		bs, _ := json.Marshal(body)
		rd = bytes.NewReader(bs)
	}
	req, _ := http.NewRequest(method, br.b.ts.URL+path, rd)
	req.Header.Set("Origin", br.b.ts.URL)
	req.Header.Set("X-CSRF-Token", br.csrf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return br.do(req)
}

var secretRE = regexp.MustCompile(`<code>([A-Z2-7 ]{20,})</code>`)
var codesRE = regexp.MustCompile(`<li>([a-z2-9]{4}-[a-z2-9]{4})</li>`)

// signup runs the whole sign-up and returns the TOTP secret and recovery codes. The browser ends
// up signed in.
func (br *browser) signup(email, password string) (secret string, codes []string) {
	br.b.t.Helper()
	t := br.b.t
	br.get("/signup")
	if r := br.form("/signup", url.Values{"email": {email}, "password": {password}}); r.StatusCode != 200 {
		t.Fatalf("signup: %d %s", r.StatusCode, r.body)
	}
	link := br.b.link(br.b.waitMail(email, "Verify"))
	if r := br.get(link); r.StatusCode != 200 {
		t.Fatalf("verify page: %d", r.StatusCode)
	}
	if r := br.form(link, url.Values{"password": {password}}); r.StatusCode != 303 {
		t.Fatalf("verify: %d %s", r.StatusCode, r.body)
	}
	page := br.get("/signup/2fa")
	m := secretRE.FindStringSubmatch(page.body)
	if m == nil {
		t.Fatalf("no TOTP key on the setup page: %s", page.body)
	}
	secret = strings.ReplaceAll(m[1], " ", "")
	code, _ := auth.TOTP(secret, br.b.clock.now())
	r := br.form("/signup/2fa", url.Values{"code": {code}})
	if r.StatusCode != 200 {
		t.Fatalf("2fa setup: %d %s", r.StatusCode, r.body)
	}
	for _, mm := range codesRE.FindAllStringSubmatch(r.body, -1) {
		codes = append(codes, mm[1])
	}
	if len(codes) != auth.RecoveryCodeCount {
		t.Fatalf("recovery codes: %v", codes)
	}
	br.b.clock.advance(31 * time.Second) // next TOTP step
	return secret, codes
}

// login runs both steps with a TOTP code and reports whether it ended signed in.
func (br *browser) login(email, password, secret string) bool {
	br.b.t.Helper()
	br.get("/login")
	r := br.form("/login", url.Values{"email": {email}, "password": {password}})
	if r.StatusCode != 303 {
		return false
	}
	br.get("/login/2fa")
	code, _ := auth.TOTP(secret, br.b.clock.now())
	r = br.form("/login/2fa", url.Values{"code": {code}})
	br.b.clock.advance(31 * time.Second)
	return r.StatusCode == 303 && r.Header.Get("Location") == "/" && br.cookie("maestria_session") != ""
}

func (br *browser) signedIn() bool { return br.api("GET", "/api/me", nil).StatusCode == 200 }

const testPassword = "correct horse battery staple"

// newServer starts an http.Server on a free port and returns its base URL.
func newServer(t *testing.T, s *http.Server) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(ln)
	t.Cleanup(func() { s.Close() })
	return "http://" + ln.Addr().String()
}
