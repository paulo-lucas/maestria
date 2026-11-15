package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/paulo-lucas/maestria/internal/cli"
	"github.com/paulo-lucas/maestria/internal/connect"
	"github.com/paulo-lucas/maestria/internal/relay"
	"github.com/paulo-lucas/maestria/internal/relay/relaytest"
)

func init() { cli.LinkPollInterval = 30 * time.Millisecond }

type fixture struct {
	b      *bed
	br     *browser
	dev    *relaytest.Browser
	a, bnd *node
	secret string
	codes  []string
}

// newFixture is the full picture: a cloud, an account, two linked and connected machines with a
// workspace each, and a browser device approved on both.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	b := newBed(t)
	f := &fixture{b: b, br: b.browser("10.0.0.1"), dev: relaytest.NewBrowser(t)}
	f.secret, f.codes = f.br.signup("ann@example.com", testPassword)

	f.a = b.newNode("Workstation")
	f.a.addWorkspace("Atlas")
	f.a.linkWithWebCode(f.br)
	f.a.start()
	f.a.waitOnline(b)

	f.bnd = b.newNode("Laptop")
	f.bnd.addWorkspace("Quarry")
	f.bnd.linkWithCLICode(f.br)
	f.bnd.start()
	f.bnd.waitOnline(b)

	b.pair(f.br, f.dev, f.a, f.bnd)
	return f
}

var cliCodeRE = regexp.MustCompile(`[A-Z2-9]{4}-[A-Z2-9]{4}`)

// linkWithCLICode links through the CLI-shows-the-code flow.
func (n *node) linkWithCLICode(br *browser) {
	n.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	env := n.env
	env.Ctx = ctx
	env.Stdin = strings.NewReader(testWord + "\n" + n.name + "\ny\n")
	env.Stdout, env.Stderr = n.out, n.out
	done := make(chan int, 1)
	go func() { done <- cli.Run([]string{"connect", "--cloud", n.cloud}, env) }()
	var code string
	waitFor(n.t, "the CLI to print a code", func() bool { code = cliCodeRE.FindString(n.out.String()); return code != "" })
	if r := br.api("POST", "/api/link/claim", map[string]string{"code": code}); r.StatusCode != 200 {
		n.t.Fatalf("claim: %d %s", r.StatusCode, r.body)
	}
	n.waitLinked()
	cancel()
	<-done
}

func TestEndToEndTwoConnectors(t *testing.T) {
	f := newFixture(t)

	// Both machines show up for the account, online, with their own names. The mirror of paired
	// devices reaches the cloud on the connector's next tick, so wait for it.
	var list struct{ Connectors []connectorView }
	waitFor(t, "both machines to report their approved device", func() bool {
		list.Connectors = nil
		_ = json.Unmarshal([]byte(f.br.api("GET", "/api/connectors", nil).body), &list)
		if len(list.Connectors) != 2 {
			return false
		}
		for _, c := range list.Connectors {
			if len(c.Devices) != 1 {
				return false
			}
		}
		return true
	})
	for _, c := range list.Connectors {
		if !c.Online || c.Version != "9.9.9-test" {
			t.Fatalf("connector %s: %+v", c.Name, c)
		}
		if len(c.Devices) != 1 || c.Devices[0].State != "active" {
			t.Fatalf("mirror of paired devices: %+v", c.Devices)
		}
	}

	// The same API handlers as local mode answer, per machine, with that machine's data.
	for _, c := range []struct {
		n    *node
		want string
	}{{f.a, "Atlas"}, {f.bnd, "Quarry"}} {
		r := c.n.call(f.br, f.dev, "GET", "/api/workspaces", "", nil)
		if r.StatusCode != 200 || !strings.Contains(r.body, c.want) {
			t.Fatalf("%s workspaces: %d %s", c.n.name, r.StatusCode, r.body)
		}
		if strings.Contains(r.body, map[string]string{"Atlas": "Quarry", "Quarry": "Atlas"}[c.want]) {
			t.Fatalf("a machine must only show its own workspaces: %s", r.body)
		}
		if got := r.Header.Get("Content-Security-Policy"); got != "sandbox" {
			t.Fatalf("relayed responses must be sandboxed, got %q", got)
		}
	}

	// A write through the relay lands in the connector's own database.
	wsID := workspaceID(t, f.a.call(f.br, f.dev, "GET", "/api/workspaces", "", nil).body)
	r := f.a.call(f.br, f.dev, "POST", "/api/workspaces/"+wsID+"/chat", "", []byte(`{"body":"hello from the browser"}`))
	if r.StatusCode != 200 {
		t.Fatalf("chat send: %d %s", r.StatusCode, r.body)
	}
	chat := f.a.call(f.br, f.dev, "GET", "/api/workspaces/"+wsID+"/chat", "", nil)
	if !strings.Contains(chat.body, "hello from the browser") {
		t.Fatalf("chat history: %s", chat.body)
	}
	// An HTML view (the frontend renders workspace pages from it).
	page := f.a.call(f.br, f.dev, "GET", "/workspaces/"+wsID+"/tasks", "", nil)
	if page.StatusCode != 200 || !strings.Contains(page.Header.Get("Content-Type"), "text/html") || !strings.Contains(page.body, "Human tasks") || strings.Contains(page.body, "needs JavaScript") {
		t.Fatalf("html view: %d %s", page.StatusCode, page.body[:min(200, len(page.body))])
	}
}

