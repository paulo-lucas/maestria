package cloud

import (
	"errors"
	"net/http"
)

// inviteInvalidPage is the one answer for every token that does not work (unknown, expired, used,
// withdrawn, or bound to another address): the same words and the same status.
func (s *Server) inviteInvalidPage(w http.ResponseWriter) {
	s.notice(w, http.StatusForbidden, &pageData{Title: "Invitation", Heading: "This invitation doesn't work",
		Info:        "It is invalid or has expired. Ask the person who invited you for a new one.",
		PrimaryHref: "/login", PrimaryLabel: "Log in"})
}

func (s *Server) signupClosedPage(w http.ResponseWriter) {
	s.notice(w, http.StatusForbidden, &pageData{Title: "Sign-up is closed", Heading: "Sign-up is closed",
		Info:        "This server is not creating new accounts right now.",
		PrimaryHref: "/login", PrimaryLabel: "Log in"})
}

func (s *Server) getSignup(w http.ResponseWriter, r *http.Request) {
	if sessionFrom(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	d := &pageData{Title: "Sign up", CSRF: s.loginCSRF(w, r)}
	switch s.cfg.Signup {
	case SignupClosed:
		s.signupClosedPage(w)
		return
	case SignupOpen:
	default:
		token := r.URL.Query().Get("invite")
		if token == "" {
			s.notice(w, http.StatusOK, &pageData{Title: "Invitation needed", Heading: "Sign-up is by invitation",
				Info:        "Ask an administrator of this server to invite you. They will send you a link to sign up.",
				PrimaryHref: "/login", PrimaryLabel: "Log in"})
			return
		}
		info, err := s.acc.PeekInvite(ipOf(r), token)
		var le *LockedError
		switch {
		case errors.As(err, &le):
			s.locked(w, "signup", le, &pageData{Title: "Sign up"})
			return
		case errors.Is(err, ErrInviteInvalid):
			s.inviteInvalidPage(w)
			return
		case err != nil:
			s.serverError(w, r, err)
			return
		}
		d.Invite, d.InviteBound = token, info.Bound
	}
	s.render(w, "signup", d)
}

func (s *Server) postSignup(w http.ResponseWriter, r *http.Request) {
	email := r.PostForm.Get("email")
	invite := r.PostForm.Get("invite")
	if s.cfg.Signup == SignupOpen {
		invite = ""
	}
	err := s.acc.Signup(ipOf(r), email, r.PostForm.Get("password"), invite)
	var le *LockedError
	var pe *PasswordError
	again := func() *pageData { return &pageData{Title: "Sign up", Email: email, Invite: invite} }
	switch {
	case errors.Is(err, ErrSignupClosed):
		s.signupClosedPage(w)
	case errors.Is(err, ErrInviteInvalid):
		s.inviteInvalidPage(w)
	case errors.As(err, &le):
		s.locked(w, "signup", le, again())
	case errors.As(err, &pe):
		d := again()
		d.CSRF, d.Error = s.freshLoginCSRF(w), pe.Msg
		w.WriteHeader(http.StatusUnprocessableEntity)
		s.render(w, "signup", d)
	case err != nil:
		s.serverError(w, r, err)
	default:
		norm, _ := NormalizeEmail(email)
		s.render(w, "signup-sent", &pageData{Title: "Check your email", CSRF: s.loginCSRF(w, r), Email: norm})
	}
}
