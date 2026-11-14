package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paulo-lucas/maestria/internal/auth"
	"github.com/paulo-lucas/maestria/internal/config"
	"github.com/paulo-lucas/maestria/internal/service"
	"github.com/paulo-lucas/maestria/internal/workspaces"
)

func TestEmbeddedSkillMatchesSource(t *testing.T) {
	srcPath := filepath.Join("..", "..", "skills", "maestria-maestro", "SKILL.md")
	want, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read %s: %v", srcPath, err)
	}
	norm := func(b []byte) []byte { return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")) }
	if !bytes.Equal(norm(embeddedSkillMD), norm(want)) {
		t.Fatalf("internal/cli/skill/SKILL.md differs from %s; copy it over so the embedded skill stays in sync", srcPath)
	}
}

func TestParseSkillSelection(t *testing.T) {
	cases := []struct {
		raw   string
		c, ag bool
		none  bool
		ok    bool
	}{
		{"claude", true, false, false, true},
		{"antigravity", false, true, false, true},
		{"claude,antigravity", true, true, false, true},
		{"antigravity, claude", true, true, false, true},
		{"both", true, true, false, true},
		{"all", true, true, false, true},
		{"none", false, false, true, true},
		{"", false, false, false, false},
		{"   ", false, false, false, false},
		{"unknown", false, false, false, false},
		{"claude,none", false, false, false, false},
		{"claude,", false, false, false, false},
	}
	for _, tc := range cases {
		c, ag, n, err := ParseSkillSelection(tc.raw)
		if tc.ok && (err != nil || c != tc.c || ag != tc.ag || n != tc.none) {
			t.Errorf("ParseSkillSelection(%q) = (%v,%v,%v,%v), want (%v,%v,%v,nil)", tc.raw, c, ag, n, err, tc.c, tc.ag, tc.none)
		}
		if !tc.ok && err == nil {
			t.Errorf("ParseSkillSelection(%q) expected error", tc.raw)
		}
	}
}