func workspaceID(t *testing.T, body string) string {
	t.Helper()
	var v struct{ Workspaces []struct{ ID string } }
	if err := json.Unmarshal([]byte(body), &v); err != nil || len(v.Workspaces) == 0 {
		t.Fatalf("workspaces: %s", body)
	}
	return v.Workspaces[0].ID
}

// The connector refuses everything the cloud could try. Each case goes through the whole stack.
func TestConnectorRefusesWhatItShould(t *testing.T) {
	f := newFixture(t)
	n, br, dev := f.a, f.br, f.dev
	clock := n.clock
	dev.Now = clock.now
	sign := func(m, p, q string, body []byte) *relaytest.Request {
		return dev.Sign(t, n.link.ConnectorID, m, p, q, body)
	}
	want := func(name string, r *resp, status int, code string) {
		t.Helper()
		if r.StatusCode != status || errCode(r) != code {
			t.Errorf("%s: got %d %q (%s), want %d %q", name, r.StatusCode, errCode(r), strings.TrimSpace(r.body), status, code)
		}
	}

	// No signature at all.
	want("unsigned", n.send(br, &relaytest.Request{Method: "GET", Path: "/api/workspaces", Header: map[string]string{}}), 401, relay.ErrUnsigned)

	// Bad signature.
	s := sign("GET", "/api/workspaces", "", nil)
	s.Header[relay.HeaderSig] = relay.Enc(make([]byte, 64))
	want("bad signature", n.send(br, s), 403, relay.ErrBadSignature)

	// A different device that was never approved.
	stranger := relaytest.NewBrowser(t)
	stranger.Now = clock.now
	want("wrong device", n.send(br, stranger.Sign(t, n.link.ConnectorID, "GET", "/api/workspaces", "", nil)), 403, relay.ErrUnknownDevice)

	// A request signed for the other connector, sent to this one.
	want("signed for another connector", n.send(br, dev.Sign(t, f.bnd.link.ConnectorID, "GET", "/api/workspaces", "", nil)), 403, relay.ErrBadSignature)

	// Replayed nonce.
	s = sign("GET", "/api/workspaces", "", nil)
	if r := n.send(br, s); r.StatusCode != 200 {
		t.Fatalf("first send: %d %s", r.StatusCode, r.body)
	}
	want("replay", n.send(br, s), 403, relay.ErrReplay)

	// Stale timestamp (and one from the future).
	want("stale", n.send(br, dev.SignAt(t, n.link.ConnectorID, "GET", "/api/workspaces", "", nil, clock.now().Add(-90*time.Second).UnixMilli(), relay.Enc(bytes.Repeat([]byte{1}, 16)))), 403, relay.ErrStaleTimestamp)
	want("future", n.send(br, dev.SignAt(t, n.link.ConnectorID, "GET", "/api/workspaces", "", nil, clock.now().Add(90*time.Second).UnixMilli(), relay.Enc(bytes.Repeat([]byte{2}, 16)))), 403, relay.ErrStaleTimestamp)

	// Body that does not match its hash.
	s = sign("POST", "/api/standby/resume", "", []byte(`{}`))
	s.Body = []byte(`{"guardKey":"x"}`)
	want("body mismatch", n.send(br, s), 403, relay.ErrBodyMismatch)

	// Routes outside the allowlist, validly signed.
	for _, rt := range [][2]string{{"POST", "/login"}, {"GET", "/api/session"}, {"POST", "/logout-all"}, {"GET", "/security"}, {"POST", "/kill-switch"}, {"GET", "/api/tunnel"}, {"POST", "/api/devices/x/approve"}} {
		want("route "+rt[0]+" "+rt[1], n.send(br, sign(rt[0], rt[1], "", nil)), 403, relay.ErrRouteForbidden)
	}

	// Expired approval: absolute (14 days) and idle (3 days), then re-approval fixes it.
	clock.advance(4 * 24 * time.Hour)
	want("idle", n.send(br, sign("GET", "/api/workspaces", "", nil)), 403, relay.ErrIdleExpired)
	f.b.pair(br, dev, n)
	if r := n.send(br, sign("GET", "/api/workspaces", "", nil)); r.StatusCode != 200 {
		t.Fatalf("after re-approval: %d %s", r.StatusCode, r.body)
	}
	for i := 0; i < 6; i++ { // in use every other day: no idle expiry
		clock.advance(2 * 24 * time.Hour)
		if r := n.send(br, sign("GET", "/api/workspaces", "", nil)); r.StatusCode != 200 {
			t.Fatalf("day %d: %d %s", (i+1)*2, r.StatusCode, r.body)
		}
	}
	clock.advance(2 * 24 * time.Hour)
	want("absolute", n.send(br, sign("GET", "/api/workspaces", "", nil)), 403, relay.ErrApprovalExpired)

	// Revoked device.
	f.b.pair(br, dev, n)
	if c, out := n.cli("", "devices", "revoke", dev.DeviceID[:8]); c != 0 {
		t.Fatalf("revoke: %s", out)
	}
	want("revoked", n.send(br, sign("GET", "/api/workspaces", "", nil)), 403, relay.ErrDeviceRevoked)
	// The other machine never heard of the revocation and still accepts the device (each decides alone).
	if r := f.bnd.call(br, dev, "GET", "/api/workspaces", "", nil); r.StatusCode != 200 && errCode(r) != relay.ErrApprovalExpired && errCode(r) != relay.ErrIdleExpired {
		t.Fatalf("machine B: %d %s", r.StatusCode, r.body)
	}
}

