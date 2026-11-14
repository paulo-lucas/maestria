package connect

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/paulo-lucas/maestria/internal/audit"
	"github.com/paulo-lucas/maestria/internal/devices"
	"github.com/paulo-lucas/maestria/internal/relay"
)

// Gate decides whether a relayed request may run (docs/hosted-protocol.md, section 7.2). It trusts
// nothing the cloud says: the connector id is its own, and everything else is checked against the
// device's signature and the machine's policy.
type Gate struct {
	ConnectorID string
	Devices     *devices.Manager
	Killed      func() bool
	Audit       *audit.Log
}

// Request is a relayed request as the connector received it, headers lower-cased.
type Request struct {
	Method string
	Path   string
	Query  string
	Header map[string]string
	Body   []byte
}

// Check runs the gate. It returns the verified device, or the error code to answer with.
func (g *Gate) Check(r Request) (*devices.Device, string) {
	if g.Killed != nil && g.Killed() {
		return nil, relay.ErrKillSwitch
	}
	if !g.Devices.Policy().Enabled {
		return nil, relay.ErrRelayDisabled
	}
	if len(r.Body) > relay.MaxRequestBody {
		return nil, relay.ErrTooLarge
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		return nil, relay.ErrBadRequest
	}
	if relay.ValidatePath(r.Path) != nil {
		return nil, relay.ErrBadRequest
	}
	h := r.Header
	if h[strings.ToLower(relay.HeaderSig)] == "" || h[strings.ToLower(relay.HeaderDevice)] == "" ||
		h[strings.ToLower(relay.HeaderNonce)] == "" || h[strings.ToLower(relay.HeaderTS)] == "" || h[strings.ToLower(relay.HeaderBody)] == "" {
		return nil, relay.ErrUnsigned
	}
	ts, err := strconv.ParseInt(h[strings.ToLower(relay.HeaderTS)], 10, 64)
	if err != nil {
		return nil, relay.ErrBadRequest
	}
	dev, code := g.Devices.Verify(g.ConnectorID, devices.Request{
		ConnectorID: g.ConnectorID,
		DeviceID:    h[strings.ToLower(relay.HeaderDevice)],
		Method:      r.Method, Path: r.Path, RawQuery: r.Query,
		BodyHash: h[strings.ToLower(relay.HeaderBody)], TS: ts,
		Nonce: h[strings.ToLower(relay.HeaderNonce)], Sig: h[strings.ToLower(relay.HeaderSig)],
		Body: r.Body,
	})
	if code != "" {
		return nil, code
	}
	if !relay.Allowed(r.Method, r.Path) {
		return dev, relay.ErrRouteForbidden
	}
	if !g.Devices.Allow(dev.ID, relay.IsWrite(r.Method)) {
		return dev, relay.ErrRateLimited
	}
	return dev, ""
}

// Refused records a refusal in the local audit log (throttled per code, so a flood of bad
// requests cannot fill the table). Paths are logged, never bodies or signatures.
func (g *Gate) Refused(code, device, method, path string) {
	if g.Audit == nil {
		return
	}
	_ = g.Audit.WriteThrottled("relay:"+code+":"+device, time.Minute, "relay_refused", "relay",
		map[string]any{"code": code, "device": device, "method": method, "path": clip(path, 100)})
}

// Wrote records an executed relayed write (path and outcome only).
func (g *Gate) Wrote(device, method, path string, status int) {
	if g.Audit == nil || method == http.MethodGet {
		return
	}
	_ = g.Audit.Write("relay_write", "relay", map[string]any{"device": device, "method": method, "path": clip(path, 100), "status": status})
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
