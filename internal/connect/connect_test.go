package connect

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/paulo-lucas/maestria/internal/audit"
	"github.com/paulo-lucas/maestria/internal/devices"
	"github.com/paulo-lucas/maestria/internal/relay"
	"github.com/paulo-lucas/maestria/internal/relay/relaytest"
	"github.com/paulo-lucas/maestria/internal/store"
)

const cid = "cn_0123456789abcdef0123456789abcdef"

// fakeCloud accepts one connector socket and lets the test play the cloud.
type fakeCloud struct {
	t      *testing.T
	srv    *httptest.Server
	conns  chan *websocket.Conn
	authFn atomic.Pointer[func(r *http.Request) int]
}

func newFakeCloud(t *testing.T) *fakeCloud {
	fc := &fakeCloud{t: t, conns: make(chan *websocket.Conn, 4)}
	fc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fn := fc.authFn.Load(); fn != nil {
			if code := (*fn)(r); code != 0 {
				w.WriteHeader(code)
				return
			}
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c.SetReadLimit(relay.MaxFrame)
		fc.conns <- c
		<-r.Context().Done()
	}))
	t.Cleanup(fc.srv.Close)
	return fc
}

func (fc *fakeCloud) next(t *testing.T) *websocket.Conn {
	t.Helper()
	select {
	case c := <-fc.conns:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("connector did not dial")
		return nil
	}
}

