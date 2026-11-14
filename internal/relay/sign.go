package relay

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// SigningVersion is the first line of the canonical signing string.
const SigningVersion = "maestria-relay-v1"

// SignedRequest is everything a device signs for one relayed request.
type SignedRequest struct {
	ConnectorID string
	DeviceID    string
	Method      string
	Path        string
	RawQuery    string
	BodySHA256  string // lower-case hex
	TS          int64  // Unix ms
	Nonce       string
}

// Canonical returns the exact string that is signed (docs/hosted-protocol.md, section 7.1).
func (s SignedRequest) Canonical() string {
	return strings.Join([]string{
		SigningVersion,
		s.ConnectorID,
		s.DeviceID,
		strings.ToUpper(s.Method),
		s.Path,
		s.RawQuery,
		s.BodySHA256,
		strconv.FormatInt(s.TS, 10),
		s.Nonce,
	}, "\n")
}

// BodyHash is the lower-case hex sha256 of a body.
func BodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// cleanField reports whether s is free of characters that could split or smuggle a field of the
// canonical string.
func cleanField(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] == 0x7f {
			return false
		}
	}
	return true
}

// ValidateSigned checks the shape of the fields before they are used. It does not verify anything
// cryptographic.
func ValidateSigned(s SignedRequest) error {
	for _, f := range []struct{ name, v string }{{"connector", s.ConnectorID}, {"device", s.DeviceID}, {"nonce", s.Nonce}, {"body hash", s.BodySHA256}} {
		if f.v == "" || len(f.v) > 80 || !cleanField(f.v) {
			return fmt.Errorf("invalid %s", f.name)
		}
	}
	if len(s.BodySHA256) != 64 {
		return errors.New("invalid body hash")
	}
	if _, err := hex.DecodeString(s.BodySHA256); err != nil || s.BodySHA256 != strings.ToLower(s.BodySHA256) {
		return errors.New("invalid body hash")
	}
	if len(s.Nonce) < 16 || len(s.Nonce) > 64 {
		return errors.New("invalid nonce")
	}
	for i := 0; i < len(s.Nonce); i++ {
		c := s.Nonce[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return errors.New("invalid nonce")
		}
	}
	if s.Method != "GET" && s.Method != "POST" {
		return errors.New("invalid method")
	}
	if err := ValidatePath(s.Path); err != nil {
		return err
	}
	if len(s.RawQuery) > 2048 || !cleanQuery(s.RawQuery) {
		return errors.New("invalid query")
	}
	return nil
}

func cleanQuery(q string) bool {
	for i := 0; i < len(q); i++ {
		if q[i] < 0x21 || q[i] == 0x7f || q[i] == '#' {
			return false
		}
	}
	return true
}

// ValidatePath accepts only a plain absolute path: no empty, dot or dot-dot segment, no encoded
// slash, dot or backslash (they would let the allowlist and the router disagree), no controls.
func ValidatePath(p string) error {
	if p == "" || len(p) > 512 || p[0] != '/' {
		return errors.New("invalid path")
	}
	low := strings.ToLower(p)
	for _, bad := range []string{"%2f", "%2e", "%5c", "%00"} {
		if strings.Contains(low, bad) {
			return errors.New("invalid path")
		}
	}
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c < 0x21 || c == 0x7f || c == '\\' || c == '?' || c == '#' {
			return errors.New("invalid path")
		}
	}
	if p == "/" {
		return nil
	}
	for _, seg := range strings.Split(p[1:], "/") {
		if seg == "" || seg == "." || seg == ".." {
			return errors.New("invalid path")
		}
	}
	return nil
}

// ParseSPKI parses a DER SubjectPublicKeyInfo holding an ECDSA P-256 key.
func ParseSPKI(spki []byte) (*ecdsa.PublicKey, error) {
	if len(spki) == 0 || len(spki) > 256 {
		return nil, errors.New("invalid public key")
	}
	k, err := x509.ParsePKIXPublicKey(spki)
	if err != nil {
		return nil, errors.New("invalid public key")
	}
	pub, ok := k.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, errors.New("public key is not ECDSA P-256")
	}
	return pub, nil
}

// DeviceID derives the device id from the SPKI bytes: hex(sha256)[:32].
func DeviceID(spki []byte) string {
	sum := sha256.Sum256(spki)
	return hex.EncodeToString(sum[:])[:32]
}

// MarshalSPKI encodes a public key as SPKI DER.
func MarshalSPKI(pub *ecdsa.PublicKey) ([]byte, error) { return x509.MarshalPKIXPublicKey(pub) }

// Sign signs the canonical string with ECDSA P-256/SHA-256 and returns the IEEE P1363 signature
// (r || s, 64 bytes) as b64u, which is what WebCrypto produces.
func Sign(priv *ecdsa.PrivateKey, canonical string) (string, error) {
	h := sha256.Sum256([]byte(canonical))
	r, s, err := ecdsa.Sign(rand.Reader, priv, h[:])
	if err != nil {
		return "", err
	}
	out := make([]byte, 64)
	r.FillBytes(out[:32])
	s.FillBytes(out[32:])
	return Enc(out), nil
}

// VerifySignature checks a P1363 b64u signature over canonical.
func VerifySignature(pub *ecdsa.PublicKey, canonical, sig string) bool {
	raw, err := Dec(sig)
	if err != nil || len(raw) != 64 {
		return false
	}
	h := sha256.Sum256([]byte(canonical))
	return ecdsa.Verify(pub, h[:], new(big.Int).SetBytes(raw[:32]), new(big.Int).SetBytes(raw[32:]))
}

// Pairing (docs/hosted-protocol.md, section 6).

const (
	pairCommitTag = "maestria-pair-commit-v1"
	pairSASTag    = "maestria-pair-sas-v1"
)

// PairCommit is what the browser sends before it reveals its key: b64u sha256(tag ‖ spki ‖ nonce).
func PairCommit(spki, nonce []byte) string {
	h := sha256.New()
	h.Write([]byte(pairCommitTag))
	h.Write(spki)
	h.Write(nonce)
	return Enc(h.Sum(nil))
}

// PairCode is the 6-digit code both sides compute: sha256(tag ‖ spki ‖ nonce ‖ r), first four
// bytes big-endian, mod 10^6.
func PairCode(spki, nonce, r []byte) string {
	h := sha256.New()
	h.Write([]byte(pairSASTag))
	h.Write(spki)
	h.Write(nonce)
	h.Write(r)
	n := binary.BigEndian.Uint32(h.Sum(nil)[:4]) % 1000000
	return fmt.Sprintf("%06d", n)
}

// FormatPairCode renders 123456 as "12 · 34 · 56".
func FormatPairCode(code string) string {
	if len(code) != 6 {
		return code
	}
	return code[0:2] + " · " + code[2:4] + " · " + code[4:6]
}

// NormalizePairCode strips spaces, dots and dashes from user input.
func NormalizePairCode(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}
