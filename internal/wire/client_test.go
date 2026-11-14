package wire_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/maestria-dev/maestria/internal/wire"
)

func TestPinnedTLSAcceptsMatchingKeyAndRefusesAnyOther(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()

	ok := wire.NewClient(f.Host, f.Pin(), wire.ScopeRead, "", "")
	if _, err := ok.Info(ctx); err != nil {
		t.Fatalf("matching pin refused: %v", err)
	}

	var wrong wire.Pin
	bad := wire.NewClient(f.Host, wrong, wire.ScopeRead, "", "")
	_, err := bad.Info(ctx)
	var pe *wire.PinError
	if !errors.As(err, &pe) {
		t.Fatalf("wrong pin: want PinError, got %v", err)
	}
	if wire.Classify(err) != wire.StatePinMismatch {
		t.Fatalf("classify = %s", wire.Classify(err))
	}

	// The host rotates its certificate: a client pinned to the old key must refuse the new one.
	f.RotateCert()
	_, err = ok.Info(ctx)
	if !errors.As(err, &pe) {
		t.Fatalf("rotated cert accepted or wrong error: %v", err)
	}
}

func TestRequestsSendHostLocalhostAndCloseConnections(t *testing.T) {
	f := newFake(t)
	c := wire.NewClient(f.Host, f.Pin(), wire.ScopeRead, "", "")
	if _, err := c.Info(context.Background()); err != nil {
		t.Fatal(err)
	}
	reqs := f.Requests()
	if len(reqs) != 1 || reqs[0].Host != "localhost" {
		t.Fatalf("requests = %+v", reqs)
	}
}

func TestErrorsNeverContainTheToken(t *testing.T) {
	f := newFake(t)
	const token = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	c := wire.NewClient(f.Host, f.Pin(), wire.ScopeRead, token, "33333333-3333-4333-8333-333333333331")
	_, err := c.Workspaces(context.Background()) // 401: unknown token
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("err = %v", err)
	}
	if wire.Classify(err) != wire.StateRevoked {
		t.Fatalf("classify = %s", wire.Classify(err))
	}
	// A socket that fails to connect (wrong pin) must not echo ?token=… either.
	var wrong wire.Pin
	bad := wire.NewClient(f.Host, wrong, wire.ScopeRead, token, "")
	err = bad.FeedStream(context.Background(), wsWire, func(*wire.FeedSnapshot) {})
	if err == nil || strings.Contains(err.Error(), token) || strings.Contains(fmt.Sprint(err), "token=") {
		t.Fatalf("stream err leaks: %v", err)
	}
}

func TestCredentialsNeverPrintTheirToken(t *testing.T) {
	cred := wire.Credential{Token: "supersecret", DeviceID: "d"}
	for _, s := range []string{fmt.Sprint(cred), fmt.Sprintf("%v %+v %#v %s", cred, cred, cred, cred)} {
		if strings.Contains(s, "supersecret") {
			t.Fatalf("leaked: %s", s)
		}
	}
	c := &wire.Credentials{Host: "h", Guest: cred, Owner: &cred}
	if strings.Contains(fmt.Sprintf("%+v %#v", c, c), "supersecret") {
		t.Fatal("Credentials leaked a token through fmt")
	}
}