func readFrame(t *testing.T, c *websocket.Conn) relay.Frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var f relay.Frame
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func writeFrame(t *testing.T, c *websocket.Conn, f relay.Frame) {
	t.Helper()
	b, _ := json.Marshal(f)
	if err := c.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

type rig struct {
	t       *testing.T
	fc      *fakeCloud
	conn    *websocket.Conn
	dev     *devices.Manager
	browser *relaytest.Browser
	killed  bool
	killMu  sync.Mutex
	cancel  context.CancelFunc
	done    chan error
	client  *Client
	audit   *audit.Log
	reqSeq  int
}

func (r *rig) kill(v bool) { r.killMu.Lock(); r.killed = v; r.killMu.Unlock() }
func (r *rig) isKilled() bool {
	r.killMu.Lock()
	defer r.killMu.Unlock()
	return r.killed
}

func newRig(t *testing.T, handler http.Handler) *rig {
	t.Helper()
	db, err := store.OpenPath(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	fc := newFakeCloud(t)
	r := &rig{t: t, fc: fc, browser: relaytest.NewBrowser(t), done: make(chan error, 1), audit: audit.New(db, nil)}
	r.dev = devices.New(db, nil, nil)
	link := &Link{Cloud: fc.srv.URL, ConnectorID: cid, Credential: relay.FormatCredential(cid, strings.Repeat("s", 43)), Name: "Test box"}
	gate := &Gate{ConnectorID: cid, Devices: r.dev, Killed: r.isKilled, Audit: r.audit}
	r.client = &Client{Link: link, Version: "test", Name: link.Name, Handler: handler, Gate: gate, Devices: r.dev, Killed: r.isKilled,
		MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond, Tick: 50 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() { r.done <- r.client.Run(ctx) }()
	r.conn = fc.next(t)
	hello := readFrame(t, r.conn)
	if hello.T != relay.TypeHello || hello.Proto != relay.Proto || hello.Name != "Test box" {
		t.Fatalf("hello: %+v", hello)
	}
	t.Cleanup(func() { cancel(); r.conn.CloseNow() })
	return r
}

// approve pairs the rig's browser directly on the connector (as the CLI would).
func (r *rig) approve() {
	p := r.browser.StartPairing(r.t)
	if _, err := r.dev.Offer("pair-"+r.browser.DeviceID[:8], p.Commit, "Test", ""); err != nil {
		r.t.Fatal(err)
	}
	code, err := r.dev.Reveal("pair-"+r.browser.DeviceID[:8], relay.Enc(r.browser.SPKI), relay.Enc(p.Nonce))
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.dev.Approve(code); err != nil {
		r.t.Fatal(err)
	}
}

// do sends a signed request over the fake cloud and collects the response.
func (r *rig) do(req *relaytest.Request) (relay.Frame, []byte) {
	r.t.Helper()
	r.reqSeq++
	id := "r_" + strings.Repeat("a", 7) + string(rune('a'+r.reqSeq%26))
	hdr := map[string]string{}
	for k, v := range req.Header {
		hdr[strings.ToLower(k)] = v
	}
	hdr["content-type"] = "application/json"
	writeFrame(r.t, r.conn, relay.Frame{T: relay.TypeReq, ID: id, Method: req.Method, Path: req.Path, Query: req.RawQuery, Header: hdr, Body: relay.Enc(req.Body)})
	return r.collect(id)
}

func (r *rig) collect(id string) (relay.Frame, []byte) {
	r.t.Helper()
	var head relay.Frame
	var body []byte
	for {
		f := readFrame(r.t, r.conn)
		if f.ID != id {
			continue
		}
		switch f.T {
		case relay.TypeErr:
			return f, nil
		case relay.TypeRes:
			head = f
		case relay.TypeChunk:
		default:
			continue
		}
		b, _ := relay.Dec(f.Body)
		body = append(body, b...)
		writeFrame(r.t, r.conn, relay.Frame{T: relay.TypeAck, ID: id, N: int64(len(b))})
		if !f.More {
			return head, body
		}
	}
}

func api() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/inbox", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"items":[],"status":"`+r.URL.Query().Get("status")+`","device":"`+relay.DeviceFrom(r.Context())+`"}`)
	})
	mux.HandleFunc("POST /api/workspaces/{id}/chat", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"echo":` + string(b) + `,"ws":"` + r.PathValue("id") + `"}`))
	})
	mux.HandleFunc("GET /api/usage", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte(strings.Repeat("x", 3*relay.MaxResponseBody)))
	})
	mux.HandleFunc("POST /api/inbox/dismiss-notices", func(w http.ResponseWriter, r *http.Request) {
		// A write that executes but whose response is too large for the relay.
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte(strings.Repeat("x", 3*relay.MaxResponseBody)))
	})
	mux.HandleFunc("GET /api/floors", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("y", 200*1024)))
	})
	mux.HandleFunc("GET /api/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	mux.HandleFunc("GET /api/wire", func(w http.ResponseWriter, r *http.Request) { panic("boom") })
	return mux
}

func TestServesSignedRequestsWithTheSameHandlers(t *testing.T) {
	r := newRig(t, api())
	r.approve()
	r.browser.Now = time.Now

	head, body := r.do(r.browser.Sign(t, cid, "GET", "/api/inbox", "status=open", nil))
	if head.Status != 200 || !strings.Contains(string(body), `"status":"open"`) || !strings.Contains(string(body), r.browser.DeviceID) {
		t.Fatalf("GET: %+v %s", head, body)
	}
	if head.Header["content-type"] != "application/json" {
		t.Fatalf("headers: %v", head.Header)
	}
	head, body = r.do(r.browser.Sign(t, cid, "POST", "/api/workspaces/atlas/chat", "", []byte(`{"body":"hi"}`)))
	if head.Status != 200 || string(body) != `{"echo":{"body":"hi"},"ws":"atlas"}` {
		t.Fatalf("POST: %+v %s", head, body)
	}
	// Writes are audited without bodies.
	entries, _ := r.audit.Recent(10)
	found := false
	for _, e := range entries {
		if e.Event == "relay_write" {
			found = true
			if strings.Contains(e.Detail, "hi") && strings.Contains(e.Detail, `"body"`) {
				t.Fatalf("audit leaked a body: %s", e.Detail)
			}
		}
	}
	if !found {
		t.Fatal("relay write not audited")
	}
}

func TestRefusals(t *testing.T) {
	r := newRig(t, api())
	r.approve()
	other := relaytest.NewBrowser(t)

	expectErr := func(name, code string, f relay.Frame) {
		t.Helper()
		if f.T != relay.TypeErr || f.Code != code || f.Status != relay.StatusFor(code) {
			t.Errorf("%s: got %+v, want err %s", name, f, code)
		}
	}
	// No signature at all.
	writeFrame(t, r.conn, relay.Frame{T: relay.TypeReq, ID: "r_unsigned", Method: "GET", Path: "/api/inbox"})
	f, _ := r.collect("r_unsigned")
	expectErr("unsigned", relay.ErrUnsigned, f)

	// Tampered by the cloud: path, method, query, body, device.
	good := func() *relaytest.Request {
		return r.browser.Sign(t, cid, "POST", "/api/workspaces/atlas/chat", "", []byte(`{"body":"hi"}`))
	}
	send := func(mut func(*relaytest.Request)) relay.Frame {
		q := good()
		mut(q)
		f, _ := r.do(q)
		return f
	}
	expectErr("path", relay.ErrBadSignature, send(func(q *relaytest.Request) { q.Path = "/api/workspaces/other/chat" }))
	expectErr("query", relay.ErrBadSignature, send(func(q *relaytest.Request) { q.RawQuery = "x=1" }))
	expectErr("body", relay.ErrBodyMismatch, send(func(q *relaytest.Request) { q.Body = []byte(`{"body":"evil"}`) }))
	expectErr("method", relay.ErrBadSignature, send(func(q *relaytest.Request) {
		q.Method = "GET"
		q.Body = nil
		q.Header[relay.HeaderBody] = relay.BodyHash(nil)
	}))

	// Wrong device: signed by a key the machine never approved.
	f, _ = r.do(other.Sign(t, cid, "GET", "/api/inbox", "", nil))
	expectErr("wrong device", relay.ErrUnknownDevice, f)

	// Replay of an accepted request.
	q := r.browser.Sign(t, cid, "GET", "/api/inbox", "", nil)
	if h, _ := r.do(q); h.Status != 200 {
		t.Fatalf("first use: %+v", h)
	}
	f, _ = r.do(q)
	expectErr("replay", relay.ErrReplay, f)

	// Stale timestamp.
	f, _ = r.do(r.browser.SignAt(t, cid, "GET", "/api/inbox", "", nil, time.Now().Add(-2*time.Minute).UnixMilli(), relay.Enc(make([]byte, 16))))
	expectErr("stale", relay.ErrStaleTimestamp, f)

	// A route outside the allowlist, validly signed.
	for _, rt := range [][2]string{{"POST", "/login"}, {"GET", "/api/session"}, {"POST", "/api/devices/x/approve"}, {"GET", "/security"}, {"POST", "/logout-all"}} {
		f, _ = r.do(r.browser.Sign(t, cid, rt[0], rt[1], "", nil))
		expectErr("route "+rt[1], relay.ErrRouteForbidden, f)
	}
	// Junk paths never reach the router.
	f, _ = r.do(r.browser.Sign(t, cid, "GET", "/api/../login", "", nil))
	expectErr("dotdot", relay.ErrBadRequest, f)

	// Revoked device.
	if err := r.dev.Revoke(r.browser.DeviceID); err != nil {
		t.Fatal(err)
	}
	f, _ = r.do(r.browser.Sign(t, cid, "GET", "/api/inbox", "", nil))
	expectErr("revoked", relay.ErrDeviceRevoked, f)
}

func TestKillSwitchBlocksEverything(t *testing.T) {
	r := newRig(t, api())
	r.approve()
	if h, _ := r.do(r.browser.Sign(t, cid, "GET", "/api/inbox", "", nil)); h.Status != 200 {
		t.Fatal("baseline")
	}
	r.kill(true)
	f, _ := r.do(r.browser.Sign(t, cid, "GET", "/api/inbox", "", nil))
	if f.T != relay.TypeErr || f.Code != relay.ErrKillSwitch {
		t.Fatalf("kill switch: %+v", f)
	}
	// Pairing is refused too, and the cloud hears about the kill switch.
	writeFrame(t, r.conn, relay.Frame{T: relay.TypePairOffer, Pair: "pair-kill-1", Commit: relay.Enc(make([]byte, 32)), Label: "x"})
	for {
		f := readFrame(t, r.conn)
		if f.T == relay.TypePairState && f.Pair == "pair-kill-1" {
			if f.State != relay.PairRejected || f.Code != relay.ErrKillSwitch {
				t.Fatalf("pair under kill switch: %+v", f)
			}
			break
		}
	}
	r.kill(false)
	if h, _ := r.do(r.browser.Sign(t, cid, "GET", "/api/inbox", "", nil)); h.Status != 200 {
		t.Fatal("relay must work again once the switch is off locally")
	}
}

func TestKillSwitchAnnouncedInHello(t *testing.T) {
	r := newRig(t, api())
	r.kill(true)
	for {
		f := readFrame(t, r.conn)
		if f.T == relay.TypeHello {
			if !f.KillSwitch {
				t.Fatal("hello must carry the kill switch")
			}
			return
		}
	}
}

func TestResponseLimitsAndFlowControl(t *testing.T) {
	r := newRig(t, api())
	r.approve()

	// A large (non-stream) body flows in chunks under the credit window and arrives whole.
	head, body := r.do(r.browser.Sign(t, cid, "GET", "/api/floors", "", nil))
	if head.Status != 200 || len(body) != 200*1024 {
		t.Fatalf("large body: %d %d", head.Status, len(body))
	}

	// A response larger than the limit is aborted.
	f, _ := r.do(r.browser.Sign(t, cid, "GET", "/api/usage", "", nil))
	if f.T != relay.TypeErr || f.Code != relay.ErrTooLarge {
		t.Fatalf("oversize: %+v", f)
	}
	// A write whose response is too large has still executed: it must be audited.
	f, _ = r.do(r.browser.Sign(t, cid, "POST", "/api/inbox/dismiss-notices", "", nil))
	if f.T != relay.TypeErr || f.Code != relay.ErrTooLarge {
		t.Fatalf("oversize write: %+v", f)
	}
	entries, _ := r.audit.Recent(10)
	audited := false
	for _, e := range entries {
		if e.Event == "relay_write" && strings.Contains(e.Detail, "dismiss-notices") {
			audited = true
		}
	}
	if !audited {
		t.Fatal("an executed write with an oversize response was not audited")
	}
	// Oversized request bodies are refused before anything runs.
	big := make([]byte, relay.MaxRequestBody+1)
	f, _ = r.do(r.browser.Sign(t, cid, "POST", "/api/workspaces/a/chat", "", big))
	if f.T != relay.TypeErr || f.Code != relay.ErrTooLarge {
		t.Fatalf("big request: %+v", f)
	}
	// A panic in a handler is an internal error, not a dead connector.
	f, _ = r.do(r.browser.Sign(t, cid, "GET", "/api/wire", "", nil))
	if f.T != relay.TypeErr || f.Code != relay.ErrInternal {
		t.Fatalf("panic: %+v", f)
	}
	if h, _ := r.do(r.browser.Sign(t, cid, "GET", "/api/inbox", "", nil)); h.Status != 200 {
		t.Fatal("connector must survive a panicking handler")
	}
}

func TestFlowControlStopsWithoutAcks(t *testing.T) {
	r := newRig(t, api())
	r.approve()
	// Without acks the connector stops after the window: it never floods the cloud.
	q := r.browser.Sign(t, cid, "GET", "/api/floors", "", nil)
	hdr := map[string]string{}
	for k, v := range q.Header {
		hdr[strings.ToLower(k)] = v
	}
	writeFrame(t, r.conn, relay.Frame{T: relay.TypeReq, ID: "r_noack", Method: "GET", Path: q.Path, Header: hdr})
	var gotBytes atomic.Int64
	go func() {
		for {
			_, data, err := r.conn.Read(context.Background())
			if err != nil {
				return
			}
			var f relay.Frame
			_ = json.Unmarshal(data, &f)
			if f.ID == "r_noack" {
				b, _ := relay.Dec(f.Body)
				gotBytes.Add(int64(len(b)))
			}
		}
	}()
	time.Sleep(600 * time.Millisecond)
	got := int(gotBytes.Load())
	if got == 0 || got > relay.FlowWindow+relay.BodyChunk {
		t.Fatalf("without acks the connector sent %d bytes (window %d)", got, relay.FlowWindow)
	}
	writeFrame(t, r.conn, relay.Frame{T: relay.TypeCancel, ID: "r_noack"})

}

func TestStreamEndsOnCancelAndOnRevocation(t *testing.T) {
	r := newRig(t, api())
	r.approve()
	q := r.browser.Sign(t, cid, "GET", "/api/stream", "", nil)
	hdr := map[string]string{}
	for k, v := range q.Header {
		hdr[strings.ToLower(k)] = v
	}
	writeFrame(t, r.conn, relay.Frame{T: relay.TypeReq, ID: "r_stream", Method: "GET", Path: "/api/stream", Header: hdr})
	var head relay.Frame
	for {
		f := readFrame(t, r.conn)
		if f.ID == "r_stream" && f.T == relay.TypeRes {
			head = f
			break
		}
	}
	if head.Status != 200 || !head.More {
		t.Fatalf("stream head: %+v", head)
	}
	writeFrame(t, r.conn, relay.Frame{T: relay.TypeCancel, ID: "r_stream"})
	// The connector keeps serving other requests.
	if h, _ := r.do(r.browser.Sign(t, cid, "GET", "/api/inbox", "", nil)); h.Status != 200 {
		t.Fatal("after cancel")
	}
}

func TestPairingMessages(t *testing.T) {
	r := newRig(t, api())
	p := r.browser.StartPairing(t)
	writeFrame(t, r.conn, relay.Frame{T: relay.TypePairOffer, Pair: "pair-msg-01", Commit: p.Commit, Label: "Safari", UA: "UA"})
	var ch relay.Frame
	for ch.T != relay.TypePairChallenge {
		ch = readFrame(t, r.conn)
	}
	if ch.Pair != "pair-msg-01" || ch.R == "" {
		t.Fatalf("challenge: %+v", ch)
	}
	// The good path: reveal, local approval, then the cloud hears "approved".
	p2 := r.browser.StartPairing(t)
	writeFrame(t, r.conn, relay.Frame{T: relay.TypePairOffer, Pair: "pair-msg-02", Commit: p2.Commit, Label: "Safari"})
	for ch.T = ""; ch.T != relay.TypePairChallenge || ch.Pair != "pair-msg-02"; {
		ch = readFrame(t, r.conn)
	}
	writeFrame(t, r.conn, relay.Frame{T: relay.TypePairReveal, Pair: "pair-msg-02", SPKI: relay.Enc(r.browser.SPKI), Nonce: relay.Enc(p2.Nonce)})
	code := r.browser.Code(t, p2, ch.R)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("pairing never became approvable")
		}
		if _, err := r.dev.Approve(code); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	for {
		f := readFrame(t, r.conn)
		if f.T == relay.TypePairState && f.Pair == "pair-msg-02" && f.State == relay.PairApproved {
			if f.Code != "" {
				t.Fatal("the code must never be sent to the cloud")
			}
			break
		}
	}

	// A reveal with the wrong key is a mismatch and never produces a code the user could approve.
	evil := relaytest.NewBrowser(t)
	writeFrame(t, r.conn, relay.Frame{T: relay.TypePairReveal, Pair: "pair-msg-01", SPKI: relay.Enc(evil.SPKI), Nonce: relay.Enc(p.Nonce)})
	for {
		f := readFrame(t, r.conn)
		if f.T == relay.TypePairState && f.State == relay.PairMismatch {
			break
		}
	}
	pend, _ := r.dev.Pending()
	if len(pend) != 0 {
		t.Fatalf("mismatched pairing is still pending: %+v", pend)
	}

	// A failed reveal pauses pairing: the next offer is refused and no challenge is sent.
	p3 := r.browser.StartPairing(t)
	writeFrame(t, r.conn, relay.Frame{T: relay.TypePairOffer, Pair: "pair-msg-03", Commit: p3.Commit, Label: "Safari"})
	for {
		f := readFrame(t, r.conn)
		if f.T == relay.TypePairChallenge && f.Pair == "pair-msg-03" {
			t.Fatal("an offer right after a failed reveal got a challenge")
		}
		if f.T == relay.TypePairState && f.Pair == "pair-msg-03" {
			if f.State != relay.PairRejected || f.Code != "pairing_paused" {
				t.Fatalf("paused offer: %+v", f)
			}
			break
		}
	}
}

func TestForgetDevicesAndDeviceReports(t *testing.T) {
	r := newRig(t, api())
	r.approve()
	writeFrame(t, r.conn, relay.Frame{T: relay.TypeForget, At: time.Now().UnixMilli()})
	deadline := time.Now().Add(3 * time.Second)
	for {
		f := readFrame(t, r.conn)
		if f.T == relay.TypeHello && len(f.Devices) == 1 && f.Devices[0].State == "revoked" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("devices were not revoked/reported")
		}
	}
	f, _ := r.do(r.browser.Sign(t, cid, "GET", "/api/inbox", "", nil))
	if f.Code != relay.ErrDeviceRevoked {
		t.Fatalf("after forget: %+v", f)
	}
}

func TestReconnectsWithBackoffAndStopsWhenRevoked(t *testing.T) {
	r := newRig(t, api())
	r.conn.CloseNow()
	c2 := r.fc.next(t) // reconnect
	if f := readFrame(t, c2); f.T != relay.TypeHello {
		t.Fatalf("hello after reconnect: %+v", f)
	}
	deny := func(*http.Request) int { return http.StatusUnauthorized }
	r.fc.authFn.Store(&deny)
	c2.CloseNow()
	select {
	case err := <-r.done:
		if err != ErrRevoked {
			t.Fatalf("run ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a revoked connector must stop retrying")
	}
}

func TestCleanShutdown(t *testing.T) {
	r := newRig(t, api())
	closed := make(chan websocket.StatusCode, 1)
	go func() {
		// The cloud always reads, so it answers the close handshake.
		for {
			if _, _, err := r.conn.Read(context.Background()); err != nil {
				closed <- websocket.CloseStatus(err)
				return
			}
		}
	}()
	r.cancel()
	select {
	case err := <-r.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not stop")
	}
	if got := <-closed; got != websocket.StatusNormalClosure {
		t.Fatalf("the connector must close with a normal closure, got %v", got)
	}
}

func TestLinkFile(t *testing.T) {
	dir := t.TempDir()
	if l, err := LoadLink(dir); l != nil || err != nil {
		t.Fatalf("empty: %v %v", l, err)
	}
	l := &Link{Cloud: "https://c.example", ConnectorID: cid, Credential: relay.FormatCredential(cid, strings.Repeat("a", 43)), Name: "Box"}
	if err := SaveLink(dir, l); err != nil {
		t.Fatal(err)
	}
	got, err := LoadLink(dir)
	if err != nil || got.Name != "Box" || got.Credential != l.Credential {
		t.Fatalf("%v %v", got, err)
	}
	if err := DeleteLink(dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := LoadLink(dir); got != nil {
		t.Fatal("not deleted")
	}
}

func TestSocketURLRules(t *testing.T) {
	for in, want := range map[string]string{
		"https://cloud.example":  "wss://cloud.example/connector",
		"https://cloud.example/": "wss://cloud.example/connector",
		"https://x.example/sub":  "wss://x.example/sub/connector",
		"http://127.0.0.1:8080":  "ws://127.0.0.1:8080/connector",
		"http://localhost:8080/": "ws://localhost:8080/connector",
	} {
		got, err := relay.SocketURL(in)
		if err != nil || got != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"http://cloud.example", "ftp://x", "cloud.example", "", "ws://cloud.example"} {
		if _, err := relay.SocketURL(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}
