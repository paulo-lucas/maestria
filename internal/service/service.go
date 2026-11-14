// Package service installs, controls, inspects, and reads logs for the per-user Maestria
// background service on macOS (launchd), Linux (systemd --user), and Windows (Task Scheduler).
package service

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/paulo-lucas/maestria/internal/config"
	"github.com/paulo-lucas/maestria/internal/privfile"
)

const (
	LaunchdLabel    = "dev.maestria.panel"
	SystemdUnit     = "maestria.service"
	WindowsTaskName = "MaestriaPanel"

	ModeServe   = "serve"
	ModeConnect = "connect"
	// ModeBoth runs the local panel and the hosted connection in one process
	// ("maestria serve --connect").
	ModeBoth = "both"
)

// ModeArgs returns the command-line arguments a service runs for mode (after the binary).
func ModeArgs(mode string) []string {
	switch mode {
	case ModeConnect:
		return []string{"connect"}
	case ModeBoth:
		return []string{"serve", "--connect"}
	}
	return []string{"serve"}
}

// Error marks a user-facing service management error.
type Error struct{ err error }

func (e *Error) Error() string { return e.err.Error() }
func (e *Error) Unwrap() error { return e.err }

func serviceErrorf(format string, args ...any) error {
	return &Error{err: fmt.Errorf(format, args...)}
}

// CmdResult is the result of invoking a service manager command.
type CmdResult struct {
	OK     bool
	Code   int
	Stdout string
	Stderr string
}

// CommandRunner executes a service manager command (launchctl, systemctl, schtasks).
type CommandRunner func(ctx context.Context, argv []string) CmdResult

// DefaultRunner executes real OS service manager commands with a timeout.
func DefaultRunner(ctx context.Context, argv []string) CmdResult {
	if len(argv) == 0 || argv[0] == "" {
		return CmdResult{OK: true}
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	outStr := strings.TrimSpace(stdout.String())
	errStr := strings.TrimSpace(stderr.String())
	if err == nil {
		return CmdResult{OK: true, Code: 0, Stdout: outStr, Stderr: errStr}
	}
	code := -1
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	}
	if errStr == "" {
		errStr = err.Error()
	}
	if len(errStr) > 500 {
		errStr = errStr[:500]
	}
	return CmdResult{OK: false, Code: code, Stdout: outStr, Stderr: errStr}
}

// Reachability describes whether the local panel HTTP server responds.
type Reachability struct {
	Reachable  bool   `json:"reachable"`
	URL        string `json:"url"`
	StatusCode int    `json:"statusCode,omitempty"`
}

// CheckReachable probes http://127.0.0.1:<port>/login with a short timeout.
func CheckReachable(host string, port int, timeout time.Duration) Reachability {
	probeHost := host
	if probeHost == "" || probeHost == "0.0.0.0" || probeHost == "::" {
		probeHost = config.Host
	}
	url := fmt.Sprintf("http://%s:%d", probeHost, port)
	if timeout <= 0 {
		timeout = 600 * time.Millisecond
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url + "/login")
	if err != nil {
		return Reachability{Reachable: false, URL: url}
	}
	_ = resp.Body.Close()
	return Reachability{Reachable: true, URL: url, StatusCode: resp.StatusCode}
}

// StatusReport describes the current installation and runtime state of the service.
type StatusReport struct {
	Platform  string       `json:"platform"`
	Manager   string       `json:"manager"`
	UnitPath  string       `json:"unitPath"`
	LogDir    string       `json:"logDir"`
	Installed bool         `json:"installed"`
	Loaded    bool         `json:"loaded"`
	Running   bool         `json:"running"`
	Server    Reachability `json:"server"`
}

// InstallResult describes the files written during service installation.
type InstallResult struct {
	UnitPath string `json:"unitPath"`
	LogDir   string `json:"logDir"`
	Mode     string `json:"mode"`
}

// LogFileContent holds the trailing lines of one service log file.
type LogFileContent struct {
	Path    string `json:"path"`
	Exists  bool   `json:"exists"`
	Content string `json:"content"`
}

