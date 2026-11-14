package cloud

import (
	"errors"
	"net/http"
	"strings"

	"github.com/paulo-lucas/maestria/internal/relay"
)

// Connector-facing link API: no cookies, no CSRF (nothing ambient to abuse), a body limit, and
// per-IP rate limits inside Connectors.

func (s *Server) linkError(w http.ResponseWriter, r *http.Request, err error) {
	var le *LockedError
	switch {
	case errors.As(err, &le):
		writeJSON(w, http.StatusTooManyRequests, relay.ErrorBody{Error: "rate_limited"})
	case errors.Is(err, ErrInvalid):
		writeJSON(w, http.StatusNotFound, relay.ErrorBody{Error: "invalid_code"})
	default:
		s.serverError(w, r, err)
	}
}

func cleanMachine(m relay.Machine) relay.Machine {
	return relay.Machine{Host: clip(strings.TrimSpace(m.Host), 100), OS: clip(m.OS, 20), Arch: clip(m.Arch, 20), Version: clip(m.Version, 40)}
}

func (s *Server) baseURL() string { return strings.TrimRight(s.cfg.PublicURL, "/") }

func (s *Server) apiLinkRedeem(w http.ResponseWriter, r *http.Request) {
	var in relay.RedeemRequest
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, relay.ErrorBody{Error: "bad_request"})
		return
	}
	out, err := s.cons.Redeem(ipOf(r), in.Code, cleanMachine(in.Machine))
	if err != nil {
		s.linkError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiLinkConfirm(w http.ResponseWriter, r *http.Request) {
	var in relay.ConfirmRequest
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, relay.ErrorBody{Error: "bad_request"})
		return
	}
	out, err := s.cons.Confirm(ipOf(r), in.Code, in.Token, in.Name)
	if err != nil {
		s.linkError(w, r, err)
		return
	}
	s.hub.notify(s.userOf(out.ConnectorID))
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) userOf(connectorID string) string {
	if cn, _ := s.cons.Get(connectorID); cn != nil {
		return cn.UserID
	}
	return ""
}

func (s *Server) apiLinkStart(w http.ResponseWriter, r *http.Request) {
	var in relay.StartRequest
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, relay.ErrorBody{Error: "bad_request"})
		return
	}
	out, err := s.cons.StartCLI(ipOf(r), cleanMachine(in.Machine), s.baseURL())
	if err != nil {
		s.linkError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiLinkPoll(w http.ResponseWriter, r *http.Request) {
	var in relay.PollRequest
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, relay.ErrorBody{Error: "bad_request"})
		return
	}
	in.Name = clip(in.Name, 100)
	out, err := s.cons.Poll(ipOf(r), in)
	if err != nil {
		s.linkError(w, r, err)
		return
	}
	if out.Linked != nil {
		s.hub.notify(s.userOf(out.Linked.ConnectorID))
	}
	writeJSON(w, http.StatusOK, out)
}

// apiUnlink revokes the calling connector's own credential (`maestria connect unlink`).
func (s *Server) apiUnlink(w http.ResponseWriter, r *http.Request) {
	authz := r.Header.Get("Authorization")
	cred := strings.TrimPrefix(authz, "Bearer ")
	if cred == authz {
		writeJSON(w, http.StatusUnauthorized, relay.ErrorBody{Error: "unauthorized"})
		return
	}
	cn, err := s.cons.Authenticate(cred)
	if err != nil || cn == nil {
		writeJSON(w, http.StatusUnauthorized, relay.ErrorBody{Error: "unauthorized"})
		return
	}
	if err := s.cons.Revoke(cn.UserID, cn.ID, ipOf(r), "connector"); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.hub.Disconnect(cn.ID)
	s.hub.notify(cn.UserID)
	w.WriteHeader(http.StatusNoContent)
}
