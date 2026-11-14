package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maestria-dev/maestria/internal/service"
)

func TestServiceCLI(t *testing.T) {
	f := newFixture(t)
	bin := filepath.Join(t.TempDir(), "maestria")
	if err := os.WriteFile(bin, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}

	active := false
	env := f.env("")
	env.ServiceBinaryPath = bin
	env.ServiceRunner = func(_ context.Context, argv []string) service.CmdResult {
		for _, a := range argv {
			switch a {
			case "is-active":
				if active {
					return service.CmdResult{OK: true, Stdout: "active"}
				}
				return service.CmdResult{OK: false, Code: 3}
			case "start", "restart":
				active = true
			case "stop":
				active = false
			}
		}
		return service.CmdResult{OK: true}
	}
	env.ServiceReachability = func(host string, port int) service.Reachability {
		return service.Reachability{Reachable: active, URL: "http://127.0.0.1:4777"}
	}

	if code := f.run(env, "service", "install", "--mode", "serve"); code != 0 {
		t.Fatalf("install: %d %s %s", code, f.out.String(), f.errOut.String())
	}
	if !strings.Contains(f.out.String(), "Installed Maestria service (serve)") {
		t.Fatalf("install output: %s", f.out.String())
	}

	if code := f.run(env, "service", "start"); code != 0 {
		t.Fatalf("start: %d %s", code, f.errOut.String())
	}
	if code := f.run(env, "service", "status", "--json"); code != 0 {
		t.Fatalf("status: %d %s", code, f.errOut.String())
	}
	var report service.StatusReport
	if err := json.Unmarshal(f.out.Bytes(), &report); err != nil || !report.Installed || !report.Running {
		t.Fatalf("status json: %+v err=%v", report, err)
	}

	if code := f.run(env, "service", "logs"); code != 0 || !strings.Contains(f.out.String(), "No service logs found") {
		t.Fatalf("logs before write: %d %s", code, f.out.String())
	}
	if err := os.WriteFile(filepath.Join(report.LogDir, "panel.out.log"), []byte("hello\nworld\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := f.run(env, "service", "logs", "-n", "1"); code != 0 || !strings.Contains(f.out.String(), "world") || strings.Contains(f.out.String(), "hello") {
		t.Fatalf("logs -n 1: %d %s", code, f.out.String())
	}

	if code := f.run(env, "service", "restart"); code != 0 {
		t.Fatalf("restart: %d %s", code, f.errOut.String())
	}
	if code := f.run(env, "service", "stop"); code != 0 {
		t.Fatalf("stop: %d %s", code, f.errOut.String())
	}
	if code := f.run(env, "service", "uninstall"); code != 0 {
		t.Fatalf("uninstall: %d %s", code, f.errOut.String())
	}
	if code := f.run(env, "service", "install", "--mode", "bogus"); code != 2 {
		t.Fatalf("expected usage error 2 for bogus mode, got %d", code)
	}
}
