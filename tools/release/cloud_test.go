package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// maestria-cloud ships for the same five targets as maestria, each with a checksum, the licence
// and README, and the version metadata.
func TestReleaseBuildsCloudForAllFiveTargets(t *testing.T) {
	if len(defaultTargets) != 5 {
		t.Fatalf("expected 5 default targets, got %d", len(defaultTargets))
	}
	outDir := t.TempDir()
	var stdout bytes.Buffer
	err := runRelease(releaseOpts{
		OutDir: outDir, RepoRoot: repoRoot(t), Version: "v0.2.0-test", Commit: "deadbeef", Date: "2026-11-14T12:00:00Z",
		Targets: defaultTargets, Binaries: []string{"maestria-cloud"},
	}, &stdout)
	if err != nil {
		t.Fatalf("runRelease: %v", err)
	}

	sums, err := os.ReadFile(filepath.Join(outDir, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tgt := range defaultTargets {
		ext, bin := ".tar.gz", "maestria-cloud"
		if tgt.OS == "windows" {
			ext, bin = ".zip", "maestria-cloud.exe"
		}
		name := "maestria-cloud_0.2.0-test_" + tgt.OS + "_" + tgt.Arch + ext
		data, err := os.ReadFile(filepath.Join(outDir, name))
		if err != nil {
			t.Errorf("missing %s: %v", name, err)
			continue
		}
		sum := sha256.Sum256(data)
		if line := hex.EncodeToString(sum[:]) + "  " + name; !strings.Contains(string(sums), line) {
			t.Errorf("SHA256SUMS lacks %q", line)
		}
		entries := archiveEntries(t, name, data)
		for _, want := range []string{bin, "LICENSE", "README.md"} {
			if _, ok := entries[want]; !ok {
				t.Errorf("%s lacks %s (has %v)", name, want, keys(entries))
			}
		}
		if tgt.OS != "windows" && entries[bin]&0o111 == 0 {
			t.Errorf("%s: %s is not executable (mode %o)", name, bin, entries[bin])
		}
	}
	if strings.Contains(string(sums), "  maestria_") {
		t.Error("only maestria-cloud was requested; the CLI must not be in this build")
	}

	// The host build runs and reports the injected metadata.
	var hostName, hostBin string
	if runtime.GOOS == "windows" {
		hostName, hostBin = "maestria-cloud_0.2.0-test_windows_"+runtime.GOARCH+".zip", "maestria-cloud.exe"
	} else {
		hostName, hostBin = "maestria-cloud_0.2.0-test_"+runtime.GOOS+"_"+runtime.GOARCH+".tar.gz", "maestria-cloud"
	}
	if _, err := os.Stat(filepath.Join(outDir, hostName)); err != nil {
		t.Skipf("no build for the host platform %s/%s in the default targets", runtime.GOOS, runtime.GOARCH)
	}
	data, _ := os.ReadFile(filepath.Join(outDir, hostName))
	bin := filepath.Join(t.TempDir(), hostBin)
	if err := os.WriteFile(bin, extractFile(t, hostName, data, hostBin), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(bin, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("version: %v\n%s", err, out)
	}
	if want := "maestria-cloud 0.2.0-test (commit deadbeef, built 2026-11-14T12:00:00Z)"; !strings.Contains(string(out), want) {
		t.Fatalf("version output %q, want %q", out, want)
	}
}

func TestReleaseRejectsUnknownBinary(t *testing.T) {
	err := runRelease(releaseOpts{OutDir: t.TempDir(), RepoRoot: repoRoot(t), Version: "v0.0.1", Targets: defaultTargets[:1], Binaries: []string{"nope"}}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), `"nope"`) {
		t.Fatalf("got %v", err)
	}
}

func TestSplitList(t *testing.T) {
	got := splitList(" maestria , maestria-cloud ,, ")
	if len(got) != 2 || got[0] != "maestria" || got[1] != "maestria-cloud" || len(splitList("")) != 0 {
		t.Fatalf("%v", got)
	}
}

func keys(m map[string]int64) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// archiveEntries lists a .tar.gz or .zip with each entry's mode.
func archiveEntries(t *testing.T, name string, data []byte) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	if strings.HasSuffix(name, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range zr.File {
			out[f.Name] = int64(f.Mode().Perm())
		}
		return out
	}
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = h.Mode
	}
}

func extractFile(t *testing.T, name string, data []byte, want string) []byte {
	t.Helper()
	if strings.HasSuffix(name, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range zr.File {
			if f.Name == want {
				rc, _ := f.Open()
				defer rc.Close()
				b, _ := io.ReadAll(rc)
				return b
			}
		}
		t.Fatalf("%s not in %s", want, name)
	}
	gr, _ := gzip.NewReader(bytes.NewReader(data))
	tr := tar.NewReader(gr)
	for {
		h, err := tr.Next()
		if err != nil {
			t.Fatalf("%s not in %s", want, name)
		}
		if h.Name == want {
			b, _ := io.ReadAll(tr)
			return b
		}
	}
}
