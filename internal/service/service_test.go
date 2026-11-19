package service

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/paulo-lucas/maestria/internal/config"
)

func testEnv(goos string, vars map[string]string) config.Env {
	return config.Env{
		GOOS: goos,
		Getenv: func(k string) string {
			if vars == nil {
				return ""
			}
			return vars[k]
		},
	}
}

func TestRenderLaunchdPlist(t *testing.T) {
	plist := RenderLaunchdPlist(
		"/usr/local/bin/maestria & panel",
		ModeServe,
		"/tmp/cfg<1>",
		testEnv("darwin", map[string]string{
			"PATH":         "/custom/bin:/usr/bin",
			"MAESTRI_HOME": "/Users/alice/.maestri",
		}),
	)

	for _, want := range []string{
		"<string>" + LaunchdLabel + "</string>",
		"<string>/usr/local/bin/maestria &amp; panel</string>",
		"<string>serve</string>",
		"<key>RunAtLoad</key>\n  <true/>",
		"<key>KeepAlive</key>\n  <true/>",
		"<string>/tmp/cfg&lt;1&gt;/logs/panel.out.log</string>",
		"<string>/tmp/cfg&lt;1&gt;/logs/panel.err.log</string>",
		"<key>MAESTRIA_CONFIG_DIR</key>\n    <string>/tmp/cfg&lt;1&gt;</string>",
		"<key>MAESTRI_HOME</key>\n    <string>/Users/alice/.maestri</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist missing %q\n%s", want, plist)
		}
	}
	if err := xml.Unmarshal([]byte(plist), new(any)); err != nil {
		t.Fatalf("plist is not valid XML: %v", err)
	}

	connectPlist := RenderLaunchdPlist("/usr/local/bin/maestria", ModeConnect, "/tmp/cfg", testEnv("darwin", nil))
	if !strings.Contains(connectPlist, "<string>connect</string>") {
		t.Errorf("connect plist missing connect arg:\n%s", connectPlist)
	}
}

func TestRenderSystemdUnit(t *testing.T) {
	unit := RenderSystemdUnit(
		"/opt/maestria/bin/maestria",
		ModeServe,
		"/home/alice/.config/maestria",
		testEnv("linux", map[string]string{"PATH": "/usr/local/bin:/usr/bin"}),
	)
	for _, want := range []string{
		"[Unit]",
		"[Service]",
		`ExecStart="/opt/maestria/bin/maestria" serve`,
		`Environment="MAESTRIA_CONFIG_DIR=/home/alice/.config/maestria"`,
		"StandardOutput=append:/home/alice/.config/maestria/logs/panel.out.log",
		"StandardError=append:/home/alice/.config/maestria/logs/panel.err.log",
		"Restart=always",
		"[Install]\nWantedBy=default.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit missing %q\n%s", want, unit)
		}
	}

	connectUnit := RenderSystemdUnit("/usr/bin/maestria", ModeConnect, "/home/alice/.config/maestria", testEnv("linux", nil))
	if !strings.Contains(connectUnit, `ExecStart="/usr/bin/maestria" connect`) {
		t.Errorf("connect unit missing connect arg:\n%s", connectUnit)
	}
}

