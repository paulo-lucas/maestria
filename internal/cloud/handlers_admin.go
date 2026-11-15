package cloud

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/paulo-lucas/maestria/internal/relay"
)

// Everything an administrator does on the web is here. Being an administrator is read from the
// users table on every request (so "user demote" works on the next click), nothing in a request
// can set it, and a person who is not one gets 404: the pages do not exist for them.

// adminOnly returns the signed-in administrator, or answers 404 and returns nil.
func (s *Server) adminOnly(w http.ResponseWriter, r *http.Request, api bool) *User {
	var u *User
	if sess := sessionFrom(r); sess != nil {
		u, _ = s.acc.UserByID(sess.User.ID)
	}
	if u == nil || !u.IsAdmin || !u.CanLogin() {
		if api {
			writeJSON(w, http.StatusNotFound, relay.ErrorBody{Error: "not_found"})
		} else {
			deny(w, http.StatusNotFound, "Not found")
		}
		return nil
	}
	return u
}

// invitePage is one row of the invitations list. It carries no token: that was shown once.
type invitePage struct {
	ID, Note, For, Expires, Status, StatusLabel, UsedBy string
	CanRevoke                                           bool
}

func (s *Server) invitePages() ([]invitePage, error) {
	list, err := s.acc.ListInvites()
	if err != nil {
		return nil, err
	}
	users := map[string]string{}
	if all, err := s.acc.ListUsers(); err == nil {
		for _, u := range all {
			users[u.ID] = u.Email
		}
	}
	now := s.now()
	out := make([]invitePage, 0, len(list))
	for i := range list {
		in := &list[i]
		st := in.Status(now)
		p := invitePage{ID: in.ID, Note: in.Note, For: "Anyone with the link", Expires: time.UnixMilli(in.ExpiresAt).Format("Jan 2, 15:04"), Status: st, CanRevoke: st == "active"}
		if in.Email != "" {
			p.For = MaskEmail(in.Email)
		}
		switch st {
		case "active":
			p.StatusLabel = "Waiting"
		case "used":
			p.StatusLabel = "Used"
			if e := users[in.UsedBy]; e != "" {
				p.UsedBy = MaskEmail(e)
			}
		case "revoked":
			p.StatusLabel = "Revoked"
		default:
			p.StatusLabel = "Expired"
		}
		out = append(out, p)
	}
	return out, nil
}

func (s *Server) apiInviteCreate(w http.ResponseWriter, r *http.Request) {
	u := s.adminOnly(w, r, true)
	if u == nil {
		return
	}
	var in struct {
		Note      string `json:"note"`
		Email     string `json:"email"`
		Expires   string `json:"expires"`
		SendEmail bool   `json:"sendEmail"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, relay.ErrorBody{Error: "Check the form and try again."})
		return
	}
	if in.Expires == "" {
		in.Expires = "7d"
	}
	ttl, err := ParseExpiry(in.Expires)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, relay.ErrorBody{Error: err.Error()})
		return
	}
	inv, tok, err := s.acc.CreateInvite(InviteOptions{By: u.ID, IP: ipOf(r), TTL: ttl, Note: in.Note, Email: in.Email, SendEmail: in.SendEmail})
	var le *LockedError
	switch {
	case errors.As(err, &le):
		msg := lockedMessage(le.RetryAfter)
		if le.Reason == "mail" {
			msg = mailLockedMessage(le.RetryAfter)
		}
		w.Header().Set("Retry-After", itoa(int(le.RetryAfter.Seconds())+1))
		writeJSON(w, http.StatusTooManyRequests, relay.ErrorBody{Error: msg})
		return
	case errors.Is(err, ErrBadExpiry), errors.Is(err, ErrBadNote), errors.Is(err, ErrBadEmail):
		writeJSON(w, http.StatusBadRequest, relay.ErrorBody{Error: err.Error()})
		return
	case errors.Is(err, ErrNoMailer):
		writeJSON(w, http.StatusBadRequest, relay.ErrorBody{Error: "This server cannot send email. Copy the link instead."})
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": inv.ID, "link": s.acc.InviteLink(tok), "expiresAt": inv.ExpiresAt, "emailed": in.SendEmail})
}

func (s *Server) apiInviteRevoke(w http.ResponseWriter, r *http.Request) {
	u := s.adminOnly(w, r, true)
	if u == nil {
		return
	}
	switch err := s.acc.RevokeInvite(u.ID, ipOf(r), r.PathValue("id")); {
	case errors.Is(err, ErrInvalid):
		writeJSON(w, http.StatusNotFound, relay.ErrorBody{Error: "not_found"})
	case err != nil:
		s.serverError(w, r, err)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
