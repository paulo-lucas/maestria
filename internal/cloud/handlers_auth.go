package cloud

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/paulo-lucas/maestria/internal/auth"
)

var csrfShape = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// loginCSRF returns the pre-login CSRF token, reusing a well-formed existing cookie.
func (s *Server) loginCSRF(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(s.csrfCookie); err == nil && csrfShape.MatchString(c.Value) {
		return c.Value
	}
	return s.freshLoginCSRF(w)
}

func (s *Server) freshLoginCSRF(w http.ResponseWriter) string {
	tok := auth.RandomToken()
	s.setCookie(w, s.csrfCookie, tok, 0)
	return tok
}

func (s *Server) csrfRejectPage(w http.ResponseWriter, r *http.Request) {
	d := &pageData{Title: "Log in", CSRF: s.freshLoginCSRF(w), Error: "Your form expired or did not match its security token. Please try again."}
	w.WriteHeader(http.StatusForbidden)
	s.render(w, "login", d)
}

func (s *Server) render(w http.ResponseWriter, page string, d *pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.ui.Render(w, page, d); err != nil {
		s.log.Error("render failed", "page", page, "error", err.Error())
		deny(w, http.StatusInternalServerError, "Internal error")
	}
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
	return "Too many attempts. Try again in " + strconv.Itoa(m) + " " + unit + "."
}

func (s *Server) locked(w http.ResponseWriter, page string, le *LockedError, d *pageData) {
	w.Header().Set("Retry-After", strconv.Itoa(int(le.RetryAfter.Seconds())))
	d.CSRF, d.Error = s.freshLoginCSRF(w), lockedMessage(le.RetryAfter)
	w.WriteHeader(http.StatusTooManyRequests)
	s.render(w, page, d)
}

func (s *Server) notice(w http.ResponseWriter, code int, d *pageData) {
	if d.Title == "" {
		d.Title = d.Heading
	}
	w.WriteHeader(code)
	s.render(w, "notice", d)
}

// ---- sign-up ----

func (s *Server) getSignup(w http.ResponseWriter, r *http.Request) {
	if sessionFrom(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, "signup", &pageData{Title: "Sign up", CSRF: s.loginCSRF(w, r)})
}

func (s *Server) postSignup(w http.ResponseWriter, r *http.Request) {
	email := r.PostForm.Get("email")
	err := s.acc.Signup(ipOf(r), email, r.PostForm.Get("password"))
	var le *LockedError
	var pe *PasswordError
	switch {
	case errors.As(err, &le):
		s.locked(w, "signup", le, &pageData{Title: "Sign up", Email: email})
	case errors.As(err, &pe):
		w.WriteHeader(http.StatusUnprocessableEntity)
		s.render(w, "signup", &pageData{Title: "Sign up", CSRF: s.freshLoginCSRF(w), Email: email, Error: pe.Msg})
	case err != nil:
		s.serverError(w, r, err)
	default:
		norm, _ := NormalizeEmail(email)
		s.render(w, "signup-sent", &pageData{Title: "Check your email", CSRF: s.loginCSRF(w, r), Email: norm})
	}
}

func (s *Server) postResend(w http.ResponseWriter, r *http.Request) {
	email := r.PostForm.Get("email")
	s.acc.ResendVerify(ipOf(r), email)
	norm, _ := NormalizeEmail(email)
	s.render(w, "signup-sent", &pageData{Title: "Check your email", CSRF: s.loginCSRF(w, r), Email: norm, Info: "If that address can still be verified, a new link is on its way."})
}

func (s *Server) getVerify(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	u, err := s.acc.PeekVerify(token)
	if err != nil || u == nil {
		s.notice(w, http.StatusGone, &pageData{Title: "Link expired", Heading: "This link doesn't work anymore",
			Info: "Verification links expire after 30 minutes and work once.", PrimaryHref: "/signup", PrimaryLabel: "Start again", SecondaryHref: "/login", SecondaryLabel: "Log in"})
		return
	}
	s.render(w, "verify-confirm", &pageData{Title: "Confirm your email", CSRF: s.loginCSRF(w, r), Email: u.Email, Token: token})
}

func (s *Server) postVerify(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	setup, err := s.acc.ConfirmVerify(ipOf(r), token, r.PostForm.Get("password"))
	var le *LockedError
	switch {
	case errors.As(err, &le):
		s.locked(w, "verify-confirm", le, &pageData{Title: "Confirm your email", Token: token})
	case errors.Is(err, ErrExpired):
		s.notice(w, http.StatusGone, &pageData{Title: "Link expired", Heading: "This link doesn't work anymore", Info: "Verification links expire after 30 minutes and work once.",
			PrimaryHref: "/signup/continue", PrimaryLabel: "Continue setting up", SecondaryHref: "/login", SecondaryLabel: "Log in"})
	case errors.Is(err, ErrInvalid):
		u, _ := s.acc.PeekVerify(token)
		d := &pageData{Title: "Confirm your email", CSRF: s.freshLoginCSRF(w), Token: token, Error: "That password isn't right."}
		if u != nil {
			d.Email = u.Email
		}
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, "verify-confirm", d)
	case err != nil:
		s.serverError(w, r, err)
	default:
		s.setCookie(w, s.setupCookie, setup, SetupTTL)
		http.Redirect(w, r, "/signup/2fa", http.StatusSeeOther)
	}
}

