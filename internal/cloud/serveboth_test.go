package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maestria-dev/maestria/internal/auth"
	"github.com/maestria-dev/maestria/internal/cli"
	"github.com/maestria-dev/maestria/internal/connect"
	"github.com/maestria-dev/maestria/internal/relay/relaytest"
	"github.com/maestria-dev/maestria/internal/runlock"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// localPanel gives a node what `maestria serve` needs: the local login and a port.
func (n *node) localPanel(t *testing.T, relayJSON string) int {
	t.Helper()
	hash, err := auth.HashPassword(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := auth.GenerateTOTPSecret()
	if err := auth.SaveSecrets(n.dir, &auth.Secrets{PasswordHash: hash, TOTPSecret: secret, CreatedAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	cfg := fmt.Sprintf(`{"port": %d, "cookieSecure": false%s}`, port, relayJSON)
	if err := os.WriteFile(filepath.Join(n.dir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return port
}

// serve runs `maestria serve <args>` in the background and returns how to stop it.
type served struct {
	cancel   context.CancelFunc
	finished chan struct{} // closed when serve returned
	code     int
	out      *syncBuf
}

func (n *node) serve(args ...string) *served {
	ctx, cancel := context.WithCancel(context.Background())
	env := n.env
	env.Ctx = ctx
	out := &syncBuf{}
	env.Stdin, env.Stdout, env.Stderr = strings.NewReader(""), out, out
	s := &served{cancel: cancel, finished: make(chan struct{}), out: out}
	go func() {
		s.code = cli.Run(append([]string{"serve"}, args...), env)
		close(s.finished)
	}()
	n.t.Cleanup(func() {
		cancel()
		select {
		case <-s.finished:
		case <-time.After(5 * time.Second):
		}
	})
	return s
}

func (s *served) stop(t *testing.T) int {
	t.Helper()
	s.cancel()
	select {
	case <-s.finished:
		return s.code
	case <-time.After(8 * time.Second):
		t.Fatal("serve did not stop")
		return -1
	}
}

func waitPanel(t *testing.T, port int) {
	t.Helper()
	waitFor(t, "the local panel", func() bool {
		res, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/login", port))
		if err != nil {
			return false
		}
		res.Body.Close()
		return res.StatusCode == 200
	})
}

// One process runs the local panel and the cloud connection, shares the database, takes the run
// lock, and leaves the CLI commands usable.
func TestServeRunsPanelAndConnectionInOneProcess(t *testing.T) {
	b := newBed(t)
	br := b.browser("10.0.0.1")
	br.signup("ann@example.com", testPassword)
	n := b.newNode("Workstation")
	n.addWorkspace("Atlas")
	n.linkWithWebCode(br)
	port := n.localPanel(t, "")

	s := n.serve()
	waitPanel(t, port)
	n.link, _ = connect.LoadLink(n.dir)
	n.waitOnline(b)
	if !strings.Contains(s.out.String(), "Also connecting to") {
		t.Fatalf("serve must say it also connects:\n%s", s.out.String())
	}

	// The CLI keeps working next to the running process: approve a browser, then use the relay.
	dev := relaytest.NewBrowser(t)
	b.pair(br, dev, n)
	if code, out := n.cli("", "status"); code != 0 {
		t.Fatalf("status next to a running serve: %d %s", code, out)
	}
	if code, out := n.cli("", "connect", "status"); code != 0 || !strings.Contains(out, "online") || !strings.Contains(out, "Approved devices: 1") {
		t.Fatalf("connect status: %d %s", code, out)
	}
	if r := n.call(br, dev, "GET", "/api/workspaces", "", nil); r.StatusCode != 200 || !strings.Contains(r.body, "Atlas") {
		t.Fatalf("relayed request: %d %s", r.StatusCode, r.body)
	}

	// The state row shows this very process (one process, one connection).
	db := n.openDB(t)
	var raw string
	_ = db.QueryRow("SELECT value FROM kv WHERE key = 'connect.state'").Scan(&raw)
	db.Close()
	var st struct{ PID int }
	_ = json.Unmarshal([]byte(raw), &st)
	if st.PID != os.Getpid() {
		t.Fatalf("connect state belongs to pid %d, want this process (%d)", st.PID, os.Getpid())
	}

	// A second process that would also run serve or connect on this directory is refused.
	for _, args := range [][]string{{"serve"}, {"serve", "--no-connect"}, {"connect"}} {
		code, out := n.cli("", args...)
		if code == 0 || !strings.Contains(out, "already running") || !strings.Contains(out, "MAESTRIA_CONFIG_DIR") {
			t.Errorf("maestria %s next to a running serve: exit %d\n%s", strings.Join(args, " "), code, out)
		}
	}
	// A different config directory is a different machine and is not affected.
	other := b.newNode("Other")
	if l, err := runlock.Acquire(other.dir, "serve"); err != nil {
		t.Fatalf("another directory must be free: %v", err)
	} else {
		l.Release()
	}

	// Stopping serve closes the connection and frees the lock.
	if code := s.stop(t); code != 0 {
		t.Fatalf("serve exit code %d\n%s", code, s.out.String())
	}
	n.waitOffline(b)
	l, err := runlock.Acquire(n.dir, "connect")
	if err != nil {
		t.Fatalf("the lock must be free after serve stopped: %v", err)
	}
	l.Release()
}

func TestServeConnectFlags(t *testing.T) {
	b := newBed(t)
	br := b.browser("10.0.0.1")
	br.signup("ann@example.com", testPassword)
	n := b.newNode("Workstation")
	port := n.localPanel(t, "")

	// Not linked: plain serve is local only, --connect refuses with a pointer to the fix.
	if code, out := n.cli("", "serve", "--connect"); code == 0 || !strings.Contains(out, "not linked") {
		t.Fatalf("--connect without a link: %d %s", code, out)
	}
	s := n.serve()
	waitPanel(t, port)
	if strings.Contains(s.out.String(), "Also connecting") {
		t.Fatal("an unlinked machine must not try to connect")
	}
	if code := s.stop(t); code != 0 {
		t.Fatalf("exit %d", code)
	}

	// Linked: serve connects by itself; --no-connect keeps it local.
	n.linkWithWebCode(br)
	s = n.serve("--no-connect")
	waitPanel(t, port)
	time.Sleep(300 * time.Millisecond)
	if _, _, ok := b.srv.hub.Online(n.link.ConnectorID); ok {
		t.Fatal("--no-connect must not connect")
	}
	s.stop(t)

	// relay.enabled=false in config: plain serve stays local, --connect says why it cannot.
	port = n.localPanel(t, `, "relay": {"enabled": false}`)
	if code, out := n.cli("", "serve", "--connect"); code == 0 || !strings.Contains(out, "relay is turned off") {
		t.Fatalf("--connect with the relay off: %d %s", code, out)
	}
	s = n.serve()
	waitPanel(t, port)
	time.Sleep(300 * time.Millisecond)
	if _, _, ok := b.srv.hub.Online(n.link.ConnectorID); ok {
		t.Fatal("relay.enabled=false must keep serve local")
	}
	s.stop(t)

	if code, out := n.cli("", "serve", "--connect", "--no-connect"); code != 2 || !strings.Contains(out, "Usage") {
		t.Fatalf("conflicting flags: %d %s", code, out)
	}
}

// A revoked link must not take the local panel down with it.
func TestServeSurvivesARevokedLink(t *testing.T) {
	b := newBed(t)
	br := b.browser("10.0.0.1")
	br.signup("ann@example.com", testPassword)
	n := b.newNode("Workstation")
	n.linkWithWebCode(br)
	port := n.localPanel(t, "")
	s := n.serve()
	waitPanel(t, port)
	n.waitOnline(b)

	if r := br.api("POST", "/api/connectors/"+n.link.ConnectorID+"/revoke", nil); r.StatusCode != 200 {
		t.Fatalf("revoke: %d %s", r.StatusCode, r.body)
	}
	waitFor(t, "the revoked message", func() bool { return strings.Contains(s.out.String(), "link was revoked") })
	res, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/login", port))
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("the local panel must keep running: %v", err)
	}
	res.Body.Close()
	if code := s.stop(t); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// Linking while serve runs unlinked tells the person to restart it.
func TestConnectWhileServeRunsExplainsTheRestart(t *testing.T) {
	b := newBed(t)
	br := b.browser("10.0.0.1")
	br.signup("ann@example.com", testPassword)
	n := b.newNode("Workstation")
	port := n.localPanel(t, "")
	s := n.serve()
	waitPanel(t, port)

	r := br.api("POST", "/api/link/codes", nil)
	var c struct{ Code string }
	_ = json.Unmarshal([]byte(r.body), &c)
	code, out := n.cli("Workstation\ny\n", "connect", "--link", c.Code, "--cloud", b.ts.URL)
	if code != 0 || !strings.Contains(out, "Linked as") || !strings.Contains(out, "Restart it") {
		t.Fatalf("link next to a running serve: %d\n%s", code, out)
	}
	s.stop(t)
}
