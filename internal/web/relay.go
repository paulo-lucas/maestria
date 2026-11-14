package web

import (
	"context"
	"net/http"
	"time"

	"github.com/maestria-dev/maestria/internal/auth"
	"github.com/maestria-dev/maestria/internal/relay"
)

// RelayHandler serves relayed (hosted-mode) requests with the same handlers as local mode. It
// replaces the local cookie, CSRF, Origin and Host checks, which the connector's relay gate does
// instead (signature, allowlist, policy), so it must only ever be called after that gate passed.
// The handlers see a fixed internal session that never reaches a browser.
func (s *Server) RelayHandler() http.Handler {
	sess := &auth.Session{ID: "relay", CSRFToken: "relay", ExpiresAt: time.Now().Add(100 * 365 * 24 * time.Hour)}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), sessionKey, sess)
		ctx = context.WithValue(ctx, ipKey, "relay")
		w.Header().Set("Cache-Control", "no-store")
		s.mux.ServeHTTP(w, r.WithContext(ctx))
	})
}

// apiRelayInfo reports what the connector enforces for relayed requests (read-only).
func (s *Server) apiRelayInfo(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"killed": s.Tunnel.Killed(), "connector": s.ConnectorName}
	if s.Devices != nil {
		p := s.Devices.Policy()
		out["enabled"] = p.Enabled
		out["policy"] = map[string]any{
			"approvalAbsoluteDays": int(p.Absolute / (24 * time.Hour)),
			"idleDays":             int(p.Idle / (24 * time.Hour)),
			"ratePerMinute":        p.RatePerMinute,
		}
	} else {
		out["enabled"] = false
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiDevices(w http.ResponseWriter, r *http.Request) {
	if s.Devices == nil {
		writeJSON(w, http.StatusOK, map[string]any{"devices": []relay.DeviceInfo{}})
		return
	}
	infos, err := s.Devices.Infos()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
		return
	}
	for i := range infos {
		infos[i].PublicKey = ""
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": infos, "current": relay.DeviceFrom(r.Context())})
}

// apiDeviceRevoke only ever tightens: it ends a device. Approving one is local-only.
func (s *Server) apiDeviceRevoke(w http.ResponseWriter, r *http.Request) {
	if s.Devices == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	id := r.PathValue("id")
	if err := s.Devices.Revoke(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	_ = s.Audit.Write("device_revoked", ipOf(r), map[string]any{"device": id, "by": relay.DeviceFrom(r.Context())})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
