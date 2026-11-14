// Package web is the panel's HTTP server: loopback-only listener, Host allowlist, optional
// Cloudflare Access check, Origin and CSRF checks on every write, security headers with a strict
// CSP, cookie-based sessions, and the JSON API under /api/.
package web

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/maestria-dev/maestria/internal/activity"
	"github.com/maestria-dev/maestria/internal/audit"
	"github.com/maestria-dev/maestria/internal/auth"
	"github.com/maestria-dev/maestria/internal/config"
	"github.com/maestria-dev/maestria/internal/docs"
	"github.com/maestria-dev/maestria/internal/floors"
	"github.com/maestria-dev/maestria/internal/tunnel"
	"github.com/maestria-dev/maestria/internal/ui"
)

// CSP is the Content-Security-Policy sent on every response: no inline script or style.
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
	Config   *config.Config
	DB       *sql.DB
	Secrets  *auth.Secrets
	Audit    *audit.Log
	Sessions *auth.SessionStore
	Limiter  *auth.LoginLimiter
	Tunnel   *tunnel.Controller
	// Access is the optional Cloudflare Access verifier.
	Access *auth.AccessVerifier
	// LiveFloors optionally enriches Present floors with live runtime data (e.g. Wire).
	LiveFloors floors.LiveSource
	// Activity is the Maestri Wire mirror and what the panel derives from it. nil means Wire is
	// not wired in: every Wire view then shows its "not connected" state.
	Activity *activity.Service
	// Env overrides OS environment lookup (used in tests).
	Env config.Env
	Now func() time.Time
}

// Server is the panel's http.Handler.
type Server struct {
	Deps
	ui    *ui.UI
	docs  *docs.Viewer
	auth  *auth.Authenticator
	mux   *http.ServeMux
	hosts map[string]bool
	env   config.Env
	cfgMu sync.RWMutex

	bg              sync.WaitGroup
	broker          *sseBroker
	ssePollInterval time.Duration

	sessionCookie string
	pendingCookie string
	csrfCookie    string
}

