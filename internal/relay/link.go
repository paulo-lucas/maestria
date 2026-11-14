package relay

import (
	"context"
	"errors"
	"net/url"
	"strings"
)

// Link API (connector to cloud, no cookies). docs/hosted-protocol.md, section 4.

// Machine describes the connector's machine to the cloud.
type Machine struct {
	Host    string `json:"host"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	Version string `json:"version"`
}

// RedeemRequest is POST /api/link/redeem.
type RedeemRequest struct {
	Code string `json:"code"`
	Machine
}

// RedeemResponse answers it. The code is not consumed yet.
type RedeemResponse struct {
	AccountHint string `json:"accountHint"`
	DefaultName string `json:"defaultName"`
	Token       string `json:"token"`
}

// ConfirmRequest is POST /api/link/confirm.
type ConfirmRequest struct {
	Code  string `json:"code"`
	Token string `json:"token"`
	Name  string `json:"name"`
}

// Linked is what the connector receives once, after confirming.
type Linked struct {
	ConnectorID string `json:"connectorId"`
	Credential  string `json:"credential"`
	Name        string `json:"name"`
}

// StartRequest is POST /api/link/start (the code is shown by the CLI).
type StartRequest struct{ Machine }

// StartResponse answers it.
type StartResponse struct {
	Code      string `json:"code"`
	PollToken string `json:"pollToken"`
	ExpiresAt int64  `json:"expiresAt"`
	URL       string `json:"url"`
}

// PollRequest is POST /api/link/poll.
type PollRequest struct {
	PollToken string `json:"pollToken"`
	Confirm   bool   `json:"confirm,omitempty"`
	Name      string `json:"name,omitempty"`
}

// PollResponse answers it. Linked is set only on the confirming call.
type PollResponse struct {
	Status      string  `json:"status"` // pending | claimed | expired | linked
	AccountHint string  `json:"accountHint,omitempty"`
	DefaultName string  `json:"defaultName,omitempty"`
	Linked      *Linked `json:"linked,omitempty"`
}

// ErrorBody is the JSON body of every refusal.
type ErrorBody struct {
	Error string `json:"error"`
}

// CredentialPrefix starts every connector credential.
const CredentialPrefix = "mc1."

// FormatCredential builds mc1.<id>.<secret>.
func FormatCredential(id, secret string) string { return CredentialPrefix + id + "." + secret }

// ParseCredential splits a credential into connector id and secret.
func ParseCredential(c string) (id, secret string, ok bool) {
	if !strings.HasPrefix(c, CredentialPrefix) || len(c) > 200 {
		return "", "", false
	}
	parts := strings.Split(c[len(CredentialPrefix):], ".")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "cn_") || len(parts[0]) < 8 || len(parts[1]) < 32 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// SocketURL turns the cloud's base URL into the connector WebSocket URL. It refuses plain http
// and ws unless the host is loopback, so a credential never travels in clear text.
func SocketURL(base string) (string, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || u.Host == "" {
		return "", errors.New("invalid cloud URL")
	}
	switch u.Scheme {
	case "https", "wss":
		u.Scheme = "wss"
	case "http", "ws":
		if !IsLoopbackHost(u.Hostname()) {
			return "", errors.New("the cloud URL must be https (plain http is only allowed for localhost)")
		}
		u.Scheme = "ws"
	default:
		return "", errors.New("the cloud URL must start with https://")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/connector"
	u.RawQuery, u.Fragment = "", ""
	return u.String(), nil
}

// APIURL joins the cloud base URL and an API path, with the same https rule.
func APIURL(base, path string) (string, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || u.Host == "" {
		return "", errors.New("invalid cloud URL")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !IsLoopbackHost(u.Hostname()) {
			return "", errors.New("the cloud URL must be https (plain http is only allowed for localhost)")
		}
	default:
		return "", errors.New("the cloud URL must start with https://")
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	return u.String(), nil
}

// IsLoopbackHost reports whether host is localhost or a loopback address.
func IsLoopbackHost(host string) bool {
	h := strings.ToLower(host)
	return h == "localhost" || h == "127.0.0.1" || h == "::1" || h == "[::1]" || strings.HasSuffix(h, ".localhost")
}

type ctxKey int

const deviceKey ctxKey = 1

// WithDevice records which device a relayed request was verified for.
func WithDevice(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, deviceKey, id)
}

// DeviceFrom returns the verified device id of a relayed request, or "".
func DeviceFrom(ctx context.Context) string {
	s, _ := ctx.Value(deviceKey).(string)
	return s
}
