package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/paulo-lucas/maestria/internal/activity"
	"github.com/paulo-lucas/maestria/internal/store"
	"github.com/paulo-lucas/maestria/internal/wire"
	"github.com/paulo-lucas/maestria/internal/wire/wiretest"
)

func pairEnv(t *testing.T, stdin string, secrets ...string) (*Env, string, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	env, cfgDir, _, out, errBuf := newConnectorEnv(t, time.Date(2026, 11, 14, 15, 0, 0, 0, time.UTC))
	env.Stdin = strings.NewReader(stdin)
	i := 0
	env.ReadSecret = func(string) (string, error) {
		s := secrets[i]
		i++
		return s, nil
	}
	return env, cfgDir, out, errBuf
}

func TestWirePairStatusUnpair(t *testing.T) {
	fake, err := wiretest.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	fake.SetPassword("hunter2")

	env, cfgDir, out, errBuf := pairEnv(t, fake.Host+"\n"+fake.PinHex()+"\n", "hunter2")
	runCLI(t, env, out, errBuf, "wire", "pair")
	if !strings.Contains(out.String(), "Paired with") {
		t.Fatalf("pair output: %s", out.String())
	}
	path := filepath.Join(cfgDir, wire.FileName)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("wire.json mode = %v", fi.Mode().Perm())
	}
	creds, err := wire.LoadCredentials(path)
	if err != nil || creds.Owner == nil || creds.Pin != fake.Pin().Hex() {
		t.Fatalf("creds = %+v %v", creds, err)
	}
	secrets := []string{creds.Guest.Token, creds.Owner.Token, "hunter2"}
	noSecrets := func(label, s string) {
		for _, sec := range secrets {
			if strings.Contains(s, sec) {
				t.Fatalf("%s leaks a secret: %s", label, s)
			}
		}
	}
	noSecrets("pair output", out.String()+errBuf.String())

	// status: host, pin, roles, capabilities; no tokens.
	env.Stdin = strings.NewReader("")
	text := runCLI(t, env, out, errBuf, "wire", "status")
	for _, want := range []string{fake.Host, fake.Pin().Hex(), "connected", "Reads:         guest", "Writes:        owner", "feedSnapshots", "terminalChat"} {
		if !strings.Contains(text, want) {
			t.Errorf("status missing %q:\n%s", want, text)
		}
	}
	noSecrets("status", text)
	var js wireStatusOut
	if err := json.Unmarshal([]byte(runCLI(t, env, out, errBuf, "wire", "status", "--json")), &js); err != nil || js.Link != "connected" || js.GuestRole != "guest" || js.OwnerRole != "owner" {
		t.Fatalf("status json: %+v %v", js, err)
	}

	// Status only calls /api/info: nothing that could wake a terminal.
	for _, r := range fake.Requests() {
		if r.Path != "/api/info" && r.Path != "/pair" && !strings.HasPrefix(r.Path, "/api/devices/") {
			t.Fatalf("status touched %s", r.Path)
		}
	}

	// A changed certificate shows up as a pin mismatch, and the CLI explains it.
	fake.RotateCert()
	text = runCLI(t, env, out, errBuf, "wire", "status")
	if !strings.Contains(text, "pin_mismatch") || !strings.Contains(text, "does not match") {
		t.Fatalf("pin mismatch status:\n%s", text)
	}
	fake.RotateCert() // new cert again; restore a pin we can reach by re-pairing below

	// Unpair against an unreachable-by-pin host still deletes the file and says what is left to do.
	text = runCLI(t, env, out, errBuf, "wire", "unpair")
	if !strings.Contains(text, "Deleted wire.json") || !strings.Contains(text, "Could not revoke") {
		t.Fatalf("unpair (mismatch):\n%s", text)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("wire.json still there")
	}
	text = runCLI(t, env, out, errBuf, "wire", "status")
	if !strings.Contains(text, "not paired") {
		t.Fatalf("status after unpair:\n%s", text)
	}
}

func TestWireUnpairRevokesBothDevices(t *testing.T) {
	fake, _ := wiretest.New()
	t.Cleanup(fake.Close)
	fake.SetPassword("pw")
	env, cfgDir, out, errBuf := pairEnv(t, fake.Host+"\n"+fake.PinBase64()+"\n", "pw") // the QR's base64 key
	runCLI(t, env, out, errBuf, "wire", "pair")
	if len(fake.Devices()) != 2 {
		t.Fatalf("devices = %d", len(fake.Devices()))
	}
	text := runCLI(t, env, out, errBuf, "wire", "unpair")
	if !strings.Contains(text, "Revoked the read-only device") || !strings.Contains(text, "Revoked the owner device") {
		t.Fatalf("unpair:\n%s", text)
	}
	if n := len(fake.Devices()); n != 0 {
		t.Fatalf("%d devices left in Maestri", n)
	}
	if _, err := os.Stat(filepath.Join(cfgDir, wire.FileName)); !os.IsNotExist(err) {
		t.Fatal("wire.json still there")
	}
	// Nothing to do the second time.
	if !strings.Contains(runCLI(t, env, out, errBuf, "wire", "unpair"), "Not paired") {
		t.Fatal("second unpair")
	}
}

func TestWirePairWithCodesAsksForASecondCode(t *testing.T) {
	fake, _ := wiretest.New()
	t.Cleanup(fake.Close)
	c1, c2 := fake.NewCode(), fake.NewCode()
	env, cfgDir, out, errBuf := pairEnv(t, fake.PinHex()+"\n", c1, c2) // --host given: only the key is asked
	if code := Run([]string{"wire", "pair", "--host", fake.Host}, *env); code != 0 {
		t.Fatalf("pair failed: %s", errBuf.String())
	}
	_ = out
	if !strings.Contains(out.String(), "works once") {
		t.Fatalf("no hint about the second code: %s", out.String())
	}
	if _, err := wire.LoadCredentials(filepath.Join(cfgDir, wire.FileName)); err != nil {
		t.Fatal(err)
	}
}

