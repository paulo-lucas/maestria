package cloud

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/paulo-lucas/maestria/internal/relay"
	"github.com/paulo-lucas/maestria/internal/update"
)

type connectorView struct {
	ID          string             `json:"id"`
	Name        string             `json:"name"`
	Online      bool               `json:"online"`
	Version     string             `json:"version"`
	OS          string             `json:"os"`
	Arch        string             `json:"arch"`
	Host        string             `json:"host"`
	KillSwitch  bool               `json:"killSwitch"`
	CreatedAt   int64              `json:"createdAt"`
	LastSeenAt  int64              `json:"lastSeenAt"`
	ConnectedAt int64              `json:"connectedAt,omitempty"`
	Devices     []relay.DeviceInfo `json:"devices"`
	// UpdateAvailable is the newest release when this machine runs an older one ("" otherwise).
	UpdateAvailable string `json:"updateAvailable,omitempty"`
}

func (s *Server) views(userID string) ([]connectorView, error) {
	list, err := s.cons.ForUser(userID)
	if err != nil {
		return nil, err
	}
	out := make([]connectorView, 0, len(list))
	for _, cn := range list {
		v := connectorView{ID: cn.ID, Name: cn.Name, Version: cn.Version, OS: cn.OS, Arch: cn.Arch, Host: cn.Host, KillSwitch: cn.KillSwitch,
			CreatedAt: cn.CreatedAt, LastSeenAt: cn.LastSeenAt}
		if hello, since, ok := s.hub.Online(cn.ID); ok {
			v.Online, v.ConnectedAt = true, since.UnixMilli()
			v.Version, v.OS, v.Arch, v.KillSwitch = hello.Version, hello.OS, hello.Arch, hello.KillSwitch
			v.LastSeenAt = s.now().UnixMilli()
		}
		v.UpdateAvailable = s.updateFor(v.Version)
		devs, _ := s.cons.Devices(cn.ID)
		if devs == nil {
			devs = []relay.DeviceInfo{}
		}
		v.Devices = devs
		out = append(out, v)
	}
	return out, nil
}

// latest is the newest known release ("" when unknown).
func (s *Server) latest() string {
	if s.releases == nil {
		return ""
	}
	return s.releases.LatestKnown()
}

// updateFor returns the newest release as "v1.4.0" when version is behind it, else "".
func (s *Server) updateFor(version string) string {
	if latest := s.latest(); latest != "" && update.Behind(version, latest) {
		return displayVersion(latest)
	}
	return ""
}

// displayVersion shows 2.0.3 as v2.0.3 and leaves names like "dev" alone.
func displayVersion(v string) string {
	if v != "" && v[0] >= '0' && v[0] <= '9' {
		return "v" + v
	}
	return v
}

func prettyOS(os, arch string) string {
	name := map[string]string{"darwin": "macOS", "windows": "Windows", "linux": "Linux"}[os]
	if name == "" {
		name = os
	}
	switch {
	case name == "" && arch == "":
		return ""
	case arch == "":
		return name
	case name == "":
		return arch
	}
	return name + " · " + arch
}

