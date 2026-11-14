package service

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestValidateModeAcceptsBoth(t *testing.T) {
	for in, want := range map[string]string{"": ModeServe, "serve": ModeServe, "CONNECT": ModeConnect, " both ": ModeBoth} {
		got, err := ValidateMode(in)
		if err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	if _, err := ValidateMode("everything"); err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("an unknown mode must be refused and name the choices: %v", err)
	}
}

func TestModeArgs(t *testing.T) {
	join := func(m string) string { return strings.Join(ModeArgs(m), " ") }
	if join(ModeServe) != "serve" || join(ModeConnect) != "connect" || join(ModeBoth) != "serve --connect" || join("") != "serve" {
		t.Fatalf("%q %q %q", join(ModeServe), join(ModeConnect), join(ModeBoth))
	}
}

// "both" runs `maestria serve --connect` in every service manager, as separate arguments where
// the manager needs them.
func TestBothModeInEveryServiceManager(t *testing.T) {
	plist := RenderLaunchdPlist("/usr/local/bin/maestria", ModeBoth, "/tmp/cfg", testEnv("darwin", nil))
	if !strings.Contains(plist, "<string>serve</string>\n    <string>--connect</string>") {
		t.Errorf("launchd arguments must be separate strings:\n%s", plist)
	}
	if err := xml.Unmarshal([]byte(plist), new(any)); err != nil {
		t.Fatalf("plist is not valid XML: %v", err)
	}
	unit := RenderSystemdUnit("/usr/bin/maestria", ModeBoth, "/home/alice/.config/maestria", testEnv("linux", nil))
	if !strings.Contains(unit, `ExecStart="/usr/bin/maestria" serve --connect`) {
		t.Errorf("systemd unit:\n%s", unit)
	}
	script := RenderWindowsServiceScript(`C:\maestria\maestria.exe`, ModeBoth, `C:\cfg`, testEnv("windows", nil))
	if !strings.Contains(script, `maestria.exe" serve --connect >>`) {
		t.Errorf("windows script:\n%s", script)
	}
}