func TestWirePairOnThisMachineTrustsThePresentedKeyWithEnter(t *testing.T) {
	fake, _ := wiretest.New()
	t.Cleanup(fake.Close)
	fake.SetPassword("right")
	// Host, then Enter at the key prompt (trust on first use, loopback only).
	env, cfgDir, out, errBuf := pairEnv(t, fake.Host+"\n\n", "right")
	if code := Run([]string{"wire", "pair"}, *env); code != 0 {
		t.Fatalf("pair failed: %s\n%s", errBuf.String(), out.String())
	}
	if !strings.Contains(out.String(), wire.FormatPin(mustPin(t, fake.PinHex()))) {
		t.Fatalf("the presented key was not shown for comparison:\n%s", out.String())
	}
	creds, err := wire.LoadCredentials(filepath.Join(cfgDir, wire.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if want := mustPin(t, fake.PinHex()).Hex(); creds.Pin != want {
		t.Fatalf("pinned %q, want %q", creds.Pin, want)
	}
}

func mustPin(t *testing.T, s string) wire.Pin {
	t.Helper()
	p, err := wire.ParsePin(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFetchPinRefusesNonLoopback(t *testing.T) {
	if _, err := wire.FetchPin(context.Background(), "example.com:7434"); err == nil {
		t.Fatal("FetchPin must refuse a non-loopback host")
	}
}

func TestWirePairErrorsAreExplainedAndLeaveNothingBehind(t *testing.T) {
	fake, _ := wiretest.New()
	t.Cleanup(fake.Close)
	fake.SetPassword("right")
	cases := []struct {
		name, stdin, secret, want string
	}{
		{"wrong password", fake.Host + "\n" + fake.PinHex() + "\n", "wrong", "Wrong pairing code or password."},
		// On this machine a wrong or malformed key is explained and asked again (here stdin ends).
		{"wrong key", fake.Host + "\n" + strings.Repeat("ab", 32) + "\n", "right", "does not match the one Wire presents"},
		{"bad key text", fake.Host + "\nnot-a-key\n", "right", "security key must be"},
		{"pairing code typed as key", fake.Host + "\n123456\n", "right", "looks like the 6-digit pairing code"},
	}
	for _, c := range cases {
		env, cfgDir, out, errBuf := pairEnv(t, c.stdin, c.secret)
		out.Reset()
		if code := Run([]string{"wire", "pair"}, *env); code == 0 {
			t.Fatalf("%s: succeeded", c.name)
		}
		if !strings.Contains(errBuf.String()+out.String(), c.want) {
			t.Errorf("%s: output = %q / %q, want %q", c.name, out.String(), errBuf.String(), c.want)
		}
		if _, err := os.Stat(filepath.Join(cfgDir, wire.FileName)); !os.IsNotExist(err) {
			t.Errorf("%s: wire.json written", c.name)
		}
		if strings.Contains(errBuf.String()+out.String(), "right") {
			t.Errorf("%s: the password was echoed", c.name)
		}
	}
	if n := len(fake.Devices()); n != 0 {
		t.Fatalf("%d devices left behind", n)
	}
}

func TestAgentBriefCLI(t *testing.T) {
	now := time.Date(2026, 11, 14, 15, 0, 0, 0, time.UTC)
	env, cfgDir, wsDir, out, errBuf := newConnectorEnv(t, now)
	runCLI(t, env, out, errBuf, "workspace", "add", wsDir, "--name", "Atlas", "--json")

	runCLI(t, env, out, errBuf, "agent", "brief", "Vesper", "Writing", "retry   tests", "-w", "atlas")
	db, _ := store.Open(cfgDir)
	b, _ := activity.Briefs(db, "atlas")
	db.Close()
	if b["vesper"] != "Writing retry tests" {
		t.Fatalf("briefs = %v", b)
	}
	// JSON, a quoted argument and stdin.
	var res struct{ Brief string }
	if err := json.Unmarshal([]byte(runCLI(t, env, out, errBuf, "agent", "brief", "Orin", "Adding validation", "-w", "atlas", "--json")), &res); err != nil || res.Brief != "Adding validation" {
		t.Fatalf("json: %+v %v", res, err)
	}
	env.Stdin = strings.NewReader("From stdin\n")
	runCLI(t, env, out, errBuf, "agent", "brief", "Wren", "-", "-w", "atlas")
	db, _ = store.Open(cfgDir)
	b, _ = activity.Briefs(db, "atlas")
	db.Close()
	if b["wren"] != "From stdin" || len(b) != 3 {
		t.Fatalf("briefs = %v", b)
	}
	// Clear.
	runCLI(t, env, out, errBuf, "agent", "brief", "Vesper", "--clear", "-w", "atlas")
	db, _ = store.Open(cfgDir)
	b, _ = activity.Briefs(db, "atlas")
	db.Close()
	if _, ok := b["vesper"]; ok {
		t.Fatalf("not cleared: %v", b)
	}
	// Errors: too long, missing text.
	out.Reset()
	errBuf.Reset()
	if code := Run([]string{"agent", "brief", "Vesper", strings.Repeat("x", 200), "-w", "atlas"}, *env); code == 0 || !strings.Contains(errBuf.String(), "160") {
		t.Fatalf("long brief: code=%d err=%s", code, errBuf.String())
	}
	if code := Run([]string{"agent", "brief", "Vesper", "-w", "atlas"}, *env); code != 2 {
		t.Fatalf("missing text: code=%d", code)
	}
}