func (s *Server) getContinue(w http.ResponseWriter, r *http.Request) {
	s.render(w, "continue", &pageData{Title: "Continue setting up", CSRF: s.loginCSRF(w, r)})
}

func (s *Server) postContinue(w http.ResponseWriter, r *http.Request) {
	email := r.PostForm.Get("email")
	setup, err := s.acc.ResumeSetup(ipOf(r), email, r.PostForm.Get("password"))
	var le *LockedError
	switch {
	case errors.As(err, &le):
		s.locked(w, "continue", le, &pageData{Title: "Continue setting up", Email: email})
	case errors.Is(err, ErrInvalid):
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, "continue", &pageData{Title: "Continue setting up", CSRF: s.freshLoginCSRF(w), Email: email, Error: "We couldn't continue with those details. Check your email for the verification link first."})
	case err != nil:
		s.serverError(w, r, err)
	default:
		s.setCookie(w, s.setupCookie, setup, SetupTTL)
		http.Redirect(w, r, "/signup/2fa", http.StatusSeeOther)
	}
}

func (s *Server) setupFrom(r *http.Request) (*Setup, error) {
	c, err := r.Cookie(s.setupCookie)
	if err != nil {
		return nil, nil
	}
	return s.acc.SetupFor(c.Value)
}

func (s *Server) renderSetup(w http.ResponseWriter, r *http.Request, su *Setup, errMsg string) {
	secret, uri, err := s.acc.BeginTOTP(su)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	path, size, err := qrPath(uri)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := &pageData{Title: "Set up two-factor", CSRF: s.loginCSRF(w, r), QRPath: path, QRSize: size, Secret: spaced(secret), Error: errMsg}
	if errMsg != "" {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}
	s.render(w, "signup-2fa", d)
}