func setupSampleMaestriEnv(t *testing.T, home string) (maestriDir, proj1, proj2 string) {
	t.Helper()
	maestriDir = filepath.Join(home, ".maestri")
	proj1 = filepath.Join(home, "projects", "alpha")
	proj2 = filepath.Join(home, "projects", "beta")
	for _, d := range []string{
		filepath.Join(home, ".gemini", "config", "skills"),
		proj1,
		proj2,
		filepath.Join(maestriDir, "usage", "providers"),
		filepath.Join(maestriDir, "workspaces", "W1"),
		filepath.Join(maestriDir, "workspaces", "W2"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(proj1, "ROADMAP.md"), []byte("# Alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(maestriDir, "usage", "providers", ".status.json"), []byte(`{"providers":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(maestriDir, "workspaces", "W1", "workspace.json"), []byte(`{"schemaVersion":2,"payload":{"name":"Alpha","workingDirectory":"`+filepath.ToSlash(proj1)+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(maestriDir, "workspaces", "W2", "workspace.json"), []byte(`{"schemaVersion":2,"payload":{"name":"Beta","workingDirectory":"`+filepath.ToSlash(proj2)+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return maestriDir, proj1, proj2
}

func TestInitNonInteractiveWithDiscoveryAndSkills(t *testing.T) {
	f := newFixture(t)
	home := t.TempDir()
	maestriDir, proj1, _ := setupSampleMaestriEnv(t, home)

	bin := filepath.Join(t.TempDir(), "maestria")
	if err := os.WriteFile(bin, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}

	env := f.env("")
	env.Config.Home = home
	env.ServiceBinaryPath = bin
	env.ServiceRunner = func(_ context.Context, _ []string) service.CmdResult {
		return service.CmdResult{OK: true}
	}

	code := f.run(env, "init", "--yes", "--maestri-dir", maestriDir, "--skip-auth", "--skill", "both", "--service", "install")
	if code != 0 {
		t.Fatalf("init failed (%d):\nstdout:\n%s\nstderr:\n%s", code, f.out.String(), f.errOut.String())
	}
	out := f.out.String()
	for _, want := range []string{
		"Verified Maestri usage providers",
		"Detected 2 new Maestri workspace(s)",
		"Installed Maestro skill:",
		"Installed Maestria service (serve):",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	cfg, err := config.Load(f.dir, env.Config)
	if err != nil || cfg.MaestriDir != maestriDir {
		t.Fatalf("cfg: %+v err=%v", cfg, err)
	}
	wsList, err := workspaces.Load(f.dir)
	if err != nil || len(wsList) != 2 || wsList[0].Path != proj1 || wsList[0].RoadmapPath != "ROADMAP.md" {
		t.Fatalf("workspaces: %+v err=%v", wsList, err)
	}

	// Verify both skills installed.
	claudeSkill := filepath.Join(home, ".claude", "skills", "maestria", "SKILL.md")
	agSkill := filepath.Join(home, ".gemini", "config", "skills", "maestria", "SKILL.md")
	for _, p := range []string{claudeSkill, agSkill} {
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("missing skill %s: %v", p, err)
		}
		if !strings.Contains(string(got), "maestria wait") {
			t.Fatalf("unexpected skill content at %s", p)
		}
	}

	// Verify service unit installed.
	if !service.IsInstalled(env.serviceOpts(service.ModeServe)) {
		t.Fatal("expected service to be installed")
	}

	// Idempotent re-run preserves existing state.
	code = f.run(env, "init", "--yes")
	if code != 0 {
		t.Fatalf("re-run failed (%d): %s", code, f.errOut.String())
	}
	wsList, err = workspaces.Load(f.dir)
	if err != nil || len(wsList) != 2 {
		t.Fatalf("workspaces after re-run: %+v err=%v", wsList, err)
	}
}

func TestInitNonInteractiveRejectsMissingUsageProviders(t *testing.T) {
	f := newFixture(t)
	badDir := filepath.Join(t.TempDir(), "not-maestri")
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatal(err)
	}
	env := f.env("")
	if code := f.run(env, "init", "--yes", "--maestri-dir", badDir, "--skip-auth", "--skill", "none"); code != 1 || !strings.Contains(f.errOut.String(), "usage/providers") {
		t.Fatalf("expected usage/providers error, got code=%d stderr=%q", code, f.errOut.String())
	}
}

func TestInitAntigravitySkillPathFallback(t *testing.T) {
	freshHome := t.TempDir()
	installed, err := InstallMaestroSkill(false, true, freshHome)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(freshHome, ".gemini", "antigravity", "skills", "maestria", "SKILL.md")
	if len(installed) != 1 || installed[0] != want {
		t.Fatalf("installed = %v, want [%s]", installed, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatal(err)
	}
}

func TestInitInteractiveWizardAndIdempotentRerun(t *testing.T) {
	f := newFixture(t)
	home := t.TempDir()
	maestriDir, proj1, _ := setupSampleMaestriEnv(t, home)
	bin := filepath.Join(t.TempDir(), "maestria")
	if err := os.WriteFile(bin, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Pass 1: interactive fresh setup selecting default maestriDir, workspace #1 only,
	// default standby, auth setup (with TOTP), skip wire, claude skill only, install+start service.
	script1 := strings.Join([]string{
		"1", // Maestri data dir: Use default
		"1", // Workspaces: pick only #1 (Alpha)
		"1", // Standby: Use defaults
		"",  // placeholder for TOTP? Wait: TOTP is read by totpFeeder below!
	}, "\n")
	_ = script1

	// Build a reader that feeds answers dynamically when "manually: " appears for TOTP.
	pr := &interactiveInitReader{
		t:     t,
		out:   f.out,
		now:   f.now,
		lines: []string{"1", "1", "1"}, // dir, workspace #1, standby default
		afterTOTP: []string{
			"https://Panel.Example.com/", // Public host name (pasted as a URL)
			"n",                          // Pair with Maestri Wire? -> No
			"2",                          // Skill: Claude Code only
			"1",                          // Service: Install and start
		},
	}

	env := f.env("", "correct horse battery", "correct horse battery")
	env.Config.Home = home
	env.Config.Getenv = func(k string) string {
		switch k {
		case "MAESTRIA_CONFIG_DIR":
			return f.dir
		case "MAESTRI_HOME":
			return maestriDir
		}
		return ""
	}
	env.Stdin = pr
	env.IsTTY = func() bool { return true }
	env.ServiceBinaryPath = bin
	env.ServiceRunner = func(_ context.Context, _ []string) service.CmdResult {
		return service.CmdResult{OK: true}
	}

	if code := f.run(env, "init"); code != 0 {
		t.Fatalf("interactive init failed (%d):\nstdout:\n%s\nstderr:\n%s", code, f.out.String(), f.errOut.String())
	}

	wsList, err := workspaces.Load(f.dir)
	if err != nil || len(wsList) != 1 || wsList[0].Path != proj1 {
		t.Fatalf("expected only workspace #1 saved, got %+v err=%v", wsList, err)
	}
	sec, err := auth.LoadSecrets(f.dir)
	if err != nil || sec == nil {
		t.Fatalf("expected credentials saved: %v", err)
	}
	if cfg, err := config.Load(f.dir, env.Config); err != nil || len(cfg.AllowedHosts) != 1 || cfg.AllowedHosts[0] != "panel.example.com" {
		t.Fatalf("expected allowedHosts [panel.example.com], got %+v err=%v", cfg, err)
	}
	if _, err := os.Stat(ClaudeSkillPath(home)); err != nil {
		t.Fatalf("expected claude skill: %v", err)
	}
	if _, err := os.Stat(AntigravitySkillPath(home)); !os.IsNotExist(err) {
		t.Fatalf("expected antigravity skill NOT installed")
	}

	// Pass 2: interactive idempotent re-run keeping everything.
	env2 := f.env("")
	env2.Config = env.Config
	env2.IsTTY = func() bool { return true }
	env2.ServiceBinaryPath = bin
	env2.ServiceRunner = env.ServiceRunner
	env2.Stdin = strings.NewReader(strings.Join([]string{
		"1",    // Keep existing config
		"none", // Skip newly detected Beta workspace
		"1",    // Keep existing standby
		"1",    // Keep existing credentials
		"",     // No extra public host
		"n",    // Skip wire pairing
		"1",    // Keep existing skill
		"1",    // Keep existing service
	}, "\n") + "\n")

	if code := f.run(env2, "init"); code != 0 {
		t.Fatalf("interactive re-run failed (%d):\nstdout:\n%s\nstderr:\n%s", code, f.out.String(), f.errOut.String())
	}
	wsAfter, err := workspaces.Load(f.dir)
	if err != nil || len(wsAfter) != 1 || wsAfter[0].Path != proj1 {
		t.Fatalf("expected 1 workspace kept after skipping Beta, got %+v err=%v", wsAfter, err)
	}
}

type interactiveInitReader struct {
	t         *testing.T
	out       *bytes.Buffer
	now       time.Time
	lines     []string
	afterTOTP []string
	sentTOTP  bool
	buf       []byte
}

func (r *interactiveInitReader) Read(p []byte) (int, error) {
	if len(r.buf) == 0 {
		if len(r.lines) > 0 {
			r.buf = []byte(r.lines[0] + "\n")
			r.lines = r.lines[1:]
		} else if !r.sentTOTP {
			out := r.out.String()
			idx := strings.Index(out, "manually: ")
			if idx < 0 {
				r.t.Fatalf("expected TOTP manual key in output:\n%s", out)
			}
			secret := strings.Fields(out[idx+len("manually: "):])[0]
			code, _ := auth.TOTP(secret, r.now)
			r.buf = []byte(code + "\n")
			r.sentTOTP = true
		} else if len(r.afterTOTP) > 0 {
			r.buf = []byte(r.afterTOTP[0] + "\n")
			r.afterTOTP = r.afterTOTP[1:]
		} else {
			return 0, os.ErrClosed
		}
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}
