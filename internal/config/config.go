// Package config resolves the config directory, loads and validates config.json, and resolves
// Maestri's data directory. The format is the same as V1's config.json, so an existing V1 config
// directory loads unchanged.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/maestria-dev/maestria/internal/privfile"
)

// Host is the only address the panel listens on. It is not configurable on purpose: the
// internet reaches the panel through a tunnel, never through an open port.
const Host = "127.0.0.1"

// DefaultPort is the panel port when config.json does not set one.
const DefaultPort = 4777

// Session is the browser session policy.
type Session struct {
	IdleMinutes   int `json:"idleMinutes"`
	AbsoluteHours int `json:"absoluteHours"`
}

// Login holds the failed-login limits (per IP and global).
type Login struct {
	MaxFailuresPerIP  int `json:"maxFailuresPerIp"`
	MaxFailuresGlobal int `json:"maxFailuresGlobal"`
	WindowSeconds     int `json:"windowSeconds"`
	LockoutSeconds    int `json:"lockoutSeconds"`
}

// CloudflareAccess enables verification of the Cf-Access-Jwt-Assertion header.
type CloudflareAccess struct {
	TeamDomain string `json:"teamDomain"`
	Audience   string `json:"audience"`
}

// Usage controls the usage reader (used by later builds; parsed now so V1 configs load).
type Usage struct {
	StaleMinutes    int  `json:"staleMinutes"`
	FallbackEnabled bool `json:"fallbackEnabled"`
	PollSeconds     int  `json:"pollSeconds"`
}

// StandbyActionTell notifies Maestro via a standby event without interrupting running agents.
const StandbyActionTell = "tell"

// StandbyActionInterrupt asks Maestro / Wire to interrupt active agents when a threshold hits.
const StandbyActionInterrupt = "interrupt"

// Standby controls the standby guard thresholds, actions, and resume behavior.
type Standby struct {
	Thresholds         map[string]map[string]int    `json:"thresholds"`
	Actions            map[string]map[string]string `json:"actions,omitempty"`
	ResumeBelowPercent *int                         `json:"resumeBelowPercent"`
	WakeOnReset        bool                         `json:"wakeOnReset"`
	NotifyViaCLI       bool                         `json:"notifyViaCli"`
}

// Tunnel holds argv commands (run without a shell) to control the public tunnel.
type Tunnel struct {
	StopCommand   []string `json:"stopCommand"`
	StartCommand  []string `json:"startCommand"`
	StatusCommand []string `json:"statusCommand"`
}

// Relay is the connector's policy for relayed (hosted-mode) requests. It is set on the machine,
// never from the web, so a compromised website cannot loosen it.
type Relay struct {
	// Enabled lets `maestria connect` serve relayed requests at all.
	Enabled bool `json:"enabled"`
	// ApprovalAbsoluteDays is how long a device stays approved, then it must be approved again.
	ApprovalAbsoluteDays int `json:"approvalAbsoluteDays"`
	// IdleDays is how long a device may stay unused before it must be approved again.
	IdleDays int `json:"idleDays"`
	// RatePerMinute limits relayed requests per device (writes count double).
	RatePerMinute int `json:"ratePerMinute"`
}

// Config is the validated configuration.
type Config struct {
	ConfigDir           string
	MaestriDir          string
	Port                int
	CookieSecure        bool
	AllowedHosts        []string
	TrustCfConnectingIP bool
	Session             Session
	Login               Login
	CloudflareAccess    *CloudflareAccess
	Usage               Usage
	Standby             Standby
	Tunnel              Tunnel
	Relay               Relay
	// Cloud is the hosted Maestria URL for `maestria connect` (the --cloud flag overrides it).
	Cloud string
}

// Error is a configuration error with a message meant for the user.
type Error struct{ msg string }

func (e *Error) Error() string { return e.msg }

func errorf(format string, args ...any) error { return &Error{msg: fmt.Sprintf(format, args...)} }