// Options configures service operations and allows test overrides.
type Options struct {
	ConfigDir         string
	HomeDir           string
	Env               config.Env
	BinaryPath        string
	Mode              string
	UID               int
	Runner            CommandRunner
	ReachabilityCheck func(host string, port int) Reachability
}

type resolvedContext struct {
	goos       string
	env        config.Env
	homeDir    string
	configDir  string
	binaryPath string
	mode       string
	uid        int
	runner     CommandRunner
	reachCheck func(host string, port int) Reachability
}

// IsSupported reports whether background service management is supported for goos.
func IsSupported(goos string) bool {
	return goos == "darwin" || goos == "linux" || goos == "windows"
}

// ValidateMode checks that mode is "serve", "connect" or "both" (empty defaults to "serve").
func ValidateMode(mode string) (string, error) {
	m := strings.ToLower(strings.TrimSpace(mode))
	if m == "" {
		return ModeServe, nil
	}
	if m == ModeServe || m == ModeConnect || m == ModeBoth {
		return m, nil
	}
	return "", serviceErrorf("invalid service mode %q (must be %q, %q or %q)", mode, ModeServe, ModeConnect, ModeBoth)
}

// DefaultBinaryPath returns the cleaned absolute path of the running executable.
func DefaultBinaryPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", serviceErrorf("cannot determine executable path: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	abs, err := filepath.Abs(exe)
	if err != nil {
		return "", serviceErrorf("cannot resolve executable path: %w", err)
	}
	return filepath.Clean(abs), nil
}

func resolveContext(opts Options) (resolvedContext, error) {
	env := opts.Env
	if env.GOOS == "" {
		env = config.OSEnv()
	}
	if env.Getenv == nil {
		env.Getenv = os.Getenv
	}
	if !IsSupported(env.GOOS) {
		return resolvedContext{}, serviceErrorf(
			"background service management is supported on macOS (launchd), Linux (systemd --user), and Windows (Task Scheduler), not %q. Run \"maestria serve\" directly",
			env.GOOS,
		)
	}
	homeDir := strings.TrimSpace(opts.HomeDir)
	if homeDir == "" {
		homeDir = env.Home
	}
	if homeDir == "" {
		homeDir, _ = os.UserHomeDir()
	}
	env.Home = homeDir

	cfgDir := strings.TrimSpace(opts.ConfigDir)
	if cfgDir == "" {
		cfgDir = config.DefaultDir(env)
	}
	mode, err := ValidateMode(opts.Mode)
	if err != nil {
		return resolvedContext{}, err
	}
	binPath := strings.TrimSpace(opts.BinaryPath)
	if binPath == "" {
		binPath, err = DefaultBinaryPath()
		if err != nil {
			return resolvedContext{}, err
		}
	}
	uid := opts.UID
	if uid <= 0 {
		if env.GOOS != "windows" {
			uid = os.Getuid()
		}
		if uid <= 0 {
			uid = 501
		}
	}
	runner := opts.Runner
	if runner == nil {
		runner = DefaultRunner
	}
	reach := opts.ReachabilityCheck
	if reach == nil {
		reach = func(host string, port int) Reachability {
			return CheckReachable(host, port, 600*time.Millisecond)
		}
	}
	return resolvedContext{
		goos:       env.GOOS,
		env:        env,
		homeDir:    homeDir,
		configDir:  cfgDir,
		binaryPath: binPath,
		mode:       mode,
		uid:        uid,
		runner:     runner,
		reachCheck: reach,
	}, nil
}