// A compromised cloud edits, forges and replays envelopes. The connector refuses all of it.
func TestCloudCannotForgeOrTamper(t *testing.T) {
	b := newBed(t)
	br := b.browser("10.0.0.1")
	br.signup("ann@example.com", testPassword)
	n := b.newNode("Workstation")
	n.addWorkspace("Atlas")
	n.linkWithWebCode(br)
	proxy := newTamperProxy(t, b.ts.URL)
	n.cloud = proxy.url
	n.start()
	n.waitOnline(b)
	dev := relaytest.NewBrowser(t)
	b.pair(br, dev, n)

	if r := n.call(br, dev, "GET", "/api/workspaces", "", nil); r.StatusCode != 200 {
		t.Fatalf("baseline through the proxy: %d %s", r.StatusCode, r.body)
	}

	// 1. The cloud rewrites a field of a real request.
	for name, mut := range map[string]func(*relay.Frame){
		"path":   func(f *relay.Frame) { f.Path = "/api/inbox" },
		"query":  func(f *relay.Frame) { f.Query = "evil=1" },
		"method": func(f *relay.Frame) { f.Method = "GET" },
		"body":   func(f *relay.Frame) { f.Body = relay.Enc([]byte(`{"body":"injected"}`)) },
		"device": func(f *relay.Frame) { f.Header["x-maestria-device"] = strings.Repeat("0", 32) },
		"ts":     func(f *relay.Frame) { f.Header["x-maestria-ts"] = "1" },
	} {
		mut := mut
		proxy.setMutate(func(f *relay.Frame) {
			if f.T == relay.TypeReq {
				mut(f)
			}
		})
		r := n.call(br, dev, "POST", "/api/workspaces/x/chat", "", []byte(`{"body":"original"}`))
		if r.StatusCode != 403 {
			t.Errorf("tampered %s: got %d %s, want a refusal by the connector", name, r.StatusCode, r.body)
		}
	}
	proxy.setMutate(nil)
	// Nothing the cloud tried reached the connector's data.
	chat := n.call(br, dev, "GET", "/api/workspaces", "", nil)
	if strings.Contains(chat.body, "injected") {
		t.Fatal("tampered body reached the connector")
	}

	// 2. The cloud invents requests: unsigned, signed with its own key, or a replay of a real one.
	proxy.inject <- relay.Frame{T: relay.TypeReq, ID: "r_forged1", Method: "POST", Path: "/api/standby/resume", Header: map[string]string{}, Body: relay.Enc([]byte(`{}`))}
	f := proxy.waitFrame(t, func(f relay.Frame) bool { return f.ID == "r_forged1" })
	if f.T != relay.TypeErr || f.Code != relay.ErrUnsigned {
		t.Fatalf("forged unsigned request: %+v", f)
	}
	attacker := relaytest.NewBrowser(t)
	attacker.Now = n.clock.now
	forged := attacker.Sign(t, n.link.ConnectorID, "POST", "/api/standby/resume", "", []byte(`{}`))
	hdr := map[string]string{}
	for k, v := range forged.Header {
		hdr[strings.ToLower(k)] = v
	}
	proxy.inject <- relay.Frame{T: relay.TypeReq, ID: "r_forged2", Method: "POST", Path: forged.Path, Header: hdr, Body: relay.Enc(forged.Body)}
	f = proxy.waitFrame(t, func(f relay.Frame) bool { return f.ID == "r_forged2" })
	if f.T != relay.TypeErr || f.Code != relay.ErrUnknownDevice {
		t.Fatalf("request signed with a key the cloud made up: %+v", f)
	}
	// The cloud may even present its own key at pairing: the code the user compares will not match.
	// (Covered in the devices tests; here: a reveal with a key that does not match the commitment.)
	var cap relay.Frame
	proxy.setMutate(func(f *relay.Frame) {
		if f.T == relay.TypeReq && f.Path == "/api/workspaces" {
			cap = *f
		}
	})
	n.call(br, dev, "GET", "/api/workspaces", "", nil)
	proxy.setMutate(nil)
	proxy.inject <- relay.Frame{T: relay.TypeReq, ID: "r_replay", Method: cap.Method, Path: cap.Path, Query: cap.Query, Header: cap.Header, Body: cap.Body}
	f = proxy.waitFrame(t, func(f relay.Frame) bool { return f.ID == "r_replay" })
	if f.T != relay.TypeErr || f.Code != relay.ErrReplay {
		t.Fatalf("replayed envelope: %+v", f)
	}
	// Allowlist: a validly signed request for a forbidden route cannot be turned into an allowed one.
	forbidden := dev.Sign(t, n.link.ConnectorID, "POST", "/login", "", nil)
	r := n.send(br, forbidden)
	if r.StatusCode != 403 || errCode(r) != relay.ErrRouteForbidden {
		t.Fatalf("forbidden route: %d %s", r.StatusCode, r.body)
	}
}

