package wire_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/paulo-lucas/maestria/internal/wire"
)

func TestProvisionWithPasswordPairsTwiceAndDemotesOnlyTheReadDevice(t *testing.T) {
	f := newFake(t)
	f.SetPassword("hunter2")
	creds, err := wire.Provision(context.Background(), f.Host, f.Pin(), "Test", "hunter2", nil)
	if err != nil {
		t.Fatal(err)
	}
	if creds.Owner == nil || creds.Guest.Token == creds.Owner.Token {
		t.Fatalf("expected two distinct tokens: %+v", creds)
	}
	roles := map[string]string{}
	for _, d := range f.Devices() {
		roles[d.Identifier] = d.Role
	}
	if roles["maestria-owner"] != "owner" || roles["maestria-guest"] != "guest" {
		t.Fatalf("roles = %v", roles)
	}
	// Pairing again with the same identifiers replaces the records instead of adding devices.
	if _, err := wire.Provision(context.Background(), f.Host, f.Pin(), "Test", "hunter2", nil); err != nil {
		t.Fatal(err)
	}
	if n := len(f.Devices()); n != 2 {
		t.Fatalf("%d devices after re-pair", n)
	}
	// The password never appears in what the fake recorded.
	for _, r := range f.Requests() {
		if strings.Contains(r.Body, "hunter2") {
			t.Fatalf("password recorded: %+v", r)
		}
	}
}

func TestProvisionWithCodesAsksForASecondSingleUseCode(t *testing.T) {
	f := newFake(t)
	first, second := f.NewCode(), f.NewCode()
	asked := 0
	creds, err := wire.Provision(context.Background(), f.Host, f.Pin(), "Test", first, func() (string, error) {
		asked++
		return second, nil
	})
	if err != nil || asked != 1 || creds.Owner == nil {
		t.Fatalf("err=%v asked=%d", err, asked)
	}
	// Reusing the same single-use code for both fails and leaves no device behind.
	f2 := newFake(t)
	code := f2.NewCode()
	_, err = wire.Provision(context.Background(), f2.Host, f2.Pin(), "Test", code, func() (string, error) { return code, nil })
	var ae *wire.APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusUnauthorized {
		t.Fatalf("err = %v", err)
	}
	if n := len(f2.Devices()); n != 0 {
		t.Fatalf("owner device left behind: %d", n)
	}
}

func TestPairErrors(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	// 401: wrong code.
	_, err := wire.Pair(ctx, f.Host, f.Pin(), "x", "id", "123456")
	var ae *wire.APIError
	if !errors.As(err, &ae) || ae.Status != 401 || !strings.Contains(wire.PairMessage(err), "Wrong") {
		t.Fatalf("401: %v", err)
	}
	// 503: password pairing is off.
	_, err = wire.Pair(ctx, f.Host, f.Pin(), "x", "id", "a-password")
	if !errors.As(err, &ae) || ae.Status != 503 || !strings.Contains(wire.PairMessage(err), "pairing code") {
		t.Fatalf("503: %v", err)
	}
	// 429 with Retry-After.
	f.SetThrottle(1)
	_, err = wire.Pair(ctx, f.Host, f.Pin(), "x", "id", "123456")
	if !errors.As(err, &ae) || ae.Status != 429 || ae.RetryAfter.Seconds() != 30 || !strings.Contains(wire.PairMessage(err), "30s") {
		t.Fatalf("429: %v (%+v)", err, ae)
	}
	// Pin mismatch is refused before anything is sent.
	var wrong wire.Pin
	_, err = wire.Pair(ctx, f.Host, wrong, "x", "id", "123456")
	if wire.Classify(err) != wire.StatePinMismatch || !strings.Contains(wire.PairMessage(err), "security key") {
		t.Fatalf("pin: %v", err)
	}
	for _, r := range f.Requests() {
		if r.Status == 0 {
			t.Fatalf("bad record: %+v", r)
		}
	}
}

func TestCredentialsFileIsPrivateAndRoundTrips(t *testing.T) {
	f := newFake(t)
	f.SetPassword("pw")
	creds, err := wire.Provision(context.Background(), f.Host, f.Pin(), "Test", "pw", nil)
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/wire.json"
	if err := creds.Save(path); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(path)
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v", fi.Mode().Perm())
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := wire.LoadCredentials(path); err == nil {
			t.Fatal("loose permissions accepted")
		}
		_ = os.Chmod(path, 0o600)
	}
	got, err := wire.LoadCredentials(path)
	if err != nil || got.Guest.Token != creds.Guest.Token || got.Owner.Token != creds.Owner.Token || got.Pin != f.Pin().Hex() {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	if _, err := wire.LoadCredentials(t.TempDir() + "/missing.json"); !errors.Is(err, wire.ErrNotPaired) {
		t.Fatalf("missing: %v", err)
	}
}

func TestNormalizeHost(t *testing.T) {
	for in, want := range map[string]string{
		"": "localhost:7434", "localhost": "localhost:7434", "localhost:7434": "localhost:7434",
		"https://192.168.1.5:9000/": "192.168.1.5:9000", "mac.local": "mac.local:7434", "[::1]": "[::1]:7434",
	} {
		got, err := wire.NormalizeHost(in)
		if err != nil || got != want {
			t.Errorf("NormalizeHost(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := wire.NormalizeHost("a b"); err == nil {
		t.Error("space accepted")
	}
}