// Env abstracts the process environment so resolution can be tested.
type Env struct {
	Getenv func(string) string
	GOOS   string
	Home   string
}

// OSEnv returns the real process environment.
func OSEnv() Env {
	home, _ := os.UserHomeDir()
	return Env{Getenv: os.Getenv, GOOS: runtime.GOOS, Home: home}
}

func (e Env) get(key string) string {
	if e.Getenv == nil {
		return ""
	}
	return strings.TrimSpace(e.Getenv(key))
}

// DefaultDir returns the config directory: $MAESTRIA_CONFIG_DIR, %AppData%\maestria on Windows,
// or ~/.config/maestria.
func DefaultDir(env Env) string {
	if explicit := env.get("MAESTRIA_CONFIG_DIR"); explicit != "" {
		return explicit
	}
	if env.GOOS == "windows" {
		return filepath.Join(appData(env), "maestria")
	}
	return filepath.Join(env.Home, ".config", "maestria")
}

func appData(env Env) string {
	if v := env.get("APPDATA"); v != "" {
		return v
	}
	return filepath.Join(env.Home, "AppData", "Roaming")
}

// ExpandHome expands a leading ~ to the home directory.
func ExpandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		return filepath.Join(home, p[2:])
	}
	return p
}

// ResolveMaestriDir resolves Maestri's data directory: the configured value, then MAESTRI_HOME,
// then ~/.maestri (on Windows, %AppData%\Maestri when ~/.maestri is absent and that exists).
func ResolveMaestriDir(configured string, env Env) (string, error) {
	pick := strings.TrimSpace(configured)
	if pick == "" {
		pick = env.get("MAESTRI_HOME")
	}
	if pick == "" {
		homeDot := filepath.Join(env.Home, ".maestri")
		if env.GOOS == "windows" && !isDir(homeDot) {
			if cand := filepath.Join(appData(env), "Maestri"); isDir(cand) {
				pick = cand
			}
		}
		if pick == "" {
			pick = homeDot
		}
	}
	expanded := ExpandHome(pick, env.Home)
	if !filepath.IsAbs(expanded) {
		return "", errorf("config: \"maestriDir\" must resolve to an absolute path (got %q)", pick)
	}
	return filepath.Clean(expanded), nil
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// Defaults returns the default configuration for configDir.
func Defaults(configDir string, env Env) (*Config, error) {
	md, err := ResolveMaestriDir("", env)
	if err != nil {
		return nil, err
	}
	resume := 80
	return &Config{
		ConfigDir:    configDir,
		MaestriDir:   md,
		Port:         DefaultPort,
		CookieSecure: true,
		AllowedHosts: []string{},
		Session:      Session{IdleMinutes: 60, AbsoluteHours: 12},
		Login:        Login{MaxFailuresPerIP: 5, MaxFailuresGlobal: 20, WindowSeconds: 900, LockoutSeconds: 900},
		Usage:        Usage{StaleMinutes: 15, FallbackEnabled: true, PollSeconds: 30},
		Standby: Standby{
			Thresholds:         map[string]map[string]int{"claude": {"session": 95, "weekly": 90}},
			Actions:            map[string]map[string]string{},
			ResumeBelowPercent: &resume,
			WakeOnReset:        true,
		},
		Tunnel: Tunnel{StopCommand: []string{}, StartCommand: []string{}, StatusCommand: []string{}},
		Relay:  Relay{Enabled: true, ApprovalAbsoluteDays: 30, IdleDays: 7, RatePerMinute: 120},
	}, nil
}

var (
	hostRE      = regexp.MustCompile(`(?i)^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?(:\d{1,5})?$`)
	idKeyRE     = regexp.MustCompile(`(?i)^[a-z0-9*][a-z0-9._:-]{0,63}$`)
	teamRE      = regexp.MustCompile(`(?i)^[a-z0-9-]+(\.[a-z0-9-]+)+$`)
	audienceRE  = regexp.MustCompile(`(?i)^[a-f0-9]{16,128}$`)
	knownTopKey = map[string]bool{
		"maestriDir": true, "port": true, "cookieSecure": true, "allowedHosts": true,
		"trustCfConnectingIp": true, "session": true, "login": true, "cloudflareAccess": true,
		"usage": true, "standby": true, "tunnel": true, "relay": true, "cloud": true,
	}
)

type obj = map[string]json.RawMessage

func decodeObj(raw json.RawMessage, name string) (obj, error) {
	var o obj
	if isNull(raw) || json.Unmarshal(raw, &o) != nil || o == nil {
		return nil, errorf("config: %q must be an object", name)
	}
	return o, nil
}

func isNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

func intField(raw json.RawMessage, name string, lo, hi int) (int, error) {
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil || f != float64(int64(f)) || f < float64(lo) || f > float64(hi) {
		return 0, errorf("config: %q must be an integer between %d and %d", name, lo, hi)
	}
	return int(f), nil
}

func boolField(raw json.RawMessage, name string) (bool, error) {
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return false, errorf("config: %q must be a boolean", name)
	}
	return b, nil
}

