// Package relay is the wire contract between the Maestria cloud, the browser and the connector:
// frames, error codes, the canonical signing string, device ids, the pairing code and the route
// allowlist. It has no I/O. See docs/hosted-protocol.md, which this package implements.
package relay

import (
	"encoding/base64"
	"time"
)

// Proto is the protocol version carried in hello.
const Proto = 1

// Header names of a signed browser request.
const (
	HeaderDevice = "X-Maestria-Device"
	HeaderTS     = "X-Maestria-Ts"
	HeaderNonce  = "X-Maestria-Nonce"
	HeaderBody   = "X-Maestria-Body"
	HeaderSig    = "X-Maestria-Sig"
)

// Limits and windows (docs/hosted-protocol.md, section 7).
const (
	ClockWindow       = 60 * time.Second
	NonceTTL          = 2 * ClockWindow
	MaxNonces         = 50000
	MaxRequestBody    = 64 * 1024
	MaxResponseBody   = 8 * 1024 * 1024
	MaxFrame          = 256 * 1024
	BodyChunk         = 32 * 1024
	MaxHeaderBytes    = 8 * 1024
	FlowWindow        = 256 * 1024
	MaxInFlight       = 32
	MaxStreamsPerUser = 8
	HeadTimeout       = 30 * time.Second
	RequestTimeout    = 60 * time.Second
	PingInterval      = 20 * time.Second
	DeadAfter         = 60 * time.Second
	StreamRecheck     = 15 * time.Second
)

// Frame types.
const (
	TypeHello         = "hello"
	TypeWelcome       = "welcome"
	TypeReq           = "req"
	TypeBody          = "body"
	TypeRes           = "res"
	TypeChunk         = "chunk"
	TypeErr           = "err"
	TypeAck           = "ack"
	TypeCancel        = "cancel"
	TypePairOffer     = "pair_offer"
	TypePairChallenge = "pair_challenge"
	TypePairReveal    = "pair_reveal"
	TypePairState     = "pair_state"
	TypeDevices       = "devices"
	TypeForget        = "forget_devices"
)

// Pairing states reported by the connector.
const (
	PairPending  = "pending"
	PairRevealed = "revealed"
	PairApproved = "approved"
	PairRejected = "rejected"
	PairExpired  = "expired"
	PairMismatch = "mismatch"
)

// DeviceInfo is the display summary of a paired device that the connector reports to the cloud.
type DeviceInfo struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	State      string `json:"state"` // active | revoked | expired
	ApprovedAt int64  `json:"approvedAt,omitempty"`
	LastUsedAt int64  `json:"lastUsedAt,omitempty"`
	PublicKey  string `json:"publicKey,omitempty"` // SPKI, b64u (display only for the cloud)
}

// Frame is one JSON message on the connector socket. Fields are used per type (see the doc).
type Frame struct {
	T string `json:"t"`

	// Requests and responses.
	ID     string            `json:"id,omitempty"`
	Method string            `json:"m,omitempty"`
	Path   string            `json:"p,omitempty"`
	Query  string            `json:"q,omitempty"`
	Header map[string]string `json:"h,omitempty"`
	Body   string            `json:"b,omitempty"` // b64u
	More   bool              `json:"more,omitempty"`
	Status int               `json:"s,omitempty"`
	Code   string            `json:"code,omitempty"`
	N      int64             `json:"n,omitempty"`

	// Pairing.
	Pair   string `json:"pair,omitempty"`
	Commit string `json:"commit,omitempty"`
	Label  string `json:"label,omitempty"`
	UA     string `json:"ua,omitempty"`
	R      string `json:"r,omitempty"`
	SPKI   string `json:"spki,omitempty"`
	Nonce  string `json:"nonce,omitempty"`
	State  string `json:"state,omitempty"`

	// Hello, welcome, devices, forget.
	Proto      int          `json:"proto,omitempty"`
	Version    string       `json:"version,omitempty"`
	OS         string       `json:"os,omitempty"`
	Arch       string       `json:"arch,omitempty"`
	Name       string       `json:"name,omitempty"`
	KillSwitch bool         `json:"killSwitch,omitempty"`
	Devices    []DeviceInfo `json:"devices,omitempty"`
	ServerTime int64        `json:"serverTime,omitempty"`
	At         int64        `json:"at,omitempty"`
}

// Enc and Dec are the base64url (no padding) codec used for every binary value.
var enc = base64.RawURLEncoding

// Enc encodes bytes as b64u.
func Enc(b []byte) string { return enc.EncodeToString(b) }

// Dec decodes b64u.
func Dec(s string) ([]byte, error) { return enc.DecodeString(s) }

// Error codes. Every refusal by the connector carries one of these.
const (
	ErrBadRequest      = "bad_request"
	ErrUnsigned        = "unsigned"
	ErrBadSignature    = "bad_signature"
	ErrBodyMismatch    = "body_mismatch"
	ErrUnknownDevice   = "unknown_device"
	ErrDeviceRevoked   = "device_revoked"
	ErrApprovalExpired = "approval_expired"
	ErrIdleExpired     = "idle_expired"
	ErrStaleTimestamp  = "stale_timestamp"
	ErrReplay          = "replay"
	ErrRouteForbidden  = "route_forbidden"
	ErrKillSwitch      = "kill_switch"
	ErrRelayDisabled   = "relay_disabled"
	ErrTooLarge        = "too_large"
	ErrRateLimited     = "rate_limited"
	ErrInternal        = "internal"

	// Raised by the cloud itself.
	ErrConnectorOffline = "connector_offline"
	ErrConnectorBusy    = "connector_busy"
	ErrGatewayTimeout   = "gateway_timeout"
)

// StatusFor is the HTTP status the cloud uses for an error code.
func StatusFor(code string) int {
	switch code {
	case ErrBadRequest:
		return 400
	case ErrUnsigned:
		return 401
	case ErrBadSignature, ErrBodyMismatch, ErrUnknownDevice, ErrDeviceRevoked, ErrApprovalExpired,
		ErrIdleExpired, ErrStaleTimestamp, ErrReplay, ErrRouteForbidden, ErrKillSwitch, ErrRelayDisabled:
		return 403
	case ErrTooLarge:
		return 413
	case ErrRateLimited:
		return 429
	case ErrConnectorOffline, ErrConnectorBusy:
		return 503
	case ErrGatewayTimeout:
		return 504
	}
	return 500
}
