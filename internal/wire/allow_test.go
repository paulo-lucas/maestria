package wire

import (
	"errors"
	"net/http"
	"testing"
)

const (
	tid = "22222222-2222-4222-8222-222222222221"
	wid = "11111111-1111-4111-8111-111111111111"
	did = "33333333-3333-4333-8333-333333333331"
)

func TestAllowlistPermitsOnlyWhatMaestriNeeds(t *testing.T) {
	type c struct {
		scope  Scope
		method string
		path   string
	}
	allowed := []c{
		{ScopeRead, "GET", "/api/info"},
		{ScopeRead, "GET", "/api/workspaces"},
		{ScopeRead, "GET", "/api/workspaces/" + wid + "/feed"},
		{ScopeRead, "GET", "/api/feed/stream"},
		{ScopeRead, "GET", "/api/terminals/" + tid + "/chat"},
		{ScopeRead, "GET", "/api/terminals/" + tid + "/chat/stream"},
		{ScopeRead, "DELETE", "/api/devices/" + did},
		{ScopeWrite, "GET", "/api/info"},
		{ScopeWrite, "POST", "/api/terminals/" + tid + "/prompt"},
		{ScopeWrite, "POST", "/api/terminals/" + tid + "/approve"},
		{ScopeWrite, "POST", "/api/terminals/" + tid + "/reject"},
		{ScopeWrite, "GET", "/api/terminals/" + tid + "/stream"},
		{ScopeWrite, "DELETE", "/api/devices/" + did},
		{ScopeSetup, "POST", "/pair"},
		{ScopeSetup, "PUT", "/api/devices/" + did + "/role"},
		{ScopeSetup, "GET", "/api/info"},
	}
	for _, a := range allowed {
		if err := Check(a.scope, a.method, a.path, did); err != nil {
			t.Errorf("%v %s %s refused: %v", a.scope, a.method, a.path, err)
		}
	}

	refused := []c{
		// The guest never writes.
		{ScopeRead, "POST", "/api/terminals/" + tid + "/prompt"},
		{ScopeRead, "POST", "/api/terminals/" + tid + "/approve"},
		{ScopeRead, "GET", "/api/terminals/" + tid + "/stream"},
		// The owner never reads feeds (an owner subscription wakes terminals) or lists workspaces.
		{ScopeWrite, "GET", "/api/feed/stream"},
		{ScopeWrite, "GET", "/api/workspaces"},
		{ScopeWrite, "GET", "/api/workspaces/" + wid + "/feed"},
		{ScopeWrite, "GET", "/api/terminals/" + tid + "/chat"},
		{ScopeWrite, "GET", "/api/terminals/" + tid + "/chat/stream"},
		// Everything else Wire can do, for every role.
		{ScopeWrite, "POST", "/api/terminals/" + tid + "/kill"},
		{ScopeWrite, "POST", "/api/terminals/" + tid + "/restart"},
		{ScopeWrite, "POST", "/api/terminals/" + tid + "/unload"},
		{ScopeWrite, "POST", "/api/terminals/" + tid + "/focus"},
		{ScopeWrite, "POST", "/api/terminals/" + tid + "/seen"},
		{ScopeWrite, "POST", "/api/terminals/" + tid + "/attachments"},
		{ScopeWrite, "PUT", "/api/terminals/" + tid + "/settings"},
		{ScopeWrite, "PUT", "/api/terminals/" + tid + "/chat"},
		{ScopeWrite, "DELETE", "/api/terminals/" + tid + "/chat/history"},
		{ScopeWrite, "POST", "/api/workspaces/" + wid + "/floors"},
		{ScopeWrite, "POST", "/api/workspaces/" + wid + "/floors/" + did + "/land"},
		{ScopeWrite, "POST", "/api/workspaces/" + wid + "/floors/" + did + "/delete"},
		{ScopeWrite, "POST", "/api/workspaces/" + wid + "/floors/" + did + "/status"},
		{ScopeWrite, "POST", "/api/workspaces/" + wid + "/floors/ground/activate"},
		{ScopeWrite, "POST", "/api/workspaces/" + wid + "/floors/" + did + "/pull-request/merge"},
		{ScopeWrite, "PUT", "/api/workspaces/" + wid + "/hooks"},
		{ScopeWrite, "POST", "/api/workspaces/" + wid + "/floors/" + did + "/hooks"},
		{ScopeWrite, "PUT", "/api/devices/" + did + "/role"},
		{ScopeWrite, "PUT", "/api/devices/" + did + "/name"},
		{ScopeWrite, "GET", "/api/devices"},
		{ScopeWrite, "POST", "/pair"},
		{ScopeRead, "POST", "/pair"},
		{ScopeRead, "PUT", "/api/devices/" + did + "/role"},
		{ScopeRead, "GET", "/api/devices"},
		{ScopeWrite, "POST", "/api/portals/" + tid + "/navigate"},
		{ScopeWrite, "POST", "/api/notes"},
		{ScopeWrite, "PATCH", "/api/terminals/" + tid + "/prompt"},
		{ScopeWrite, "PUT", "/api/terminals/" + tid + "/prompt"},
		{ScopeWrite, "DELETE", "/api/terminals/" + tid + "/prompt"},
		{ScopeWrite, "HEAD", "/api/info"},
		{ScopeSetup, "DELETE", "/api/devices/" + did},
		{ScopeSetup, "POST", "/api/terminals/" + tid + "/prompt"},
		{Scope(0), "GET", "/api/info"},
		// Malformed ids and paths.
		{ScopeWrite, "POST", "/api/terminals/not-a-uuid/prompt"},
		{ScopeWrite, "POST", "/api/terminals/" + tid + "/prompt/"},
		{ScopeWrite, "POST", "/api/terminals/" + tid + "/prompt?x=1"},
		{ScopeWrite, "POST", "/api/terminals/../terminals/" + tid + "/prompt"},
		{ScopeWrite, "POST", "//api/terminals/" + tid + "/prompt"},
		{ScopeWrite, "POST", "/api/terminals/" + tid + "/prompt#x"},
		{ScopeWrite, "POST", "/API/terminals/" + tid + "/prompt"},
		// A device may only revoke itself.
		{ScopeWrite, "DELETE", "/api/devices/33333333-3333-4333-8333-333333333399"},
		{ScopeRead, "DELETE", "/api/devices/33333333-3333-4333-8333-333333333399"},
	}
	for _, a := range refused {
		err := Check(a.scope, a.method, a.path, did)
		if !errors.Is(err, ErrNotAllowed) {
			t.Errorf("%v %s %s must be refused, got %v", a.scope, a.method, a.path, err)
		}
	}
	// Self-revocation needs a known self id.
	if err := Check(ScopeWrite, "DELETE", "/api/devices/"+did, ""); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("revocation with no self id must be refused: %v", err)
	}
}