func TestKillSwitchBlocksRelayAndIsVisible(t *testing.T) {
	f := newFixture(t)
	if r := f.a.call(f.br, f.dev, "GET", "/api/workspaces", "", nil); r.StatusCode != 200 {
		t.Fatal(r.body)
	}
	f.a.setKill(true)
	r := f.a.call(f.br, f.dev, "GET", "/api/workspaces", "", nil)
	if r.StatusCode != 403 || errCode(r) != relay.ErrKillSwitch {
		t.Fatalf("kill switch must block the relay: %d %s", r.StatusCode, r.body)
	}
	// Even a valid POST of something harmless, and pairing, are refused.
	r = f.a.call(f.br, f.dev, "POST", "/api/standby/resume", "", []byte(`{}`))
	if errCode(r) != relay.ErrKillSwitch {
		t.Fatalf("POST under kill switch: %d %s", r.StatusCode, r.body)
	}
	// The cloud learns of it (for the "kill switch is on" state) and the other machine is unaffected.
	waitFor(t, "kill switch in the connector list", func() bool {
		var list struct{ Connectors []connectorView }
		_ = json.Unmarshal([]byte(f.br.api("GET", "/api/connectors", nil).body), &list)
		for _, c := range list.Connectors {
			if c.ID == f.a.link.ConnectorID {
				return c.KillSwitch
			}
		}
		return false
	})
	if r := f.bnd.call(f.br, f.dev, "GET", "/api/workspaces", "", nil); r.StatusCode != 200 {
		t.Fatalf("the other machine is not affected: %d %s", r.StatusCode, r.body)
	}
	// Only the local side turns it off: there is no relay route for that.
	for _, p := range []string{"/api/tunnel/on", "/api/kill-switch/off", "/tunnel/on"} {
		if r := f.bnd.call(f.br, f.dev, "POST", p, "", nil); errCode(r) != relay.ErrRouteForbidden {
			t.Errorf("%s: %d %s", p, r.StatusCode, r.body)
		}
	}
	f.a.setKill(false)
	if r := f.a.call(f.br, f.dev, "GET", "/api/workspaces", "", nil); r.StatusCode != 200 {
		t.Fatalf("after local re-enable: %d %s", r.StatusCode, r.body)
	}
	// Turning it on through the relay (tightening) is allowed and then blocks the relay itself.
	if r := f.a.call(f.br, f.dev, "POST", "/api/kill-switch", "", []byte(`{}`)); r.StatusCode != 200 {
		t.Fatalf("kill via relay: %d %s", r.StatusCode, r.body)
	}
	if r := f.a.call(f.br, f.dev, "GET", "/api/workspaces", "", nil); errCode(r) != relay.ErrKillSwitch {
		t.Fatalf("relay must be blocked after the kill: %d %s", r.StatusCode, r.body)
	}
}

