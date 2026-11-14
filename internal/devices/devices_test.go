package devices

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/maestria-dev/maestria/internal/relay"
	"github.com/maestria-dev/maestria/internal/relay/relaytest"
	"github.com/maestria-dev/maestria/internal/store"
)

const cid = "cn_0123456789abcdef0123456789abcdef"

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newManager(t *testing.T) (*Manager, *clock) {
	t.Helper()
	db, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	return New(db, nil, c.now), c
}

// pair runs the whole pairing the way the cloud relays it and returns the approved browser.
func pair(t *testing.T, m *Manager, b *relaytest.Browser) {
	t.Helper()
	p := b.StartPairing(t)
	r, err := m.Offer("pair-"+b.DeviceID[:8], p.Commit, b.Label, "UA")
	if err != nil {
		t.Fatal(err)
	}
	code, err := m.Reveal("pair-"+b.DeviceID[:8], relay.Enc(b.SPKI), relay.Enc(p.Nonce))
	if err != nil {
		t.Fatal(err)
	}
	if code != b.Code(t, p, r) {
		t.Fatalf("browser and connector disagree: %s vs %s", code, b.Code(t, p, r))
	}
	if _, err := m.Approve(code); err != nil {
		t.Fatal(err)
	}
}

func req(b *relaytest.Browser, t *testing.T, c *clock, method, path, query string, body []byte) Request {
	b.Now = c.now
	s := b.Sign(t, cid, method, path, query, body)
	return fromSigned(s)
}

func fromSigned(s *relaytest.Request) Request {
	ts, _ := strconv.ParseInt(s.Header[relay.HeaderTS], 10, 64)
	return Request{
		ConnectorID: cid, DeviceID: s.Header[relay.HeaderDevice], Method: s.Method, Path: s.Path, RawQuery: s.RawQuery,
		BodyHash: s.Header[relay.HeaderBody], TS: ts, Nonce: s.Header[relay.HeaderNonce], Sig: s.Header[relay.HeaderSig], Body: s.Body,
	}
}

func TestVerifyAcceptsSignedRequest(t *testing.T) {
	m, c := newManager(t)
	b := relaytest.NewBrowser(t)
	pair(t, m, b)
	if _, code := m.Verify(cid, req(b, t, c, "GET", "/api/inbox", "status=open", nil)); code != "" {
		t.Fatalf("refused: %s", code)
	}
}

func TestVerifyRefusals(t *testing.T) {
	m, c := newManager(t)
	b := relaytest.NewBrowser(t)
	pair(t, m, b)
	other := relaytest.NewBrowser(t)

	tamper := func(name, want string, mut func(r *Request)) {
		t.Helper()
		r := req(b, t, c, "POST", "/api/workspaces/w/chat", "", []byte(`{"body":"hi"}`))
		mut(&r)
		if _, code := m.Verify(cid, r); code != want {
			t.Errorf("%s: got %q, want %q", name, code, want)
		}
	}
	tamper("method", relay.ErrBadSignature, func(r *Request) { r.Method = "GET" })
	tamper("path", relay.ErrBadSignature, func(r *Request) { r.Path = "/api/workspaces/x/chat" })
	tamper("query", relay.ErrBadSignature, func(r *Request) { r.RawQuery = "a=b" })
	tamper("timestamp", relay.ErrBadSignature, func(r *Request) { r.TS += 1000 })
	tamper("nonce", relay.ErrBadSignature, func(r *Request) { r.Nonce = relay.Enc([]byte("0123456789abcdef")) })
	tamper("signature", relay.ErrBadSignature, func(r *Request) { r.Sig = relay.Enc(make([]byte, 64)) })
	tamper("garbage signature", relay.ErrBadSignature, func(r *Request) { r.Sig = "!!" })
	tamper("body swapped, hash kept", relay.ErrBodyMismatch, func(r *Request) { r.Body = []byte(`{"body":"evil"}`) })
	tamper("body and hash swapped", relay.ErrBadSignature, func(r *Request) {
		r.Body = []byte(`{"body":"evil"}`)
		r.BodyHash = relay.BodyHash(r.Body)
	})
	tamper("unknown device", relay.ErrUnknownDevice, func(r *Request) { r.DeviceID = other.DeviceID })

	// Signed by a device the machine never approved, claiming its own id.
	r := req(other, t, c, "GET", "/api/inbox", "", nil)
	if _, code := m.Verify(cid, r); code != relay.ErrUnknownDevice {
		t.Errorf("unpaired device: %q", code)
	}
	// Signed by the wrong key but claiming the approved device's id.
	r = req(other, t, c, "GET", "/api/inbox", "", nil)
	r.DeviceID = b.DeviceID
	if _, code := m.Verify(cid, r); code != relay.ErrBadSignature {
		t.Errorf("wrong key under right id: %q", code)
	}
	// A request signed for another connector is useless here.
	b.Now = c.now
	s := b.Sign(t, "cn_ffffffffffffffffffffffffffffffff", "GET", "/api/inbox", "", nil)
	if _, code := m.Verify(cid, fromSigned(s)); code != relay.ErrBadSignature {
		t.Errorf("other connector's signature: %q", code)
	}
}

