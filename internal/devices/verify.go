package devices

import (
	"crypto/subtle"
	"errors"
	"time"

	"github.com/paulo-lucas/maestria/internal/relay"
)

// Request is a relayed request as the connector received it.
type Request struct {
	ConnectorID string
	DeviceID    string
	Method      string
	Path        string
	RawQuery    string
	BodyHash    string // the X-Maestria-Body header
	TS          int64
	Nonce       string
	Sig         string
	Body        []byte
}

// Verify runs steps 4 to 9 of the connector gate (docs/hosted-protocol.md, section 7.2): device,
// timestamp, signature, body hash, nonce replay and session policy. It returns the device or the
// error code to answer with. connectorID is this connector's own id, never taken from the request.
func (m *Manager) Verify(connectorID string, rq Request) (*Device, string) {
	signed := relay.SignedRequest{
		ConnectorID: connectorID, DeviceID: rq.DeviceID, Method: rq.Method, Path: rq.Path,
		RawQuery: rq.RawQuery, BodySHA256: rq.BodyHash, TS: rq.TS, Nonce: rq.Nonce,
	}
	if err := relay.ValidateSigned(signed); err != nil {
		return nil, relay.ErrBadRequest
	}

	dev, err := m.Get(rq.DeviceID)
	if errors.Is(err, ErrNoDevice) {
		return nil, relay.ErrUnknownDevice
	}
	if err != nil {
		return nil, relay.ErrInternal
	}
	if dev.State == "revoked" {
		return nil, relay.ErrDeviceRevoked
	}

	now := m.now()
	skew := now.UnixMilli() - rq.TS
	if skew > relay.ClockWindow.Milliseconds() || skew < -relay.ClockWindow.Milliseconds() {
		return nil, relay.ErrStaleTimestamp
	}

	raw, err := relay.Dec(dev.SPKI)
	if err != nil {
		return nil, relay.ErrInternal
	}
	pub, err := relay.ParseSPKI(raw)
	if err != nil {
		return nil, relay.ErrInternal
	}
	if !relay.VerifySignature(pub, signed.Canonical(), rq.Sig) {
		return nil, relay.ErrBadSignature
	}

	// The signature covers the claimed hash, so a cloud that swaps the body is caught here.
	got := relay.BodyHash(rq.Body)
	if subtle.ConstantTimeCompare([]byte(got), []byte(rq.BodyHash)) != 1 {
		return nil, relay.ErrBodyMismatch
	}

	if code := m.claimNonce(dev.ID, rq.Nonce, rq.TS); code != "" {
		return nil, code
	}

	if code := m.policyCheck(dev, now); code != "" {
		return nil, code
	}
	m.touchDevice(dev.ID, now)
	return dev, ""
}

// Revalidate re-checks a device that already passed Verify, for long-lived streams. It returns an
// error code when the device may no longer be served.
func (m *Manager) Revalidate(id string) string {
	dev, err := m.Get(id)
	if err != nil {
		return relay.ErrUnknownDevice
	}
	if dev.State == "revoked" {
		return relay.ErrDeviceRevoked
	}
	p := m.policy()
	now := m.now().UnixMilli()
	if dev.State == "expired" || now-dev.ApprovedAt >= p.Absolute.Milliseconds() {
		return relay.ErrApprovalExpired
	}
	// An open stream is use: only the absolute limit and revocation end it.
	return ""
}

func (m *Manager) policyCheck(dev *Device, now time.Time) string {
	p := m.policy()
	ms := now.UnixMilli()
	switch {
	case dev.State == "expired", ms-dev.ApprovedAt >= p.Absolute.Milliseconds():
		m.markExpired(dev.ID)
		return relay.ErrApprovalExpired
	case ms-dev.LastUsedAt >= p.Idle.Milliseconds():
		m.markExpired(dev.ID)
		return relay.ErrIdleExpired
	}
	return ""
}

func (m *Manager) markExpired(id string) {
	_, _ = m.db.Exec("UPDATE devices SET state = 'expired' WHERE id = ? AND state = 'active'", id)
}

func (m *Manager) touchDevice(id string, now time.Time) {
	ms := now.UnixMilli()
	m.mu.Lock()
	last := m.touch[id]
	if ms-last < touchEvery.Milliseconds() {
		m.mu.Unlock()
		return
	}
	m.touch[id] = ms
	m.mu.Unlock()
	_, _ = m.db.Exec("UPDATE devices SET last_used_at = ? WHERE id = ?", ms, id)
}

// claimNonce records (device, nonce) for the replay window. It returns an error code when the
// nonce was seen, or when the cache is full: it fails closed instead of evicting live entries,
// because eviction would reopen a replay.
func (m *Manager) claimNonce(device, nonce string, ts int64) string {
	now := m.now().UnixMilli()
	key := device + "|" + nonce
	m.mu.Lock()
	defer m.mu.Unlock()
	if exp, seen := m.nonces[key]; seen && exp > now {
		return relay.ErrReplay
	}
	if len(m.nonces) >= relay.MaxNonces/2 {
		for k, exp := range m.nonces {
			if exp <= now {
				delete(m.nonces, k)
			}
		}
	}
	if len(m.nonces) >= relay.MaxNonces {
		return relay.ErrRateLimited
	}
	m.nonces[key] = ts + relay.ClockWindow.Milliseconds() + 1000
	return ""
}

// Allow applies the per-device rate limit (fixed one-minute window; a write counts double).
func (m *Manager) Allow(device string, write bool) bool {
	limit := m.policy().RatePerMinute
	if limit <= 0 {
		return true
	}
	cost := 1
	if write {
		cost = 2
	}
	now := m.now().UnixMilli()
	m.mu.Lock()
	defer m.mu.Unlock()
	w := m.rate[device]
	if w == nil || now-w.start >= 60000 {
		if len(m.rate) > 1000 {
			m.rate = map[string]*window{}
		}
		w = &window{start: now}
		m.rate[device] = w
	}
	if w.count+cost > limit {
		return false
	}
	w.count += cost
	return true
}