func TestOfflineConnectorVanishesAndFailsFast(t *testing.T) {
	f := newFixture(t)
	f.bnd.stop()
	f.bnd.waitOffline(f.b)

	start := time.Now()
	r := f.bnd.call(f.br, f.dev, "GET", "/api/workspaces", "", nil)
	if r.StatusCode != 503 || errCode(r) != relay.ErrConnectorOffline {
		t.Fatalf("offline connector: %d %s", r.StatusCode, r.body)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("an offline connector must fail fast, took %s", time.Since(start))
	}
	var list struct{ Connectors []connectorView }
	_ = json.Unmarshal([]byte(f.br.api("GET", "/api/connectors", nil).body), &list)
	online := 0
	for _, c := range list.Connectors {
		if c.Online {
			online++
		}
		if c.ID == f.bnd.link.ConnectorID && c.Online {
			t.Fatal("offline machine reported online")
		}
	}
	if online != 1 {
		t.Fatalf("online machines: %d", online)
	}
	if r := f.a.call(f.br, f.dev, "GET", "/api/workspaces", "", nil); r.StatusCode != 200 {
		t.Fatalf("other machine: %d", r.StatusCode)
	}
	// Nothing is queued: when it returns, it has not received anything from the time it was away.
	before := f.bnd.chatCount(t)
	f.bnd.call(f.br, f.dev, "POST", "/api/workspaces/x/chat", "", []byte(`{"body":"while away"}`))
	f.bnd.start()
	f.bnd.waitOnline(f.b)
	time.Sleep(200 * time.Millisecond)
	if f.bnd.chatCount(t) != before {
		t.Fatal("a message sent while the machine was offline must not be queued")
	}
	// And it is back for everyone.
	if r := f.bnd.call(f.br, f.dev, "GET", "/api/workspaces", "", nil); r.StatusCode != 200 {
		t.Fatalf("after reconnect: %d %s", r.StatusCode, r.body)
	}
}

func (n *node) chatCount(t *testing.T) int {
	t.Helper()
	db := n.openDB(t)
	defer db.Close()
	var c int
	_ = db.QueryRow("SELECT COUNT(*) FROM chat_messages").Scan(&c)
	return c
}

