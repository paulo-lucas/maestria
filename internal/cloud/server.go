package cloud

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/paulo-lucas/maestria/internal/audit"
	"github.com/paulo-lucas/maestria/internal/auth"
	"github.com/paulo-lucas/maestria/internal/relay"
)

// CSP is sent on every page: no inline script or style, no framing, no foreign connections.
var CSP = strings.Join([]string{
	"default-src 'none'",
	"script-src 'self'",
	"style-src 'self'",
	"img-src 'self' data:",
	"connect-src 'self'",
	"font-src 'self'",
	"form-action 'self'",
	"frame-ancestors 'none'",
	"base-uri 'none'",
}, "; ")

const (
	maxJSONBody = 32 * 1024
	maxFormBody = 8 * 1024
)

// Deps are the server's collaborators.
type Deps struct {
	Config *Config
	DB     *sql.DB
	Mailer Mailer
	Log    *slog.Logger
	Now    func() time.Time
	// Sealer encrypts TOTP secrets. nil loads or creates the key in the data dir.
	Sealer *Sealer
	// Version is this server's build (from -ldflags), shown in the sidebar footer.
	Version string
	// Releases says which release is newest, from a cache (no network on a page view). nil means
	// unknown, so no machine is ever reported as behind.
	Releases Releases
}

// Server is the cloud's http.Handler.
type Server struct {
	cfg      *Config
	db       *sql.DB
	log      *slog.Logger
	now      func() time.Time
	audit    *audit.Log
	acc      *Accounts
	cons     *Connectors
	hub      *Hub
	ui       *cloudUI
	mail     *mailQueue
	mux      *http.ServeMux
	hosts    map[string]bool
	rate     *rateLimiter
	version  string
	releases Releases

	sessionCookie, pendingCookie, csrfCookie, setupCookie string

	bg sync.WaitGroup
}

// New builds the server. It does not listen.
func New(d Deps) (*Server, error) {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if d.Mailer == nil {
		return nil, errors.New("cloud: a mailer is required")
	}
	sealer := d.Sealer
	if sealer == nil {
		var err error
		if sealer, err = LoadOrCreateSealer(d.Config.DataDir); err != nil {
			return nil, err
		}
	}
	u, err := newCloudUI()
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: d.Config, db: d.DB, log: d.Log, now: d.Now, audit: audit.New(d.DB, d.Now), ui: u, mux: http.NewServeMux(), rate: newRateLimiter(d.Now), version: d.Version, releases: d.Releases}
	s.mail = newMailQueue(d.Mailer, d.Log)
	s.acc = NewAccounts(d.DB, sealer, s.audit, d.Config, s.mail.Enqueue, d.Now)
	s.cons = &Connectors{a: s.acc}
	s.hub = NewHub(s.cons, d.Log, d.Now)

	s.hosts = d.Config.Hosts()
	if len(s.hosts) == 0 || relayLoopback(d.Config.PublicURL) {
		if h, p, err := net.SplitHostPort(d.Config.Listen); err == nil {
			s.hosts[strings.ToLower(net.JoinHostPort(h, p))] = true
			s.hosts["localhost:"+p] = true
			s.hosts["127.0.0.1:"+p] = true
		}
	}
	prefix := ""
	if d.Config.Secure() {
		prefix = "__Host-"
	}
	s.sessionCookie, s.pendingCookie, s.csrfCookie, s.setupCookie = prefix+"maestria_session", prefix+"maestria_pending", prefix+"maestria_login_csrf", prefix+"maestria_setup"
	s.routes()
	return s, nil
}

func relayLoopback(publicURL string) bool {
	u, err := url.Parse(publicURL)
	return publicURL == "" || (err == nil && relay.IsLoopbackHost(u.Hostname()))
}

// Accounts exposes the account store (admin commands, tests).
func (s *Server) Accounts() *Accounts { return s.acc }

// Connectors exposes the connector store (tests).
func (s *Server) Connectors() *Connectors { return s.cons }