// New builds the server. It does not listen: see Listen.
func New(d Deps) (*Server, error) {
	if d.Now == nil {
		d.Now = time.Now
	}
	env := d.Env
	if env.Home == "" && env.Getenv == nil && env.GOOS == "" {
		env = config.OSEnv()
	}
	u, err := ui.New()
	if err != nil {
		return nil, err
	}
	s := &Server{
		Deps:   d,
		ui:     u,
		docs:   docs.NewViewer(),
		mux:    http.NewServeMux(),
		hosts:  map[string]bool{},
		env:    env,
		broker: newSSEBroker(),
	}
	if d.Activity != nil {
		d.Activity.SetOnChange(s.notifyStream)
		if d.LiveFloors == nil {
			s.LiveFloors = d.Activity
		}
	}
	port := strconv.Itoa(d.Config.Port)
	s.hosts["localhost:"+port] = true
	s.hosts["127.0.0.1:"+port] = true
	for _, h := range d.Config.AllowedHosts {
		s.hosts[h] = true
	}
	prefix := ""
	if d.Config.CookieSecure {
		prefix = "__Host-"
	}
	s.sessionCookie = prefix + "maestria_session"
	s.pendingCookie = prefix + "maestria_pending"
	s.csrfCookie = prefix + "maestria_login_csrf"
	s.auth = &auth.Authenticator{DB: d.DB, Secrets: d.Secrets, Sessions: d.Sessions, Limiter: d.Limiter, Audit: d.Audit, Now: d.Now}
	s.routes()
	return s, nil
}

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /static/{file}", func(w http.ResponseWriter, r *http.Request) { s.ui.ServeStatic(w, r, r.PathValue("file")) })

	m.HandleFunc("GET /login", s.getLogin)
	m.HandleFunc("POST /login", s.postLogin)
	m.HandleFunc("GET /login/2fa", s.getTwoFA)
	m.HandleFunc("POST /login/2fa", s.postTwoFA)
	m.HandleFunc("POST /login/recovery", s.postRecovery)
	m.HandleFunc("POST /logout", s.postLogout)
	m.HandleFunc("POST /logout-all", s.postLogoutAll)
	m.HandleFunc("POST /kill-switch", s.postKillSwitch)

	m.HandleFunc("GET /{$}", s.getOverview)
	m.HandleFunc("GET /workspaces/{id}/tasks", s.getWorkspaceTasks)
	m.HandleFunc("GET /workspaces/{id}/chat", s.getWorkspaceChat)
	m.HandleFunc("GET /workspaces/{id}/activity", s.getWorkspaceActivity)
	m.HandleFunc("GET /workspaces/{id}/floors", s.getWorkspaceFloors)
	m.HandleFunc("GET /workspaces/{id}/docs", s.getWorkspaceDocs)
	m.HandleFunc("GET /workspaces/{id}/docs/asset", s.getWorkspaceDocsAsset)
	m.HandleFunc("GET /usage", s.getUsage)
	m.HandleFunc("GET /security", s.getSecurity)

	m.HandleFunc("GET /api/session", s.apiSession)
	m.HandleFunc("GET /api/tunnel", s.apiTunnel)
	m.HandleFunc("POST /api/kill-switch", s.apiKillSwitch)
	m.HandleFunc("GET /api/audit", s.apiAudit)

	m.HandleFunc("GET /api/stream", s.apiStream)
	m.HandleFunc("GET /api/workspaces", s.apiWorkspaces)
	m.HandleFunc("GET /api/inbox", s.apiInbox)
	m.HandleFunc("POST /api/inbox/dismiss-notices", s.apiDismissNotices)
	m.HandleFunc("GET /api/workspaces/{id}/inbox", s.apiWorkspaceInbox)
	m.HandleFunc("POST /api/workspaces/{id}/inbox/dismiss-notices", s.apiDismissNotices)
	m.HandleFunc("POST /api/workspaces/{id}/inbox/{itemId}/respond", s.apiInboxRespond)
	m.HandleFunc("POST /api/workspaces/{id}/inbox/{itemId}/dismiss", s.apiInboxDismiss)
	m.HandleFunc("POST /api/workspaces/{id}/inbox/{itemId}/postpone", s.apiInboxPostpone)
	m.HandleFunc("GET /api/workspaces/{id}/chat", s.apiChatList)
	m.HandleFunc("POST /api/workspaces/{id}/chat", s.apiChatSend)
	m.HandleFunc("GET /api/wire", s.apiWire)
	m.HandleFunc("GET /api/workspaces/{id}/activity", s.apiWorkspaceActivity)
	m.HandleFunc("POST /api/workspaces/{id}/agents/{terminal}/approve", s.apiAgentAnswer(true))
	m.HandleFunc("POST /api/workspaces/{id}/agents/{terminal}/reject", s.apiAgentAnswer(false))
	m.HandleFunc("POST /api/workspaces/{id}/agents/{terminal}/prompt", s.apiAgentPrompt)
	m.HandleFunc("GET /api/floors", s.apiFloors)
	m.HandleFunc("GET /api/workspaces/{id}/floors", s.apiWorkspaceFloors)
	m.HandleFunc("GET /api/workspaces/{id}/docs", s.apiWorkspaceDocs)
	m.HandleFunc("GET /api/usage", s.apiUsage)
	m.HandleFunc("POST /api/standby/config", s.apiStandbyConfig)
	m.HandleFunc("POST /api/standby/resume", s.apiStandbyResume)
}

// StartBackground launches periodic session cleanup and usage/standby evaluation until ctx cancels.
func (s *Server) StartBackground(ctx context.Context) {
	if s.Activity != nil {
		s.Activity.Start(ctx)
		go s.forwardWireChanges(ctx)
	}
	pollSecs := s.Config.Usage.PollSeconds
	if pollSecs < 5 {
		pollSecs = 30
	}
	go func() {
		_, _, changed := s.evaluateUsageAndStandby()
		if changed {
			s.notifyStream("standby")
		}
		usageTick := time.NewTicker(time.Duration(pollSecs) * time.Second)
		sessionTick := time.NewTicker(10 * time.Minute)
		defer usageTick.Stop()
		defer sessionTick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-sessionTick.C:
				_ = s.Sessions.PurgeExpired()
			case <-usageTick.C:
				_, _, changed := s.evaluateUsageAndStandby()
				if changed {
					s.notifyStream("standby")
				}
			}
		}
	}()
}

