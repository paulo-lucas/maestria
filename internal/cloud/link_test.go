package cloud

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paulo-lucas/maestria/internal/connect"
	"github.com/paulo-lucas/maestria/internal/relay"
)

func apiAs(b *bed, method, path string, body any) (int, string) {
	var rd *bytes.Reader
	if body != nil {
		bs, _ := json.Marshal(body)
		rd = bytes.NewReader(bs)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, b.ts.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer res.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(res.Body)
	return res.StatusCode, buf.String()
}

func newWebCode(t *testing.T, br *browser) string {
	t.Helper()
	r := br.api("POST", "/api/link/codes", nil)
	var c struct{ Code string }
	_ = json.Unmarshal([]byte(r.body), &c)
	if len(c.Code) != 9 || c.Code[4] != '-' {
		t.Fatalf("code format: %q (%d %s)", c.Code, r.StatusCode, r.body)
	}
	return c.Code
}

func TestLinkCodeTTLSingleUseAndHashing(t *testing.T) {
	b := newBed(t)
	br := b.browser("10.0.0.1")
	br.signup("ann@example.com", testPassword)
	m := relay.RedeemRequest{Machine: relay.Machine{Host: "box", OS: "linux", Arch: "amd64", Version: "1"}}

	// Expires after 10 minutes.
	code := newWebCode(t, br)
	b.clock.advance(11 * time.Minute)
	m.Code = code
	if st, _ := apiAs(b, "POST", "/api/link/redeem", m); st != 404 {
		t.Fatalf("expired code redeemed: %d", st)
	}

	// Single use.
	code = newWebCode(t, br)
	m.Code = code
	st, body := apiAs(b, "POST", "/api/link/redeem", m)
	if st != 200 {
		t.Fatalf("redeem: %d %s", st, body)
	}
	var red relay.RedeemResponse
	_ = json.Unmarshal([]byte(body), &red)
	if red.AccountHint != "a***@example.com" {
		t.Fatalf("hint: %q (the full address must not be revealed to whoever holds a code)", red.AccountHint)
	}
	// Redeeming alone changes nothing: no connector exists yet.
	if cons, _ := b.srv.cons.ForUser(sessUserID(b)); len(cons) != 0 {
		t.Fatal("redeem must not create a connector")
	}
	// Wrong confirmation token.
	if st, _ := apiAs(b, "POST", "/api/link/confirm", relay.ConfirmRequest{Code: code, Token: "nope", Name: "x"}); st != 404 {
		t.Fatalf("wrong token: %d", st)
	}
	st, body = apiAs(b, "POST", "/api/link/confirm", relay.ConfirmRequest{Code: code, Token: red.Token, Name: "Workstation"})
	if st != 200 {
		t.Fatalf("confirm: %d %s", st, body)
	}
	var linked relay.Linked
	_ = json.Unmarshal([]byte(body), &linked)
	if !strings.HasPrefix(linked.Credential, "mc1."+linked.ConnectorID+".") || linked.Name != "Workstation" {
		t.Fatalf("linked: %+v", linked)
	}
	if st, _ := apiAs(b, "POST", "/api/link/confirm", relay.ConfirmRequest{Code: code, Token: red.Token, Name: "again"}); st != 404 {
		t.Fatalf("a code works once: %d", st)
	}
	if st, _ := apiAs(b, "POST", "/api/link/redeem", m); st != 404 {
		t.Fatalf("a used code cannot be redeemed: %d", st)
	}

	// The cloud keeps only hashes: neither the code nor the credential is in the database.
	_, secret, _ := relay.ParseCredential(linked.Credential)
	var credHash string
	if err := b.srv.db.QueryRow("SELECT credential_hash FROM connectors WHERE id = ?", linked.ConnectorID).Scan(&credHash); err != nil {
		t.Fatal(err)
	}
	if credHash == secret || credHash != sha256Hex(secret) {
		t.Fatal("credential must be stored as its sha256")
	}
	b.srv.db.Exec("PRAGMA wal_checkpoint(FULL)")
	entries, _ := os.ReadDir(b.cfg.DataDir)
	norm := NormalizeLinkCode(code)
	for _, e := range entries {
		data, _ := os.ReadFile(filepath.Join(b.cfg.DataDir, e.Name()))
		for _, secretText := range []string{secret, norm, strings.ToUpper(norm), code} {
			if bytes.Contains(data, []byte(secretText)) {
				t.Fatalf("%s contains %q in clear", e.Name(), secretText[:4]+"…")
			}
		}
	}
	// The status endpoint tells the page the code was used.
	var c struct{ ID string }
	_ = json.Unmarshal([]byte(br.api("POST", "/api/link/codes", nil).body), &c)
	r := br.api("GET", "/api/link/codes/"+c.ID, nil)
	if !strings.Contains(r.body, `"waiting"`) {
		t.Fatalf("status: %s", r.body)
	}
	if r := br.api("GET", "/api/link/codes/"+strings.Repeat("0", 32), nil); r.StatusCode != 404 {
		t.Fatalf("unknown id: %d", r.StatusCode)
	}
}

func sessUserID(b *bed) string {
	u, _ := b.srv.acc.UserByEmail("ann@example.com")
	return u.ID
}

func TestLinkCodeGuessingIsRateLimited(t *testing.T) {
	b := newBed(t)
	var last int
	for i := 0; i < 25; i++ {
		last, _ = apiAs(b, "POST", "/api/link/redeem", relay.RedeemRequest{Code: "AAAA-BBB" + string(rune('A'+i%20))})
	}
	if last != 429 {
		t.Fatalf("guessing codes must hit the rate limit, last status %d", last)
	}
}

func TestCLICodeFlow(t *testing.T) {
	b := newBed(t)
	br := b.browser("10.0.0.1")
	br.signup("ann@example.com", testPassword)
	other := b.browser("10.0.0.2")
	other.signup("eve@example.com", testPassword)

	st, body := apiAs(b, "POST", "/api/link/start", relay.StartRequest{Machine: relay.Machine{Host: "laptop", OS: "windows", Arch: "amd64", Version: "2"}})
	var start relay.StartResponse
	_ = json.Unmarshal([]byte(body), &start)
	if st != 200 || len(start.Code) != 9 || start.PollToken == "" {
		t.Fatalf("start: %d %s", st, body)
	}
	poll := func(confirm bool, name string) relay.PollResponse {
		_, body := apiAs(b, "POST", "/api/link/poll", relay.PollRequest{PollToken: start.PollToken, Confirm: confirm, Name: name})
		var p relay.PollResponse
		_ = json.Unmarshal([]byte(body), &p)
		return p
	}
	if p := poll(false, ""); p.Status != "pending" {
		t.Fatalf("before claim: %+v", p)
	}
	// Confirming before anybody claimed the code gives nothing.
	if p := poll(true, "x"); p.Linked != nil {
		t.Fatal("the credential must not be issued before the code is claimed")
	}
	if r := br.api("POST", "/api/link/claim", map[string]string{"code": "ZZZZ-ZZZZ"}); r.StatusCode != 404 {
		t.Fatalf("unknown code: %d", r.StatusCode)
	}
	if r := br.api("POST", "/api/link/claim", map[string]string{"code": strings.ToLower(start.Code)}); r.StatusCode != 200 {
		t.Fatalf("claim: %d %s", r.StatusCode, r.body)
	}
	// A second account cannot take over a claimed code.
	if r := other.api("POST", "/api/link/claim", map[string]string{"code": start.Code}); r.StatusCode != 404 {
		t.Fatalf("second claim: %d", r.StatusCode)
	}
	if p := poll(false, ""); p.Status != "claimed" || p.AccountHint != "a***@example.com" {
		t.Fatalf("after claim: %+v", p)
	}
	p := poll(true, "Laptop")
	if p.Status != "linked" || p.Linked == nil || p.Linked.Name != "Laptop" {
		t.Fatalf("confirm: %+v", p)
	}
	// Shown once.
	if _, body := apiAs(b, "POST", "/api/link/poll", relay.PollRequest{PollToken: start.PollToken, Confirm: true}); strings.Contains(body, "credential") {
		t.Fatalf("credential issued twice: %s", body)
	}
	cons, _ := b.srv.cons.ForUser(sessUserID(b))
	if len(cons) != 1 || cons[0].OS != "windows" {
		t.Fatalf("connectors: %+v", cons)
	}
	// Expiry.
	_, body = apiAs(b, "POST", "/api/link/start", relay.StartRequest{})
	_ = json.Unmarshal([]byte(body), &start)
	b.clock.advance(11 * time.Minute)
	if p := poll(false, ""); p.Status != "expired" {
		t.Fatalf("expired: %+v", p)
	}
	if r := br.api("POST", "/api/link/claim", map[string]string{"code": start.Code}); r.StatusCode != 404 {
		t.Fatalf("claim of an expired code: %d", r.StatusCode)
	}
}

func TestConnectorSocketAuth(t *testing.T) {
	f := newFixture(t)
	cred := f.a.link.Credential
	dial := func(authz, origin string) int {
		req, _ := http.NewRequest("GET", f.b.ts.URL+"/connector", nil)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Version", "13")
		req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if c := dial("", ""); c != 401 {
		t.Errorf("no credential: %d", c)
	}
	if c := dial("Bearer mc1."+f.a.link.ConnectorID+"."+strings.Repeat("x", 43), ""); c != 401 {
		t.Errorf("wrong secret: %d", c)
	}
	if c := dial("Bearer garbage", ""); c != 401 {
		t.Errorf("garbage: %d", c)
	}
	if c := dial("Bearer "+cred, "https://evil.example"); c != 403 {
		t.Errorf("a browser origin must never reach the connector socket: %d", c)
	}
	if c := dial("Bearer "+cred, ""); c != 101 {
		t.Errorf("the right credential upgrades: %d", c)
	}
	// Disabled owners and revoked connectors are refused.
	if _, err := f.b.srv.acc.SetDisabled("ann@example.com", true); err != nil {
		t.Fatal(err)
	}
	if c := dial("Bearer "+cred, ""); c != 401 {
		t.Errorf("disabled owner: %d", c)
	}
	// The hub drops sockets of a disabled owner on its next sweep.
	f.b.srv.hub.Sweep()
	f.a.waitOffline(f.b)
	// The connector leaves for good (it was refused): its client stops retrying.
	select {
	case err := <-f.a.done:
		if err != connect.ErrRevoked {
			t.Fatalf("client: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client kept retrying with a refused credential")
	}
	f.a.cancel = nil
}

func TestUnlinkNeedsTheCredential(t *testing.T) {
	f := newFixture(t)
	if st, _ := apiAs(f.b, "DELETE", "/api/connector", nil); st != 401 {
		t.Fatalf("no credential: %d", st)
	}
	req, _ := http.NewRequest("DELETE", f.b.ts.URL+"/api/connector", nil)
	req.Header.Set("Authorization", "Bearer "+f.a.link.Credential)
	req.Header.Set("Origin", "https://evil.example")
	res, _ := http.DefaultClient.Do(req)
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatalf("browser origin: %d", res.StatusCode)
	}
}
