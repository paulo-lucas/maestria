package web

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/paulo-lucas/maestria/internal/auth"
	"github.com/paulo-lucas/maestria/internal/ui"
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
	// Only these fixed messages can be shown: the query value selects one, it is never echoed.
	switch r.URL.Query().Get("error") {
	case "invalid":
		d.Error = "Invalid password or code."
	case "expired":
		d.Error = "Your sign-in expired. Please start again."
	}
	if s.Tunnel.Killed() {
		d.Banner, d.Locked, d.Error = s.killedBanner(), true, ""
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

// postLogin is step 1. Whatever the password was, a live answer moves on to the second-factor
// page: the verdict stays on the server, so this response is no oracle for the password.
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
	case auth.Locked:
		s.renderLocked(w, res, d)
		return
	case auth.Throttled:
		s.renderThrottled(w, res, ui.PageLogin, d)
		return
	}
	s.setCookie(w, s.pendingCookie, res.Token, auth.PendingTTL)
	http.Redirect(w, r, "/login/2fa", http.StatusSeeOther)
}

func (s *Server) renderLocked(w http.ResponseWriter, res auth.Result, d *ui.Data) {
	w.Header().Set("Retry-After", strconv.Itoa(int(res.RetryAfter.Seconds())))
	d.Title, d.CSRF, d.Error = "Log in", s.freshLoginCSRF(w), lockedMessage(res.RetryAfter)
	w.WriteHeader(http.StatusTooManyRequests)
	s.render(w, ui.PageLogin, d)
}

// renderThrottled answers 429 when the password-check budget is used up or busy. It says nothing
// about the credentials, so it is no oracle.
func (s *Server) renderThrottled(w http.ResponseWriter, res auth.Result, page string, d *ui.Data) {
	secs := int(res.RetryAfter.Seconds())
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	d.CSRF, d.Error = s.freshLoginCSRF(w), "Too many sign-in attempts right now. Wait a moment and try again."
	if page == ui.PageTwoFA {
		d.Title = "Two-factor check"
	} else {
		d.Title = "Log in"
	}
	w.WriteHeader(http.StatusTooManyRequests)
	s.render(w, page, d)
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
	if p, _ := s.Sessions.PendingFor(s.pendingToken(r), ipOf(r)); p == nil || s.Tunnel.Killed() {
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
	switch res.Outcome {
	case auth.OK:
		s.clearCookie(w, s.pendingCookie)
		s.clearCookie(w, s.csrfCookie)
		s.setCookie(w, s.sessionCookie, res.Token, 0)
		http.Redirect(w, r, "/", http.StatusSeeOther)
	case auth.Locked:
		s.clearCookie(w, s.pendingCookie)
		s.renderLocked(w, res, &ui.Data{})
	case auth.Throttled:
		// The pending login is kept: nothing was checked, so the person can try again.
		s.renderThrottled(w, res, ui.PageTwoFA, &ui.Data{})
	case auth.Expired:
		s.clearCookie(w, s.pendingCookie)
		http.Redirect(w, r, "/login?error=expired", http.StatusSeeOther)
	default:
		// One generic failure for a wrong password and a wrong code alike: back to step 1.
		s.clearCookie(w, s.pendingCookie)
		http.Redirect(w, r, "/login?error=invalid", http.StatusSeeOther)
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
	d := &ui.Data{
		Title: title, Nav: nav, Scope: "all", CSRF: sess.CSRFToken,
		Listen:         fmt.Sprintf("127.0.0.1:%d", s.Config.Port),
		SessionExpires: sess.ExpiresAt.Local().Format("Jan 2, 15:04"),
		IdleMinutes:    s.Config.Session.IdleMinutes, AbsoluteHours: s.Config.Session.AbsoluteHours,
		SessionCount: n, RecoveryLeft: left,
		TunnelStop: len(s.Config.Tunnel.StopCommand) > 0,
	}
	s.populateShell(d, "all")
	return d
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
		"login_throttled":      "Sign-in paused: too many password checks",
		"pair_offered":         "A browser asked to be approved (hosted mode)",
		"pair_refused":         "A pairing was refused: too many tries",
		"pair_mismatch":        "A pairing key did not match: pairing paused",
		"pair_bad_code":        "Wrong pairing code typed on this machine",
		"device_approved":      "Browser approved on this machine",
		"cli_confirmed":        "A sensitive command was confirmed at the terminal",
		"cli_confirm_failed":   "A sensitive command was refused: wrong confirmation word",
		"cli_confirm_refused":  "A sensitive command was refused: no interactive terminal",
		"relay_write":          "Change made through the hosted relay",
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
	// Without the refusals: anyone can cause those, and they must not push a sign-in or an
	// approval out of the last 50 rows. They stay in the log (and in GET /api/audit).
	entries, err := s.Audit.RecentSignificant(50)
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
	if s.Updates != nil {
		if n := s.Updates.Cached(); n != nil {
			d.Update = &ui.UpdateNote{Version: n.Version}
			if strings.HasPrefix(n.URL, "https://github.com/") {
				d.Update.URL = n.URL
			}
		}
	}
	s.render(w, ui.PageSecurity, d)
}

// postUpdateDismiss closes the "newer release" note for one version. It changes nothing but a cache
// file, and like every write it is session-, origin- and CSRF-checked and audited.
func (s *Server) postUpdateDismiss(w http.ResponseWriter, r *http.Request) {
	if s.Updates != nil {
		if v := strings.TrimSpace(r.PostFormValue("version")); v != "" && len(v) <= 64 {
			s.Updates.Dismiss(v)
			_ = s.Audit.Write("update_dismiss", ipOf(r), map[string]any{"version": v, "via": "web"})
		}
	}
	http.Redirect(w, r, "/security", http.StatusSeeOther)
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