// isPublic reports whether a path is reachable without a session.
func isPublic(p string) bool {
	switch p {
	case "/login", "/login/2fa", "/login/recovery":
		return true
	}
	return strings.HasPrefix(p, "/static/")
}

type ctxKey int

const (
	sessionKey ctxKey = 1
	ipKey      ctxKey = 2
)

func sessionFrom(r *http.Request) *auth.Session {
	s, _ := r.Context().Value(sessionKey).(*auth.Session)
	return s
}

// ---- gate ----

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(code int) { w.status = code; w.ResponseWriter.WriteHeader(code) }

func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func isSafe(method string) bool { return method == http.MethodGet || method == http.MethodHead }

// ServeHTTP runs the gate in a fixed order: security headers, Host allowlist, Cloudflare Access,
// Origin check for writes, session, CSRF, then the route.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.setSecurityHeaders(w)
	ip := s.clientIP(r)

	// DNS-rebinding defence: only answer to known Host names.
	if !s.hosts[strings.ToLower(r.Host)] {
		deny(w, http.StatusMisdirectedRequest, "Misdirected request")
		return
	}

	if s.Access != nil && !s.Access.Verify(r.Context(), r.Header.Get("Cf-Access-Jwt-Assertion")) {
		_ = s.Audit.WriteThrottled("cf:"+ip, time.Minute, "access_jwt_reject", ip, nil)
		deny(w, http.StatusForbidden, "Forbidden")
		return
	}

	if !isSafe(r.Method) && !sameOrigin(r) {
		_ = s.Audit.WriteThrottled("origin:"+ip, time.Minute, "origin_reject", ip, map[string]any{"path": clip(r.URL.Path, 100)})
		deny(w, http.StatusForbidden, "Forbidden")
		return
	}

	// Body limits apply to everything that reads a body.
	if !isSafe(r.Method) {
		limit := int64(maxJSONBody)
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
			limit = maxFormBody
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
	}

	var sess *auth.Session
	if c, err := r.Cookie(s.sessionCookie); err == nil {
		var verr error
		sess, verr = s.Sessions.Validate(c.Value)
		if verr != nil {
			s.serverError(w, r, verr)
			return
		}
		if sess != nil && s.Tunnel.Killed() {
			sess = nil // belt and braces: sessions are destroyed by the kill switch
		}
	}

	public := isPublic(r.URL.Path)
	if sess == nil && !public {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/"):
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
		case r.Method == http.MethodGet:
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		default:
			deny(w, http.StatusUnauthorized, "Unauthorized")
		}
		return
	}

	if !isSafe(r.Method) {
		if !s.csrfOK(w, r, sess, public) {
			_ = s.Audit.Write("csrf_reject", ip, map[string]any{"path": clip(r.URL.Path, 100)})
			if public {
				s.loginCSRFReject(w, r)
				return
			}
			deny(w, http.StatusForbidden, "Invalid CSRF token")
			return
		}
	}

	ctx := context.WithValue(r.Context(), sessionKey, sess)
	r = r.WithContext(context.WithValue(ctx, ipKey, ip))
	if sess != nil && !isSafe(r.Method) {
		// Every authenticated write is audited (path and outcome only, never bodies).
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		s.mux.ServeHTTP(rec, r)
		_ = s.Audit.Write("write", ip, map[string]any{"method": r.Method, "path": clip(r.URL.Path, 100), "status": rec.status})
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
	if s.Config.CookieSecure {
		h.Set("Strict-Transport-Security", "max-age=31536000")
	}
	h.Set("Cache-Control", "no-store")
	// No CORS, ever: Access-Control-* headers are never set.
}

