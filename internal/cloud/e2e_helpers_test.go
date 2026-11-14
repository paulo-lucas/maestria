package cloud

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/paulo-lucas/maestria/internal/cli"
	"github.com/paulo-lucas/maestria/internal/config"
	"github.com/paulo-lucas/maestria/internal/connect"
	"github.com/paulo-lucas/maestria/internal/relay"
	"github.com/paulo-lucas/maestria/internal/relay/relaytest"
	"github.com/paulo-lucas/maestria/internal/store"
	"github.com/paulo-lucas/maestria/internal/workspaces"
)

// node is a real connector: its own config dir and database, the real web handlers behind the
// relay gate, and the real outbound client.
type node struct {
	t      *testing.T
	b      *bed
	name   string
	dir    string
	env    cli.Env
	out    *syncBuf
	clock  *fakeClock
	link   *connect.Link
	conn   *cli.Connector
	cancel context.CancelFunc
	done   chan error
	cloud  string // URL the connector dials (the cloud, or a tampering proxy)
}

func (b *bed) newNode(name string) *node {
	b.t.Helper()
	dir := b.t.TempDir()
	home := b.t.TempDir()
	n := &node{t: b.t, b: b, name: name, dir: dir, out: &syncBuf{}, clock: &fakeClock{t: b.clock.now()}, cloud: b.ts.URL}
	getenv := func(k string) string {
		switch k {
		case "MAESTRIA_CONFIG_DIR":
			return dir
		case "MAESTRI_HOME":
			return filepath.Join(home, ".maestri")
		}
		return ""
	}
	n.env = cli.Env{Stdin: strings.NewReader("y\n"), Stdout: n.out, Stderr: n.out, Config: config.Env{Getenv: getenv, GOOS: "linux", Home: home}, Now: n.clock.now, Version: "9.9.9-test"}
	return n
}

// cli runs a maestria command line against this node's config dir.
func (n *node) cli(stdin string, args ...string) (int, string) {
	out := &syncBuf{}
	env := n.env
	env.Stdin, env.Stdout, env.Stderr = strings.NewReader(stdin), out, out
	code := cli.Run(args, env)
	return code, out.String()
}

func (n *node) addWorkspace(name string) workspaces.Workspace {
	n.t.Helper()
	dir := filepath.Join(n.t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		n.t.Fatal(err)
	}
	ws, err := workspaces.Add(n.dir, workspaces.AddInput{Name: name, Path: dir, MaestroAgent: "Maestro", MaestroProvider: "claude"})
	if err != nil {
		n.t.Fatal(err)
	}
	return ws
}

// linkWithWebCode links through the web-code flow, driven through the real CLI.
func (n *node) linkWithWebCode(br *browser) {
	n.t.Helper()
	r := br.api("POST", "/api/link/codes", nil)
	var c struct{ Code, ID string }
	_ = json.Unmarshal([]byte(r.body), &c)
	if c.Code == "" {
		n.t.Fatalf("no link code: %d %s", r.StatusCode, r.body)
	}
	ctx, cancel := context.WithCancel(context.Background())
	env := n.env
	env.Ctx = ctx
	env.Stdin = strings.NewReader(n.name + "\ny\n")
	env.Stdout, env.Stderr = n.out, n.out
	done := make(chan int, 1)
	go func() { done <- cli.Run([]string{"connect", "--link", c.Code, "--cloud", n.cloud}, env) }()
	n.waitLinked()
	cancel()
	<-done
}

