package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// RFC 6238 TOTP: HMAC-SHA1, 30 s step, 6 digits (what every authenticator app supports).

// TOTPStep is the TOTP time step.
const TOTPStep = 30 * time.Second

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateTOTPSecret returns a new random 160-bit secret, base32 without padding.
func GenerateTOTPSecret() (string, error) {
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return b32.EncodeToString(buf), nil
}

// DecodeTOTPSecret decodes a base32 secret, tolerating spaces, lowercase and padding.
func DecodeTOTPSecret(s string) ([]byte, error) {
	clean := strings.ToUpper(strings.TrimRight(strings.Join(strings.Fields(s), ""), "="))
	key, err := b32.DecodeString(clean)
	if err != nil || len(key) == 0 {
		return nil, fmt.Errorf("invalid base32 TOTP secret")
	}
	return key, nil
}

// HOTP computes an RFC 4226 code.
func HOTP(key []byte, counter uint64, digits int) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, code%mod)
}

// TOTPStepAt returns the step number for t.
func TOTPStepAt(t time.Time) int64 { return t.Unix() / int64(TOTPStep/time.Second) }

// TOTP returns the 6-digit code for t.
func TOTP(secret string, t time.Time) (string, error) {
	key, err := DecodeTOTPSecret(secret)
	if err != nil {
		return "", err
	}
	return HOTP(key, uint64(TOTPStepAt(t)), 6), nil
}

func isSixDigits(code string) bool {
	if len(code) != 6 {
		return false
	}
	for i := 0; i < 6; i++ {
		if code[i] < '0' || code[i] > '9' {
			return false
		}
	}
	return true
}

// VerifyTOTP checks code against the steps in [now-window, now+window]. It returns the matching
// step so the caller can reject replays. Every candidate is compared in constant time.
func VerifyTOTP(secret, code string, now time.Time, window int) (step int64, ok bool) {
	if !isSixDigits(code) {
		return 0, false
	}
	key, err := DecodeTOTPSecret(secret)
	if err != nil {
		return 0, false
	}
	cur := TOTPStepAt(now)
	for s := cur - int64(window); s <= cur+int64(window); s++ {
		if s < 0 {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(HOTP(key, uint64(s), 6)), []byte(code)) == 1 {
			step, ok = s, true
		}
	}
	return step, ok
}

// OTPAuthURI builds the otpauth:// URI that authenticator apps scan.
func OTPAuthURI(secret, account, issuer string) string {
	label := url.PathEscape(issuer) + ":" + url.PathEscape(account)
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", "6")
	q.Set("period", "30")
	return "otpauth://totp/" + label + "?" + q.Encode()
}
