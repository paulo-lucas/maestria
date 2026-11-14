package wire

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"strings"
)

// Pin is the SHA-256 of the host certificate's SubjectPublicKeyInfo (DER).
type Pin [32]byte

// Hex returns the lowercase hex form used in wire.json.
func (p Pin) Hex() string { return hex.EncodeToString(p[:]) }

// ParsePin accepts the "Security key" from Maestri's Manual tab (hex, optionally colon- or
// space-separated) or the QR code's base64 serverKeyHash.
func ParsePin(s string) (Pin, error) {
	var p Pin
	clean := strings.NewReplacer(":", "", " ", "", "\t", "", "\n", "", "\r", "").Replace(strings.TrimSpace(s))
	if len(clean) == 64 {
		if b, err := hex.DecodeString(clean); err == nil {
			copy(p[:], b)
			return p, nil
		}
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(clean); err == nil && len(b) == 32 {
			copy(p[:], b)
			return p, nil
		}
	}
	return p, errors.New("the security key must be the 64-digit hex key from the Manual tab, or the base64 key from the QR code")
}

// FormatPin renders a pin the way Maestri's Manual tab shows it: upper-case hex pairs joined by
// colons.
func FormatPin(p Pin) string {
	h := strings.ToUpper(p.Hex())
	parts := make([]string, 0, len(h)/2)
	for i := 0; i < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":")
}

// IsLoopbackHost reports whether host (name or address, with or without port) is this machine.
func IsLoopbackHost(host string) bool {
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	h = strings.Trim(strings.ToLower(h), "[]")
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// FetchPin connects to a Wire host on this machine and returns the pin of the certificate it
// presents, so pairing on the same machine can trust it on first use. It only completes a TLS
// handshake and sends no request. It refuses any host that is not a loopback address.
func FetchPin(ctx context.Context, host string) (Pin, error) {
	if !IsLoopbackHost(host) {
		return Pin{}, errors.New("trust on first use only applies to Wire on this machine")
	}
	d := tls.Dialer{Config: &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         "localhost",
		InsecureSkipVerify: true, //nolint:gosec // reading the certificate to pin it; nothing is sent
	}}
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return Pin{}, err
	}
	defer conn.Close()
	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return Pin{}, errors.New("Wire presented no certificate")
	}
	return SPKIHash(certs[0]), nil
}

// SPKIHash returns the pin of a certificate.
func SPKIHash(cert *x509.Certificate) Pin { return sha256.Sum256(cert.RawSubjectPublicKeyInfo) }

// TLSConfig returns a TLS config that trusts exactly the pinned public key. Wire's certificate is
// self-signed, so the usual chain validation cannot apply: the handshake is accepted only when
// the leaf's SPKI hash equals the pin, and refused otherwise. There is no way to skip it.
func TLSConfig(pin Pin) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: "localhost",
		// Chain verification is replaced by VerifyConnection, which runs on every connection
		// (resumed ones included) and is mandatory here.
		InsecureSkipVerify: true, //nolint:gosec // pinned SPKI check follows
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return &PinError{Want: pin.Hex()}
			}
			got := SPKIHash(cs.PeerCertificates[0])
			if got != pin {
				return &PinError{Got: got.Hex(), Want: pin.Hex()}
			}
			return nil
		},
	}
}