func argvField(raw json.RawMessage, name string) ([]string, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || items == nil || len(items) > 16 {
		return nil, errorf("config: %q must be an array of at most 16 command arguments", name)
	}
	out := make([]string, 0, len(items))
	for i, it := range items {
		var s string
		if err := json.Unmarshal(it, &s); err != nil || s == "" || len(s) > 512 {
			return nil, errorf("config: \"%s[%d]\" must be a non-empty string", name, i)
		}
		out = append(out, s)
	}
	return out, nil
}

// Parse validates raw config.json bytes over the defaults. Unknown keys are rejected.
func Parse(data []byte, configDir string, env Env) (*Config, error) {
	cfg, err := Defaults(configDir, env)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return cfg, nil
	}
	var top obj
	if err := json.Unmarshal(data, &top); err != nil || top == nil {
		return nil, errorf("config: top level must be a JSON object")
	}
	for k := range top {
		if !knownTopKey[k] {
			return nil, errorf("config: unknown key %q (note: the listen address is fixed to %s)", k, Host)
		}
	}
	if raw, ok := top["maestriDir"]; ok && !isNull(raw) {
		var s string
		if json.Unmarshal(raw, &s) != nil || strings.TrimSpace(s) == "" {
			return nil, errorf("config: \"maestriDir\" must be a non-empty path string")
		}
		if cfg.MaestriDir, err = ResolveMaestriDir(s, env); err != nil {
			return nil, err
		}
	}
	if raw, ok := top["port"]; ok {
		if cfg.Port, err = intField(raw, "port", 1, 65535); err != nil {
			return nil, err
		}
	}
	if raw, ok := top["cookieSecure"]; ok {
		if cfg.CookieSecure, err = boolField(raw, "cookieSecure"); err != nil {
			return nil, err
		}
	}
	if raw, ok := top["trustCfConnectingIp"]; ok {
		if cfg.TrustCfConnectingIP, err = boolField(raw, "trustCfConnectingIp"); err != nil {
			return nil, err
		}
	}
	if raw, ok := top["allowedHosts"]; ok {
		var hosts []json.RawMessage
		if json.Unmarshal(raw, &hosts) != nil || hosts == nil || len(hosts) > 20 {
			return nil, errorf("config: \"allowedHosts\" must be an array of at most 20 host names")
		}
		cfg.AllowedHosts = make([]string, 0, len(hosts))
		for _, h := range hosts {
			var s string
			if json.Unmarshal(h, &s) != nil || !hostRE.MatchString(s) {
				return nil, errorf("config: invalid allowedHosts entry")
			}
			cfg.AllowedHosts = append(cfg.AllowedHosts, strings.ToLower(s))
		}
	}
	if raw, ok := top["session"]; ok {
		o, err := decodeObj(raw, "session")
		if err != nil {
			return nil, err
		}
		if v, ok := o["idleMinutes"]; ok {
			if cfg.Session.IdleMinutes, err = intField(v, "session.idleMinutes", 1, 24*60); err != nil {
				return nil, err
			}
		}
		if v, ok := o["absoluteHours"]; ok {
			if cfg.Session.AbsoluteHours, err = intField(v, "session.absoluteHours", 1, 24*30); err != nil {
				return nil, err
			}
		}
	}
	if raw, ok := top["login"]; ok {
		o, err := decodeObj(raw, "login")
		if err != nil {
			return nil, err
		}
		fields := []struct {
			key    string
			dst    *int
			lo, hi int
		}{
			{"maxFailuresPerIp", &cfg.Login.MaxFailuresPerIP, 1, 100},
			{"maxFailuresGlobal", &cfg.Login.MaxFailuresGlobal, 1, 1000},
			{"windowSeconds", &cfg.Login.WindowSeconds, 10, 86400},
			{"lockoutSeconds", &cfg.Login.LockoutSeconds, 10, 86400},
		}
		for _, f := range fields {
			if v, ok := o[f.key]; ok {
				if *f.dst, err = intField(v, "login."+f.key, f.lo, f.hi); err != nil {
					return nil, err
				}
			}
		}
	}
	if raw, ok := top["cloudflareAccess"]; ok && !isNull(raw) {
		var c struct {
			TeamDomain *string `json:"teamDomain"`
			Audience   *string `json:"audience"`
		}
		if json.Unmarshal(raw, &c) != nil || c.TeamDomain == nil || c.Audience == nil {
			return nil, errorf("config: \"cloudflareAccess\" needs string \"teamDomain\" and \"audience\"")
		}
		if !teamRE.MatchString(*c.TeamDomain) {
			return nil, errorf("config: \"cloudflareAccess.teamDomain\" must be a bare host name, e.g. myteam.cloudflareaccess.com")
		}
		if !audienceRE.MatchString(*c.Audience) {
			return nil, errorf("config: \"cloudflareAccess.audience\" must be the application AUD tag (hex)")
		}
		cfg.CloudflareAccess = &CloudflareAccess{TeamDomain: strings.ToLower(*c.TeamDomain), Audience: *c.Audience}
	}
	if raw, ok := top["usage"]; ok {
		o, err := decodeObj(raw, "usage")
		if err != nil {
			return nil, err
		}
		if v, ok := o["staleMinutes"]; ok {
			if cfg.Usage.StaleMinutes, err = intField(v, "usage.staleMinutes", 1, 1440); err != nil {
				return nil, err
			}
		}
		if v, ok := o["fallbackEnabled"]; ok {
			if cfg.Usage.FallbackEnabled, err = boolField(v, "usage.fallbackEnabled"); err != nil {
				return nil, err
			}
		}
		if v, ok := o["pollSeconds"]; ok {
			if cfg.Usage.PollSeconds, err = intField(v, "usage.pollSeconds", 5, 3600); err != nil {
				return nil, err
			}
		}
	}
	if raw, ok := top["standby"]; ok {
		if err := parseStandby(raw, cfg); err != nil {
			return nil, err
		}
	}
	if raw, ok := top["tunnel"]; ok {
		o, err := decodeObj(raw, "tunnel")
		if err != nil {
			return nil, err
		}
		for key, dst := range map[string]*[]string{
			"stopCommand": &cfg.Tunnel.StopCommand, "startCommand": &cfg.Tunnel.StartCommand, "statusCommand": &cfg.Tunnel.StatusCommand,
		} {
			if v, ok := o[key]; ok {
				if *dst, err = argvField(v, "tunnel."+key); err != nil {
					return nil, err
				}
			}
		}
	}
	if raw, ok := top["cloud"]; ok && !isNull(raw) {
		var u string
		if json.Unmarshal(raw, &u) != nil || !validCloudURL(u) {
			return nil, errorf("config: \"cloud\" must be an https URL like https://maestria.example (http is only allowed for localhost)")
		}
		cfg.Cloud = strings.TrimRight(u, "/")
	}
	if raw, ok := top["relay"]; ok {
		o, err := decodeObj(raw, "relay")
		if err != nil {
			return nil, err
		}
		if v, ok := o["enabled"]; ok {
			if cfg.Relay.Enabled, err = boolField(v, "relay.enabled"); err != nil {
				return nil, err
			}
		}
		for _, f := range []struct {
			key    string
			dst    *int
			lo, hi int
		}{
			{"approvalAbsoluteDays", &cfg.Relay.ApprovalAbsoluteDays, 1, 365},
			{"idleDays", &cfg.Relay.IdleDays, 1, 365},
			{"ratePerMinute", &cfg.Relay.RatePerMinute, 1, 6000},
		} {
			if v, ok := o[f.key]; ok {
				if *f.dst, err = intField(v, "relay."+f.key, f.lo, f.hi); err != nil {
					return nil, err
				}
			}
		}
	}
	return cfg, nil
}