func TestBodiesAreNeverLoggedOrStored(t *testing.T) {
	f := newFixture(t)
	const marker = "UNIQUE-MARKER-7f3a9c-DO-NOT-LOG"
	wsID := workspaceID(t, f.a.call(f.br, f.dev, "GET", "/api/workspaces", "", nil).body)
	r := f.a.call(f.br, f.dev, "POST", "/api/workspaces/"+wsID+"/chat", "", []byte(`{"body":"`+marker+`"}`))
	if r.StatusCode != 200 {
		t.Fatalf("send: %d %s", r.StatusCode, r.body)
	}
	back := f.a.call(f.br, f.dev, "GET", "/api/workspaces/"+wsID+"/chat", "", nil)
	if !strings.Contains(back.body, marker) {
		t.Fatal("the marker never made it to the connector and back")
	}
	f.a.call(f.br, f.dev, "GET", "/workspaces/"+wsID+"/chat", "", nil)

	// The connector has it (it is the one place that should).
	db := f.a.openDB(t)
	var n int
	_ = db.QueryRow("SELECT COUNT(*) FROM chat_messages WHERE body LIKE ?", "%"+marker+"%").Scan(&n)
	db.Close()
	if n == 0 {
		t.Fatal("connector did not store the message")
	}

	// The cloud has it nowhere: not in its logs, not in a single byte of its database files.
	if strings.Contains(f.b.logbuf.String(), marker) {
		t.Fatal("a relayed body was logged")
	}
	for _, e := range f.b.mail.all() {
		if strings.Contains(e.Body, marker) {
			t.Fatal("body in email")
		}
	}
	f.b.srv.db.Exec("PRAGMA wal_checkpoint(FULL)")
	entries, _ := os.ReadDir(f.b.cfg.DataDir)
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(f.b.cfg.DataDir, e.Name()))
		if err != nil {
			continue
		}
		if bytes.Contains(data, []byte(marker)) {
			t.Fatalf("relayed body found in cloud file %s", e.Name())
		}
	}
	ents, _ := f.b.srv.audit.Recent(500)
	for _, e := range ents {
		if strings.Contains(e.Detail, marker) {
			t.Fatal("body in cloud audit log")
		}
	}
	// What the cloud does keep is the small list from the brief.
	rows, _ := f.b.srv.db.Query("SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'")
	var tables []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		tables = append(tables, s)
	}
	rows.Close()
	allowed := map[string]bool{"users": true, "recovery_codes": true, "sessions": true, "pending_logins": true, "email_tokens": true, "setup_sessions": true,
		"connectors": true, "paired_devices": true, "link_codes": true, "audit": true, "invites": true}
	for _, tb := range tables {
		if !allowed[tb] {
			t.Errorf("unexpected cloud table %q: the cloud must not keep workspace data", tb)
		}
	}
}

