package web

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/maestria-dev/maestria/internal/auth"
	"github.com/maestria-dev/maestria/internal/ui"
)

var csrfShape = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

func (s *Server) killedBanner() *ui.Banner {
	return &ui.Banner{
		Status: "danger", Icon: "i-power", Title: "Kill switch is on",
		Text: `Panel access is disabled. Turn it off on this machine with "maestria tunnel on".`,
	}
}

// loginCSRF returns the pre-login CSRF token, reusing a well-formed existing cookie: browsers
// often load /login twice (prefetch, reload, two tabs) and rotating it each time would make the
// first form's token stale.
func (s *Server) loginCSRF(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(s.csrfCookie); err == nil && csrfShape.MatchString(c.Value) {
		return c.Value
	}
	tok := auth.RandomToken()
	s.setCookie(w, s.csrfCookie, tok, 0)
	return tok
}

// freshLoginCSRF always rotates the token (after a failed attempt).
func (s *Server) freshLoginCSRF(w http.ResponseWriter) string {
	tok := auth.RandomToken()
	s.setCookie(w, s.csrfCookie, tok, 0)
	return tok
}

func (s *Server) getLogin(w http.ResponseWriter, r *http.Request) {
	if sessionFrom(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	d := &ui.Data{Title: "Log in", CSRF: s.loginCSRF(w, r)}
	if s.Tunnel.Killed() {
		d.Banner, d.Locked = s.killedBanner(), true
	}
	s.render(w, ui.PageLogin, d)
}

func lockedMessage(retry time.Duration) string {
	m := int((retry + time.Minute - 1) / time.Minute)
	if m < 1 {
		m = 1
	}
	unit := "minutes"
	if m == 1 {
		unit = "minute"
	}
	return fmt.Sprintf("Too many failed attempts. Try again in %d %s.", m, unit)
}

func (s *Server) postLogin(w http.ResponseWriter, r *http.Request) {
	ip := ipOf(r)
	d := &ui.Data{Title: "Log in"}
	if s.Tunnel.Killed() {
		_ = s.Audit.WriteThrottled("killed:"+ip, time.Minute, "login_blocked_killed", ip, nil)
		d.CSRF, d.Banner, d.Locked = s.freshLoginCSRF(w), s.killedBanner(), true
		w.WriteHeader(http.StatusForbidden)
		s.render(w, ui.PageLogin, d)
		return
	}
	res, err := s.auth.Password(ip, r.PostForm.Get("password"))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	switch res.Outcome {
	case auth.OK:
		s.setCookie(w, s.pendingCookie, res.Token, auth.PendingTTL)
		http.Redirect(w, r, "/login/2fa", http.StatusSeeOther)
	case auth.Locked:
		w.Header().Set("Retry-After", strconv.Itoa(int(res.RetryAfter.Seconds())))
		d.CSRF, d.Error = s.freshLoginCSRF(w), lockedMessage(res.RetryAfter)
		w.WriteHeader(http.StatusTooManyRequests)
		s.render(w, ui.PageLogin, d)
	default:
		d.CSRF, d.Error = s.freshLoginCSRF(w), "Invalid credentials."
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, ui.PageLogin, d)
	}
}

func (s *Server) pendingToken(r *http.Request) string {
	if c, err := r.Cookie(s.pendingCookie); err == nil {
		return c.Value
	}
	return ""
}

