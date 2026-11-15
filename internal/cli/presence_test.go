package cli

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/paulo-lucas/maestria/internal/audit"
	"github.com/paulo-lucas/maestria/internal/auth"
	"github.com/paulo-lucas/maestria/internal/config"
	"github.com/paulo-lucas/maestria/internal/devices"
	"github.com/paulo-lucas/maestria/internal/relay"
	"github.com/paulo-lucas/maestria/internal/relay/relaytest"
	"github.com/paulo-lucas/maestria/internal/store"
)

// offerPairing puts a revealed pairing in the connector database and returns its code.
func offerPairing(t *testing.T, f *fixture, label string) (code string, b *relaytest.Browser) {
	t.Helper()
	db, err := store.Open(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	dm := devices.New(db, nil, func() time.Time { return f.now })
	b = relaytest.NewBrowser(t)
	p := b.StartPairing(t)
	r, err := dm.Offer("pair-cli-0001", p.Commit, label, "UA")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dm.Reveal("pair-cli-0001", relay.Enc(b.SPKI), relay.Enc(p.Nonce)); err != nil {
		t.Fatal(err)
	}
	return relay.FormatPairCode(b.Code(t, p, r)), b
}

func auditEvents(t *testing.T, f *fixture) map[string]int {
	t.Helper()
	db, err := store.Open(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	entries, _ := audit.New(db, nil).Recent(200)
	out := map[string]int{}
	for _, e := range entries {
		out[e.Event]++
	}
	return out
}

func TestApproveNeedsAHumanAtATerminal(t *testing.T) {
	f := newFixture(t)
	code, _ := offerPairing(t, f, "Safari on iPhone")

	// A script or an agent with no terminal: refused, whatever it types, and --yes does not exist.
	env := f.env(testWord + "\n")
	if c := f.run(env, "devices", "approve", code); c == 0 || !strings.Contains(f.errOut.String(), "interactive terminal") {
		t.Fatalf("approve without a TTY: exit %d, %s", c, f.errOut.String())
	}
	if c := f.run(f.env(testWord+"\n"), "devices", "approve", code, "--yes"); c == 0 {
		t.Fatal("--yes must not bypass the confirmation")
	}
	if strings.Contains(f.out.String(), "Approved") {
		t.Fatal("approved without a person")
	}

	// At a terminal but with a wrong word, or nothing typed: refused.
	wrong := f.humanEnv("")
	wrong.Stdin = strings.NewReader("not-the-word\n")
	if c := f.run(wrong, "devices", "approve", code); c == 0 || !strings.Contains(f.errOut.String(), "not the word") {
		t.Fatalf("wrong word: exit %d, %s", c, f.errOut.String())
	}
	empty := f.humanEnv("")
	empty.Stdin = strings.NewReader("")
	if c := f.run(empty, "devices", "approve", code); c == 0 {
		t.Fatal("no input must not approve")
	}
	f.run(f.env(""), "devices", "list")
	if strings.Contains(f.out.String(), "Safari") {
		t.Fatalf("a device was approved: %s", f.out.String())
	}

	// A person typing the word it just printed: approved, and the screen showed what is approved.
	if c := f.run(f.humanEnv(""), "devices", "approve", code); c != 0 {
		t.Fatalf("approve at a terminal: exit %d, %s %s", c, f.out.String(), f.errOut.String())
	}
	out := f.out.String()
	for _, want := range []string{"Safari on iPhone", "Key fingerprint", "Offered at", code, "type this word: " + testWord, "Approved"} {
		if !strings.Contains(out, want) {
			t.Errorf("approve output lacks %q:\n%s", want, out)
		}
	}
	f.run(f.env(""), "devices", "list")
	if !strings.Contains(f.out.String(), "Safari on iPhone") || !strings.Contains(f.out.String(), "key ") {
		t.Fatalf("devices list: %s", f.out.String())
	}

	ev := auditEvents(t, f)
	for _, want := range []string{"cli_confirm_refused", "cli_confirm_failed", "cli_confirmed", "device_approved"} {
		if ev[want] == 0 {
			t.Errorf("no %q entry in the audit log: %v", want, ev)
		}
	}
}

func TestPendingShowsKeyTimeAndBudget(t *testing.T) {
	f := newFixture(t)
	code, b := offerPairing(t, f, "Safari\x1b]52;c;ZXZpbA==\x07 on iPhone")
	if c := f.run(f.env(""), "devices", "pending"); c != 0 {
		t.Fatalf("pending: %d", c)
	}
	out := f.out.String()
	for _, want := range []string{code, relay.Fingerprint(b.DeviceID), "offered", "1 of 5"} {
		if !strings.Contains(out, want) {
			t.Errorf("pending output lacks %q:\n%s", want, out)
		}
	}
	if strings.ContainsAny(out, "\x1b\x07") {
		t.Fatalf("control characters reached the terminal: %q", out)
	}
}

func TestKillSwitchCanOnlyBeLiftedByAPerson(t *testing.T) {
	f := newFixture(t)
	if err := config.SaveRaw(f.dir, []byte(`{"tunnel":{"stopCommand":["true"],"startCommand":["true"]}}`), f.env("").Config); err != nil {
		t.Fatal(err)
	}
	if c := f.run(f.env(""), "tunnel", "off"); c != 0 {
		t.Fatalf("off: %d", c)
	}
	if c := f.run(f.env(testWord+"\n"), "tunnel", "on"); c == 0 {
		t.Fatal("tunnel on without a TTY must be refused")
	}
	f.run(f.env(""), "tunnel", "status")
	if !strings.Contains(f.out.String(), "LOCKED") {
		t.Fatalf("the kill switch was lifted: %s", f.out.String())
	}
	wrong := f.humanEnv("")
	wrong.Stdin = strings.NewReader("nope\n")
	if c := f.run(wrong, "tunnel", "on"); c == 0 {
		t.Fatal("tunnel on with the wrong word must be refused")
	}
	if c := f.run(f.humanEnv(""), "tunnel", "on"); c != 0 {
		t.Fatalf("tunnel on by a person: %d %s", c, f.errOut.String())
	}
	if ev := auditEvents(t, f); ev["cli_confirm_refused"] == 0 || ev["cli_confirmed"] == 0 {
		t.Fatalf("audit: %v", ev)
	}
}

func TestLinkingNeedsAPersonAndYesDoesNotSkipIt(t *testing.T) {
	f := newFixture(t)
	for _, args := range [][]string{
		{"connect", "--link", "ABCD-EFGH", "--cloud", "https://cloud.example.com", "--yes"},
		{"connect", "--cloud", "https://cloud.example.com", "--yes"},
	} {
		if c := f.run(f.env(testWord+"\ny\n"), args...); c == 0 || !strings.Contains(f.errOut.String(), "interactive terminal") {
			t.Fatalf("%v without a TTY: exit %d, %s", args, c, f.errOut.String())
		}
	}
	// Nothing was linked.
	if c := f.run(f.env(""), "connect", "status"); c != 0 || !strings.Contains(f.out.String(), "Not linked") {
		t.Fatalf("status: %d %s", c, f.out.String())
	}
}

// wordThen types the confirmation word, then lets next answer (the authenticator-code feeder).
type wordThen struct {
	word string
	next io.Reader
	done bool
}

func (w *wordThen) Read(p []byte) (int, error) {
	if !w.done {
		w.done = true
		return copy(p, w.word+"\n"), nil
	}
	return w.next.Read(p)
}

func existingCredentials(t *testing.T, f *fixture) (hash string) {
	t.Helper()
	hash, err := auth.HashPassword("the original long password")
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.SaveSecrets(f.dir, &auth.Secrets{PasswordHash: hash, TOTPSecret: "JBSWY3DPEHPK3PXP", CreatedAt: "2026-01-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	return hash
}

func storedHash(t *testing.T, f *fixture) string {
	t.Helper()
	s, err := auth.LoadSecrets(f.dir)
	if err != nil || s == nil {
		t.Fatalf("secrets: %v %v", s, err)
	}
	return s.PasswordHash
}

// Replacing the credentials is the same kind of decision as approving a device: a script or an
// agent that pipes a password and reads the new authenticator key off the screen must not be able
// to do it.
func TestReplacingCredentialsNeedsAPerson(t *testing.T) {
	f := newFixture(t)
	original := existingCredentials(t, f)

	// No terminal: refused with --force, whatever is piped in, and before any prompt.
	env := f.env(testWord+"\n", "correct horse battery", "correct horse battery")
	if c := f.run(env, "auth", "setup", "--force"); c == 0 || !strings.Contains(f.errOut.String(), "interactive terminal") {
		t.Fatalf("--force without a terminal: exit %d, %s", c, f.errOut.String())
	}
	if strings.Contains(f.out.String(), "Scan this QR code") || strings.Contains(f.out.String(), "manually") {
		t.Fatalf("the new authenticator key was shown to a script:\n%s", f.out.String())
	}
	// Without --force and without a terminal it is refused too, and still changes nothing.
	if c := f.run(f.env(""), "auth", "setup"); c == 0 {
		t.Fatal("setup over existing credentials must not succeed")
	}
	if storedHash(t, f) != original {
		t.Fatal("the credentials changed")
	}

	// At a terminal with the wrong word: refused.
	wrong := f.humanEnv("")
	wrong.Stdin = strings.NewReader("nope\n")
	if c := f.run(wrong, "auth", "setup", "--force"); c == 0 || !strings.Contains(f.errOut.String(), "not the word") {
		t.Fatalf("wrong word: exit %d, %s", c, f.errOut.String())
	}
	if storedHash(t, f) != original {
		t.Fatal("the credentials changed after a wrong word")
	}

	// A person who types the word it printed can replace them.
	human := f.env("", "correct horse battery", "correct horse battery")
	human.IsTTY = func() bool { return true }
	human.ConfirmWord = func() string { return testWord }
	human.Stdin = &wordThen{word: testWord, next: &codeFeeder{f: f}}
	if c := f.run(human, "auth", "setup", "--force"); c != 0 {
		t.Fatalf("setup by a person: exit %d\n%s\n%s", c, f.out.String(), f.errOut.String())
	}
	if storedHash(t, f) == original {
		t.Fatal("the credentials were not replaced")
	}
	ev := auditEvents(t, f)
	for _, want := range []string{"cli_confirm_refused", "cli_confirm_failed", "cli_confirmed", "auth_setup"} {
		if ev[want] == 0 {
			t.Errorf("no %q in the audit log: %v", want, ev)
		}
	}
}

// First-time setup has nothing to replace and keeps working (the wizard and scripted installs).
func TestFirstTimeSetupDoesNotNeedAConfirmationWord(t *testing.T) {
	f := newFixture(t)
	env := f.env("", "correct horse battery", "correct horse battery")
	env.Stdin = &codeFeeder{f: f}
	if c := f.run(env, "auth", "setup"); c != 0 {
		t.Fatalf("first setup: exit %d\n%s", c, f.errOut.String())
	}
}