func parseStandbyWindowMap[T any](raw json.RawMessage, section string, parse func(json.RawMessage, string) (T, error)) (map[string]map[string]T, error) {
	providers, err := decodeObj(raw, section)
	if err != nil {
		return nil, err
	}
	out := make(map[string]map[string]T, len(providers))
	for pid, wraw := range providers {
		if !idKeyRE.MatchString(pid) {
			return nil, errorf("config: invalid \"%s.%s\"", section, pid)
		}
		windows, err := decodeObj(wraw, section+"."+pid)
		if err != nil {
			return nil, err
		}
		wm := make(map[string]T, len(windows))
		for wk, val := range windows {
			if !idKeyRE.MatchString(wk) {
				return nil, errorf("config: invalid standby window key %q", wk)
			}
			parsed, err := parse(val, section+"."+pid+"."+wk)
			if err != nil {
				return nil, err
			}
			wm[wk] = parsed
		}
		out[pid] = wm
	}
	return out, nil
}

func actionField(raw json.RawMessage, name string) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", errorf("config: %q must be a string", name)
	}
	switch s {
	case StandbyActionTell, StandbyActionInterrupt:
		return s, nil
	default:
		return "", errorf("config: %q must be %q or %q", name, StandbyActionTell, StandbyActionInterrupt)
	}
}

