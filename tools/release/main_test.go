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

func TestReleaseBuildsHostArchiveChecksumsAndVersion(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	outDir := t.TempDir()
	var stdout bytes.Buffer
	hbOut := filepath.Join(outDir, "maestria.rb")

	opts := releaseOpts{
		OutDir:           outDir,
		RepoRoot:         root,
		Version:          "v0.2.0-test",
		Commit:           "deadbeef",
		Date:             "2026-11-14T12:00:00Z",
		Targets:          []target{{OS: runtime.GOOS, Arch: runtime.GOARCH}},
		HomebrewTemplate: filepath.Join(root, "packaging", "homebrew", "maestria.rb"),
		HomebrewOut:      hbOut,
	}
	if err := runRelease(opts, &stdout); err != nil {
		t.Fatalf("runRelease failed: %v", err)
	}

	ext := ".tar.gz"
	binName := "maestria"
	if runtime.GOOS == "windows" {
		ext = ".zip"
		binName = "maestria.exe"
	}
	archiveName := "maestria_0.2.0-test_" + runtime.GOOS + "_" + runtime.GOARCH + ext
	archivePath := filepath.Join(outDir, archiveName)
	archiveBytes, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatalf("missing archive %s: %v", archivePath, err)
	}

	sumBytes := sha256.Sum256(archiveBytes)
	wantHash := hex.EncodeToString(sumBytes[:])
	sumsRaw, err := os.ReadFile(filepath.Join(outDir, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	wantLine := wantHash + "  " + archiveName
	if !strings.Contains(string(sumsRaw), wantLine) {
		t.Fatalf("SHA256SUMS missing %q:\n%s", wantLine, sumsRaw)
	}

	// Extract binary and verify contents + executable mode + version output.
	extractDir := t.TempDir()
	extractedBin := filepath.Join(extractDir, binName)
	entries := map[string]bool{}

	if runtime.GOOS == "windows" {
		zr, err := zip.NewReader(bytes.NewReader(archiveBytes), int64(len(archiveBytes)))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range zr.File {
			entries[f.Name] = true
			if f.Name == binName {
				rc, err := f.Open()
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(rc)
				_ = rc.Close()
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(extractedBin, data, 0o755); err != nil {
					t.Fatal(err)
				}
			}
		}
	} else {
		gr, err := gzip.NewReader(bytes.NewReader(archiveBytes))
		if err != nil {
			t.Fatal(err)
		}
		tr := tar.NewReader(gr)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			entries[hdr.Name] = true
			if hdr.Name == binName {
				if hdr.Mode&0o111 == 0 {
					t.Fatalf("tar binary mode %o is not executable", hdr.Mode)
				}
				data, err := io.ReadAll(tr)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(extractedBin, data, 0o755); err != nil {
					t.Fatal(err)
				}
			}
		}
		_ = gr.Close()
	}

	for _, want := range []string{binName, "LICENSE", "README.md"} {
		if !entries[want] {
			t.Errorf("archive missing %q (got %v)", want, entries)
		}
	}

	out, err := exec.Command(extractedBin, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("running extracted binary version: %v\n%s", err, out)
	}
	wantVer := "maestria 0.2.0-test (commit deadbeef, built 2026-11-14T12:00:00Z)"
	if !strings.Contains(string(out), wantVer) {
		t.Fatalf("version output = %q, want substring %q", string(out), wantVer)
	}

	hbRaw, err := os.ReadFile(hbOut)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(hbRaw), `version "0.2.0-test"`) {
		t.Fatalf("rendered homebrew formula missing version:\n%s", hbRaw)
	}
}