func TestRenderWindowsServiceScriptAndTaskXML(t *testing.T) {
	script := RenderWindowsServiceScript(
		`C:\Program Files\maestria\maestria.exe`,
		ModeServe,
		`C:\Users\alice\AppData\Roaming\maestria`,
		testEnv("windows", map[string]string{
			"PATH":         `C:\Windows\System32`,
			"MAESTRI_HOME": `C:\Users\alice\.maestri`,
		}),
	)
	for _, want := range []string{
		`set "MAESTRIA_CONFIG_DIR=C:\Users\alice\AppData\Roaming\maestria"`,
		`set "MAESTRI_HOME=C:\Users\alice\.maestri"`,
		`"C:\Program Files\maestria\maestria.exe" serve >> "C:\Users\alice\AppData\Roaming\maestria\logs\panel.log" 2>&1`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("windows script missing %q\n%s", want, script)
		}
	}

	taskXML := RenderWindowsTaskXML(
		`C:\Users\alice\AppData\Roaming\maestria\maestria-service.cmd`,
		`C:\Users\alice\AppData\Roaming\maestria`,
		`DESKTOP-1\a&b`,
	)
	for _, want := range []string{
		`<?xml version="1.0" encoding="UTF-16"?>`,
		"<LogonTrigger>\r\n      <Enabled>true</Enabled>\r\n      <UserId>DESKTOP-1\\a&amp;b</UserId>\r\n    </LogonTrigger>",
		"<Principal id=\"Author\">\r\n      <UserId>DESKTOP-1\\a&amp;b</UserId>",
		`<RunLevel>LeastPrivilege</RunLevel>`,
		`<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>`,
		`<Command>cmd.exe</Command>`,
		`<Arguments>/c "C:\Users\alice\AppData\Roaming\maestria\maestria-service.cmd"</Arguments>`,
		`<WorkingDirectory>C:\Users\alice\AppData\Roaming\maestria</WorkingDirectory>`,
	} {
		if !strings.Contains(taskXML, want) {
			t.Errorf("task XML missing %q\n%s", want, taskXML)
		}
	}
	if err := parseUTF16XML(EncodeWindowsTaskXML(taskXML)); err != nil {
		t.Fatalf("task XML is not valid XML: %v", err)
	}
}

// parseUTF16XML checks that b is UTF-16 LE with a byte order mark, as schtasks needs, and
// that it parses as XML.
func parseUTF16XML(b []byte) error {
	if len(b) < 2 || b[0] != 0xFF || b[1] != 0xFE || len(b)%2 != 0 {
		return fmt.Errorf("not UTF-16 LE with a BOM: % x", b[:min(len(b), 4)])
	}
	units := make([]uint16, 0, len(b)/2-1)
	for i := 2; i < len(b); i += 2 {
		units = append(units, uint16(b[i])|uint16(b[i+1])<<8)
	}
	dec := xml.NewDecoder(strings.NewReader(string(utf16.Decode(units))))
	dec.CharsetReader = func(label string, r io.Reader) (io.Reader, error) {
		if !strings.EqualFold(label, "UTF-16") {
			return nil, fmt.Errorf("declared encoding %q, want UTF-16", label)
		}
		return r, nil // already decoded above
	}
	return dec.Decode(new(any))
}

func TestEncodeWindowsTaskXML(t *testing.T) {
	got := EncodeWindowsTaskXML("<é\U0001F600")
	want := []byte{0xFF, 0xFE, '<', 0x00, 0xE9, 0x00, 0x3D, 0xD8, 0x00, 0xDE}
	if !bytes.Equal(got, want) {
		t.Fatalf("EncodeWindowsTaskXML = % x, want % x", got, want)
	}
}