func TestReplayAndClock(t *testing.T) {
	m, c := newManager(t)
	b := relaytest.NewBrowser(t)
	pair(t, m, b)
	b.Now = c.now

	r := fromSigned(b.Sign(t, cid, "GET", "/api/inbox", "", nil))
	if _, code := m.Verify(cid, r); code != "" {
		t.Fatal(code)
	}
	if _, code := m.Verify(cid, r); code != relay.ErrReplay {
		t.Fatalf("replay: %q", code)
	}
	// Still a replay at the very end of its window.
	c.advance(59 * time.Second)
	if _, code := m.Verify(cid, r); code != relay.ErrReplay {
		t.Fatalf("late replay: %q", code)
	}
	// After the window it is stale, whatever the nonce cache says.
	c.advance(5 * time.Second)
	if _, code := m.Verify(cid, r); code != relay.ErrStaleTimestamp {
		t.Fatalf("stale: %q", code)
	}
	// Future timestamps are refused beyond the window too.
	f := fromSigned(b.SignAt(t, cid, "GET", "/api/inbox", "", nil, c.now().Add(61*time.Second).UnixMilli(), relay.Enc(make([]byte, 16))))
	if _, code := m.Verify(cid, f); code != relay.ErrStaleTimestamp {
		t.Fatalf("future: %q", code)
	}
	ok := fromSigned(b.SignAt(t, cid, "GET", "/api/inbox", "", nil, c.now().Add(59*time.Second).UnixMilli(), relay.Enc([]byte("abcdefghijklmnop"))))
	if _, code := m.Verify(cid, ok); code != "" {
		t.Fatalf("within window: %q", code)
	}
}

func TestNonceCacheFailsClosed(t *testing.T) {
	m, c := newManager(t)
	b := relaytest.NewBrowser(t)
	pair(t, m, b)
	b.Now = c.now
	for i := 0; i < relay.MaxNonces; i++ {
		m.nonces["x|"+strconv.Itoa(i)] = c.now().Add(time.Hour).UnixMilli()
	}
	r := fromSigned(b.Sign(t, cid, "GET", "/api/inbox", "", nil))
	if _, code := m.Verify(cid, r); code != relay.ErrRateLimited {
		t.Fatalf("full cache must refuse, got %q", code)
	}
	if len(m.nonces) != relay.MaxNonces {
		t.Fatalf("live nonces were evicted: %d", len(m.nonces))
	}
}

func TestRevokedExpiredAndIdle(t *testing.T) {
	m, c := newManager(t)
	b := relaytest.NewBrowser(t)
	pair(t, m, b)
	b.Now = c.now
	use := func() string {
		_, code := m.Verify(cid, fromSigned(b.Sign(t, cid, "GET", "/api/inbox", "", nil)))
		return code
	}
	if use() != "" {
		t.Fatal("fresh device must work")
	}

	// Idle: no use for longer than the idle limit.
	c.advance(7*24*time.Hour + time.Minute)
	if got := use(); got != relay.ErrIdleExpired {
		t.Fatalf("idle: %q", got)
	}
	if got := use(); got != relay.ErrIdleExpired && got != relay.ErrApprovalExpired {
		t.Fatalf("stays expired: %q", got)
	}
	// Re-approval brings it back.
	pair2(t, m, b)
	if use() != "" {
		t.Fatal("re-approved device must work")
	}

	// Absolute: in use every day, but approval is older than 30 days.
	for i := 0; i < 29; i++ {
		c.advance(24 * time.Hour)
		if got := use(); got != "" {
			t.Fatalf("day %d: %q", i, got)
		}
	}
	c.advance(25 * time.Hour)
	if got := use(); got != relay.ErrApprovalExpired {
		t.Fatalf("absolute: %q", got)
	}
	if code := m.Revalidate(b.DeviceID); code != relay.ErrApprovalExpired {
		t.Fatalf("revalidate expired: %q", code)
	}

	pair2(t, m, b)
	if use() != "" {
		t.Fatal(use())
	}
	if err := m.Revoke(b.DeviceID); err != nil {
		t.Fatal(err)
	}
	if got := use(); got != relay.ErrDeviceRevoked {
		t.Fatalf("revoked: %q", got)
	}
	if code := m.Revalidate(b.DeviceID); code != relay.ErrDeviceRevoked {
		t.Fatalf("revalidate revoked: %q", code)
	}
	// A revoked key cannot be approved again.
	p := b.StartPairing(t)
	if _, err := m.Offer("again-0001", p.Commit, "x", ""); err != nil {
		t.Fatal(err)
	}
	code, err := m.Reveal("again-0001", relay.Enc(b.SPKI), relay.Enc(p.Nonce))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Approve(code); err != ErrKeyRevoked {
		t.Fatalf("approve revoked key: %v", err)
	}
}

