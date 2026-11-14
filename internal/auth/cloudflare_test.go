package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/maestria-dev/maestria/internal/auth"
	"github.com/maestria-dev/maestria/internal/auth/authtest"
)

const (
	cfTeam = "team.cloudflareaccess.com"
	cfAud  = "abcdef0123456789abcdef0123456789"
)

func TestAccessVerifier(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	good := authtest.NewKey(t, "k1")
	other := authtest.NewKey(t, "k1") // same kid, different key
	fetches := 0
	fetch := func(_ context.Context, team string) ([]auth.JWK, error) {
		fetches++
		if team != cfTeam {
			t.Fatalf("fetched keys for %q", team)
		}
		return []auth.JWK{good.JWK(), {Kid: "ec", Kty: "EC"}}, nil
	}
	v := auth.NewAccessVerifier(cfTeam, cfAud, fetch, func() time.Time { return now })
	ctx := context.Background()
	hdr := map[string]any{"alg": "RS256", "kid": "k1"}
	claims := func(mut func(map[string]any)) map[string]any {
		c := map[string]any{"iss": "https://" + cfTeam, "aud": []string{cfAud}, "exp": now.Unix() + 300, "nbf": now.Unix() - 10}
		if mut != nil {
			mut(c)
		}
		return c
	}

	if !v.Verify(ctx, good.Sign(t, hdr, claims(nil))) {
		t.Fatal("valid token rejected")
	}
	if !v.Verify(ctx, good.Sign(t, hdr, claims(func(c map[string]any) { c["aud"] = cfAud }))) {
		t.Fatal("string audience rejected")
	}

	tok := good.Sign(t, hdr, claims(nil))
	rejects := map[string]string{
		"missing":     "",
		"malformed":   "a.b",
		"garbage":     "x.y.z",
		"tampered":    tok[:len(tok)-4] + "AAAA",
		"wrong key":   other.Sign(t, hdr, claims(nil)),
		"wrong aud":   good.Sign(t, hdr, claims(func(c map[string]any) { c["aud"] = []string{"ffff0000ffff0000"} })),
		"wrong iss":   good.Sign(t, hdr, claims(func(c map[string]any) { c["iss"] = "https://evil.cloudflareaccess.com" })),
		"expired":     good.Sign(t, hdr, claims(func(c map[string]any) { c["exp"] = now.Unix() - 1 })),
		"no exp":      good.Sign(t, hdr, claims(func(c map[string]any) { delete(c, "exp") })),
		"future nbf":  good.Sign(t, hdr, claims(func(c map[string]any) { c["nbf"] = now.Unix() + 300 })),
		"alg none":    good.Sign(t, map[string]any{"alg": "none", "kid": "k1"}, claims(nil)),
		"alg HS256":   good.Sign(t, map[string]any{"alg": "HS256", "kid": "k1"}, claims(nil)),
		"unknown kid": good.Sign(t, map[string]any{"alg": "RS256", "kid": "k9"}, claims(nil)),
		"no kid":      good.Sign(t, map[string]any{"alg": "RS256"}, claims(nil)),
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
	key := authtest.NewKey(t, "k1")
	v := auth.NewAccessVerifier(cfTeam, cfAud, func(context.Context, string) ([]auth.JWK, error) {
		return nil, errors.New("network down")
	}, func() time.Time { return now })
	tok := key.Sign(t, map[string]any{"alg": "RS256", "kid": "k1"},
		map[string]any{"iss": "https://" + cfTeam, "aud": cfAud, "exp": now.Unix() + 60})
	if v.Verify(context.Background(), tok) {
		t.Fatal("must fail closed when keys cannot be fetched")
	}
}
