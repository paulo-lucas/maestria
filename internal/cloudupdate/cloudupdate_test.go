package cloudupdate

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

	"github.com/paulo-lucas/maestria/internal/update"
)

func fakeRelease(t *testing.T, content string) *httptest.Server {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "maestria-cloud", Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg})
	tw.Write([]byte(content))
	tw.Close()
	gz.Close()
	archive := buf.Bytes()
	name := update.ArchiveName("maestria-cloud", "v1.2.0", "linux", "amd64")
	sum := sha256.Sum256(archive)
	sums := update.ChecksumLine(hex.EncodeToString(sum[:]), name)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			fmt.Fprintf(w, `{"tag_name":"v1.2.0","assets":[{"name":%q,"size":%d,"browser_download_url":"%s/dl/a"},{"name":"SHA256SUMS","size":%d,"browser_download_url":"%s/dl/s"}]}`,
				name, len(archive), srv.URL, len(sums), srv.URL)
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

type rig struct {
	env      Env
	out, err *bytes.Buffer
	exe      string
	commands []string
	restarts int32
	backups  int32
}

// newRig wires an installed "maestria-cloud", a fake GitHub, a fake systemd and a /healthz that
// answers according to healthy(restarts).
func newRig(t *testing.T, version string, healthy func(restarts int32) bool) *rig {
	t.Helper()
	r := &rig{out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	srv := fakeRelease(t, "NEW")
	r.exe = filepath.Join(t.TempDir(), "maestria-cloud")
	if err := os.WriteFile(r.exe, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/healthz" || !healthy(atomic.LoadInt32(&r.restarts)) {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"status":"ok"}`)
	}))
	t.Cleanup(health.Close)
	backup := filepath.Join(t.TempDir(), "backup")
	os.WriteFile(backup, []byte("#!/bin/sh\n"), 0o755)

	env := map[string]string{
		"MAESTRIA_CLOUD_DIR":    t.TempDir(),
		"MAESTRIA_CLOUD_LISTEN": strings.TrimPrefix(health.URL, "http://"),
	}
	r.env = Env{
		Stdin: strings.NewReader(""), Stdout: r.out, Stderr: r.err, Version: version, GOOS: "linux",
		Getenv: func(k string) string { return env[k] }, Home: t.TempDir(), IsTTY: func() bool { return false },
		BackupScript: backup, HealthTimeout: 300 * time.Millisecond, HealthPoll: 10 * time.Millisecond,
		Runner: func(_ context.Context, argv []string) (string, error) {
			r.commands = append(r.commands, strings.Join(argv, " "))
			switch {
			case len(argv) == 3 && argv[1] == "is-active":
				return "active", nil
			case len(argv) == 3 && argv[1] == "restart":
				atomic.AddInt32(&r.restarts, 1)
			case argv[0] == backup:
				if b, _ := os.ReadFile(r.exe); string(b) != "OLD" {
					t.Errorf("the backup ran after the binary changed (%q)", b)
				}
				atomic.AddInt32(&r.backups, 1)
			}
			return "", nil
		},
		Hook: func(u *update.Updater) {
			u.APIBase, u.Insecure, u.ExePath, u.GOOS, u.GOARCH, u.Verify = srv.URL, true, r.exe, "linux", "amd64", nil
		},
	}
	return r
}

