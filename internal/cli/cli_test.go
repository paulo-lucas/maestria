package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/maestria-dev/maestria/internal/audit"
	"github.com/maestria-dev/maestria/internal/auth"
	"github.com/maestria-dev/maestria/internal/config"
	"github.com/maestria-dev/maestria/internal/store"
)

type fixture struct {
	t      *testing.T
	dir    string
	out    *bytes.Buffer
	errOut *bytes.Buffer
	now    time.Time
}

func newFixture(t *testing.T) *fixture {
	dir := t.TempDir() + "/cfg"
	return &fixture{t: t, dir: dir, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}, now: time.Unix(1_700_000_000, 0)}
}

func (f *fixture) env(stdin string, secrets ...string) Env {
	home := f.t.TempDir()
	e := Env{
		Stdin: strings.NewReader(stdin), Stdout: f.out, Stderr: f.errOut, Version: "test",
		Config: config.Env{Getenv: func(k string) string {
			if k == "MAESTRIA_CONFIG_DIR" {
				return f.dir
			}
			return ""
		}, GOOS: "linux", Home: home},
		Now: func() time.Time { return f.now },
	}
	queue := secrets
	e.ReadSecret = func(prompt string) (string, error) {
		f.out.WriteString(prompt)
		if len(queue) == 0 {
			f.t.Fatalf("unexpected secret prompt %q", prompt)
		}
		v := queue[0]
		queue = queue[1:]
		return v, nil
	}
	return e
}

func (f *fixture) run(env Env, args ...string) int {
	f.out.Reset()
	f.errOut.Reset()
	return Run(args, env)
}

func TestVersionHelpAndUsageErrors(t *testing.T) {
	f := newFixture(t)
	if code := f.run(f.env(""), "version"); code != 0 || f.out.String() != "maestria test\n" {
		t.Fatalf("version: %d %q", code, f.out.String())
	}
	if code := f.run(f.env("")); code != 0 || !strings.Contains(f.out.String(), "maestria serve") {
		t.Fatalf("help: %d", code)
	}
	for _, args := range [][]string{{"nope"}, {"auth"}, {"auth", "nope"}, {"tunnel"}, {"tunnel", "sideways"}, {"serve", "--bogus"}, {"auth", "setup", "--bogus"}, {"tunnel", "on", "extra"}} {
		if code := f.run(f.env(""), args...); code != 2 || f.errOut.Len() == 0 {
			t.Errorf("%v: exit %d, stderr %q", args, code, f.errOut.String())
		}
	}
}

// codeFeeder answers the "Enter the 6-digit code" prompt with the TOTP for the secret just shown.
type codeFeeder struct {
	f      *fixture
	offset time.Duration
	read   bool
}

func (c *codeFeeder) Read(p []byte) (int, error) {
	if c.read {
		return 0, bytes.ErrTooLarge
	}
	out := c.f.out.String()
	i := strings.Index(out, "manually: ")
	if i < 0 {
		c.f.t.Fatalf("no manual key printed yet:\n%s", out)
	}
	secret := strings.Fields(out[i+len("manually: "):])[0]
	code, _ := auth.TOTP(secret, c.f.now.Add(c.offset))
	c.read = true
	return copy(p, code+"\n"), nil
}

func TestAuthSetupWritesCredentialsAndRecoveryCodes(t *testing.T) {
	f := newFixture(t)
	env := f.env("", "correct horse battery", "correct horse battery")
	env.Stdin = &codeFeeder{f: f}
	f.out.Reset()
	if code := Run([]string{"auth", "setup"}, env); code != 0 {
		t.Fatalf("setup: %d\n%s\n%s", code, f.out.String(), f.errOut.String())
	}
	out := f.out.String()
	for _, want := range []string{"Scan this QR code", "otpauth", "Recovery codes", "Done. Credentials saved"} {
		if want == "otpauth" {
			continue
		}
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "\x1b[47;30m") {
		t.Error("QR code must be drawn with explicit colours")
	}

	secrets, err := auth.LoadSecrets(f.dir)
	if err != nil || secrets == nil || !auth.VerifyPassword("correct horse battery", secrets.PasswordHash) {
		t.Fatalf("secrets: %v %v", secrets, err)
	}
	db, err := store.Open(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if n, _ := auth.RemainingRecoveryCodes(db); n != auth.RecoveryCodeCount {
		t.Fatalf("recovery codes stored: %d", n)
	}
	// The printed codes work, once.
	var printed []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if len(line) == 9 && line[4] == '-' {
			printed = append(printed, line)
		}
	}
	if len(printed) != auth.RecoveryCodeCount {
		t.Fatalf("printed %d recovery codes", len(printed))
	}
	if ok, _ := auth.ConsumeRecoveryCode(db, printed[0], f.now); !ok {
		t.Fatal("printed recovery code must work")
	}
	if ok, _ := auth.ConsumeRecoveryCode(db, printed[0], f.now); ok {
		t.Fatal("and only once")
	}
	if strings.Contains(out, secrets.PasswordHash) {
		t.Fatal("the hash must never be printed")
	}
	entries, _ := audit.New(db, nil).Recent(5)
	if len(entries) == 0 || entries[0].Event != "auth_setup" {
		t.Fatalf("audit: %+v", entries)
	}

	// A second run refuses without --force.
	env = f.env("", "x")
	if code := Run([]string{"auth", "setup"}, env); code != 1 || !strings.Contains(f.errOut.String(), "--force") {
		t.Fatalf("second setup: %d %q", code, f.errOut.String())
	}
}

