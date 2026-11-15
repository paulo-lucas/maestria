package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testEnv(home string, vars map[string]string) Env {
	return Env{Getenv: func(k string) string { return vars[k] }, GOOS: runtime.GOOS, Home: home}
}

func TestDefaultDir(t *testing.T) {
	home := t.TempDir()
	if got := DefaultDir(testEnv(home, map[string]string{"MAESTRIA_CONFIG_DIR": "/x/y"})); got != "/x/y" {
		t.Fatalf("explicit dir: %q", got)
	}
	env := testEnv(home, nil)
	env.GOOS = "linux"
	if got, want := DefaultDir(env), filepath.Join(home, ".config", "maestria"); got != want {
		t.Fatalf("posix: %q want %q", got, want)
	}
	env.GOOS = "windows"
	if got, want := DefaultDir(env), filepath.Join(home, "AppData", "Roaming", "maestria"); got != want {
		t.Fatalf("windows default: %q want %q", got, want)
	}
	appdata := filepath.Join(home, "Roaming")
	env = testEnv(home, map[string]string{"APPDATA": appdata})
	env.GOOS = "windows"
	if got, want := DefaultDir(env), filepath.Join(appdata, "maestria"); got != want {
		t.Fatalf("windows APPDATA: %q want %q", got, want)
	}
}