func rel(t time.Time, now time.Time) string {
	d := now.Sub(t)
	switch {
	case t.IsZero() || t.UnixMilli() <= 0:
		return "never"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		n := int(d.Minutes())
		return fmt.Sprintf("%d min ago", n)
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

func span(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "moments"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}

func (s *Server) shellData(r *http.Request, title, nav string) *pageData {
	sess := sessionFrom(r)
	initial := "?"
	if sess.User.Email != "" {
		initial = strings.ToUpper(sess.User.Email[:1])
	}
	return &pageData{Title: title, Nav: nav, CSRF: sess.CSRFToken, Email: sess.User.Email, Initial: initial,
		Scripts: []string{"hosted-keys.js", "hosted.js", "panel.js"}, PublicURL: s.cfg.PublicURL,
		ServerVersion: displayVersion(s.version), ServerUpdate: s.updateFor(s.version)}
}

// getApp serves the app shell for every in-app route. Cloud-owned pages are rendered here;
// pages made of connector data are filled in by the browser after it signs requests.
func (s *Server) getApp(w http.ResponseWriter, r *http.Request) {
	// /c/<id>/... is both a page address and the relay address. A request that carries the
	// signature header is the app asking a machine; anything else is a person opening the page.
	if strings.HasPrefix(r.URL.Path, "/c/") && r.Header.Get(relay.HeaderSig) != "" {
		s.relayHandler(w, r)
		return
	}
	path := r.URL.Path
	nav, page, title := "overview", "app-js", "Overview"
	switch {
	case path == "/connectors":
		nav, page, title = "connectors", "connectors", "Connectors"
	case path == "/connectors/link":
		nav, page, title = "connectors", "connector-link", "Link a machine"
	case path == "/security":
		nav, title = "security", "Devices & security"
	case path == "/pair":
		d := s.shellData(r, "Approve this browser", "security")
		d.Scripts = []string{"hosted-keys.js", "pair.js"}
		s.render(w, "pair", d)
		return
	case path == "/usage" || strings.HasSuffix(path, "/usage"):
		nav, title = "usage", "Usage & standby"
	case strings.HasPrefix(path, "/workspaces/") || strings.HasPrefix(path, "/c/"):
		nav, title = "workspace", "Workspace"
	}
	d := s.shellData(r, title, nav)
	if page == "connectors" {
		views, err := s.views(sessionFrom(r).User.ID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		now := s.now()
		for _, v := range views {
			cp := connectorPage{ID: v.ID, Name: v.Name, Online: v.Online, Version: displayVersion(v.Version), System: prettyOS(v.OS, v.Arch),
				KillSwitch: v.KillSwitch, Linked: time.UnixMilli(v.CreatedAt).Format("Jan 2"), UpdateAvailable: v.UpdateAvailable}
			if v.Online {
				cp.Connected = span(now.Sub(time.UnixMilli(v.ConnectedAt)))
			} else if v.LastSeenAt > 0 {
				t := time.UnixMilli(v.LastSeenAt)
				cp.LastSeen = t.Format("Jan 2, 15:04") + " · " + rel(t, now)
			} else {
				cp.LastSeen = "never connected"
			}
			d.Connectors = append(d.Connectors, cp)
		}
	}
	if r.Header.Get("X-Fragment") == "1" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Page-Title", title)
		w.Header().Set("X-Page-Nav", nav)
		if err := s.ui.RenderContent(w, page, d); err != nil {
			s.serverError(w, r, err)
		}
		return
	}
	s.render(w, page, d)
}

func (s *Server) apiMe(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	writeJSON(w, http.StatusOK, map[string]any{"email": sess.User.Email, "csrfToken": sess.CSRFToken, "expiresAt": sess.ExpiresAt.UnixMilli(), "publicUrl": s.cfg.PublicURL})
}

func (s *Server) apiConnectors(w http.ResponseWriter, r *http.Request) {
	views, err := s.views(sessionFrom(r).User.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"connectors": views, "now": s.now().UnixMilli()})
}

func (s *Server) apiRename(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &in); err != nil || strings.TrimSpace(in.Name) == "" {
		writeJSON(w, http.StatusBadRequest, relay.ErrorBody{Error: "bad_request"})
		return
	}
	if err := s.cons.Rename(sessionFrom(r).User.ID, r.PathValue("id"), in.Name); err != nil {
		writeJSON(w, http.StatusNotFound, relay.ErrorBody{Error: "not_found"})
		return
	}
	s.hub.notify(sessionFrom(r).User.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) apiRevoke(w http.ResponseWriter, r *http.Request) {
	u := sessionFrom(r).User
	id := r.PathValue("id")
	if err := s.cons.Revoke(u.ID, id, ipOf(r), "web"); err != nil {
		writeJSON(w, http.StatusNotFound, relay.ErrorBody{Error: "not_found"})
		return
	}
	s.hub.Disconnect(id)
	s.hub.notify(u.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- linking (browser side) ----

func (s *Server) apiLinkCode(w http.ResponseWriter, r *http.Request) {
	u := sessionFrom(r).User
	if !s.rate.Allow("linkcode:"+u.ID, 20, time.Hour) {
		writeJSON(w, http.StatusTooManyRequests, relay.ErrorBody{Error: "rate_limited"})
		return
	}
	code, id, exp, err := s.cons.CreateWebCode(u.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.acc.event(u.ID, "link_code_created", ipOf(r), nil)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "code": FormatLinkCode(code), "expiresAt": exp.UnixMilli(), "publicUrl": s.cfg.PublicURL})
}

func (s *Server) apiLinkCodeStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.cons.WebCodeStatus(sessionFrom(r).User.ID, r.PathValue("id"))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if st == nil {
		writeJSON(w, http.StatusNotFound, relay.ErrorBody{Error: "not_found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": st.State, "connectorId": st.ConnectorID, "name": st.Name, "expiresAt": st.Expires.UnixMilli()})
}

func (s *Server) apiLinkClaim(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code string `json:"code"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, relay.ErrorBody{Error: "bad_request"})
		return
	}
	err := s.cons.Claim(ipOf(r), sessionFrom(r).User.ID, in.Code)
	var le *LockedError
	switch {
	case errors.As(err, &le):
		writeJSON(w, http.StatusTooManyRequests, relay.ErrorBody{Error: "rate_limited"})
	case errors.Is(err, ErrInvalid):
		writeJSON(w, http.StatusNotFound, relay.ErrorBody{Error: "invalid_code"})
	case err != nil:
		s.serverError(w, r, err)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// ---- pairing (browser side) ----

func (s *Server) apiPairStart(w http.ResponseWriter, r *http.Request) {
	u := sessionFrom(r).User
	var in struct {
		Commit string `json:"commit"`
		Label  string `json:"label"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, relay.ErrorBody{Error: "bad_request"})
		return
	}
	if !s.rate.Allow("pair:"+u.ID, 30, 10*time.Minute) {
		writeJSON(w, http.StatusTooManyRequests, relay.ErrorBody{Error: "rate_limited"})
		return
	}
	cons, err := s.cons.ForUser(u.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p, err := s.hub.StartPairing(u.ID, in.Commit, in.Label, r.UserAgent(), cons)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, relay.ErrorBody{Error: "bad_request"})
		return
	}
	s.acc.event(u.ID, "pairing_started", ipOf(r), nil)
	v, _ := s.hub.PairingView(u.ID, p.ID)
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) apiPairGet(w http.ResponseWriter, r *http.Request) {
	v, err := s.hub.PairingView(sessionFrom(r).User.ID, r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, relay.ErrorBody{Error: "not_found"})
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) apiPairReveal(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ConnectorID string `json:"connectorId"`
		SPKI        string `json:"spki"`
		Nonce       string `json:"nonce"`
	}
	if err := decodeJSON(r, &in); err != nil || len(in.SPKI) > 400 || len(in.Nonce) > 100 {
		writeJSON(w, http.StatusBadRequest, relay.ErrorBody{Error: "bad_request"})
		return
	}
	if err := s.hub.Reveal(sessionFrom(r).User.ID, r.PathValue("id"), in.ConnectorID, in.SPKI, in.Nonce); err != nil {
		writeJSON(w, http.StatusNotFound, relay.ErrorBody{Error: "not_found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// apiPresence streams the connector list whenever it changes (online, offline, kill switch,
// devices, pairing progress). It carries metadata only, never workspace data.
func (s *Server) apiPresence(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		deny(w, http.StatusInternalServerError, "Streaming unsupported")
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	u := sessionFrom(r).User
	ch, cancel := s.hub.Subscribe(u.ID)
	defer cancel()
	send := func() bool {
		views, err := s.views(u.ID)
		if err != nil {
			return false
		}
		b, _ := json.Marshal(map[string]any{"connectors": views, "now": s.now().UnixMilli()})
		if _, err := fmt.Fprintf(w, "event: connectors\ndata: %s\n\n", b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !send() {
		return
	}
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			// Coalesce bursts.
			time.Sleep(50 * time.Millisecond)
			select {
			case <-ch:
			default:
			}
			if !send() {
				return
			}
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