func spaced(s string) string {
	var sb strings.Builder
	for i, r := range s {
		if i > 0 && i%4 == 0 {
			sb.WriteByte(' ')
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

func (s *Server) getSetup2FA(w http.ResponseWriter, r *http.Request) {
	su, err := s.setupFrom(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if su == nil {
		http.Redirect(w, r, "/signup/continue", http.StatusSeeOther)
		return
	}
	s.renderSetup(w, r, su, "")
}

func (s *Server) postSetup2FA(w http.ResponseWriter, r *http.Request) {
	su, err := s.setupFrom(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if su == nil {
		http.Redirect(w, r, "/signup/continue", http.StatusSeeOther)
		return
	}
	codes, sess, err := s.acc.FinishTOTP(ipOf(r), r.UserAgent(), su, r.PostForm.Get("code"))
	var le *LockedError
	switch {
	case errors.As(err, &le):
		s.locked(w, "signup-2fa", le, &pageData{Title: "Set up two-factor"})
	case errors.Is(err, ErrInvalid):
		s.renderSetup(w, r, su, "That code didn't work. Check the time on your phone and try again.")
	case err != nil:
		s.serverError(w, r, err)
	default:
		s.clearCookie(w, s.setupCookie)
		s.clearCookie(w, s.csrfCookie)
		s.setCookie(w, s.sessionCookie, sess, 0)
		s.render(w, "recovery-codes", &pageData{Title: "Recovery codes", Codes: codes, Scripts: []string{"auth.js"}})
	}
}

// ---- login ----

func (s *Server) getLogin(w http.ResponseWriter, r *http.Request) {
	if sessionFrom(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	d := &pageData{Title: "Log in", CSRF: s.loginCSRF(w, r)}
	switch r.URL.Query().Get("error") {
	case "invalid":
		d.Error = "Invalid email, password or code."
	case "expired":
		d.Error = "Your sign-in expired. Please start again."
	case "reset":
		d.Info = "Password changed."
	}
	s.render(w, "login", d)
}

func (s *Server) postLogin(w http.ResponseWriter, r *http.Request) {
	tok, err := s.acc.LoginPassword(ipOf(r), r.PostForm.Get("email"), r.PostForm.Get("password"))
	var le *LockedError
	switch {
	case errors.As(err, &le):
		s.locked(w, "login", le, &pageData{Title: "Log in"})
	case err != nil:
		s.serverError(w, r, err)
	default:
		s.setCookie(w, s.pendingCookie, tok, auth.PendingTTL)
		http.Redirect(w, r, "/login/2fa", http.StatusSeeOther)
	}
}

func (s *Server) pendingToken(r *http.Request) string {
	if c, err := r.Cookie(s.pendingCookie); err == nil {
		return c.Value
	}
	return ""
}

func (s *Server) getLogin2FA(w http.ResponseWriter, r *http.Request) {
	if sessionFrom(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !s.acc.HasPending(s.pendingToken(r), ipOf(r)) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.render(w, "login-2fa", &pageData{Title: "Two-factor check", CSRF: s.loginCSRF(w, r)})
}

func (s *Server) postLogin2FA(w http.ResponseWriter, r *http.Request) {
	s.second(w, r, "totp", r.PostForm.Get("code"))
}
func (s *Server) postLoginRecovery(w http.ResponseWriter, r *http.Request) {
	s.second(w, r, "recovery", r.PostForm.Get("recovery"))
}

func (s *Server) second(w http.ResponseWriter, r *http.Request, method, code string) {
	sess, err := s.acc.LoginSecond(ipOf(r), r.UserAgent(), s.pendingToken(r), method, code)
	var le *LockedError
	switch {
	case errors.As(err, &le):
		s.clearCookie(w, s.pendingCookie)
		s.locked(w, "login", le, &pageData{Title: "Log in"})
	case errors.Is(err, ErrExpired):
		s.clearCookie(w, s.pendingCookie)
		http.Redirect(w, r, "/login?error=expired", http.StatusSeeOther)
	case errors.Is(err, ErrInvalid):
		// One generic failure for a wrong email, password or code: back to step 1.
		s.clearCookie(w, s.pendingCookie)
		http.Redirect(w, r, "/login?error=invalid", http.StatusSeeOther)
	case err != nil:
		s.serverError(w, r, err)
	default:
		s.clearCookie(w, s.pendingCookie)
		s.clearCookie(w, s.csrfCookie)
		s.setCookie(w, s.sessionCookie, sess, 0)
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

func (s *Server) postLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(s.sessionCookie); err == nil {
		_ = s.acc.Destroy(c.Value)
	}
	if sess := sessionFrom(r); sess != nil {
		s.acc.event(sess.User.ID, "logout", ipOf(r), nil)
	}
	s.clearCookie(w, s.sessionCookie)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---- password reset ----

func (s *Server) getResetRequest(w http.ResponseWriter, r *http.Request) {
	s.render(w, "reset-request", &pageData{Title: "Reset password", CSRF: s.loginCSRF(w, r)})
}

func (s *Server) postResetRequest(w http.ResponseWriter, r *http.Request) {
	s.acc.RequestReset(ipOf(r), r.PostForm.Get("email"))
	s.notice(w, http.StatusOK, &pageData{Title: "Check your email", Heading: "Check your email",
		Info: "If an account exists for that address, we sent a reset link. It expires in 30 minutes.", PrimaryHref: "/login", PrimaryLabel: "Back to log in"})
}

func (s *Server) getReset(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	u, err := s.acc.PeekReset(token)
	if err != nil {
		s.notice(w, http.StatusGone, &pageData{Title: "Link expired", Heading: "This link doesn't work anymore",
			Info: "Reset links expire after 30 minutes and work once.", PrimaryHref: "/reset", PrimaryLabel: "Send a new link"})
		return
	}
	s.render(w, "reset-form", &pageData{Title: "Choose a new password", CSRF: s.loginCSRF(w, r), Email: u.Email, Token: token})
}

func (s *Server) postReset(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	method, code := "totp", r.PostForm.Get("code")
	if rc := strings.TrimSpace(r.PostForm.Get("recovery")); rc != "" {
		method, code = "recovery", rc
	}
	sess, u, err := s.acc.CompleteReset(ipOf(r), r.UserAgent(), token, r.PostForm.Get("password"), method, code)
	var le *LockedError
	var pe *PasswordError
	again := func(msg string) {
		d := &pageData{Title: "Choose a new password", CSRF: s.freshLoginCSRF(w), Token: token, Error: msg, UseRecovery: method == "recovery"}
		if pu, _ := s.acc.PeekReset(token); pu != nil {
			d.Email = pu.Email
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		s.render(w, "reset-form", d)
	}
	switch {
	case errors.As(err, &le):
		s.locked(w, "reset-form", le, &pageData{Title: "Choose a new password", Token: token})
	case errors.As(err, &pe):
		again(pe.Msg)
	case errors.Is(err, ErrExpired):
		s.notice(w, http.StatusGone, &pageData{Title: "Link expired", Heading: "This link doesn't work anymore",
			Info: "Reset links expire after 30 minutes and work once.", PrimaryHref: "/reset", PrimaryLabel: "Send a new link"})
	case errors.Is(err, ErrInvalid):
		again("That code didn't work. Check the time on your phone, or use a recovery code.")
	case err != nil:
		s.serverError(w, r, err)
	default:
		// Paired devices must be approved again on every machine.
		s.forgetDevices(u.ID)
		s.clearCookie(w, s.csrfCookie)
		s.setCookie(w, s.sessionCookie, sess, 0)
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

// forgetDevices asks every connector of the user to revoke its paired devices.
func (s *Server) forgetDevices(userID string) {
	if err := s.cons.MarkForget(userID); err != nil {
		s.log.Warn("could not flag connectors to forget devices", "error", err.Error())
		return
	}
	list, _ := s.cons.ForUser(userID)
	at := s.now().UnixMilli()
	for _, cn := range list {
		s.hub.Forget(cn.ID, at)
	}
}
