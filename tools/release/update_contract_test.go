package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/paulo-lucas/maestria/internal/update"
)

// What this tool publishes must be exactly what `maestria update` installs: build a real release
// for this machine, serve it like GitHub would, and update a stand-in binary from it.
func TestPublishedReleaseInstallsThroughTheUpdater(t *testing.T) {
	outDir := t.TempDir()
	err := runRelease(releaseOpts{
		OutDir: outDir, RepoRoot: repoRoot(t), Version: "v7.8.9", Commit: "deadbeef", Date: "2026-11-14T12:00:00Z",
		Targets: []target{{OS: runtime.GOOS, Arch: runtime.GOARCH}}, Binaries: []string{update.BinaryMaestria},
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("runRelease: %v", err)
	}

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/dl/") {
			http.ServeFile(w, r, filepath.Join(outDir, filepath.Base(r.URL.Path)))
			return
		}
		var assets []string
		entries, _ := os.ReadDir(outDir)
		for _, e := range entries {
			info, _ := e.Info()
			assets = append(assets, fmt.Sprintf(`{"name":%q,"size":%d,"browser_download_url":%q}`, e.Name(), info.Size(), srv.URL+"/dl/"+e.Name()))
		}
		fmt.Fprintf(w, `{"tag_name":"v7.8.9","assets":[%s]}`, strings.Join(assets, ","))
	}))
	defer srv.Close()

	exe := filepath.Join(t.TempDir(), update.BinaryFile(update.BinaryMaestria, runtime.GOOS))
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	u := &update.Updater{
		Binary: update.BinaryMaestria, Current: "1.0.0", APIBase: srv.URL, Insecure: true, ExePath: exe,
		Verify: func(ctx context.Context, p string) error {
			out, err := exec.CommandContext(ctx, p, "version").CombinedOutput()
			if err != nil || !strings.Contains(string(out), "maestria 7.8.9") {
				return fmt.Errorf("version output %q: %v", out, err)
			}
			return nil
		},
	}
	rel, err := u.Latest(context.Background(), update.ChannelStable)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.Apply(context.Background(), rel); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	out, err := exec.Command(exe, "version").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "maestria 7.8.9") {
		t.Fatalf("installed binary says %q (%v)", out, err)
	}
}