func TestOnlyEscapeMayBeTypedIntoATerminal(t *testing.T) {
	if err := CheckInput([]byte{0x1b}); err != nil {
		t.Fatal(err)
	}
	for _, in := range [][]byte{nil, {}, []byte("y"), []byte("y\n"), {0x1b, 0x1b}, {0x03}, []byte("\x1b[A"), []byte("rm -rf /\n")} {
		if err := CheckInput(in); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("%q must be refused", in)
		}
	}
}

func TestClientRefusesBeforeAnyNetworkIO(t *testing.T) {
	// The host does not exist: if the allowlist did not stop the call first we would see a dial
	// error instead of ErrNotAllowed.
	var pin Pin
	guest := NewClient("127.0.0.1:1", pin, ScopeRead, "tok", did)
	owner := NewClient("127.0.0.1:1", pin, ScopeWrite, "tok", did)
	if err := guest.Prompt(t.Context(), tid, "hi"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("guest prompt: %v", err)
	}
	if _, err := owner.Workspaces(t.Context()); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("owner workspaces: %v", err)
	}
	if err := owner.FeedStream(t.Context(), wid, func(*FeedSnapshot) {}); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("owner feed: %v", err)
	}
	if err := guest.Interrupt(t.Context(), tid); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("guest interrupt: %v", err)
	}
	if err := guest.do(t.Context(), http.MethodPost, "/api/terminals/"+tid+"/kill", nil, nil); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("kill: %v", err)
	}
}
