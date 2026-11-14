package wire

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Sentinel errors.
var (
	// ErrNotPaired means wire.json does not exist.
	ErrNotPaired = errors.New("wire: not paired")
	// ErrNotConnected means the Wire link is down, so there is no live data to serve.
	ErrNotConnected = errors.New("wire: not connected")
	// ErrNoCapability means the host does not advertise what the feature needs.
	ErrNoCapability = errors.New("wire: the host does not support this")
	// ErrNoOwner means only the guest token is held, so no write can be made.
	ErrNoOwner = errors.New("wire: no owner token (writes are off)")
	// ErrNotAllowed is returned for every request outside the allowlist.
	ErrNotAllowed = errors.New("wire: request is not allowed")
	// ErrUnknownTerminal means the terminal is not in the workspace's current feed.
	ErrUnknownTerminal = errors.New("wire: terminal is not in this workspace")
	// ErrPromptChanged means the Y/n the human saw is no longer the one pending.
	ErrPromptChanged = errors.New("wire: the prompt changed")
	// ErrAmbiguous means a send may or may not have been delivered. It is never retried.
	ErrAmbiguous = errors.New("wire: delivery unconfirmed")
)

// PinError is returned when the host's certificate does not match the pinned key.
type PinError struct{ Got, Want string }

func (e *PinError) Error() string {
	return "the host's security key does not match the pinned key (expected " + short(e.Want) + ", got " + short(e.Got) + ")"
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12] + "…"
	}
	return h
}

// APIError is an error response from the host.
type APIError struct {
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = e.Code
	}
	if msg == "" {
		msg = "request failed"
	}
	return fmt.Sprintf("wire: %s (HTTP %d)", msg, e.Status)
}

// redacted wraps an error whose text had a secret removed, keeping errors.Is and errors.As working.
type redacted struct {
	msg string
	err error
}

func (r *redacted) Error() string { return r.msg }
func (r *redacted) Unwrap() error { return r.err }

// scrub removes secrets from an error's text.
func scrub(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	out := msg
	for _, s := range secrets {
		if s != "" {
			out = strings.ReplaceAll(out, s, "[redacted]")
		}
	}
	if out == msg {
		return err
	}
	return &redacted{msg: out, err: err}
}

// State is the health of the Wire link as the panel shows it.
type State string

// Link states.
const (
	StateDisabled     State = "disabled"     // not paired
	StateConnecting   State = "connecting"   // paired, first contact pending
	StateConnected    State = "connected"    // live
	StateUnreachable  State = "unreachable"  // Wire is off, or the host is down
	StatePinMismatch  State = "pin_mismatch" // certificate changed: refused
	StateRevoked      State = "revoked"      // token rejected (401)
	StateIncompatible State = "incompatible" // protocolVersion is not 1
	StateRole         State = "role"         // a token has the wrong role
)

// Classify turns an error from the host into a link state.
func Classify(err error) State {
	var pe *PinError
	if errors.As(err, &pe) {
		return StatePinMismatch
	}
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == 401 {
		return StateRevoked
	}
	return StateUnreachable
}