// Hub exposes the hub (tests).
func (s *Server) Hub() *Hub { return s.hub }

// Audit exposes the audit log.
func (s *Server) Audit() *audit.Log { return s.audit }

// Close stops background work.
func (s *Server) Close() {
	s.mail.Close()
}

// StartBackground runs periodic cleanup until ctx ends.
func (s *Server) StartBackground(ctx context.Context) {
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		purge := time.NewTicker(10 * time.Minute)
		sweep := time.NewTicker(15 * time.Second)
		defer purge.Stop()
		defer sweep.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-purge.C:
				s.acc.Purge()
			case <-sweep.C:
				s.hub.Sweep()
			}
		}
	}()
}

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	m.HandleFunc("GET /static/{file}", func(w http.ResponseWriter, r *http.Request) { s.ui.ServeStatic(w, r, r.PathValue("file")) })

	// Account pages.
	m.HandleFunc("GET /signup", s.getSignup)
	m.HandleFunc("POST /signup", s.postSignup)
	m.HandleFunc("POST /signup/resend", s.postResend)
	m.HandleFunc("GET /signup/continue", s.getContinue)
	m.HandleFunc("POST /signup/continue", s.postContinue)
	m.HandleFunc("GET /signup/2fa", s.getSetup2FA)
	m.HandleFunc("POST /signup/2fa", s.postSetup2FA)
	m.HandleFunc("GET /verify/{token}", s.getVerify)
	m.HandleFunc("POST /verify/{token}", s.postVerify)
	m.HandleFunc("GET /login", s.getLogin)
	m.HandleFunc("POST /login", s.postLogin)
	m.HandleFunc("GET /login/2fa", s.getLogin2FA)
	m.HandleFunc("POST /login/2fa", s.postLogin2FA)
	m.HandleFunc("POST /login/recovery", s.postLoginRecovery)
	m.HandleFunc("GET /reset", s.getResetRequest)
	m.HandleFunc("POST /reset", s.postResetRequest)
	m.HandleFunc("GET /reset/{token}", s.getReset)
	m.HandleFunc("POST /reset/{token}", s.postReset)
	m.HandleFunc("POST /logout", s.postLogout)

	// App pages: the same shell, the app routes on the client.
	for _, p := range []string{"/{$}", "/connectors", "/connectors/link", "/admin/invites", "/security", "/pair", "/usage", "/insights", "/workspaces/{rest...}", "/c/{id}/workspaces/{rest...}", "/c/{id}/usage", "/c/{id}/insights"} {
		m.HandleFunc("GET "+p, s.getApp)
	}

	// JSON API (browser).
	m.HandleFunc("GET /api/me", s.apiMe)
	m.HandleFunc("GET /api/connectors", s.apiConnectors)
	m.HandleFunc("POST /api/connectors/{id}/rename", s.apiRename)
	m.HandleFunc("POST /api/connectors/{id}/revoke", s.apiRevoke)
	m.HandleFunc("POST /api/link/codes", s.apiLinkCode)
	m.HandleFunc("GET /api/link/codes/{id}", s.apiLinkCodeStatus)
	m.HandleFunc("POST /api/link/claim", s.apiLinkClaim)
	m.HandleFunc("POST /api/invites", s.apiInviteCreate)
	m.HandleFunc("POST /api/invites/{id}/revoke", s.apiInviteRevoke)
	m.HandleFunc("POST /api/pair", s.apiPairStart)
	m.HandleFunc("GET /api/pair/{id}", s.apiPairGet)
	m.HandleFunc("POST /api/pair/{id}/reveal", s.apiPairReveal)
	m.HandleFunc("GET /api/presence", s.apiPresence)

	// JSON API (connector, no cookies).
	m.HandleFunc("POST /api/link/start", s.apiLinkStart)
	m.HandleFunc("POST /api/link/poll", s.apiLinkPoll)
	m.HandleFunc("POST /api/link/redeem", s.apiLinkRedeem)
	m.HandleFunc("POST /api/link/confirm", s.apiLinkConfirm)
	m.HandleFunc("DELETE /api/connector", s.apiUnlink)
	m.HandleFunc("GET /connector", func(w http.ResponseWriter, r *http.Request) {
		s.hub.ServeConnector(w, r, s.clientIP(r), func(event, ip string, detail map[string]any) { _ = s.audit.Write(event, ip, detail) })
	})

	// The relay.
	m.HandleFunc("GET /c/{id}/{path...}", s.relayHandler)
	m.HandleFunc("POST /c/{id}/{path...}", s.relayHandler)
}

