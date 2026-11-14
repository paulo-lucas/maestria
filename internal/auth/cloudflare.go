package auth

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// JWK is the subset of a JSON Web Key used by Cloudflare Access.
type JWK struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// JWKSFetcher returns the team's published signing keys.
type JWKSFetcher func(ctx context.Context, teamDomain string) ([]JWK, error)

// FetchJWKS downloads https://<teamDomain>/cdn-cgi/access/certs.
func FetchJWKS(ctx context.Context, teamDomain string) ([]JWK, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+teamDomain+"/cdn-cgi/access/certs", nil)
	if err != nil {
		return nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("JWKS fetch failed: %d", res.StatusCode)
	}
	var body struct {
		Keys []JWK `json:"keys"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, res.Body, 256*1024)).Decode(&body); err != nil {
		return nil, err
	}
	return body.Keys, nil
}

// AccessVerifier verifies the Cloudflare Access JWT (Cf-Access-Jwt-Assertion): RS256 signature
// against the team's published keys, issuer, audience and expiry. Defence in depth behind the
// panel's own login. It fails closed: no keys, no access.
type AccessVerifier struct {
	teamDomain string
	audience   string
	fetch      JWKSFetcher
	now        func() time.Time

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	lastFetch time.Time
}

const jwksRefreshMin = time.Minute

// NewAccessVerifier returns a verifier. fetch and now may be nil.
func NewAccessVerifier(teamDomain, audience string, fetch JWKSFetcher, now func() time.Time) *AccessVerifier {
	if fetch == nil {
		fetch = FetchJWKS
	}
	if now == nil {
		now = time.Now
	}
	return &AccessVerifier{teamDomain: teamDomain, audience: audience, fetch: fetch, now: now, keys: map[string]*rsa.PublicKey{}}
}

func rsaKey(j JWK) (*rsa.PublicKey, error) {
	n, err := base64.RawURLEncoding.DecodeString(j.N)
	if err != nil || len(n) < 256 {
		return nil, errors.New("bad modulus")
	}
	e, err := base64.RawURLEncoding.DecodeString(j.E)
	if err != nil || len(e) == 0 || len(e) > 4 {
		return nil, errors.New("bad exponent")
	}
	exp := int(new(big.Int).SetBytes(e).Int64())
	if exp < 3 {
		return nil, errors.New("bad exponent")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exp}, nil
}

func (v *AccessVerifier) key(ctx context.Context, kid string) *rsa.PublicKey {
	v.mu.Lock()
	defer v.mu.Unlock()
	if k := v.keys[kid]; k != nil {
		return k
	}
	t := v.now()
	if !v.lastFetch.IsZero() && t.Sub(v.lastFetch) < jwksRefreshMin {
		return nil
	}
	v.lastFetch = t
	jwks, err := v.fetch(ctx, v.teamDomain)
	if err != nil {
		return nil
	}
	next := map[string]*rsa.PublicKey{}
	for _, j := range jwks {
		if j.Kty != "RSA" || j.Kid == "" {
			continue
		}
		if k, err := rsaKey(j); err == nil {
			next[j.Kid] = k
		}
	}
	v.keys = next
	return next[kid]
}

func b64JSON(s string, dst any) bool {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return false
	}
	return json.Unmarshal(raw, dst) == nil
}

// Verify reports whether token is a valid Access JWT for this team and audience.
func (v *AccessVerifier) Verify(ctx context.Context, token string) bool {
	if token == "" || len(token) > 8192 {
		return false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	var payload struct {
		Exp *float64        `json:"exp"`
		Nbf *float64        `json:"nbf"`
		Iss string          `json:"iss"`
		Aud json.RawMessage `json:"aud"`
	}
	if !b64JSON(parts[0], &header) || !b64JSON(parts[1], &payload) {
		return false
	}
	if header.Alg != "RS256" || header.Kid == "" {
		return false
	}
	key := v.key(ctx, header.Kid)
	if key == nil {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig) != nil {
		return false
	}
	nowSec := float64(v.now().Unix())
	if payload.Exp == nil || *payload.Exp <= nowSec {
		return false
	}
	if payload.Nbf != nil && *payload.Nbf > nowSec+30 {
		return false
	}
	if payload.Iss != "https://"+v.teamDomain {
		return false
	}
	var one string
	if json.Unmarshal(payload.Aud, &one) == nil {
		return one == v.audience
	}
	var many []string
	if json.Unmarshal(payload.Aud, &many) == nil {
		for _, a := range many {
			if a == v.audience {
				return true
			}
		}
	}
	return false
}
