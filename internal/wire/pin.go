package wire

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
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