type ctxKey int

const (
	sessKey ctxKey = 1
	ipKey   ctxKey = 2
)

func sessionFrom(r *http.Request) *Session { s, _ := r.Context().Value(sessKey).(*Session); return s }
func ipOf(r *http.Request) string          { ip, _ := r.Context().Value(ipKey).(string); return ip }

// routeClass decides which checks a request gets.
//
//	connector: called by the connector binary (no cookies, no CSRF, no Origin allowed)
//	public:    pre-login pages (login CSRF cookie)
//	app:       needs a session
func routeClass(method, path string) string {
	switch {
	case path == "/connector", path == "/healthz",
		path == "/api/link/start", path == "/api/link/poll", path == "/api/link/redeem", path == "/api/link/confirm", path == "/api/connector":
		return "connector"
	case strings.HasPrefix(path, "/static/"):
		return "public"
	}
	switch path {
	case "/signup", "/signup/resend", "/signup/continue", "/signup/2fa", "/login", "/login/2fa", "/login/recovery", "/reset":
		return "public"
	}
	if strings.HasPrefix(path, "/verify/") || strings.HasPrefix(path, "/reset/") {
		return "public"
	}
	return "app"
}

func isSafe(m string) bool { return m == http.MethodGet || m == http.MethodHead }

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(code int)        { w.status = code; w.ResponseWriter.WriteHeader(code) }
func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ServeHTTP runs the gate in a fixed order: security headers, Host allowlist, Origin and body
// limits, session, CSRF, route.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.setSecurityHeaders(w)
	ip := s.clientIP(r)
	class := routeClass(r.Method, r.URL.Path)

	if r.URL.Path != "/healthz" && !s.hosts[strings.ToLower(r.Host)] {
		deny(w, http.StatusMisdirectedRequest, "Misdirected request")
		return
	}

	if class == "connector" {
		// Not for browsers: a request with an Origin is a web page trying to reach it.
		if r.Header.Get("Origin") != "" {
			deny(w, http.StatusForbidden, "Forbidden")
			return
		}
		if !isSafe(r.Method) {
			r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
		}
		ctx := context.WithValue(r.Context(), ipKey, ip)
		s.mux.ServeHTTP(w, r.WithContext(ctx))
		return
	}

	if !isSafe(r.Method) {
		if !sameOrigin(r) {
			if s.audit.Allow("origin:all", 2*time.Second) {
				_ = s.audit.WriteThrottled("origin:"+ip, time.Minute, "origin_reject", ip, map[string]any{"path": clip(r.URL.Path, 100)})
			}
			deny(w, http.StatusForbidden, "Forbidden")
			return
		}
		limit := int64(maxJSONBody)
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
			limit = maxFormBody
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
	}

	var sess *Session
	if c, err := r.Cookie(s.sessionCookie); err == nil {
		var err error
		sess, err = s.acc.Validate(c.Value)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
	}

	if class == "app" && sess == nil {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/"), strings.HasPrefix(r.URL.Path, "/c/"):
			writeJSON(w, http.StatusUnauthorized, relay.ErrorBody{Error: "unauthenticated"})
		case r.Method == http.MethodGet:
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		default:
			deny(w, http.StatusUnauthorized, "Unauthorized")
		}
		return
	}

	if !isSafe(r.Method) && !s.csrfOK(r, sess, class) {
		// Anyone can cause this with a bad token and a matching Origin: throttled per address
		// and for the whole server.
		if s.audit.Allow("csrf:"+ip, time.Minute) && s.audit.Allow("csrf:all", 2*time.Second) {
			_ = s.audit.Write("csrf_reject", ip, map[string]any{"path": clip(r.URL.Path, 100)})
		}
		if class == "public" {
			s.csrfRejectPage(w, r)
			return
		}
		writeJSON(w, http.StatusForbidden, relay.ErrorBody{Error: "csrf"})
		return
	}

	ctx := context.WithValue(r.Context(), sessKey, sess)
	r = r.WithContext(context.WithValue(ctx, ipKey, ip))
	if sess != nil && !isSafe(r.Method) && !strings.HasPrefix(r.URL.Path, "/c/") {
		// Authenticated writes are audited: path and outcome, never bodies.
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		s.mux.ServeHTTP(rec, r)
		_ = s.audit.Write("write", ip, map[string]any{"user": sess.User.ID, "method": r.Method, "path": clip(r.URL.Path, 100), "status": rec.status})
		return
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", CSP)
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
	if s.cfg.Secure() {
		h.Set("Strict-Transport-Security", "max-age=31536000")
	}
	h.Set("Cache-Control", "no-store")
	// No CORS, ever: Access-Control-* headers are never set.
}