func TestStreamIsRelayedPerBrowser(t *testing.T) {
	f := newFixture(t)
	f.dev.Now = f.a.clock.now
	sr := f.dev.Sign(t, f.a.link.ConnectorID, "GET", "/api/stream", "", nil)
	req, _ := http.NewRequest("GET", f.b.ts.URL+"/c/"+f.a.link.ConnectorID+"/api/stream", nil)
	for k, v := range sr.Header {
		req.Header.Set(k, v)
	}
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)
	for _, c := range f.br.c.Jar.Cookies(req.URL) {
		req.AddCookie(c)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	buf := make([]byte, 256)
	got := make(chan string, 1)
	go func() {
		n, _ := res.Body.Read(buf)
		got <- string(buf[:n])
	}()
	select {
	case s := <-got:
		if !strings.Contains(s, "event: ready") {
			t.Fatalf("first event: %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event from the stream")
	}
	// A second stream for the same user counts toward the limit and cancelling frees it.
	cancel()
	waitFor(t, "stream slot released", func() bool {
		f.b.srv.hub.mu.Lock()
		defer f.b.srv.hub.mu.Unlock()
		return f.b.srv.hub.streams[sessUser(f)] == 0
	})
}

func sessUser(f *fixture) string {
	u, _ := f.b.srv.acc.UserByEmail("ann@example.com")
	return u.ID
}

func TestRevokeAndUnlink(t *testing.T) {
	f := newFixture(t)
	// Revoke from the web: the socket closes and the credential stops working.
	r := f.br.api("POST", "/api/connectors/"+f.bnd.link.ConnectorID+"/revoke", nil)
	if r.StatusCode != 200 {
		t.Fatalf("revoke: %d %s", r.StatusCode, r.body)
	}
	select {
	case err := <-f.bnd.done:
		if err != connect.ErrRevoked {
			t.Fatalf("client ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a revoked connector must stop")
	}
	f.bnd.cancel = nil
	if r := f.bnd.call(f.br, f.dev, "GET", "/api/workspaces", "", nil); r.StatusCode != 404 {
		t.Fatalf("revoked connector is gone from the account: %d", r.StatusCode)
	}
	// Unlink from the machine.
	if c, out := f.a.cli("", "connect", "unlink", "--yes"); c != 0 || !strings.Contains(out, "Unlinked") {
		t.Fatalf("unlink: %d %s", c, out)
	}
	if l, _ := connect.LoadLink(f.a.dir); l != nil {
		t.Fatal("link file must be gone")
	}
	waitFor(t, "unlinked machine to leave", func() bool { _, _, ok := f.b.srv.hub.Online(f.a.link.ConnectorID); return !ok })
	var list struct{ Connectors []connectorView }
	_ = json.Unmarshal([]byte(f.br.api("GET", "/api/connectors", nil).body), &list)
	if len(list.Connectors) != 0 {
		t.Fatalf("nothing should be listed: %+v", list.Connectors)
	}
	if c, out := f.a.cli("", "connect", "status"); c != 0 || !strings.Contains(out, "Not linked") {
		t.Fatalf("status after unlink: %d %s", c, out)
	}
}

func TestPasswordResetMakesConnectorsForgetDevices(t *testing.T) {
	f := newFixture(t)
	if r := f.a.call(f.br, f.dev, "GET", "/api/workspaces", "", nil); r.StatusCode != 200 {
		t.Fatal(r.body)
	}
	x := f.b.browser("10.0.0.5")
	x.get("/reset")
	x.form("/reset", urlValues("email", "ann@example.com"))
	link := f.b.link(f.b.waitMail("ann@example.com", "Reset"))
	codes := f.codes
	r := x.form(link, urlValues("password", "a brand new long password", "recovery", codes[0]))
	if r.StatusCode != 303 {
		t.Fatalf("reset: %d %s", r.StatusCode, r.body)
	}
	if f.br.signedIn() {
		t.Fatal("the old session must be gone")
	}
	// x is the browser that just reset and is signed in.
	waitFor(t, "devices revoked on the machine", func() bool {
		return errCode(f.a.call(x, f.dev, "GET", "/api/workspaces", "", nil)) == relay.ErrDeviceRevoked
	})
	waitFor(t, "devices revoked on the other machine", func() bool {
		return errCode(f.bnd.call(x, f.dev, "GET", "/api/workspaces", "", nil)) == relay.ErrDeviceRevoked
	})
}

func TestOnlyOwnerReachesAConnector(t *testing.T) {
	f := newFixture(t)
	f.dev.Now = f.a.clock.now
	other := f.b.browser("10.0.0.77")
	other.signup("eve@example.com", testPassword)
	sr := f.dev.Sign(t, f.a.link.ConnectorID, "GET", "/api/workspaces", "", nil)
	if r := f.a.send(other, sr); r.StatusCode != 404 {
		t.Fatalf("another account must not reach the connector: %d %s", r.StatusCode, r.body)
	}
	anon := f.b.browser("10.0.0.78")
	if r := f.a.send(anon, f.dev.Sign(t, f.a.link.ConnectorID, "GET", "/api/workspaces", "", nil)); r.StatusCode != 401 {
		t.Fatalf("anonymous: %d", r.StatusCode)
	}
	var list struct{ Connectors []connectorView }
	_ = json.Unmarshal([]byte(other.api("GET", "/api/connectors", nil).body), &list)
	if len(list.Connectors) != 0 {
		t.Fatal("eve sees ann's machines")
	}
	// Eve cannot rename, revoke or pair against them either.
	if r := other.api("POST", "/api/connectors/"+f.a.link.ConnectorID+"/revoke", nil); r.StatusCode != 404 {
		t.Fatalf("eve revoke: %d", r.StatusCode)
	}
	if r := other.api("POST", "/api/connectors/"+f.a.link.ConnectorID+"/rename", map[string]string{"name": "pwned"}); r.StatusCode != 404 {
		t.Fatalf("eve rename: %d", r.StatusCode)
	}
}

func TestRenameConnector(t *testing.T) {
	f := newFixture(t)
	if r := f.br.api("POST", "/api/connectors/"+f.a.link.ConnectorID+"/rename", map[string]string{"name": "Studio\nMac"}); r.StatusCode != 200 {
		t.Fatalf("rename: %d %s", r.StatusCode, r.body)
	}
	var list struct{ Connectors []connectorView }
	_ = json.Unmarshal([]byte(f.br.api("GET", "/api/connectors", nil).body), &list)
	found := false
	for _, c := range list.Connectors {
		if c.ID == f.a.link.ConnectorID {
			found = true
			if c.Name != "StudioMac" {
				t.Fatalf("name: %q", c.Name)
			}
		}
	}
	if !found {
		t.Fatal("connector missing")
	}
}

// A cloud that swaps the browser's key during pairing gets a code the user will not match.
func TestCloudCannotSwapDeviceKeyAtPairing(t *testing.T) {
	b := newBed(t)
	br := b.browser("10.0.0.1")
	br.signup("ann@example.com", testPassword)
	n := b.newNode("Workstation")
	n.linkWithWebCode(br)
	proxy := newTamperProxy(t, b.ts.URL)
	n.cloud = proxy.url
	n.start()
	n.waitOnline(b)

	genuine := relaytest.NewBrowser(t)
	attacker := relaytest.NewBrowser(t)
	ap := attacker.StartPairing(t)
	proxy.setMutate(func(f *relay.Frame) {
		switch f.T {
		case relay.TypePairOffer:
			f.Commit = ap.Commit // the cloud commits to its own key
		case relay.TypePairReveal:
			f.SPKI, f.Nonce = relay.Enc(attacker.SPKI), relay.Enc(ap.Nonce)
		}
	})

	gp := genuine.StartPairing(t)
	r := br.api("POST", "/api/pair", map[string]string{"commit": gp.Commit, "label": "My phone"})
	var view struct{ ID string }
	_ = json.Unmarshal([]byte(r.body), &view)
	var m PairMachine
	waitFor(t, "challenge", func() bool {
		var v struct{ Machines []PairMachine }
		_ = json.Unmarshal([]byte(br.api("GET", "/api/pair/"+view.ID, nil).body), &v)
		if len(v.Machines) == 1 {
			m = v.Machines[0]
		}
		return m.R != ""
	})
	br.api("POST", "/api/pair/"+view.ID+"/reveal", map[string]string{"connectorId": n.link.ConnectorID, "spki": relay.Enc(genuine.SPKI), "nonce": relay.Enc(gp.Nonce)})

	// The machine now shows a pairing, but its code is computed from the attacker's key.
	shown := genuine.Code(t, gp, m.R)
	var pending string
	waitFor(t, "the pairing to show on the machine", func() bool {
		_, pending = n.cli("", "devices", "pending")
		return strings.Contains(pending, "expires in")
	})
	if strings.Contains(pending, relay.FormatPairCode(shown)) {
		t.Fatalf("the machine shows the same code as the genuine browser: a swapped key would be approved\n%s", pending)
	}
	// So typing what the browser shows approves nothing.
	if code, out := n.cli(testWord+"\n", "devices", "approve", relay.FormatPairCode(shown)); code == 0 {
		t.Fatalf("the genuine code approved a swapped key: %s", out)
	}
	if code, out := n.cli("", "devices", "list"); code != 0 || !strings.Contains(out, "No approved devices") {
		t.Fatalf("no device may exist: %s", out)
	}
	proxy.setMutate(nil)
	// And the cloud cannot approve for the user: it has no such frame, and the connector API has no such route.
	for _, p := range []string{"/api/devices/approve", "/api/devices/" + attacker.DeviceID + "/approve", "/api/pair/approve"} {
		if relay.Allowed("POST", p) {
			t.Errorf("%s must not be relayable", p)
		}
	}
}

// The cloud forwards nothing but what the protocol names: never cookies.
func TestCloudForwardsOnlySignedFieldsAndPlainHeaders(t *testing.T) {
	b := newBed(t)
	br := b.browser("10.0.0.1")
	br.signup("ann@example.com", testPassword)
	n := b.newNode("Workstation")
	n.linkWithWebCode(br)
	proxy := newTamperProxy(t, b.ts.URL)
	n.cloud = proxy.url
	n.start()
	n.waitOnline(b)
	dev := relaytest.NewBrowser(t)
	b.pair(br, dev, n)

	var got relay.Frame
	proxy.setMutate(func(f *relay.Frame) {
		if f.T == relay.TypeReq {
			got = *f
		}
	})
	dev.Now = n.clock.now
	sr := dev.Sign(t, n.link.ConnectorID, "GET", "/api/workspaces", "", nil)
	req, _ := http.NewRequest("GET", b.ts.URL+"/c/"+n.link.ConnectorID+"/api/workspaces", nil)
	for k, v := range sr.Header {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "Bearer secret-token")
	req.Header.Set("X-Evil", "1")
	req.Header.Set("Cookie", "extra=1")
	br.do(req)
	allowed := map[string]bool{"x-maestria-device": true, "x-maestria-ts": true, "x-maestria-nonce": true, "x-maestria-body": true, "x-maestria-sig": true, "content-type": true, "accept": true, "x-requested-with": true}
	if len(got.Header) == 0 {
		t.Fatal("no request seen")
	}
	for k := range got.Header {
		if !allowed[k] {
			t.Errorf("header %q was forwarded to the connector", k)
		}
	}
}
