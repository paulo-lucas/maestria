package cli

import (
	"strings"
	"testing"

	"github.com/paulo-lucas/maestria/internal/config"
)

func TestConfigAllowAndDisallowHost(t *testing.T) {
	f := newFixture(t)
	env := f.env("")

	if code := f.run(env, "config", "hosts"); code != 0 || !strings.Contains(f.out.String(), "No extra hosts") {
		t.Fatalf("hosts on empty config: code=%d out=%q err=%q", code, f.out.String(), f.errOut.String())
	}
	if code := f.run(env, "config", "allow-host", "https://Panel.Example.com/path"); code != 0 {
		t.Fatalf("allow-host failed: %d %s", code, f.errOut.String())
	}
	if code := f.run(env, "config", "allow-host", "panel.example.com"); code != 0 || !strings.Contains(f.out.String(), "already allowed") {
		t.Fatalf("duplicate allow-host: code=%d out=%q", code, f.out.String())
	}
	if code := f.run(env, "config", "allow-host", "other.example.com:8443"); code != 0 {
		t.Fatalf("allow-host with port failed: %d %s", code, f.errOut.String())
	}
	cfg, err := config.Load(f.dir, env.Config)
	if err != nil || strings.Join(cfg.AllowedHosts, ",") != "panel.example.com,other.example.com:8443" {
		t.Fatalf("allowedHosts = %v err=%v", cfg.AllowedHosts, err)
	}
	if code := f.run(env, "config", "disallow-host", "panel.example.com"); code != 0 {
		t.Fatalf("disallow-host failed: %d %s", code, f.errOut.String())
	}
	cfg, err = config.Load(f.dir, env.Config)
	if err != nil || strings.Join(cfg.AllowedHosts, ",") != "other.example.com:8443" {
		t.Fatalf("allowedHosts after disallow = %v err=%v", cfg.AllowedHosts, err)
	}
	for _, bad := range []string{"bad host", "-x.example.com", "a_b.example.com", "evil.com;rm"} {
		if code := f.run(env, "config", "allow-host", bad); code == 0 {
			t.Errorf("allow-host %q should fail", bad)
		}
	}
}

func TestInitAllowHostFlag(t *testing.T) {
	f := newFixture(t)
	home := t.TempDir()
	maestriDir, _, _ := setupSampleMaestriEnv(t, home)
	env := f.env("")
	env.Config.Home = home
	if code := f.run(env, "init", "--yes", "--maestri-dir", maestriDir, "--skip-auth", "--skill", "none", "--service", "none", "--allow-host", "panel.example.com"); code != 0 {
		t.Fatalf("init failed (%d): %s", code, f.errOut.String())
	}
	cfg, err := config.Load(f.dir, env.Config)
	if err != nil || len(cfg.AllowedHosts) != 1 || cfg.AllowedHosts[0] != "panel.example.com" {
		t.Fatalf("allowedHosts = %v err=%v", cfg.AllowedHosts, err)
	}
	out := f.out.String()
	for _, want := range []string{"Panel:   http://127.0.0.1:4777", "Public:  https://panel.example.com"} {
		if !strings.Contains(out, want) {
			t.Errorf("init summary missing %q:\n%s", want, out)
		}
	}
}