// clientIP is the connecting address, or the proxy-reported one when a proxy is trusted.
func (s *Server) clientIP(r *http.Request) string {
	switch s.cfg.TrustProxy {
	case "cf":
		if v := strings.TrimSpace(r.Header.Get("Cf-Connecting-Ip")); v != "" {
			if a, err := netip.ParseAddr(v); err == nil {
				return a.String()
			}
		}
	case "xff":
		parts := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
		i := len(parts) - s.cfg.ProxyHops
		if i >= 0 && i < len(parts) {
			if a, err := netip.ParseAddr(strings.TrimSpace(parts[i])); err == nil {
				return a.String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// sameOrigin is the cross-site write defence on top of CSRF tokens.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	fetchSite := r.Header.Get("Sec-Fetch-Site")
	if fetchSite != "" && fetchSite != "same-origin" {
		return false
	}
	switch origin {
	case "":
		return false
	case "null":
		return fetchSite == "same-origin"
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

func safeEqual(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// csrfOK checks the synchronizer token: the session's on app routes, the pre-login cookie's on
// public ones. It comes from X-CSRF-Token or the _csrf form field.
func (s *Server) csrfOK(r *http.Request, sess *Session, class string) bool {
	supplied := r.Header.Get("X-CSRF-Token")
	if supplied == "" && strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		if err := r.ParseForm(); err != nil {
			return false
		}
		supplied = r.PostForm.Get("_csrf")
	}
	expected := ""
	if class == "public" {
		if c, err := r.Cookie(s.csrfCookie); err == nil {
			expected = c.Value
		}
	} else if sess != nil {
		expected = sess.CSRFToken
	}
	return expected != "" && supplied != "" && safeEqual(expected, supplied)
}

// ---- cookies and responses ----

func (s *Server) setCookie(w http.ResponseWriter, name, value string, maxAge time.Duration) {
	c := &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: s.cfg.Secure(), SameSite: http.SameSiteStrictMode}
	if maxAge > 0 {
		c.MaxAge = int(maxAge.Seconds())
	}
	http.SetCookie(w, c)
}

func (s *Server) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.cfg.Secure(), SameSite: http.SameSiteStrictMode})
}

func deny(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(msg))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "path", r.URL.Path, "error", err.Error())
	_ = s.audit.WriteThrottled("err:"+r.URL.Path, time.Minute, "server_error", ipOf(r), map[string]any{"path": clip(r.URL.Path, 100)})
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeJSON(w, http.StatusInternalServerError, relay.ErrorBody{Error: "internal"})
		return
	}
	deny(w, http.StatusInternalServerError, "Internal error")
}

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

var _ = auth.RandomToken

// Releases is what the server needs to know about published releases: the newest one, from a
// cache. *update.Notifier satisfies it.
type Releases interface {
	LatestKnown() string
}