func joinOS(goos string, parts ...string) string {
	if goos == "windows" {
		cleaned := make([]string, 0, len(parts))
		for _, p := range parts {
			if p != "" {
				cleaned = append(cleaned, strings.TrimRight(strings.ReplaceAll(p, "/", `\`), `\`))
			}
		}
		return strings.Join(cleaned, `\`)
	}
	return path.Join(parts...)
}

func dirOS(goos, p string) string {
	if goos == "windows" {
		p = strings.ReplaceAll(p, "/", `\`)
		idx := strings.LastIndex(p, `\`)
		if idx < 0 {
			return "."
		}
		return p[:idx]
	}
	return path.Dir(p)
}

// LogDir returns the service log directory inside configDir.
func LogDir(configDir string) string {
	return filepath.Join(configDir, "logs")
}

// LogFiles returns the expected service log file paths for goos inside configDir.
func LogFiles(configDir, goos string) []string {
	if goos == "" {
		goos = runtime.GOOS
	}
	dir := filepath.Join(configDir, "logs")
	if goos == "windows" {
		return []string{filepath.Join(dir, "panel.log")}
	}
	return []string{
		filepath.Join(dir, "panel.out.log"),
		filepath.Join(dir, "panel.err.log"),
	}
}

// LaunchdPlistPath returns the macOS LaunchAgent plist path under homeDir.
func LaunchdPlistPath(homeDir string) string {
	return filepath.Join(homeDir, "Library", "LaunchAgents", LaunchdLabel+".plist")
}

// SystemdUnitPath returns the Linux systemd --user unit file path.
func SystemdUnitPath(homeDir string, env config.Env) string {
	var xdg string
	if env.Getenv != nil {
		xdg = strings.TrimSpace(env.Getenv("XDG_CONFIG_HOME"))
	}
	base := filepath.Join(homeDir, ".config")
	if xdg != "" {
		base = xdg
	}
	return filepath.Join(base, "systemd", "user", SystemdUnit)
}

// WindowsServiceScriptPath returns the Windows service wrapper script path inside configDir.
func WindowsServiceScriptPath(configDir string) string {
	return filepath.Join(configDir, "maestria-service.cmd")
}

// WindowsTaskXMLPath returns the Windows Task Scheduler XML path inside configDir.
func WindowsTaskXMLPath(configDir string) string {
	return filepath.Join(configDir, "maestria-task.xml")
}

// UnitFile returns the primary service definition path for the resolved platform.
func UnitFile(opts Options) (string, error) {
	ctx, err := resolveContext(opts)
	if err != nil {
		return "", err
	}
	return unitFileFromCtx(ctx), nil
}

func unitFileFromCtx(ctx resolvedContext) string {
	switch ctx.goos {
	case "darwin":
		return LaunchdPlistPath(ctx.homeDir)
	case "linux":
		return SystemdUnitPath(ctx.homeDir, ctx.env)
	default:
		return WindowsServiceScriptPath(ctx.configDir)
	}
}

// IsInstalled reports whether the service definition file exists.
func IsInstalled(opts Options) bool {
	u, err := UnitFile(opts)
	if err != nil {
		return false
	}
	_, err = os.Stat(u)
	return err == nil
}

func buildServiceEnv(binaryPath, configDir, goos string, env config.Env) map[string]string {
	delim := ":"
	if goos == "windows" {
		delim = ";"
	}
	binDir := dirOS(goos, binaryPath)
	rawPath := ""
	if env.Getenv != nil {
		rawPath = env.Getenv("PATH")
		if rawPath == "" {
			rawPath = env.Getenv("Path")
		}
	}
	var candidates []string
	if binDir != "" && binDir != "." {
		candidates = append(candidates, binDir)
	}
	if rawPath != "" {
		candidates = append(candidates, strings.Split(rawPath, delim)...)
	}
	if goos != "windows" {
		candidates = append(candidates, "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin")
	}
	seen := map[string]bool{}
	var parts []string
	for _, p := range candidates {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		parts = append(parts, p)
	}
	out := map[string]string{
		"PATH":                strings.Join(parts, delim),
		"MAESTRIA_CONFIG_DIR": configDir,
	}
	if env.Getenv != nil {
		if mh := strings.TrimSpace(env.Getenv("MAESTRI_HOME")); mh != "" {
			out["MAESTRI_HOME"] = mh
		}
	}
	return out
}

func sortedEnvKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// RenderLaunchdPlist renders the macOS LaunchAgent plist for the given binary, mode, and configDir.
func RenderLaunchdPlist(binaryPath, mode, configDir string, env config.Env) string {
	if mode == "" {
		mode = ModeServe
	}
	logDir := joinOS("darwin", configDir, "logs")
	envVars := buildServiceEnv(binaryPath, configDir, "darwin", env)
	var envLines []string
	for _, k := range sortedEnvKeys(envVars) {
		envLines = append(envLines,
			fmt.Sprintf("    <key>%s</key>", xmlEscape(k)),
			fmt.Sprintf("    <string>%s</string>", xmlEscape(envVars[k])),
		)
	}
	lines := []string{
		`<?xml version="1.0" encoding="UTF-8"?>`,
		`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">`,
		`<plist version="1.0">`,
		`<dict>`,
		`  <key>Label</key>`,
		fmt.Sprintf(`  <string>%s</string>`, LaunchdLabel),
		`  <key>ProgramArguments</key>`,
		`  <array>`,
		fmt.Sprintf(`    <string>%s</string>`, xmlEscape(binaryPath)),
	}
	for _, a := range ModeArgs(mode) {
		lines = append(lines, fmt.Sprintf(`    <string>%s</string>`, xmlEscape(a)))
	}
	lines = append(lines,
		`  </array>`,
		`  <key>EnvironmentVariables</key>`,
		`  <dict>`,
	)
	lines = append(lines, envLines...)
	lines = append(lines,
		`  </dict>`,
		`  <key>RunAtLoad</key>`,
		`  <true/>`,
		`  <key>KeepAlive</key>`,
		`  <true/>`,
		`  <key>StandardOutPath</key>`,
		fmt.Sprintf(`  <string>%s</string>`, xmlEscape(joinOS("darwin", logDir, "panel.out.log"))),
		`  <key>StandardErrorPath</key>`,
		fmt.Sprintf(`  <string>%s</string>`, xmlEscape(joinOS("darwin", logDir, "panel.err.log"))),
		`</dict>`,
		`</plist>`,
		"",
	)
	return strings.Join(lines, "\n")
}

func escapeSystemdArg(raw string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`)
	return `"` + r.Replace(raw) + `"`
}

func escapeSystemdPath(raw string) string {
	return strings.ReplaceAll(raw, `%`, `%%`)
}

// RenderSystemdUnit renders the Linux systemd --user unit file.
func RenderSystemdUnit(binaryPath, mode, configDir string, env config.Env) string {
	if mode == "" {
		mode = ModeServe
	}
	logDir := joinOS("linux", configDir, "logs")
	envVars := buildServiceEnv(binaryPath, configDir, "linux", env)
	var envLines []string
	for _, k := range sortedEnvKeys(envVars) {
		envLines = append(envLines, "Environment="+escapeSystemdArg(k+"="+envVars[k]))
	}
	lines := []string{
		"[Unit]",
		"Description=Maestria control panel",
		"After=network-online.target",
		"",
		"[Service]",
		"Type=simple",
		fmt.Sprintf("ExecStart=%s %s", escapeSystemdArg(binaryPath), strings.Join(ModeArgs(mode), " ")),
	}
	lines = append(lines, envLines...)
	lines = append(lines,
		"StandardOutput=append:"+escapeSystemdPath(joinOS("linux", logDir, "panel.out.log")),
		"StandardError=append:"+escapeSystemdPath(joinOS("linux", logDir, "panel.err.log")),
		"Restart=always",
		"RestartSec=3",
		"",
		"[Install]",
		"WantedBy=default.target",
		"",
	)
	return strings.Join(lines, "\n")
}

func escapeCmdValue(raw string) string {
	r := strings.NewReplacer(`%`, `%%`, `"`, ``)
	return r.Replace(raw)
}

// RenderWindowsServiceScript renders the .cmd wrapper invoked by Windows Task Scheduler.
func RenderWindowsServiceScript(binaryPath, mode, configDir string, env config.Env) string {
	if mode == "" {
		mode = ModeServe
	}
	logDir := joinOS("windows", configDir, "logs")
	logFile := joinOS("windows", logDir, "panel.log")
	envVars := buildServiceEnv(binaryPath, configDir, "windows", env)
	var envLines []string
	for _, k := range sortedEnvKeys(envVars) {
		envLines = append(envLines, fmt.Sprintf(`set "%s=%s"`, k, escapeCmdValue(envVars[k])))
	}
	lines := []string{
		"@echo off",
		"setlocal",
		fmt.Sprintf(`set "MAESTRIA_LOG_DIR=%s"`, escapeCmdValue(logDir)),
		`if not exist "%MAESTRIA_LOG_DIR%" mkdir "%MAESTRIA_LOG_DIR%"`,
	}
	lines = append(lines, envLines...)
	lines = append(lines,
		fmt.Sprintf(`"%s" %s >> "%s" 2>&1`, escapeCmdValue(binaryPath), strings.Join(ModeArgs(mode), " "), escapeCmdValue(logFile)),
		"",
	)
	return strings.Join(lines, "\r\n")
}

// RenderWindowsTaskXML renders a per-user Windows Task Scheduler XML definition (no admin required).
func RenderWindowsTaskXML(scriptPath, configDir string) string {
	lines := []string{
		`<?xml version="1.0" encoding="UTF-8"?>`,
		`<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">`,
		`  <RegistrationInfo>`,
		`    <Description>Maestria control panel</Description>`,
		`  </RegistrationInfo>`,
		`  <Triggers>`,
		`    <LogonTrigger>`,
		`      <Enabled>true</Enabled>`,
		`    </LogonTrigger>`,
		`  </Triggers>`,
		`  <Principals>`,
		`    <Principal id="Author">`,
		`      <LogonType>InteractiveToken</LogonType>`,
		`      <RunLevel>LeastPrivilege</RunLevel>`,
		`    </Principal>`,
		`  </Principals>`,
		`  <Settings>`,
		`    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>`,
		`    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>`,
		`    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>`,
		`    <AllowHardTerminate>true</AllowHardTerminate>`,
		`    <StartWhenAvailable>true</StartWhenAvailable>`,
		`    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>`,
		`    <AllowStartOnDemand>true</AllowStartOnDemand>`,
		`    <Enabled>true</Enabled>`,
		`    <Hidden>true</Hidden>`,
		`    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>`,
		`    <RestartOnFailure>`,
		`      <Interval>PT1M</Interval>`,
		`      <Count>999</Count>`,
		`    </RestartOnFailure>`,
		`  </Settings>`,
		`  <Actions Context="Author">`,
		`    <Exec>`,
		`      <Command>cmd.exe</Command>`,
		fmt.Sprintf(`      <Arguments>/c "%s"</Arguments>`, xmlEscape(scriptPath)),
		fmt.Sprintf(`      <WorkingDirectory>%s</WorkingDirectory>`, xmlEscape(configDir)),
		`    </Exec>`,
		`  </Actions>`,
		`</Task>`,
		"",
	}
	return strings.Join(lines, "\r\n")
}

// Install writes the platform service unit/plist/task files and registers the service.
func Install(ctx context.Context, opts Options) (InstallResult, error) {
	c, err := resolveContext(opts)
	if err != nil {
		return InstallResult{}, err
	}
	if _, err := os.Stat(c.binaryPath); err != nil {
		return InstallResult{}, serviceErrorf("maestria binary not found at %s: %w", c.binaryPath, err)
	}
	if err := privfile.EnsureDir(c.configDir); err != nil {
		return InstallResult{}, err
	}
	logDir := LogDir(c.configDir)
	if err := privfile.EnsureDir(logDir); err != nil {
		return InstallResult{}, err
	}

	switch c.goos {
	case "darwin":
		plist := LaunchdPlistPath(c.homeDir)
		if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
			return InstallResult{}, err
		}
		body := RenderLaunchdPlist(c.binaryPath, c.mode, c.configDir, c.env)
		if err := os.WriteFile(plist, []byte(body), 0o644); err != nil {
			return InstallResult{}, err
		}
		return InstallResult{UnitPath: plist, LogDir: logDir, Mode: c.mode}, nil

	case "windows":
		script := WindowsServiceScriptPath(c.configDir)
		taskXML := WindowsTaskXMLPath(c.configDir)
		if err := os.WriteFile(script, []byte(RenderWindowsServiceScript(c.binaryPath, c.mode, c.configDir, c.env)), 0o600); err != nil {
			return InstallResult{}, err
		}
		if err := os.WriteFile(taskXML, []byte(RenderWindowsTaskXML(script, c.configDir)), 0o600); err != nil {
			return InstallResult{}, err
		}
		res := c.runner(ctx, []string{"schtasks", "/Create", "/TN", WindowsTaskName, "/XML", taskXML, "/F"})
		if !res.OK {
			return InstallResult{}, serviceErrorf("schtasks /Create failed: %s", firstErr(res.Stderr, "unknown error"))
		}
		return InstallResult{UnitPath: script, LogDir: logDir, Mode: c.mode}, nil

	default:
		unit := SystemdUnitPath(c.homeDir, c.env)
		if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
			return InstallResult{}, err
		}
		body := RenderSystemdUnit(c.binaryPath, c.mode, c.configDir, c.env)
		if err := os.WriteFile(unit, []byte(body), 0o644); err != nil {
			return InstallResult{}, err
		}
		reload := c.runner(ctx, []string{"systemctl", "--user", "daemon-reload"})
		if !reload.OK {
			return InstallResult{}, serviceErrorf("systemctl --user daemon-reload failed: %s", firstErr(reload.Stderr, "unknown error"))
		}
		enable := c.runner(ctx, []string{"systemctl", "--user", "enable", SystemdUnit})
		if !enable.OK {
			return InstallResult{}, serviceErrorf("systemctl --user enable %s failed: %s", SystemdUnit, firstErr(enable.Stderr, "unknown error"))
		}
		return InstallResult{UnitPath: unit, LogDir: logDir, Mode: c.mode}, nil
	}
}

// Start starts the installed background service.
func Start(ctx context.Context, opts Options) error {
	c, err := resolveContext(opts)
	if err != nil {
		return err
	}
	unit := unitFileFromCtx(c)
	if _, err := os.Stat(unit); errors.Is(err, fs.ErrNotExist) {
		return serviceErrorf(`service is not installed. Run "maestria service install" first`)
	}

	switch c.goos {
	case "darwin":
		domain := fmt.Sprintf("gui/%d", c.uid)
		target := domain + "/" + LaunchdLabel
		if probe := c.runner(ctx, []string{"launchctl", "print", target}); probe.OK {
			if kick := c.runner(ctx, []string{"launchctl", "kickstart", "-k", target}); !kick.OK {
				return serviceErrorf("launchctl kickstart failed: %s", firstErr(kick.Stderr, "unknown error"))
			}
			return nil
		}
		if boot := c.runner(ctx, []string{"launchctl", "bootstrap", domain, unit}); !boot.OK {
			return serviceErrorf("launchctl bootstrap failed: %s", firstErr(boot.Stderr, "unknown error"))
		}
		return nil

	case "windows":
		if run := c.runner(ctx, []string{"schtasks", "/Run", "/TN", WindowsTaskName}); !run.OK {
			return serviceErrorf("schtasks /Run failed: %s", firstErr(run.Stderr, "unknown error"))
		}
		return nil

	default:
		if res := c.runner(ctx, []string{"systemctl", "--user", "start", SystemdUnit}); !res.OK {
			return serviceErrorf("systemctl --user start failed: %s", firstErr(res.Stderr, "unknown error"))
		}
		return nil
	}
}

// Stop stops the background service if running.
func Stop(ctx context.Context, opts Options) (bool, error) {
	c, err := resolveContext(opts)
	if err != nil {
		return false, err
	}
	switch c.goos {
	case "darwin":
		target := fmt.Sprintf("gui/%d/%s", c.uid, LaunchdLabel)
		if probe := c.runner(ctx, []string{"launchctl", "print", target}); !probe.OK {
			return false, nil
		}
		bootout := c.runner(ctx, []string{"launchctl", "bootout", target})
		if !bootout.OK {
			if recheck := c.runner(ctx, []string{"launchctl", "print", target}); recheck.OK {
				return false, serviceErrorf("launchctl bootout failed: %s", firstErr(bootout.Stderr, "unknown error"))
			}
		}
		return true, nil

	case "windows":
		probe := c.runner(ctx, []string{"schtasks", "/Query", "/TN", WindowsTaskName, "/FO", "LIST"})
		if !probe.OK || !isWindowsTaskActive(probe.Stdout) {
			return false, nil
		}
		if end := c.runner(ctx, []string{"schtasks", "/End", "/TN", WindowsTaskName}); !end.OK {
			return false, serviceErrorf("schtasks /End failed: %s", firstErr(end.Stderr, "unknown error"))
		}
		return true, nil

	default:
		unit := SystemdUnitPath(c.homeDir, c.env)
		if _, err := os.Stat(unit); errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if active := c.runner(ctx, []string{"systemctl", "--user", "is-active", SystemdUnit}); !active.OK {
			return false, nil
		}
		if res := c.runner(ctx, []string{"systemctl", "--user", "stop", SystemdUnit}); !res.OK {
			return false, serviceErrorf("systemctl --user stop failed: %s", firstErr(res.Stderr, "unknown error"))
		}
		return true, nil
	}
}

// Restart restarts the installed background service.
func Restart(ctx context.Context, opts Options) error {
	c, err := resolveContext(opts)
	if err != nil {
		return err
	}
	unit := unitFileFromCtx(c)
	if _, err := os.Stat(unit); errors.Is(err, fs.ErrNotExist) {
		return serviceErrorf(`service is not installed. Run "maestria service install" first`)
	}

	switch c.goos {
	case "darwin":
		domain := fmt.Sprintf("gui/%d", c.uid)
		target := domain + "/" + LaunchdLabel
		// kickstart -k kills and restarts a loaded job in one step. A bootout followed right away
		// by bootstrap races launchd's teardown and fails with "5: Input/output error".
		if kick := c.runner(ctx, []string{"launchctl", "kickstart", "-k", target}); kick.OK {
			return nil
		}
		// Not loaded (e.g. after a bootout): load it, which starts it (RunAtLoad).
		if boot := c.runner(ctx, []string{"launchctl", "bootstrap", domain, unit}); !boot.OK {
			return serviceErrorf("launchctl bootstrap failed: %s", firstErr(boot.Stderr, "unknown error"))
		}
		return nil

	case "windows":
		_ = c.runner(ctx, []string{"schtasks", "/End", "/TN", WindowsTaskName})
		if run := c.runner(ctx, []string{"schtasks", "/Run", "/TN", WindowsTaskName}); !run.OK {
			return serviceErrorf("schtasks /Run failed: %s", firstErr(run.Stderr, "unknown error"))
		}
		return nil

	default:
		if res := c.runner(ctx, []string{"systemctl", "--user", "restart", SystemdUnit}); !res.OK {
			return serviceErrorf("systemctl --user restart failed: %s", firstErr(res.Stderr, "unknown error"))
		}
		return nil
	}
}

// Uninstall stops, deregisters, and removes the service files.
func Uninstall(ctx context.Context, opts Options) (removed bool, unitPath string, err error) {
	c, err := resolveContext(opts)
	if err != nil {
		return false, "", err
	}
	unit := unitFileFromCtx(c)
	_, statErr := os.Stat(unit)
	existed := statErr == nil

	switch c.goos {
	case "darwin":
		target := fmt.Sprintf("gui/%d/%s", c.uid, LaunchdLabel)
		_ = c.runner(ctx, []string{"launchctl", "bootout", target})
		if err := removeIfExists(unit); err != nil {
			return false, unit, err
		}
		return existed, unit, nil

	case "windows":
		_ = c.runner(ctx, []string{"schtasks", "/End", "/TN", WindowsTaskName})
		probe := c.runner(ctx, []string{"schtasks", "/Query", "/TN", WindowsTaskName, "/FO", "LIST"})
		if probe.OK {
			del := c.runner(ctx, []string{"schtasks", "/Delete", "/TN", WindowsTaskName, "/F"})
			if !del.OK {
				return false, unit, serviceErrorf("schtasks /Delete failed: %s", firstErr(del.Stderr, "unknown error"))
			}
		}
		if err := removeIfExists(unit); err != nil {
			return false, unit, err
		}
		_ = removeIfExists(WindowsTaskXMLPath(c.configDir))
		return existed || probe.OK, unit, nil

	default:
		_ = c.runner(ctx, []string{"systemctl", "--user", "disable", "--now", SystemdUnit})
		if err := removeIfExists(unit); err != nil {
			return false, unit, err
		}
		_ = c.runner(ctx, []string{"systemctl", "--user", "daemon-reload"})
		return existed, unit, nil
	}
}

// Status inspects the service manager and panel reachability.
func Status(ctx context.Context, opts Options) (StatusReport, error) {
	c, err := resolveContext(opts)
	if err != nil {
		return StatusReport{}, err
	}
	unit := unitFileFromCtx(c)
	_, statErr := os.Stat(unit)
	installed := statErr == nil

	cfg, err := config.Load(c.configDir, c.env)
	if err != nil {
		return StatusReport{}, err
	}
	srv := c.reachCheck(config.Host, cfg.Port)
	logDir := LogDir(c.configDir)

	switch c.goos {
	case "darwin":
		target := fmt.Sprintf("gui/%d/%s", c.uid, LaunchdLabel)
		probe := c.runner(ctx, []string{"launchctl", "print", target})
		loaded := probe.OK
		running := loaded && (containsFold(probe.Stdout, "state = running") || hasLaunchdPID(probe.Stdout) || srv.Reachable)
		return StatusReport{
			Platform:  "darwin",
			Manager:   "launchd",
			UnitPath:  unit,
			LogDir:    logDir,
			Installed: installed,
			Loaded:    loaded,
			Running:   running,
			Server:    srv,
		}, nil

	case "windows":
		probe := c.runner(ctx, []string{"schtasks", "/Query", "/TN", WindowsTaskName, "/FO", "LIST"})
		loaded := probe.OK
		running := (loaded && isWindowsTaskRunning(probe.Stdout)) || srv.Reachable
		return StatusReport{
			Platform:  "windows",
			Manager:   "task-scheduler",
			UnitPath:  unit,
			LogDir:    logDir,
			Installed: installed,
			Loaded:    loaded,
			Running:   running,
			Server:    srv,
		}, nil

	default:
		active := c.runner(ctx, []string{"systemctl", "--user", "is-active", SystemdUnit})
		running := active.OK && strings.TrimSpace(active.Stdout) == "active"
		return StatusReport{
			Platform:  "linux",
			Manager:   "systemd-user",
			UnitPath:  unit,
			LogDir:    logDir,
			Installed: installed,
			Loaded:    installed,
			Running:   running || srv.Reachable,
			Server:    srv,
		}, nil
	}
}

// ReadLogs returns the trailing lines of each service log file in configDir/logs.
func ReadLogs(opts Options, lines int) ([]LogFileContent, string, error) {
	c, err := resolveContext(opts)
	if err != nil {
		return nil, "", err
	}
	if lines <= 0 {
		lines = 100
	}
	logDir := LogDir(c.configDir)
	paths := LogFiles(c.configDir, c.goos)
	out := make([]LogFileContent, 0, len(paths))
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			out = append(out, LogFileContent{Path: p, Exists: false})
			continue
		}
		if err != nil {
			return nil, logDir, err
		}
		out = append(out, LogFileContent{
			Path:    p,
			Exists:  true,
			Content: tailLines(string(data), lines),
		})
	}
	return out, logDir, nil
}

func tailLines(s string, maxLines int) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	parts := strings.Split(s, "\n")
	if len(parts) > maxLines {
		parts = parts[len(parts)-maxLines:]
	}
	return strings.Join(parts, "\n") + "\n"
}

func removeIfExists(p string) error {
	err := os.Remove(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func firstErr(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

func hasLaunchdPID(stdout string) bool {
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(strings.ToLower(line))
		if strings.HasPrefix(line, "pid = ") {
			val := strings.TrimSpace(strings.TrimPrefix(line, "pid = "))
			if n, err := strconv.Atoi(val); err == nil && n > 0 {
				return true
			}
		}
	}
	return false
}

func isWindowsTaskActive(stdout string) bool {
	return isWindowsTaskRunning(stdout) || hasStatusValue(stdout, "queued")
}

func isWindowsTaskRunning(stdout string) bool {
	return hasStatusValue(stdout, "running")
}

func hasStatusValue(stdout, want string) bool {
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if idx := strings.IndexByte(line, ':'); idx > 0 {
			key := strings.TrimSpace(line[:idx])
			val := strings.TrimSpace(line[idx+1:])
			if strings.EqualFold(key, "Status") && strings.EqualFold(val, want) {
				return true
			}
		}
	}
	return false
}