func (s *Server) getTwoFA(w http.ResponseWriter, r *http.Request) {
	if sessionFrom(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if ok, _ := s.Sessions.PendingValid(s.pendingToken(r)); !ok || s.Tunnel.Killed() {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.render(w, ui.PageTwoFA, &ui.Data{Title: "Two-factor check", CSRF: s.loginCSRF(w, r)})
}

func (s *Server) postTwoFA(w http.ResponseWriter, r *http.Request) {
	s.secondFactor(w, r, false)
}

func (s *Server) postRecovery(w http.ResponseWriter, r *http.Request) {
	s.secondFactor(w, r, true)
}

func (s *Server) secondFactor(w http.ResponseWriter, r *http.Request, recovery bool) {
	ip := ipOf(r)
	if s.Tunnel.Killed() {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	pending := s.pendingToken(r)
	var (
		res auth.Result
		err error
	)
	if recovery {
		res, err = s.auth.Recovery(ip, r.UserAgent(), pending, r.PostForm.Get("recovery"))
	} else {
		res, err = s.auth.TOTP(ip, r.UserAgent(), pending, r.PostForm.Get("code"))
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := &ui.Data{Title: "Two-factor check", RecoveryOpen: recovery}
	setErr := func(msg string) {
		if recovery {
			d.RecoveryErr = msg
		} else {
			d.Error = msg
		}
	}
	switch res.Outcome {
	case auth.OK:
		s.clearCookie(w, s.pendingCookie)
		s.clearCookie(w, s.csrfCookie)
		s.setCookie(w, s.sessionCookie, res.Token, 0)
		http.Redirect(w, r, "/", http.StatusSeeOther)
	case auth.Expired:
		s.clearCookie(w, s.pendingCookie)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	case auth.Locked:
		s.clearCookie(w, s.pendingCookie)
		w.Header().Set("Retry-After", strconv.Itoa(int(res.RetryAfter.Seconds())))
		d.Title, d.CSRF = "Log in", s.freshLoginCSRF(w)
		d.Error, d.RecoveryOpen = lockedMessage(res.RetryAfter), false
		w.WriteHeader(http.StatusTooManyRequests)
		s.render(w, ui.PageLogin, d)
	default:
		d.CSRF = s.freshLoginCSRF(w)
		what := "code"
		if recovery {
			what = "recovery code"
		}
		msg := fmt.Sprintf("That %s didn't work.", what)
		if !recovery {
			msg += " Check the time on your phone and try again."
		}
		if res.Remaining > 0 {
			msg += fmt.Sprintf(" %d attempt%s left.", res.Remaining, map[bool]string{true: "", false: "s"}[res.Remaining == 1])
		}
		setErr(msg)
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, ui.PageTwoFA, d)
	}
}

func (s *Server) postLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(s.sessionCookie); err == nil {
		_ = s.Sessions.Destroy(c.Value)
	}
	_ = s.Audit.Write("logout", ipOf(r), nil)
	s.clearCookie(w, s.sessionCookie)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) postLogoutAll(w http.ResponseWriter, r *http.Request) {
	n, err := s.Sessions.DestroyAll()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	_ = s.Audit.Write("logout_all", ipOf(r), map[string]any{"sessions": n, "via": "web"})
	s.clearCookie(w, s.sessionCookie)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// postKillSwitch is the form version of the kill switch. There is deliberately no route that
// turns it off: re-enabling is local-only (`maestria tunnel on`).
func (s *Server) postKillSwitch(w http.ResponseWriter, r *http.Request) {
	if _, err := s.Tunnel.Kill(r.Context(), "web", ipOf(r)); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.clearCookie(w, s.sessionCookie)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) appData(r *http.Request, title, nav string) *ui.Data {
	sess := sessionFrom(r)
	left, _ := auth.RemainingRecoveryCodes(s.DB)
	n, _ := s.Sessions.Count()
	return &ui.Data{
		Title: title, Nav: nav, CSRF: sess.CSRFToken,
		Listen:         fmt.Sprintf("127.0.0.1:%d", s.Config.Port),
		SessionExpires: sess.ExpiresAt.Local().Format("Jan 2, 15:04"),
		IdleMinutes:    s.Config.Session.IdleMinutes, AbsoluteHours: s.Config.Session.AbsoluteHours,
		SessionCount: n, RecoveryLeft: left,
		TunnelStop: len(s.Config.Tunnel.StopCommand) > 0,
	}
}

func (s *Server) getOverview(w http.ResponseWriter, r *http.Request) {
	s.render(w, ui.PageOverview, s.appData(r, "Overview", "overview"))
}

var eventLabels = map[string]struct{ status, label string }{
	"login_fail":           {"warn", "Failed"},
	"login_lockout":        {"danger", "Locked out"},
	"login_blocked":        {"danger", "Blocked"},
	"login_blocked_killed": {"danger", "Blocked"},
	"csrf_reject":          {"danger", "Blocked"},
	"origin_reject":        {"danger", "Blocked"},
	"access_jwt_reject":    {"danger", "Blocked"},
	"kill_switch":          {"danger", "Kill switch"},
}

func describeEvent(event, detail string) string {
	names := map[string]string{
		"login_password_ok":    "Password accepted",
		"login_ok":             "Signed in",
		"login_fail":           "Sign-in attempt",
		"login_lockout":        "Too many failed sign-ins",
		"login_blocked":        "Sign-in refused while locked out",
		"login_blocked_killed": "Sign-in refused: kill switch is on",
		"recovery_code_used":   "Recovery code used",
		"logout":               "Signed out",
		"logout_all":           "Signed out everywhere",
		"kill_switch":          "Kill switch turned on",
		"tunnel_on":            "Access turned back on locally",
		"csrf_reject":          "Request rejected: bad security token",
		"origin_reject":        "Request rejected: wrong origin",
		"access_jwt_reject":    "Request rejected: Cloudflare Access",
		"auth_setup":           "Credentials set up",
		"auth_reset_sessions":  "Sessions reset locally",
		"write":                "Change made",
	}
	n, ok := names[event]
	if !ok {
		n = strings.ReplaceAll(event, "_", " ")
	}
	_ = detail
	return n
}

func (s *Server) getSecurity(w http.ResponseWriter, r *http.Request) {
	d := s.appData(r, "Security", "security")
	entries, err := s.Audit.Recent(50)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	for _, e := range entries {
		t := time.UnixMilli(e.TS).Local()
		row := ui.AuditRow{When: t.Format("Jan 2, 15:04"), ISO: t.Format(time.RFC3339), Event: describeEvent(e.Event, e.Detail), IP: e.IP}
		if l, ok := eventLabels[e.Event]; ok {
			row.Status, row.Label = l.status, l.label
		}
		d.Audit = append(d.Audit, row)
	}
	s.render(w, ui.PageSecurity, d)
}

// ---- JSON API ----

func (s *Server) apiSession(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	writeJSON(w, http.StatusOK, map[string]any{"csrfToken": sess.CSRFToken, "expiresAt": sess.ExpiresAt.UnixMilli()})
}

func (s *Server) apiTunnel(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Tunnel.Status(r.Context()))
}

func (s *Server) apiKillSwitch(w http.ResponseWriter, r *http.Request) {
	res, err := s.Tunnel.Kill(r.Context(), "web", ipOf(r))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
		return
	}
	s.clearCookie(w, s.sessionCookie)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sessionsInvalidated": res.SessionsInvalidated, "stopResult": res.StopResult})
}

func (s *Server) apiAudit(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			limit = min(n, 200)
		}
	}
	entries, err := s.Audit.Recent(limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}
