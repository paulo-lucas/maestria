// Package authtest has helpers for tests that need Cloudflare Access tokens. Test use only.
package authtest

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"testing"
	"time"

	"github.com/paulo-lucas/maestria/internal/auth"
)

// Key is an RSA signing key that plays the role of a Cloudflare Access team key.
type Key struct {
	Key *rsa.PrivateKey
	Kid string
}

// NewKey generates a 2048-bit key with the given key id.
func NewKey(t testing.TB, kid string) *Key {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &Key{Key: k, Kid: kid}
}

// JWK is the public half as published at /cdn-cgi/access/certs.
func (k *Key) JWK() auth.JWK {
	return auth.JWK{Kid: k.Kid, Kty: "RSA",
		N: base64.RawURLEncoding.EncodeToString(k.Key.N.Bytes()),
		E: base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.Key.E)).Bytes())}
}

// Fetcher serves this key as the team's JWKS.
func (k *Key) Fetcher() auth.JWKSFetcher {
	return func(context.Context, string) ([]auth.JWK, error) { return []auth.JWK{k.JWK()}, nil }
}

// Sign signs arbitrary header and payload as an RS256 JWT.
func (k *Key) Sign(t testing.TB, header, payload map[string]any) string {
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

// Token is a valid Access token for team and audience that expires at exp.
func (k *Key) Token(t testing.TB, team, audience string, exp time.Time) string {
	return k.Sign(t, map[string]any{"alg": "RS256", "kid": k.Kid},
		map[string]any{"iss": "https://" + team, "aud": []string{audience}, "exp": exp.Unix(), "nbf": exp.Add(-time.Hour).Unix()})
}