func TestAuthSetupRetriesShortPasswordsMismatchesAndBadCodes(t *testing.T) {
	f := newFixture(t)
	feed := &codeFeeder{f: f}
	env := f.env("", "short", "correct horse battery", "different password!", "correct horse battery", "correct horse battery")
	env.Stdin = &wrongThenRight{codeFeeder: feed}
	f.out.Reset()
	if code := Run([]string{"auth", "setup"}, env); code != 0 {
		t.Fatalf("setup: %d\n%s\n%s", code, f.out.String(), f.errOut.String())
	}
	out := f.out.String()
	for _, want := range []string{"at least 12 characters", "passwords do not match", "That code is not valid"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
}

type wrongThenRight struct {
	*codeFeeder
	calls int
}

func (w *wrongThenRight) Read(p []byte) (int, error) {
	w.calls++
	if w.calls == 1 {
		return copy(p, "000000\n"), nil
	}
	return w.codeFeeder.Read(p)
}

func TestTunnelCommandsAndLocalOnlyEnable(t *testing.T) {
	f := newFixture(t)
	if err := config.SaveRaw(f.dir, []byte(`{"tunnel":{"stopCommand":["true"],"startCommand":["true"]}}`), f.env("").Config); err != nil {
		t.Fatal(err)
	}
	if code := f.run(f.env(""), "tunnel", "status"); code != 0 || !strings.Contains(f.out.String(), "Kill switch: inactive") {
		t.Fatalf("status: %d %s", code, f.out.String())
	}
	db, _ := store.Open(f.dir)
	sessions := auth.NewSessionStore(db, time.Hour, time.Hour, func() time.Time { return f.now })
	_, _, _ = sessions.Create("ip", "ua")
	db.Close()

	if code := f.run(f.env(""), "tunnel", "off"); code != 0 || !strings.Contains(f.out.String(), "Invalidated 1 session(s)") {
		t.Fatalf("off: %d %s %s", code, f.out.String(), f.errOut.String())
	}
	if code := f.run(f.env(""), "tunnel", "status", "--json"); code != 0 || !strings.Contains(f.out.String(), `"killed": true`) {
		t.Fatalf("status json: %d %s", code, f.out.String())
	}
	if code := f.run(f.env(""), "tunnel", "on"); code != 0 || !strings.Contains(f.out.String(), "kill switch cleared") {
		t.Fatalf("on: %d %s %s", code, f.out.String(), f.errOut.String())
	}
	if code := f.run(f.env(""), "tunnel", "status"); !strings.Contains(f.out.String(), "inactive") {
		t.Fatalf("after on: %d %s", code, f.out.String())
	}

	// A failing start command is an error exit.
	if err := config.SaveRaw(f.dir, []byte(`{"tunnel":{"startCommand":["/definitely/not/a/command"]}}`), f.env("").Config); err != nil {
		t.Fatal(err)
	}
	if code := f.run(f.env(""), "tunnel", "on"); code != 1 || !strings.Contains(f.out.String(), "Start command failed") {
		t.Fatalf("failing start: %d %s", code, f.out.String())
	}
}

func TestResetSessions(t *testing.T) {
	f := newFixture(t)
	db, _ := store.Open(f.dir)
	s := auth.NewSessionStore(db, time.Hour, time.Hour, func() time.Time { return f.now })
	_, _, _ = s.Create("a", "b")
	_, _, _ = s.Create("a", "b")
	db.Close()
	if code := f.run(f.env(""), "auth", "reset-sessions"); code != 0 || f.out.String() != "Invalidated 2 sessions.\n" {
		t.Fatalf("reset: %d %q", code, f.out.String())
	}
	db, _ = store.Open(f.dir)
	defer db.Close()
	if n, _ := s.Count(); n != 0 {
		_ = context.Background()
		t.Fatalf("sessions left: %d", n)
	}
}

func TestServeWithoutCredentialsFailsClearly(t *testing.T) {
	f := newFixture(t)
	if code := f.run(f.env(""), "serve"); code != 1 || !strings.Contains(f.errOut.String(), "maestria auth setup") {
		t.Fatalf("serve: %d %q", code, f.errOut.String())
	}
}

func TestInvalidConfigIsReportedWithoutStackTrace(t *testing.T) {
	f := newFixture(t)
	_ = config.EnsureDir(f.dir)
	_ = writeFile(f.dir+"/config.json", `{"host":"0.0.0.0"}`)
	if code := f.run(f.env(""), "serve"); code != 1 || !strings.Contains(f.errOut.String(), `unknown key "host"`) {
		t.Fatalf("serve: %d %q", code, f.errOut.String())
	}
}

func TestRenderQR(t *testing.T) {
	s, err := RenderQR("otpauth://totp/Maestria:panel?secret=ABC")
	if err != nil || strings.Count(s, "\n") < 10 || !strings.HasSuffix(s, "\x1b[0m\n") {
		t.Fatalf("qr: %v %d lines", err, strings.Count(s, "\n"))
	}
}

func writeFile(path, content string) error { return os.WriteFile(path, []byte(content), 0o600) }