func TestResolveMaestriDirPriority(t *testing.T) {
	home := t.TempDir()
	fromEnv := filepath.Join(home, "env-maestri")
	fromCfg := filepath.Join(home, "cfg-maestri")
	env := testEnv(home, map[string]string{"MAESTRI_HOME": fromEnv})

	if got, _ := ResolveMaestriDir(fromCfg, env); got != fromCfg {
		t.Fatalf("config should win: %q", got)
	}
	if got, _ := ResolveMaestriDir("", env); got != fromEnv {
		t.Fatalf("MAESTRI_HOME should be next: %q", got)
	}
	if got, _ := ResolveMaestriDir("", testEnv(home, nil)); got != filepath.Join(home, ".maestri") {
		t.Fatalf("default ~/.maestri: %q", got)
	}
	if got, _ := ResolveMaestriDir("~/m", testEnv(home, nil)); got != filepath.Join(home, "m") {
		t.Fatalf("~ expansion: %q", got)
	}
	if _, err := ResolveMaestriDir("relative/dir", testEnv(home, nil)); err == nil {
		t.Fatal("relative maestriDir must be refused")
	}

	// Windows: %APPDATA%\Maestri when ~/.maestri is absent and it exists.
	appdata := filepath.Join(home, "Roaming")
	if err := os.MkdirAll(filepath.Join(appdata, "Maestri"), 0o700); err != nil {
		t.Fatal(err)
	}
	win := testEnv(home, map[string]string{"APPDATA": appdata})
	win.GOOS = "windows"
	if got := mustResolve(t, win); got != filepath.Join(appdata, "Maestri") {
		t.Fatalf("windows appdata fallback: %q", got)
	}
	if err := os.MkdirAll(filepath.Join(home, ".maestri"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := mustResolve(t, win); got != filepath.Join(home, ".maestri") {
		t.Fatalf("~/.maestri wins when present: %q", got)
	}
}

func mustResolve(t *testing.T, env Env) string {
	t.Helper()
	got, err := ResolveMaestriDir("", env)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestDefaultsAreSafe(t *testing.T) {
	cfg, err := Parse(nil, t.TempDir(), testEnv(t.TempDir(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 4777 || !cfg.CookieSecure || cfg.TrustCfConnectingIP || len(cfg.AllowedHosts) != 0 || cfg.CloudflareAccess != nil {
		t.Fatalf("unsafe defaults: %+v", cfg)
	}
	if cfg.Session.IdleMinutes != 60 || cfg.Session.AbsoluteHours != 12 {
		t.Fatalf("session defaults: %+v", cfg.Session)
	}
	if cfg.Login != (Login{5, 20, 900, 900}) {
		t.Fatalf("login defaults: %+v", cfg.Login)
	}
}

func TestListenAddressIsNotConfigurable(t *testing.T) {
	for _, key := range []string{"host", "listen", "bind"} {
		_, err := Parse([]byte(`{"`+key+`":"0.0.0.0"}`), t.TempDir(), testEnv(t.TempDir(), nil))
		if err == nil || !strings.Contains(err.Error(), "unknown key") {
			t.Fatalf("%s: expected unknown key error, got %v", key, err)
		}
	}
}

func TestValidation(t *testing.T) {
	bad := []string{
		`[]`,
		`{"port":0}`, `{"port":70000}`, `{"port":"4777"}`, `{"port":12.5}`,
		`{"cookieSecure":"yes"}`,
		`{"allowedHosts":"panel.example.com"}`, `{"allowedHosts":["bad host"]}`, `{"allowedHosts":["http://x.com"]}`,
		`{"session":{"idleMinutes":0}}`, `{"session":{"absoluteHours":721}}`, `{"session":5}`,
		`{"login":{"maxFailuresPerIp":0}}`, `{"login":{"windowSeconds":5}}`,
		`{"cloudflareAccess":{"teamDomain":"x"}}`,
		`{"cloudflareAccess":{"teamDomain":"https://team.cloudflareaccess.com","audience":"abcdef0123456789"}}`,
		`{"cloudflareAccess":{"teamDomain":"team.cloudflareaccess.com","audience":"not-hex"}}`,
		`{"usage":{"pollSeconds":1}}`,
		`{"standby":{"thresholds":{"claude":{"session":101}}}}`, `{"standby":{"thresholds":{"bad key!":{}}}}`,
		`{"standby":{"resumeBelowPercent":100}}`,
		`{"updates":{"check":"no"}}`, `{"updates":true}`,
		`{"tunnel":{"stopCommand":"pkill cloudflared"}}`, `{"tunnel":{"stopCommand":[""]}}`,
		`{"tunnel":{"stopCommand":["a","a","a","a","a","a","a","a","a","a","a","a","a","a","a","a","a"]}}`,
	}
	for _, raw := range bad {
		_, err := Parse([]byte(raw), t.TempDir(), testEnv(t.TempDir(), nil))
		var ce *Error
		if !errors.As(err, &ce) {
			t.Errorf("%s: expected config error, got %v", raw, err)
		}
	}

	good := `{
		"port": 4780, "cookieSecure": false, "allowedHosts": ["Panel.Example.com", "x.example.com:443"],
		"trustCfConnectingIp": true,
		"session": {"idleMinutes": 30, "absoluteHours": 8},
		"login": {"maxFailuresPerIp": 3, "maxFailuresGlobal": 10, "windowSeconds": 60, "lockoutSeconds": 120},
		"cloudflareAccess": {"teamDomain": "Team.cloudflareaccess.com", "audience": "abcdef0123456789abcdef"},
		"usage": {"staleMinutes": 10, "fallbackEnabled": false, "pollSeconds": 60},
		"standby": {"thresholds": {"claude": {"session": 90}}, "resumeBelowPercent": null, "notifyViaCli": true},
		"tunnel": {"stopCommand": ["pkill", "-x", "cloudflared"], "startCommand": ["cloudflared", "tunnel", "run"]}
	}`
	cfg, err := Parse([]byte(good), t.TempDir(), testEnv(t.TempDir(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 4780 || cfg.CookieSecure || cfg.AllowedHosts[0] != "panel.example.com" || !cfg.TrustCfConnectingIP {
		t.Fatalf("parsed: %+v", cfg)
	}
	if cfg.CloudflareAccess.TeamDomain != "team.cloudflareaccess.com" || cfg.Session.IdleMinutes != 30 || cfg.Login.LockoutSeconds != 120 {
		t.Fatalf("parsed: %+v", cfg)
	}
	if cfg.Standby.ResumeBelowPercent != nil || cfg.Standby.Thresholds["claude"]["session"] != 90 {
		t.Fatalf("standby: %+v", cfg.Standby)
	}
	if strings.Join(cfg.Tunnel.StopCommand, " ") != "pkill -x cloudflared" || len(cfg.Tunnel.StatusCommand) != 0 {
		t.Fatalf("tunnel: %+v", cfg.Tunnel)
	}
}

// A config.json written by V1 (every key it knows) must load unchanged.
func TestLoadsV1ConfigDir(t *testing.T) {
	dir := t.TempDir()
	v1 := `{
  "maestriDir": "~/.maestri",
  "port": 4777,
  "cookieSecure": true,
  "allowedHosts": ["panel.example.com"],
  "trustCfConnectingIp": true,
  "session": { "idleMinutes": 60, "absoluteHours": 12 },
  "cloudflareAccess": null,
  "standby": { "thresholds": { "claude": { "session": 95, "weekly": 90 } }, "resumeBelowPercent": 80 },
  "tunnel": { "stopCommand": ["pkill", "-x", "cloudflared"] }
}
`
	if err := os.WriteFile(FilePath(dir), []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	cfg, err := Load(dir, testEnv(home, nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaestriDir != filepath.Join(home, ".maestri") || cfg.AllowedHosts[0] != "panel.example.com" {
		t.Fatalf("v1 config: %+v", cfg)
	}
	after, _ := os.ReadFile(FilePath(dir))
	if string(after) != v1 {
		t.Fatal("loading must not rewrite config.json")
	}
}

func TestLoadMissingAndSave(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	env := testEnv(t.TempDir(), nil)
	cfg, err := Load(dir, env)
	if err != nil || cfg.Port != DefaultPort {
		t.Fatalf("missing file should give defaults: %v %+v", err, cfg)
	}
	if err := SaveRaw(dir, []byte(`{"port":"x"}`), env); err == nil {
		t.Fatal("invalid config must not be saved")
	}
	if err := SaveRaw(dir, []byte(`{"port":4790}`+"\n"), env); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(dir, env)
	if err != nil || cfg.Port != 4790 {
		t.Fatalf("reload: %v %+v", err, cfg)
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(FilePath(dir))
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("config.json mode %v", st.Mode().Perm())
		}
		dst, _ := os.Stat(dir)
		if dst.Mode().Perm() != 0o700 {
			t.Fatalf("config dir mode %v", dst.Mode().Perm())
		}
	}
}

func TestInspectMaestriDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "maestri")
	if st := InspectMaestriDir(dir); st.HasDir {
		t.Fatal("missing dir")
	}
	providers := filepath.Join(dir, "usage", "providers")
	if err := os.MkdirAll(providers, 0o700); err != nil {
		t.Fatal(err)
	}
	if st := InspectMaestriDir(dir); !st.HasProvidersDir || st.HasStatusFile {
		t.Fatalf("%+v", st)
	}
	if err := os.WriteFile(filepath.Join(providers, ".status.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := InspectMaestriDir(dir); !st.HasStatusFile {
		t.Fatalf("%+v", st)
	}
}

func TestStandbyActionsWakeAndSave(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	env := testEnv(t.TempDir(), nil)
	if err := SaveRaw(dir, []byte(`{"port":4788,"allowedHosts":["panel.example.com"]}`+"\n"), env); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir, env)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Standby.WakeOnReset {
		t.Fatal("WakeOnReset should default to true")
	}

	resume := 70
	next := Standby{
		Thresholds:         map[string]map[string]int{"claude": {"session": 85}},
		Actions:            map[string]map[string]string{"claude": {"session": StandbyActionInterrupt}},
		ResumeBelowPercent: &resume,
		WakeOnReset:        false,
		NotifyViaCLI:       true,
	}
	if err := SaveStandby(dir, next, env); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(dir, env)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Port != 4788 || len(reloaded.AllowedHosts) != 1 || reloaded.AllowedHosts[0] != "panel.example.com" {
		t.Fatalf("unrelated keys lost: %+v", reloaded)
	}
	if reloaded.Standby.WakeOnReset || !reloaded.Standby.NotifyViaCLI || *reloaded.Standby.ResumeBelowPercent != 70 {
		t.Fatalf("standby scalar mismatch: %+v", reloaded.Standby)
	}
	if reloaded.Standby.Thresholds["claude"]["session"] != 85 || reloaded.Standby.Actions["claude"]["session"] != StandbyActionInterrupt {
		t.Fatalf("standby maps mismatch: %+v", reloaded.Standby)
	}
	if _, err := Parse([]byte(`{"standby":{"actions":{"claude":{"session":"bogus"}}}}`), dir, env); err == nil {
		t.Fatal("invalid action should fail parse")
	}
}

func TestUpdatesCheckDefaultsOnAndCanBeTurnedOff(t *testing.T) {
	env := testEnv(t.TempDir(), nil)
	cfg, err := Parse([]byte(`{}`), t.TempDir(), env)
	if err != nil || !cfg.Updates.Check {
		t.Fatalf("default: %+v %v", cfg.Updates, err)
	}
	cfg, err = Parse([]byte(`{"updates":{"check":false}}`), t.TempDir(), env)
	if err != nil || cfg.Updates.Check {
		t.Fatalf("off: %+v %v", cfg.Updates, err)
	}
}