func (n *node) waitLinked() {
	n.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if l, _ := connect.LoadLink(n.dir); l != nil {
			n.link = l
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	n.t.Fatalf("machine never linked; output:\n%s", n.out.String())
}

// start runs the connector client.
func (n *node) start() {
	n.t.Helper()
	l, err := connect.LoadLink(n.dir)
	if err != nil || l == nil {
		n.t.Fatalf("no link: %v", err)
	}
	l.Cloud = n.cloud
	n.link = l
	c, err := cli.BuildConnector(&n.env, l, nil)
	if err != nil {
		n.t.Fatal(err)
	}
	n.conn = c
	c.Client.MinBackoff, c.Client.MaxBackoff, c.Client.Tick = 20*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	n.cancel = cancel
	n.done = make(chan error, 1)
	go func() { n.done <- c.Client.Run(ctx) }()
	n.t.Cleanup(n.stop)
}

func (n *node) stop() {
	if n.cancel != nil {
		n.cancel()
		select {
		case <-n.done:
		case <-time.After(5 * time.Second):
		}
		n.cancel = nil
		if n.conn != nil {
			n.conn.Close()
			n.conn = nil
		}
	}
}

func (n *node) waitOnline(b *bed) {
	n.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, ok := b.srv.hub.Online(n.link.ConnectorID); ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	n.t.Fatalf("%s never came online", n.name)
}

func (n *node) waitOffline(b *bed) {
	n.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, ok := b.srv.hub.Online(n.link.ConnectorID); !ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	n.t.Fatalf("%s never went offline", n.name)
}

func (n *node) setKill(on bool) {
	n.t.Helper()
	v := "0"
	if on {
		v = "1"
	}
	if err := store.SetKV(n.conn.Server.DB, "tunnel_killed", v); err != nil {
		n.t.Fatal(err)
	}
}

// ---- relayed calls from a fake browser ----

// call sends a signed request through the cloud to a node.
func (n *node) call(br *browser, dev *relaytest.Browser, method, path, query string, body []byte) *resp {
	n.t.Helper()
	dev.Now = n.clock.now
	return n.send(br, dev.Sign(n.t, n.link.ConnectorID, method, path, query, body))
}

func (n *node) send(br *browser, sr *relaytest.Request) *resp {
	n.t.Helper()
	target := n.b.ts.URL + "/c/" + n.link.ConnectorID + sr.Path
	if sr.RawQuery != "" {
		target += "?" + sr.RawQuery
	}
	var rd io.Reader
	if sr.Method == "POST" {
		rd = bytes.NewReader(sr.Body)
	}
	req, _ := http.NewRequest(sr.Method, target, rd)
	for k, v := range sr.Header {
		req.Header.Set(k, v)
	}
	if sr.Method == "POST" {
		if br.csrf == "" {
			var me struct {
				CSRF string `json:"csrfToken"`
			}
			_ = json.Unmarshal([]byte(br.get("/api/me").body), &me)
			br.csrf = me.CSRF
		}
		req.Header.Set("Origin", n.b.ts.URL)
		req.Header.Set("X-CSRF-Token", br.csrf)
		req.Header.Set("Content-Type", "application/json")
	}
	return br.do(req)
}

func errCode(r *resp) string {
	var e struct{ Error string }
	_ = json.Unmarshal([]byte(r.body), &e)
	return e.Error
}

// pair drives the whole device pairing the way the frontend does, approving with the real CLI on
// each given node.
func (b *bed) pair(br *browser, dev *relaytest.Browser, nodes ...*node) {
	b.t.Helper()
	p := dev.StartPairing(b.t)
	r := br.api("POST", "/api/pair", map[string]string{"commit": p.Commit, "label": dev.Label})
	var view struct {
		ID       string `json:"id"`
		Machines []PairMachine
	}
	if err := json.Unmarshal([]byte(r.body), &view); err != nil || view.ID == "" {
		b.t.Fatalf("pair start: %d %s", r.StatusCode, r.body)
	}
	get := func() map[string]PairMachine {
		var v struct{ Machines []PairMachine }
		_ = json.Unmarshal([]byte(br.api("GET", "/api/pair/"+view.ID, nil).body), &v)
		m := map[string]PairMachine{}
		for _, x := range v.Machines {
			m[x.ConnectorID] = x
		}
		return m
	}
	for _, n := range nodes {
		var m PairMachine
		waitFor(b.t, "challenge from "+n.name, func() bool { m = get()[n.link.ConnectorID]; return m.R != "" })
		rr := br.api("POST", "/api/pair/"+view.ID+"/reveal", map[string]string{"connectorId": n.link.ConnectorID, "spki": relay.Enc(dev.SPKI), "nonce": relay.Enc(p.Nonce)})
		if rr.StatusCode != 200 {
			b.t.Fatalf("reveal: %d %s", rr.StatusCode, rr.body)
		}
		code := dev.Code(b.t, p, m.R)
		waitFor(b.t, "code visible on "+n.name, func() bool {
			c, out := n.cli("", "devices", "pending")
			return c == 0 && strings.Contains(out, relay.FormatPairCode(code))
		})
		if c, out := n.cli("", "devices", "approve", relay.FormatPairCode(code)); c != 0 {
			b.t.Fatalf("approve on %s: %s", n.name, out)
		}
		waitFor(b.t, "approved state on "+n.name, func() bool { return get()[n.link.ConnectorID].State == "approved" })
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// ---- a cloud that tampers ----

// tamperProxy sits between a connector and the cloud like a compromised cloud process would: it
// can rewrite or invent any frame sent to the connector.
type tamperProxy struct {
	srv    *http.Server
	url    string
	mu     sync.Mutex
	mutate func(*relay.Frame)
	inject chan relay.Frame
	seen   chan relay.Frame // responses the connector sent
}

func newTamperProxy(t *testing.T, upstream string) *tamperProxy {
	t.Helper()
	p := &tamperProxy{inject: make(chan relay.Frame, 8), seen: make(chan relay.Frame, 64)}
	mux := http.NewServeMux()
	mux.HandleFunc("/connector", func(w http.ResponseWriter, r *http.Request) {
		down, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		down.SetReadLimit(relay.MaxFrame)
		ctx := r.Context()
		hdr := http.Header{"Authorization": {r.Header.Get("Authorization")}}
		wsURL := "ws" + strings.TrimPrefix(upstream, "http") + "/connector"
		up, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr})
		if err != nil {
			down.Close(websocket.StatusInternalError, "upstream")
			return
		}
		up.SetReadLimit(relay.MaxFrame)
		pump := func(from, to *websocket.Conn, toConn bool) {
			for {
				mt, data, err := from.Read(ctx)
				if err != nil {
					to.CloseNow()
					return
				}
				var f relay.Frame
				if json.Unmarshal(data, &f) == nil {
					if toConn {
						p.mu.Lock()
						mut := p.mutate
						p.mu.Unlock()
						if mut != nil {
							mut(&f)
							data, _ = json.Marshal(f)
						}
					} else {
						select {
						case p.seen <- f:
						default:
						}
					}
				}
				if err := to.Write(ctx, mt, data); err != nil {
					return
				}
			}
		}
		go func() {
			for {
				select {
				case f := <-p.inject:
					b, _ := json.Marshal(f)
					_ = down.Write(ctx, websocket.MessageText, b)
				case <-ctx.Done():
					return
				}
			}
		}()
		go pump(up, down, true)
		pump(down, up, false)
	})
	p.srv = &http.Server{Handler: mux}
	ts := newServer(t, p.srv)
	p.url = ts
	return p
}

func (p *tamperProxy) setMutate(fn func(*relay.Frame)) { p.mu.Lock(); p.mutate = fn; p.mu.Unlock() }

// waitFrame returns the next frame from the connector that matches.
func (p *tamperProxy) waitFrame(t *testing.T, match func(relay.Frame) bool) relay.Frame {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f := <-p.seen:
			if match(f) {
				return f
			}
		case <-deadline:
			t.Fatal("no matching frame from the connector")
		}
	}
}

func queryEsc(s string) string { return url.QueryEscape(s) }

func urlValues(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
}

func (n *node) openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(n.dir)
	if err != nil {
		t.Fatal(err)
	}
	return db
}