var pairSeq int

func pair2(t *testing.T, m *Manager, b *relaytest.Browser) {
	t.Helper()
	pairSeq++
	id := "repair-" + strconv.Itoa(pairSeq) + "-xxxx"
	p := b.StartPairing(t)
	if _, err := m.Offer(id, p.Commit, b.Label, ""); err != nil {
		t.Fatal(err)
	}
	code, err := m.Reveal(id, relay.Enc(b.SPKI), relay.Enc(p.Nonce))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Approve(code); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyIsConfigurable(t *testing.T) {
	m, c := newManager(t)
	m.policy = func() Policy {
		return Policy{Enabled: true, Absolute: time.Hour, Idle: 10 * time.Minute, RatePerMinute: 120}
	}
	b := relaytest.NewBrowser(t)
	pair(t, m, b)
	b.Now = c.now
	c.advance(11 * time.Minute)
	if _, code := m.Verify(cid, fromSigned(b.Sign(t, cid, "GET", "/api/inbox", "", nil))); code != relay.ErrIdleExpired {
		t.Fatalf("custom idle: %q", code)
	}
}

func TestRateLimit(t *testing.T) {
	m, c := newManager(t)
	m.policy = func() Policy { return Policy{Enabled: true, Absolute: time.Hour, Idle: time.Hour, RatePerMinute: 5} }
	for i := 0; i < 5; i++ {
		if !m.Allow("d", false) {
			t.Fatalf("read %d refused", i)
		}
	}
	if m.Allow("d", false) {
		t.Fatal("over the limit")
	}
	c.advance(61 * time.Second)
	if !m.Allow("d", true) || !m.Allow("d", true) {
		t.Fatal("window did not reset")
	}
	if m.Allow("d", true) {
		t.Fatal("writes count double: 2+2 used, third must exceed 5")
	}
}

func TestPairingRules(t *testing.T) {
	m, c := newManager(t)
	b := relaytest.NewBrowser(t)
	p := b.StartPairing(t)

	if _, err := m.Offer("bad id", p.Commit, "x", ""); err != ErrBadPairing {
		t.Fatalf("bad id: %v", err)
	}
	if _, err := m.Offer("pair-0001", "short", "x", ""); err != ErrBadPairing {
		t.Fatalf("bad commit: %v", err)
	}
	r1, err := m.Offer("pair-0001", p.Commit, "x", "")
	if err != nil {
		t.Fatal(err)
	}
	if r2, _ := m.Offer("pair-0001", p.Commit, "x", ""); r2 != r1 {
		t.Fatal("offer must be idempotent")
	}

	// A key that does not match the commitment (a cloud swapping keys) ends the pairing.
	evil := relaytest.NewBrowser(t)
	if _, err := m.Reveal("pair-0001", relay.Enc(evil.SPKI), relay.Enc(p.Nonce)); err != ErrBadPairing {
		t.Fatalf("key swap: %v", err)
	}
	if _, err := m.Reveal("pair-0001", relay.Enc(b.SPKI), relay.Enc(p.Nonce)); err != ErrPairState {
		t.Fatalf("a failed pairing cannot be revived: %v", err)
	}
	ch, _ := m.Changes()
	if len(ch) != 1 || ch[0].State != relay.PairMismatch {
		t.Fatalf("changes: %+v", ch)
	}
	if again, _ := m.Changes(); len(again) != 0 {
		t.Fatal("changes must be reported once")
	}

	// Not a P-256 key.
	p2 := b.StartPairing(t)
	_, _ = m.Offer("pair-0002", p2.Commit, "x", "")
	if _, err := m.Reveal("pair-0002", relay.Enc([]byte("not a key")), relay.Enc(p2.Nonce)); err != ErrBadPairing {
		t.Fatalf("junk key: %v", err)
	}

	// Pending limit.
	for i := 0; i < MaxPending; i++ {
		if _, err := m.Offer("flood-000"+strconv.Itoa(i), p.Commit, "x", ""); err != nil {
			t.Fatalf("offer %d: %v", i, err)
		}
	}
	if _, err := m.Offer("flood-9999", p.Commit, "x", ""); err != ErrTooManyPending {
		t.Fatalf("flood: %v", err)
	}

	// Expiry frees the slots and refuses approval.
	c.advance(PairingTTL + time.Second)
	if list, _ := m.Pending(); len(list) != 0 {
		t.Fatalf("expired pairings listed: %d", len(list))
	}
	p3 := b.StartPairing(t)
	_, _ = m.Offer("late-0001", p3.Commit, "x", "")
	code, err := m.Reveal("late-0001", relay.Enc(b.SPKI), relay.Enc(p3.Nonce))
	if err != nil {
		t.Fatal(err)
	}
	c.advance(PairingTTL + time.Second)
	if _, err := m.Approve(code); err != ErrNoPairing {
		t.Fatalf("approve after expiry: %v", err)
	}
	if _, err := m.Approve("000000"); err != ErrNoPairing {
		t.Fatalf("approve unknown code: %v", err)
	}
	if _, err := m.Approve("12"); err != ErrNoPairing {
		t.Fatalf("approve junk: %v", err)
	}
}

func TestRejectAndRevokeAll(t *testing.T) {
	m, _ := newManager(t)
	b := relaytest.NewBrowser(t)
	p := b.StartPairing(t)
	_, _ = m.Offer("rej-00001", p.Commit, "x", "")
	code, _ := m.Reveal("rej-00001", relay.Enc(b.SPKI), relay.Enc(p.Nonce))
	if err := m.Reject(relay.FormatPairCode(code)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Approve(code); err != ErrNoPairing {
		t.Fatalf("rejected pairing approved: %v", err)
	}
	if list, _ := m.List(); len(list) != 0 {
		t.Fatal("rejecting must not create a device")
	}

	pair(t, m, b)
	b2 := relaytest.NewBrowser(t)
	pair(t, m, b2)
	n, err := m.RevokeAll()
	if err != nil || n != 2 {
		t.Fatalf("revoke all: %d %v", n, err)
	}
	list, _ := m.List()
	for _, d := range list {
		if d.State != "revoked" {
			t.Fatalf("%s is %s", d.ID, d.State)
		}
	}
	if err := m.Revoke("nope"); err != ErrNoDevice {
		t.Fatalf("revoke unknown: %v", err)
	}
}

// Every state change of a pairing is reported to the cloud once, in order, and an approval is
// never swallowed by an earlier report.
func TestPairingChangesAreReportedPerState(t *testing.T) {
	m, _ := newManager(t)
	b := relaytest.NewBrowser(t)
	p := b.StartPairing(t)
	if _, err := m.Offer("chg-00001", p.Commit, "x", ""); err != nil {
		t.Fatal(err)
	}
	states := func() []string {
		ch, err := m.Changes()
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range ch {
			out = append(out, c.State)
		}
		return out
	}
	if got := states(); len(got) != 1 || got[0] != relay.PairPending {
		t.Fatalf("after offer: %v", got)
	}
	code, err := m.Reveal("chg-00001", relay.Enc(b.SPKI), relay.Enc(p.Nonce))
	if err != nil {
		t.Fatal(err)
	}
	if got := states(); len(got) != 1 || got[0] != relay.PairRevealed {
		t.Fatalf("after reveal: %v", got)
	}
	if _, err := m.Approve(code); err != nil {
		t.Fatal(err)
	}
	if got := states(); len(got) != 1 || got[0] != relay.PairApproved {
		t.Fatalf("after approval: %v", got)
	}
	if got := states(); len(got) != 0 {
		t.Fatalf("nothing new to report: %v", got)
	}
	// After a reconnect everything live is reported again.
	if err := m.MarkUnreported(); err != nil {
		t.Fatal(err)
	}
	if got := states(); len(got) != 1 || got[0] != relay.PairApproved {
		t.Fatalf("after reconnect: %v", got)
	}
}

func TestPairCodeMatchesAcrossSides(t *testing.T) {
	b := relaytest.NewBrowser(t)
	n := make([]byte, 16)
	r := make([]byte, 16)
	r[0] = 7
	a := relay.PairCode(b.SPKI, n, r)
	if len(a) != 6 || a != relay.PairCode(b.SPKI, n, r) {
		t.Fatal("code must be stable and 6 digits")
	}
	r[0] = 8
	if a == relay.PairCode(b.SPKI, n, r) {
		t.Log("collision is possible (1e-6) but unexpected")
	}
	if relay.FormatPairCode("471286") != "47 · 12 · 86" || relay.NormalizePairCode("47 · 12 · 86") != "471286" {
		t.Fatal("format/normalize")
	}
}
