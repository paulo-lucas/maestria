package wire

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"
)

// Device identifiers make a re-pair replace this connector's own records instead of piling up
// new devices on the host.
const (
	ownerIdentifier = "maestria-owner"
	guestIdentifier = "maestria-guest"
)

var codeRE = regexp.MustCompile(`^[0-9]{6}$`)

// IsPairingCode reports whether s looks like Wire's six-digit pairing code (as opposed to the
// host's pairing password).
func IsPairingCode(s string) bool { return codeRE.MatchString(s) }

// PairResult is the answer to POST /pair.
type PairResult struct {
	Token    string `json:"token"`
	DeviceID string `json:"deviceId"`
	Role     string `json:"role"`
}

// Pair performs POST /pair with a six-digit code or a password.
func Pair(ctx context.Context, host string, pin Pin, deviceName, identifier, secret string) (PairResult, error) {
	c := NewClient(host, pin, ScopeSetup, "", "")
	body := map[string]string{"deviceName": deviceName, "deviceIdentifier": identifier}
	if IsPairingCode(secret) {
		body["code"] = secret
	} else {
		body["password"] = secret
	}
	var res PairResult
	err := c.do(ctx, http.MethodPost, "/pair", body, &res)
	// Never let the secret reach an error message.
	err = scrub(err, secret)
	if err != nil {
		return PairResult{}, err
	}
	if res.Token == "" || res.DeviceID == "" {
		return PairResult{}, errors.New("wire: the host did not return a token")
	}
	return res, nil
}

// PairMessage turns a pairing failure into what the person should do next.
func PairMessage(err error) string {
	var ae *APIError
	var pe *PinError
	switch {
	case errors.As(err, &pe):
		return "The security key does not match the host's certificate. Copy it again from Maestri: Settings → Wire → Manual → Security key."
	case errors.As(err, &ae):
		switch ae.Status {
		case http.StatusUnauthorized:
			return "Wrong pairing code or password."
		case http.StatusTooManyRequests:
			if ae.RetryAfter > 0 {
				return fmt.Sprintf("Too many attempts. Wait %s and try again.", ae.RetryAfter.Round(time.Second))
			}
			return "Too many attempts. Wait a bit and try again."
		case http.StatusServiceUnavailable:
			return "Password pairing is not enabled on this host. Use a pairing code from Settings → Wire → Pairing."
		case http.StatusUnsupportedMediaType:
			return "The host refused the request format (this is a bug in Maestria)."
		}
		return err.Error()
	}
	return "Could not reach Maestri Wire. Check that it is on in Settings → Wire, and the host and port."
}

// Provision pairs this connector twice and returns the credentials to store: an owner device for
// the allowlisted writes and a guest device for every read and subscription. Wire hands out owner
// by default, so the second device is demoted with the first one's authority (the owner device is
// never demoted). nextSecret is asked for a second code when the first one was a single-use code.
func Provision(ctx context.Context, host string, pin Pin, deviceName string, first string, nextSecret func() (string, error)) (*Credentials, error) {
	owner, err := Pair(ctx, host, pin, deviceName+" (owner)", ownerIdentifier, first)
	if err != nil {
		return nil, err
	}
	// From here on, a failure must not leave a live owner token behind.
	cleanup := func() {
		c := NewClient(host, pin, ScopeWrite, owner.Token, owner.DeviceID)
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = c.DeleteDevice(cctx)
	}
	second := first
	if IsPairingCode(first) {
		second, err = nextSecret() // a code is single use
		if err != nil {
			cleanup()
			return nil, err
		}
	}
	guest, err := Pair(ctx, host, pin, deviceName+" (read)", guestIdentifier, second)
	if err != nil {
		cleanup()
		return nil, err
	}
	creds := &Credentials{
		Host: host, Pin: pin.Hex(), DeviceName: deviceName, PairedAt: time.Now().UnixMilli(),
		Guest: Credential{Token: guest.Token, DeviceID: guest.DeviceID},
		Owner: &Credential{Token: owner.Token, DeviceID: owner.DeviceID},
	}
	fail := func(err error) (*Credentials, error) {
		g := NewClient(host, pin, ScopeRead, guest.Token, guest.DeviceID)
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = g.DeleteDevice(cctx)
		cleanup()
		return nil, scrub(err, owner.Token, guest.Token)
	}
	if guest.Role != "guest" {
		setup := NewClient(host, pin, ScopeSetup, owner.Token, owner.DeviceID)
		if err := setup.SetGuestRole(ctx, guest.DeviceID); err != nil {
			return fail(fmt.Errorf("could not make the read device read-only: %w", err))
		}
	}
	// Verify both roles before trusting them: a "guest" that is really an owner would wake
	// terminals on every subscription.
	gi, err := NewClient(host, pin, ScopeRead, guest.Token, guest.DeviceID).Info(ctx)
	if err != nil {
		return fail(err)
	}
	if gi.Role != "guest" {
		return fail(errors.New("the host did not make the read device read-only"))
	}
	if gi.ProtocolVersion != ProtocolVersion {
		return fail(fmt.Errorf("the host speaks Wire protocol %d; this Maestria speaks %d", gi.ProtocolVersion, ProtocolVersion))
	}
	oi, err := NewClient(host, pin, ScopeWrite, owner.Token, owner.DeviceID).Info(ctx)
	if err != nil {
		return fail(err)
	}
	if oi.Role != "owner" {
		return fail(errors.New("the host did not give the write device the owner role"))
	}
	return creds, nil
}
