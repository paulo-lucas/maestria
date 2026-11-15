package relay

import (
	"strings"
	"testing"
)

func TestAllowlist(t *testing.T) {
	yes := [][2]string{
		{"GET", "/api/workspaces"}, {"GET", "/api/workspaces/atlas/inbox"}, {"POST", "/api/workspaces/atlas/inbox/12/respond"},
		{"POST", "/api/workspaces/atlas/chat"}, {"GET", "/api/stream"}, {"GET", "/workspaces/atlas/tasks"}, {"GET", "/usage"}, {"GET", "/"},
		{"POST", "/api/devices/abc/revoke"}, {"POST", "/api/kill-switch"}, {"POST", "/api/workspaces/a/agents/t1/approve"},
		{"GET", "/insights"}, {"GET", "/api/insights"},
	}
	for _, c := range yes {
		if !Allowed(c[0], c[1]) {
			t.Errorf("%s %s should be allowed", c[0], c[1])
		}
	}
	no := [][2]string{
		{"POST", "/login"}, {"POST", "/login/2fa"}, {"POST", "/logout"}, {"POST", "/logout-all"}, {"POST", "/kill-switch"},
		{"GET", "/api/session"}, {"GET", "/api/tunnel"}, {"POST", "/api/tunnel/on"}, {"GET", "/security"}, {"POST", "/"},
		{"POST", "/api/devices/abc/approve"}, {"POST", "/api/devices"}, {"DELETE", "/api/devices/abc/revoke"},
		{"GET", "/api/workspaces/a/inbox/extra"}, {"POST", "/api/workspaces//chat"}, {"GET", "/static/panel.js"},
		{"PUT", "/api/workspaces"}, {"GET", "api/workspaces"}, {"GET", ""}, {"POST", "/api/workspaces"}, {"GET", "/api/workspaces/a/docs/asset"},
		{"POST", "/api/standby"}, {"POST", "/api/auth/setup"}, {"POST", "/api/config"}, {"GET", "/connector"},
		// Insights is read-only: nothing can write to it through the relay.
		{"POST", "/insights"}, {"POST", "/api/insights"}, {"GET", "/api/insights/extra"},
	}
	for _, c := range no {
		if Allowed(c[0], c[1]) {
			t.Errorf("%s %s must not be allowed", c[0], c[1])
		}
	}
}

func TestValidatePath(t *testing.T) {
	for _, p := range []string{"/api/inbox", "/", "/workspaces/a-b_c.d/tasks", "/a/b%20c"} {
		if err := ValidatePath(p); err != nil {
			t.Errorf("%q: %v", p, err)
		}
	}
	for _, p := range []string{"", "api", "//x", "/a//b", "/a/../b", "/a/./b", "/a/%2e%2e/b", "/a%2Fb", "/a\\b", "/a b", "/a\nb", "/a?x", "/a#x", "/a%00", "/" + strings.Repeat("a", 600)} {
		if err := ValidatePath(p); err == nil {
			t.Errorf("%q must be refused", p)
		}
	}
}

func TestCanonicalString(t *testing.T) {
	s := SignedRequest{ConnectorID: "cn_1", DeviceID: "dev", Method: "post", Path: "/api/x", RawQuery: "a=b", BodySHA256: strings.Repeat("0", 64), TS: 1700000000123, Nonce: "n"}
	want := "maestria-relay-v1\ncn_1\ndev\nPOST\n/api/x\na=b\n" + strings.Repeat("0", 64) + "\n1700000000123\nn"
	if got := s.Canonical(); got != want {
		t.Fatalf("canonical:\n%q\n%q", got, want)
	}
}

func TestValidateSigned(t *testing.T) {
	ok := SignedRequest{ConnectorID: "cn_1", DeviceID: "dev", Method: "GET", Path: "/api/x", BodySHA256: BodyHash(nil), TS: 1, Nonce: "0123456789abcdef"}
	if err := ValidateSigned(ok); err != nil {
		t.Fatal(err)
	}
	bad := []func(s *SignedRequest){
		func(s *SignedRequest) { s.Nonce = "short" },
		func(s *SignedRequest) { s.Nonce = "0123456789abcde\n" },
		func(s *SignedRequest) { s.DeviceID = "a\nb" },
		func(s *SignedRequest) { s.Method = "DELETE" },
		func(s *SignedRequest) { s.BodySHA256 = "ABC" },
		func(s *SignedRequest) { s.RawQuery = "a=b\nc" },
		func(s *SignedRequest) { s.Path = "/a/../b" },
	}
	for i, m := range bad {
		s := ok
		m(&s)
		if err := ValidateSigned(s); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

func TestStatusFor(t *testing.T) {
	if StatusFor(ErrUnsigned) != 401 || StatusFor(ErrReplay) != 403 || StatusFor(ErrConnectorOffline) != 503 || StatusFor(ErrRateLimited) != 429 {
		t.Fatal("status table")
	}
}
