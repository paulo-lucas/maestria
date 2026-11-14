package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"testing"
	"time"
)

const (
	cfTeam = "team.cloudflareaccess.com"
	cfAud  = "abcdef0123456789abcdef0123456789"
)

// CFTestKey is an RSA key with a JWKS fetcher and a token signer, for tests.
type CFTestKey struct {
	Key *rsa.PrivateKey
	Kid string
}

// NewCFTestKey generates a 2048-bit test key.
func NewCFTestKey(t testing.TB, kid string) *CFTestKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &CFTestKey{Key: k, Kid: kid}
}

func (k *CFTestKey) jwk() JWK {
	return JWK{Kid: k.Kid, Kty: "RSA",
		N: base64.RawURLEncoding.EncodeToString(k.Key.N.Bytes()),
		E: base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.Key.E)).Bytes())}
}

func (k *CFTestKey) sign(t testing.TB, header, payload map[string]any) string {
	t.Helper()
	h, _ := json.Marshal(header)
	p, _ := json.Marshal(payload)
	input := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(p)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, k.Key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestAccessVerifier(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	good := NewCFTestKey(t, "k1")
	other := NewCFTestKey(t, "k1") // same kid, different key
	fetches := 0
	fetch := func(_ context.Context, team string) ([]JWK, error) {
		fetches++
		if team != cfTeam {
			t.Fatalf("fetched keys for %q", team)
		}
		return []JWK{good.jwk(), {Kid: "ec", Kty: "EC"}}, nil
	}
	v := NewAccessVerifier(cfTeam, cfAud, fetch, func() time.Time { return now })
	ctx := context.Background()
	hdr := map[string]any{"alg": "RS256", "kid": "k1"}
	claims := func(mut func(map[string]any)) map[string]any {
		c := map[string]any{"iss": "https://" + cfTeam, "aud": []string{cfAud}, "exp": now.Unix() + 300, "nbf": now.Unix() - 10}
		if mut != nil {
			mut(c)
		}
		return c
	}

	if !v.Verify(ctx, good.sign(t, hdr, claims(nil))) {
		t.Fatal("valid token rejected")
	}
	if !v.Verify(ctx, good.sign(t, hdr, claims(func(c map[string]any) { c["aud"] = cfAud }))) {
		t.Fatal("string audience rejected")
	}

	tok := good.sign(t, hdr, claims(nil))
	rejects := map[string]string{
		"missing":     "",
		"malformed":   "a.b",
		"garbage":     "x.y.z",
		"tampered":    tok[:len(tok)-4] + "AAAA",
		"wrong key":   other.sign(t, hdr, claims(nil)),
		"wrong aud":   good.sign(t, hdr, claims(func(c map[string]any) { c["aud"] = []string{"ffff0000ffff0000"} })),
		"wrong iss":   good.sign(t, hdr, claims(func(c map[string]any) { c["iss"] = "https://evil.cloudflareaccess.com" })),
		"expired":     good.sign(t, hdr, claims(func(c map[string]any) { c["exp"] = now.Unix() - 1 })),
		"no exp":      good.sign(t, hdr, claims(func(c map[string]any) { delete(c, "exp") })),
		"future nbf":  good.sign(t, hdr, claims(func(c map[string]any) { c["nbf"] = now.Unix() + 300 })),
		"alg none":    good.sign(t, map[string]any{"alg": "none", "kid": "k1"}, claims(nil)),
		"alg HS256":   good.sign(t, map[string]any{"alg": "HS256", "kid": "k1"}, claims(nil)),
		"unknown kid": good.sign(t, map[string]any{"alg": "RS256", "kid": "k9"}, claims(nil)),
		"no kid":      good.sign(t, map[string]any{"alg": "RS256"}, claims(nil)),
	}
	for name, token := range rejects {
		if v.Verify(ctx, token) {
			t.Errorf("%s: accepted", name)
		}
	}
	if fetches != 1 {
		t.Fatalf("unknown kids must not trigger more than one fetch per minute, got %d", fetches)
	}
}

func TestAccessVerifierFailsClosed(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	key := NewCFTestKey(t, "k1")
	v := NewAccessVerifier(cfTeam, cfAud, func(context.Context, string) ([]JWK, error) {
		return nil, errors.New("network down")
	}, func() time.Time { return now })
	tok := key.sign(t, map[string]any{"alg": "RS256", "kid": "k1"},
		map[string]any{"iss": "https://" + cfTeam, "aud": cfAud, "exp": now.Unix() + 60})
	if v.Verify(context.Background(), tok) {
		t.Fatal("must fail closed when keys cannot be fetched")
	}
}