func TestLifecycleDarwin(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	cfgDir := filepath.Join(tmp, "cfg")
	binPath := filepath.Join(tmp, "maestria")
	if err := os.WriteFile(binPath, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}

	loaded := false
	var calls [][]string
	opts := Options{
		ConfigDir:  cfgDir,
		HomeDir:    home,
		Env:        testEnv("darwin", nil),
		BinaryPath: binPath,
		UID:        501,
		Runner: func(_ context.Context, argv []string) CmdResult {
			calls = append(calls, append([]string(nil), argv...))
			switch argv[1] {
			case "print":
				if loaded {
					return CmdResult{OK: true, Stdout: "state = running\npid = 4242"}
				}
				return CmdResult{OK: false, Code: 113, Stderr: "Could not find service"}
			case "bootstrap", "kickstart":
				loaded = true
				return CmdResult{OK: true}
			case "bootout":
				loaded = false
				return CmdResult{OK: true}
			}
			return CmdResult{OK: true}
		},
		ReachabilityCheck: func(host string, port int) Reachability {
			return Reachability{Reachable: loaded, URL: "http://" + host + ":" + strconv.Itoa(port)}
		},
	}

	ctx := context.Background()
	res, err := Install(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !IsInstalled(opts) {
		t.Fatal("expected installed")
	}
	if _, err := os.Stat(res.UnitPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(res.LogDir); err != nil {
		t.Fatal(err)
	}

	if err := Start(ctx, opts); err != nil {
		t.Fatal(err)
	}
	st, err := Status(ctx, opts)
	if err != nil || !st.Installed || !st.Loaded || !st.Running || !st.Server.Reachable {
		t.Fatalf("unexpected status: %+v err=%v", st, err)
	}
	before := len(calls)
	if err := Restart(ctx, opts); err != nil {
		t.Fatal(err)
	}
	// A loaded job restarts with one atomic kickstart -k, never bootout followed by bootstrap
	// (that races launchd's teardown: "Bootstrap failed: 5: Input/output error").
	restartCalls := calls[before:]
	if len(restartCalls) != 1 || strings.Join(restartCalls[0], " ") != "launchctl kickstart -k gui/501/"+LaunchdLabel {
		t.Fatalf("restart calls = %v", restartCalls)
	}
	stopped, err := Stop(ctx, opts)
	if err != nil || !stopped {
		t.Fatalf("stop: %v %v", stopped, err)
	}
	// Restarting a job that is not loaded falls back to bootstrap.
	before = len(calls)
	failKick := opts
	failKick.Runner = func(c context.Context, argv []string) CmdResult {
		if argv[1] == "kickstart" {
			calls = append(calls, append([]string(nil), argv...))
			return CmdResult{OK: false, Code: 113, Stderr: "Could not find service"}
		}
		return opts.Runner(c, argv)
	}
	if err := Restart(ctx, failKick); err != nil {
		t.Fatal(err)
	}
	if got := calls[before:]; len(got) != 2 || got[1][1] != "bootstrap" {
		t.Fatalf("fallback calls = %v", got)
	}
	if _, err := Stop(ctx, opts); err != nil {
		t.Fatal(err)
	}

	// Write a fake log line and verify ReadLogs returns it.
	if err := os.WriteFile(filepath.Join(res.LogDir, "panel.out.log"), []byte("line1\nline2\nline3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logs, logDir, err := ReadLogs(opts, 2)
	if err != nil || logDir != res.LogDir {
		t.Fatalf("ReadLogs: %v %q", err, logDir)
	}
	if len(logs) != 2 || !logs[0].Exists || logs[0].Content != "line2\nline3\n" {
		t.Fatalf("unexpected logs: %+v", logs)
	}

	removed, _, err := Uninstall(ctx, opts)
	if err != nil || !removed {
		t.Fatalf("uninstall: %v %v", removed, err)
	}
	if IsInstalled(opts) {
		t.Fatal("expected uninstalled")
	}
	if len(calls) == 0 {
		t.Fatal("expected launchctl calls")
	}
}

func TestLifecycleLinuxAndUnsupported(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	xdg := filepath.Join(tmp, "xdg")
	cfgDir := filepath.Join(tmp, "cfg")
	binPath := filepath.Join(tmp, "maestria")
	if err := os.WriteFile(binPath, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}

	active := false
	opts := Options{
		ConfigDir:  cfgDir,
		HomeDir:    home,
		Env:        testEnv("linux", map[string]string{"XDG_CONFIG_HOME": xdg}),
		BinaryPath: binPath,
		Mode:       ModeConnect,
		Runner: func(_ context.Context, argv []string) CmdResult {
			for _, a := range argv {
				if a == "is-active" {
					if active {
						return CmdResult{OK: true, Stdout: "active"}
					}
					return CmdResult{OK: false, Code: 3, Stdout: "inactive"}
				}
				if a == "start" || a == "restart" {
					active = true
				}
				if a == "stop" || a == "--now" {
					active = false
				}
			}
			return CmdResult{OK: true}
		},
		ReachabilityCheck: func(host string, port int) Reachability {
			return Reachability{Reachable: false, URL: "http://" + host + ":" + strconv.Itoa(port)}
		},
	}

	ctx := context.Background()
	res, err := Install(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	unitBytes, err := os.ReadFile(res.UnitPath)
	if err != nil || !strings.Contains(string(unitBytes), "connect") {
		t.Fatalf("unit bytes err=%v content=%s", err, unitBytes)
	}
	if err := Start(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if st, err := Status(ctx, opts); err != nil || !st.Running || st.Manager != "systemd-user" {
		t.Fatalf("status: %+v err=%v", st, err)
	}
	if err := Restart(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if stopped, err := Stop(ctx, opts); err != nil || !stopped {
		t.Fatalf("stop: %v %v", stopped, err)
	}
	if removed, _, err := Uninstall(ctx, opts); err != nil || !removed {
		t.Fatalf("uninstall: %v %v", removed, err)
	}

	if _, err := Install(ctx, Options{
		ConfigDir:  cfgDir,
		HomeDir:    home,
		Env:        testEnv("freebsd", nil),
		BinaryPath: binPath,
	}); err == nil {
		t.Fatal("expected unsupported OS error")
	}
}

func TestLifecycleWindows(t *testing.T) {
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "cfg")
	binPath := filepath.Join(tmp, "maestria.exe")
	if err := os.WriteFile(binPath, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}

	taskExists := false
	taskRunning := false
	opts := Options{
		ConfigDir:  cfgDir,
		HomeDir:    tmp,
		Env:        testEnv("windows", map[string]string{"USERDOMAIN": "DESKTOP-1", "USERNAME": "alice"}),
		BinaryPath: binPath,
		Runner: func(_ context.Context, argv []string) CmdResult {
			switch argv[1] {
			case "/Create":
				// Read what schtasks reads, and refuse it the way schtasks does.
				b, err := os.ReadFile(argv[5])
				if err != nil {
					return CmdResult{OK: false, Code: 1, Stderr: err.Error()}
				}
				if err := parseUTF16XML(b); err != nil {
					return CmdResult{OK: false, Code: 1, Stderr: "ERROR: The task XML is malformed. " + err.Error()}
				}
				if strings.Count(string(utf16Text(b)), "<UserId>") != 2 {
					return CmdResult{OK: false, Code: 1, Stderr: "ERROR: Access is denied."}
				}
				taskExists = true
				return CmdResult{OK: true}
			case "/Run":
				taskRunning = true
				return CmdResult{OK: true}
			case "/End":
				taskRunning = false
				return CmdResult{OK: true}
			case "/Delete":
				taskExists = false
				taskRunning = false
				return CmdResult{OK: true}
			case "/Query":
				if !taskExists {
					return CmdResult{OK: false, Code: 1, Stderr: "ERROR: cannot find the file"}
				}
				state := "Ready"
				if taskRunning {
					state = "Running"
				}
				return CmdResult{OK: true, Stdout: "TaskName: \\" + WindowsTaskName + "\nStatus: " + state + "\n"}
			}
			return CmdResult{OK: false}
		},
		ReachabilityCheck: func(host string, port int) Reachability {
			return Reachability{Reachable: taskRunning, URL: "http://" + host + ":" + strconv.Itoa(port)}
		},
	}

	ctx := context.Background()
	res, err := Install(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(WindowsTaskXMLPath(cfgDir)); err != nil {
		t.Fatalf("missing task xml: %v", err)
	}
	if err := Start(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if st, err := Status(ctx, opts); err != nil || !st.Running || st.Manager != "task-scheduler" {
		t.Fatalf("status: %+v err=%v", st, err)
	}
	if err := Restart(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if stopped, err := Stop(ctx, opts); err != nil || !stopped {
		t.Fatalf("stop: %v %v", stopped, err)
	}
	if err := os.WriteFile(filepath.Join(res.LogDir, "panel.log"), []byte("win-log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logs, _, err := ReadLogs(opts, 10)
	if err != nil || len(logs) != 1 || logs[0].Content != "win-log\n" {
		t.Fatalf("windows logs: %+v err=%v", logs, err)
	}
	if removed, _, err := Uninstall(ctx, opts); err != nil || !removed {
		t.Fatalf("uninstall: %v %v", removed, err)
	}
	if _, err := os.Stat(WindowsTaskXMLPath(cfgDir)); !os.IsNotExist(err) {
		t.Fatalf("task xml should be removed, err=%v", err)
	}
}

func utf16Text(b []byte) string {
	units := make([]uint16, 0, len(b)/2)
	for i := 2; i+1 < len(b); i += 2 {
		units = append(units, uint16(b[i])|uint16(b[i+1])<<8)
	}
	return string(utf16.Decode(units))
}

// The wrapper script without its scheduled task (an install that schtasks refused) is not an
// installed service, even while a "maestria serve" started by hand answers on the port.
func TestWindowsWithoutTheTask(t *testing.T) {
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "cfg")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(WindowsServiceScriptPath(cfgDir), []byte("@echo off\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var ran []string
	opts := Options{
		ConfigDir:  cfgDir,
		HomeDir:    tmp,
		Env:        testEnv("windows", map[string]string{"USERNAME": "alice"}),
		BinaryPath: filepath.Join(tmp, "maestria.exe"),
		Runner: func(_ context.Context, argv []string) CmdResult {
			ran = append(ran, argv[1])
			return CmdResult{OK: false, Code: 1, Stderr: "ERROR: The system cannot find the file specified."}
		},
		ReachabilityCheck: func(host string, port int) Reachability {
			return Reachability{Reachable: true, URL: "http://" + host + ":" + strconv.Itoa(port)}
		},
	}
	ctx := context.Background()
	if IsInstalled(opts) {
		t.Error("IsInstalled = true without the task")
	}
	st, err := Status(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if st.Installed || st.Loaded || st.Running || !st.Server.Reachable {
		t.Errorf("status without the task: %+v", st)
	}
	for name, run := range map[string]func() error{
		"start":   func() error { return Start(ctx, opts) },
		"restart": func() error { return Restart(ctx, opts) },
	} {
		ran = nil
		err := run()
		if err == nil || !strings.Contains(err.Error(), `the MaestriaPanel task does not exist`) || !strings.Contains(err.Error(), "maestria service install") {
			t.Errorf("%s: err = %v", name, err)
		}
		if len(ran) != 1 || ran[0] != "/Query" {
			t.Errorf("%s ran %v, want only the query", name, ran)
		}
	}
}

func TestWindowsUserID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("on Windows the user comes from the account, not the environment")
	}
	for _, tc := range []struct {
		vars map[string]string
		want string
	}{
		{map[string]string{"USERDOMAIN": "DESKTOP-1", "USERNAME": "alice"}, `DESKTOP-1\alice`},
		{map[string]string{"USERNAME": "alice"}, "alice"},
		{nil, ""},
	} {
		if got := windowsUserID(testEnv("windows", tc.vars)); got != tc.want {
			t.Errorf("windowsUserID(%v) = %q, want %q", tc.vars, got, tc.want)
		}
	}
}

func TestServiceCurrentOS(t *testing.T) {
	if !IsSupported(runtime.GOOS) {
		t.Skipf("unsupported host OS %s", runtime.GOOS)
	}
	bin, err := DefaultBinaryPath()
	if err != nil || !filepath.IsAbs(bin) {
		t.Fatalf("DefaultBinaryPath() = %q, err=%v", bin, err)
	}

	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	cfgDir := filepath.Join(tmp, "cfg")
	opts := Options{
		ConfigDir: cfgDir,
		HomeDir:   home,
		Env:       config.OSEnv(),
		Runner: func(_ context.Context, _ []string) CmdResult {
			return CmdResult{OK: true, Stdout: "Status: Running\nstate = running\nactive"}
		},
		ReachabilityCheck: func(host string, port int) Reachability {
			return Reachability{Reachable: false, URL: "http://" + host + ":" + strconv.Itoa(port)}
		},
	}

	ctx := context.Background()
	res, err := Install(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(res.UnitPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), bin) {
		t.Errorf("installed unit does not contain running binary %q:\n%s", bin, data)
	}
	if _, err := Status(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if removed, _, err := Uninstall(ctx, opts); err != nil || !removed {
		t.Fatalf("uninstall: %v %v", removed, err)
	}
}

func TestCheckReachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/login" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	got := CheckReachable("0.0.0.0", port, time.Second)
	if !got.Reachable || got.StatusCode != 200 {
		t.Fatalf("expected reachable 200, got %+v", got)
	}
	srv.Close()
	down := CheckReachable("127.0.0.1", port, 200*time.Millisecond)
	if down.Reachable {
		t.Fatalf("expected unreachable after close, got %+v", down)
	}
}