func parseStandby(raw json.RawMessage, cfg *Config) error {
	o, err := decodeObj(raw, "standby")
	if err != nil {
		return err
	}
	if v, ok := o["notifyViaCli"]; ok {
		if cfg.Standby.NotifyViaCLI, err = boolField(v, "standby.notifyViaCli"); err != nil {
			return err
		}
	}
	if v, ok := o["wakeOnReset"]; ok {
		if cfg.Standby.WakeOnReset, err = boolField(v, "standby.wakeOnReset"); err != nil {
			return err
		}
	}
	if v, ok := o["resumeBelowPercent"]; ok {
		if isNull(v) {
			cfg.Standby.ResumeBelowPercent = nil
		} else {
			n, err := intField(v, "standby.resumeBelowPercent", 0, 99)
			if err != nil {
				return err
			}
			cfg.Standby.ResumeBelowPercent = &n
		}
	}
	if v, ok := o["thresholds"]; ok {
		out, err := parseStandbyWindowMap(v, "standby.thresholds", func(val json.RawMessage, name string) (int, error) {
			return intField(val, name, 1, 100)
		})
		if err != nil {
			return err
		}
		cfg.Standby.Thresholds = out
	}
	if v, ok := o["actions"]; ok {
		out, err := parseStandbyWindowMap(v, "standby.actions", actionField)
		if err != nil {
			return err
		}
		cfg.Standby.Actions = out
	}
	return nil
}

// FilePath returns the path of config.json in configDir.
func FilePath(configDir string) string { return filepath.Join(configDir, "config.json") }