// clientIP is the connecting address, or CF-Connecting-IP when explicitly trusted.
func (s *Server) clientIP(r *http.Request) string {
	if s.Config.TrustCfConnectingIP {
		if v := strings.TrimSpace(r.Header.Get("Cf-Connecting-Ip")); v != "" {
			if a, err := netip.ParseAddr(v); err == nil {
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

func ipOf(r *http.Request) string { ip, _ := r.Context().Value(ipKey).(string); return ip }

// sameOrigin is the cross-site write defence (in addition to CSRF tokens): browsers always send
// Origin on POST. Some send Origin: null under strict privacy modes for same-origin form posts,
// so null is only accepted when Sec-Fetch-Site proves the request is same-origin.
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

// csrfOK checks the synchronizer token: the session's token on authenticated routes, the
// pre-login cookie's token on the login routes. The token comes from X-CSRF-Token or the _csrf form field.
func (s *Server) csrfOK(w http.ResponseWriter, r *http.Request, sess *auth.Session, public bool) bool {
	supplied := r.Header.Get("X-CSRF-Token")
	if supplied == "" && strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		if err := r.ParseForm(); err != nil {
			return false
		}
		supplied = r.PostForm.Get("_csrf")
	}
	expected := ""
	if public {
		if c, err := r.Cookie(s.csrfCookie); err == nil {
			expected = c.Value
		}
	} else if sess != nil {
		expected = sess.CSRFToken
	}
	return expected != "" && supplied != "" && safeEqual(expected, supplied)
}

func (s *Server) loginCSRFReject(w http.ResponseWriter, r *http.Request) {
	tok := auth.RandomToken()
	s.setCookie(w, s.csrfCookie, tok, 0)
	d := &ui.Data{Title: "Log in", CSRF: tok, Error: "Your login form expired or did not match its security token. Please try again."}
	w.WriteHeader(http.StatusForbidden)
	s.render(w, ui.PageLogin, d)
}

// ---- cookies and responses ----

func (s *Server) setCookie(w http.ResponseWriter, name, value string, maxAge time.Duration) {
	c := &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: s.Config.CookieSecure, SameSite: http.SameSiteStrictMode}
	if maxAge > 0 {
		c.MaxAge = int(maxAge.Seconds())
	}
	http.SetCookie(w, c)
}

func (s *Server) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.Config.CookieSecure, SameSite: http.SameSiteStrictMode})
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

func (s *Server) render(w http.ResponseWriter, page string, d *ui.Data) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.ui.Render(w, page, d); err != nil {
		// Headers are already sent only if Render wrote; it buffers, so this is a clean 500.
		deny(w, http.StatusInternalServerError, "Internal error")
	}
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	// Never leak internals; the audit log carries a hint.
	_ = s.Audit.WriteThrottled("err:"+r.URL.Path, time.Minute, "server_error", ipOf(r), map[string]any{"path": clip(r.URL.Path, 100)})
	deny(w, http.StatusInternalServerError, "Internal error")
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ---- listener ----

// ErrNotLoopback is returned when asked to listen anywhere but 127.0.0.1.
var ErrNotLoopback = errors.New("the panel only listens on " + config.Host)

// Listen opens the TCP listener. host must be exactly 127.0.0.1: any other value (0.0.0.0, ::,
// a hostname, an empty string) is refused, so a bug or a config edit cannot expose the panel.
func Listen(host string, port int) (net.Listener, error) {
	if host != config.Host {
		return nil, fmt.Errorf("%w (got %q)", ErrNotLoopback, host)
	}
	if port < 0 || port > 65535 {
		return nil, fmt.Errorf("invalid port %d", port)
	}
	ln, err := net.Listen("tcp4", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	if a, ok := ln.Addr().(*net.TCPAddr); !ok || !a.IP.Equal(net.IPv4(127, 0, 0, 1)) {
		ln.Close()
		return nil, ErrNotLoopback
	}
	return ln, nil
}

// HTTPServer wraps s with timeouts suited to a local panel.
func (s *Server) HTTPServer() *http.Server {
	return &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 * 1024,
	}
}