func (r *rig) read(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(r.exe)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCheckDoesNotTouchAnything(t *testing.T) {
	r := newRig(t, "1.0.0", func(int32) bool { return true })
	if code := Run([]string{"--check"}, r.env); code != 0 || !strings.Contains(r.out.String(), "maestria-cloud 1.2.0 is available") {
		t.Fatalf("%d %s %s", code, r.out, r.err)
	}
	if r.read(t) != "OLD" || len(r.commands) != 0 {
		t.Errorf("--check changed something: %q %v", r.read(t), r.commands)
	}
}

func TestNeedsYesWithoutATerminal(t *testing.T) {
	r := newRig(t, "1.0.0", func(int32) bool { return true })
	if code := Run(nil, r.env); code != 2 || !strings.Contains(r.err.String(), "--yes") {
		t.Fatalf("%d %s", code, r.err)
	}
	if r.read(t) != "OLD" || atomic.LoadInt32(&r.restarts) != 0 {
		t.Error("nothing may change without consent")
	}
	if code := Run([]string{"--bogus"}, r.env); code != 2 {
		t.Errorf("bad flag: %d", code)
	}
}

func TestUpdateBacksUpReplacesRestartsAndChecksHealth(t *testing.T) {
	r := newRig(t, "1.0.0", func(int32) bool { return true })
	if code := Run([]string{"--yes"}, r.env); code != 0 {
		t.Fatalf("%d %s %s", code, r.out, r.err)
	}
	if r.read(t) != "NEW" || atomic.LoadInt32(&r.restarts) != 1 || atomic.LoadInt32(&r.backups) != 1 {
		t.Errorf("binary=%q restarts=%d backups=%d", r.read(t), r.restarts, r.backups)
	}
	if !strings.Contains(r.out.String(), "(restarted)") {
		t.Errorf("output: %s", r.out)
	}
}

func TestRollsBackWhenHealthzNeverAnswers(t *testing.T) {
	// Healthy before the update and after the rollback's restart, down in between.
	r := newRig(t, "1.0.0", func(n int32) bool { return n != 1 })
	code := Run([]string{"--yes"}, r.env)
	if code != 1 || !strings.Contains(r.err.String(), "previous version was restored") {
		t.Fatalf("%d out=%s err=%s", code, r.out, r.err)
	}
	if r.read(t) != "OLD" || atomic.LoadInt32(&r.restarts) != 2 {
		t.Errorf("binary=%q restarts=%d", r.read(t), r.restarts)
	}
}

func TestDevBuildAndUpToDateAreNoOps(t *testing.T) {
	for _, v := range []string{"2.0.0-dev+abc1234", "1.2.0", "1.3.0"} {
		r := newRig(t, v, func(int32) bool { return true })
		if code := Run([]string{"--yes"}, r.env); code != 0 || r.read(t) != "OLD" || atomic.LoadInt32(&r.restarts) != 0 {
			t.Errorf("%s: %d binary=%q restarts=%d", v, code, r.read(t), r.restarts)
		}
	}
}

func TestWithoutSystemdOnlyTheBinaryChanges(t *testing.T) {
	r := newRig(t, "1.0.0", func(int32) bool { return false })
	r.env.GOOS = "darwin"
	if code := Run([]string{"--yes"}, r.env); code != 0 || r.read(t) != "NEW" || len(r.commands) != 0 {
		t.Fatalf("%d binary=%q commands=%v %s", code, r.read(t), r.commands, r.err)
	}
}

func TestStoppedServiceIsNotStarted(t *testing.T) {
	r := newRig(t, "1.0.0", func(int32) bool { return false })
	inner := r.env.Runner
	r.env.Runner = func(ctx context.Context, argv []string) (string, error) {
		if len(argv) == 3 && argv[1] == "is-active" {
			r.commands = append(r.commands, strings.Join(argv, " "))
			return "inactive", fmt.Errorf("exit status 3")
		}
		return inner(ctx, argv)
	}
	if code := Run([]string{"--yes"}, r.env); code != 0 || r.read(t) != "NEW" || atomic.LoadInt32(&r.restarts) != 0 {
		t.Fatalf("%d binary=%q restarts=%d", code, r.read(t), r.restarts)
	}
}

func TestFailedBackupStopsTheUpdate(t *testing.T) {
	r := newRig(t, "1.0.0", func(int32) bool { return true })
	inner := r.env.Runner
	r.env.Runner = func(ctx context.Context, argv []string) (string, error) {
		if argv[0] == r.env.BackupScript {
			return "disk full", fmt.Errorf("exit status 1")
		}
		return inner(ctx, argv)
	}
	if code := Run([]string{"--yes"}, r.env); code != 1 || r.read(t) != "OLD" || !strings.Contains(r.err.String(), "backup") {
		t.Fatalf("%d binary=%q %s", code, r.read(t), r.err)
	}
}
