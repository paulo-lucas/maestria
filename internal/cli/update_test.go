package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paulo-lucas/maestria/internal/service"
	"github.com/paulo-lucas/maestria/internal/update"
)

// fakeRelease serves one release (v1.2.0) of "maestria" for linux/amd64 like GitHub would.
func fakeRelease(t *testing.T, content string) *httptest.Server {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "maestria", Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg})
	tw.Write([]byte(content))
	tw.Close()
	gz.Close()
	archive := buf.Bytes()
	name := update.ArchiveName("maestria", "v1.2.0", "linux", "amd64")
	sum := sha256.Sum256(archive)
	sums := update.ChecksumLine(hex.EncodeToString(sum[:]), name)

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			fmt.Fprintf(w, `{"tag_name":"v1.2.0","html_url":"https://github.com/x/y/releases/v1.2.0","assets":[
			  {"name":%q,"size":%d,"browser_download_url":"%s/dl/a"},
			  {"name":"SHA256SUMS","size":%d,"browser_download_url":"%s/dl/s"}]}`, name, len(archive), srv.URL, len(sums), srv.URL)
		case r.URL.Path == "/dl/a":
			w.Write(archive)
		case r.URL.Path == "/dl/s":
			fmt.Fprint(w, sums)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func updateEnv(t *testing.T, f *fixture, srv *httptest.Server, version string) (Env, string) {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "maestria")
	if err := os.WriteFile(exe, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := f.env("")
	env.Version = version
	env.UpdateHook = func(u *update.Updater) {
		u.APIBase, u.Insecure, u.ExePath, u.GOOS, u.GOARCH, u.Verify = srv.URL, true, exe, "linux", "amd64", nil
		u.HealthTimeout, u.HealthPoll = 200*time.Millisecond, 10*time.Millisecond
	}
	return env, exe
}

func TestUpdateCheckAndConsent(t *testing.T) {
	f := newFixture(t)
	srv := fakeRelease(t, "NEW")
	env, exe := updateEnv(t, f, srv, "1.0.0")

	if code := f.run(env, "update", "--check"); code != 0 || !strings.Contains(f.out.String(), "maestria 1.2.0 is available (you have 1.0.0)") {
		t.Fatalf("check: %d %s %s", code, f.out.String(), f.errOut.String())
	}
	// Not on a terminal, no --yes: refused (usage error), nothing installed.
	if code := f.run(env, "update"); code != 2 || !strings.Contains(f.errOut.String(), "--yes") {
		t.Fatalf("no tty: %d %s", code, f.errOut.String())
	}
	if b, _ := os.ReadFile(exe); string(b) != "OLD" {
		t.Fatalf("binary changed without consent: %q", b)
	}
	if code := f.run(env, "update", "--channel", "nightly"); code != 2 {
		t.Fatalf("bad channel: %d", code)
	}
	if code := f.run(env, "update", "--yes"); code != 0 || !strings.Contains(f.out.String(), "maestria is now 1.2.0") {
		t.Fatalf("update: %d %s %s", code, f.out.String(), f.errOut.String())
	}
	if b, _ := os.ReadFile(exe); string(b) != "NEW" {
		t.Fatalf("binary = %q", b)
	}
	if code := f.run(env, "update", "--yes"); code != 0 {
		t.Fatalf("second update: %d", code)
	}
}

func TestUpdateDevBuildDoesNothing(t *testing.T) {
	f := newFixture(t)
	srv := fakeRelease(t, "NEW")
	env, exe := updateEnv(t, f, srv, "2.0.0-dev+abc1234")
	if code := f.run(env, "update", "--yes"); code != 0 || !strings.Contains(f.out.String(), "development build") {
		t.Fatalf("%d %s", code, f.out.String())
	}
	if b, _ := os.ReadFile(exe); string(b) != "OLD" {
		t.Fatalf("a development build was replaced: %q", b)
	}
}

// installedService installs the unit and fakes the service manager. healthy decides whether the
// panel answers after each restart.
func installedService(t *testing.T, f *fixture, env *Env, healthy func(restarts int32) bool) *int32 {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "maestria")
	if err := os.WriteFile(bin, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	var restarts int32
	env.ServiceBinaryPath = bin
	env.ServiceRunner = func(_ context.Context, argv []string) service.CmdResult {
		for _, a := range argv {
			switch a {
			case "is-active":
				return service.CmdResult{OK: true, Stdout: "active"}
			case "restart":
				atomic.AddInt32(&restarts, 1)
			}
		}
		return service.CmdResult{OK: true}
	}
	env.ServiceReachability = func(string, int) service.Reachability {
		return service.Reachability{Reachable: healthy(atomic.LoadInt32(&restarts)), URL: "http://127.0.0.1:4777"}
	}
	if code := f.run(*env, "service", "install", "--mode", "serve"); code != 0 {
		t.Fatalf("service install: %d %s", code, f.errOut.String())
	}
	return &restarts
}

func TestUpdateRestartsTheServiceAndWaitsForTheStatus(t *testing.T) {
	f := newFixture(t)
	srv := fakeRelease(t, "NEW")
	env, exe := updateEnv(t, f, srv, "1.0.0")
	restarts := installedService(t, f, &env, func(int32) bool { return true })

	if code := f.run(env, "update", "--yes"); code != 0 || !strings.Contains(f.out.String(), "(restarted)") {
		t.Fatalf("%d %s %s", code, f.out.String(), f.errOut.String())
	}
	if atomic.LoadInt32(restarts) != 1 {
		t.Errorf("restarts = %d", *restarts)
	}
	if b, _ := os.ReadFile(exe); string(b) != "NEW" {
		t.Fatalf("binary = %q", b)
	}
}

func TestUpdateRollsBackWhenThePanelDoesNotComeBack(t *testing.T) {
	f := newFixture(t)
	srv := fakeRelease(t, "NEW")
	env, exe := updateEnv(t, f, srv, "1.0.0")
	// The panel answers before the update and after the rollback's restart, never in between.
	restarts := installedService(t, f, &env, func(n int32) bool { return n != 1 })
	atomic.StoreInt32(restarts, 0)

	code := f.run(env, "update", "--yes")
	if code != 1 || !strings.Contains(f.errOut.String(), "previous version was restored") {
		t.Fatalf("%d out=%s err=%s", code, f.out.String(), f.errOut.String())
	}
	if b, _ := os.ReadFile(exe); string(b) != "OLD" {
		t.Fatalf("binary after rollback = %q", b)
	}
	if atomic.LoadInt32(restarts) != 2 {
		t.Errorf("restarts = %d, want 2 (new, then the restored one)", *restarts)
	}
}