// Load reads and validates config.json from configDir. A missing file yields the defaults.
func Load(configDir string, env Env) (*Config, error) {
	data, err := os.ReadFile(FilePath(configDir))
	if errors.Is(err, fs.ErrNotExist) {
		return Defaults(configDir, env)
	}
	if err != nil {
		return nil, errorf("config: cannot read %s: %v", FilePath(configDir), err)
	}
	return Parse(data, configDir, env)
}

// SaveRaw validates raw JSON and writes it as config.json (0600).
func SaveRaw(configDir string, data []byte, env Env) error {
	if _, err := Parse(data, configDir, env); err != nil {
		return err
	}
	return privfile.WriteAtomic(FilePath(configDir), data)
}

func loadTopMap(configDir string) (obj, bool, error) {
	top := obj{}
	existing, err := os.ReadFile(FilePath(configDir))
	if errors.Is(err, fs.ErrNotExist) {
		return top, false, nil
	}
	if err != nil {
		return nil, false, errorf("config: cannot read %s: %v", FilePath(configDir), err)
	}
	if len(bytes.TrimSpace(existing)) > 0 {
		if err := json.Unmarshal(existing, &top); err != nil {
			return nil, true, errorf("config: existing config.json is not valid JSON: %v", err)
		}
	}
	return top, true, nil
}

// RawKeys returns the top-level keys present in config.json and whether the file exists.
func RawKeys(configDir string) (map[string]json.RawMessage, bool, error) {
	return loadTopMap(configDir)
}

// SaveMaestriDir updates or removes "maestriDir" in config.json while preserving other keys.
func SaveMaestriDir(configDir, maestriDir string, custom bool, env Env) error {
	top, _, err := loadTopMap(configDir)
	if err != nil {
		return err
	}
	if custom {
		raw, err := json.Marshal(maestriDir)
		if err != nil {
			return err
		}
		top["maestriDir"] = raw
	} else {
		delete(top, "maestriDir")
	}
	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return err
	}
	return SaveRaw(configDir, append(out, '\n'), env)
}

// SaveStandby updates the "standby" section of config.json while preserving other keys.
func SaveStandby(configDir string, s Standby, env Env) error {
	top, _, err := loadTopMap(configDir)
	if err != nil {
		return err
	}
	if s.Thresholds == nil {
		s.Thresholds = map[string]map[string]int{}
	}
	rawStandby, err := json.Marshal(s)
	if err != nil {
		return err
	}
	top["standby"] = rawStandby
	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return err
	}
	return SaveRaw(configDir, append(out, '\n'), env)
}

// EnsureDir creates the config directory with owner-only permissions.
func EnsureDir(configDir string) error { return privfile.EnsureDir(configDir) }

// MaestriDirStatus reports what exists under the Maestri data dir.
type MaestriDirStatus struct {
	Dir, ProvidersDir, StatusFile          string
	HasDir, HasProvidersDir, HasStatusFile bool
}

// InspectMaestriDir checks the Maestri data dir without writing anything.
func InspectMaestriDir(dir string) MaestriDirStatus {
	providers := filepath.Join(dir, "usage", "providers")
	status := filepath.Join(providers, ".status.json")
	st := MaestriDirStatus{Dir: dir, ProvidersDir: providers, StatusFile: status}
	st.HasDir = isDir(dir)
	st.HasProvidersDir = st.HasDir && isDir(providers)
	if st.HasProvidersDir {
		if fi, err := os.Stat(status); err == nil && fi.Mode().IsRegular() {
			st.HasStatusFile = true
		}
	}
	return st
}

// validCloudURL accepts an https URL, or http for a loopback host (development).
func validCloudURL(s string) bool {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	h := strings.ToLower(u.Hostname())
	return u.Scheme == "http" && (h == "localhost" || h == "127.0.0.1" || h == "::1")
}

// ValidCloudURL reports whether s is an acceptable cloud URL (https, or http on loopback).
func ValidCloudURL(s string) bool { return validCloudURL(s) }
