package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func buildTestTarGz(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	if err := tw.WriteHeader(&tar.Header{
		Name: "maestria",
		Mode: 0o755,
		Size: int64(len(payload)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func runInstallSh(t *testing.T, baseURL, installDir string) (string, error) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", filepath.Join(root, "install.sh"))
	cmd.Env = append(os.Environ(),
		"MAESTRIA_VERSION=v0.2.0-test",
		"MAESTRIA_BASE_URL="+baseURL,
		"MAESTRIA_INSTALL_DIR="+installDir,
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestInstallShChecksumVerification(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh targets POSIX shells")
	}

	binaryPayload := []byte("#!/bin/sh\necho maestria-test\n")
	archiveBytes := buildTestTarGz(t, binaryPayload)
	sumBytes := sha256.Sum256(archiveBytes)
	validHash := hex.EncodeToString(sumBytes[:])
	archiveName := fmt.Sprintf("maestria_0.2.0-test_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)

	t.Run("valid checksum installs binary", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/" + archiveName:
				_, _ = w.Write(archiveBytes)
			case "/SHA256SUMS":
				_, _ = fmt.Fprintf(w, "%s  %s\n", validHash, archiveName)
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		installDir := filepath.Join(t.TempDir(), ".local", "bin")
		out, err := runInstallSh(t, srv.URL, installDir)
		if err != nil {
			t.Fatalf("install.sh failed: %v\noutput:\n%s", err, out)
		}
		if !strings.Contains(out, "Verified SHA256 checksum") {
			t.Fatalf("expected checksum verification message, got:\n%s", out)
		}
		got, err := os.ReadFile(filepath.Join(installDir, "maestria"))
		if err != nil {
			t.Fatalf("expected installed binary: %v", err)
		}
		if !bytes.Equal(got, binaryPayload) {
			t.Fatalf("installed binary content mismatch")
		}
	})

	t.Run("tampered checksum aborts without touching install dir", func(t *testing.T) {
		tamperedHash := strings.Repeat("0", 64)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/" + archiveName:
				_, _ = w.Write(archiveBytes)
			case "/SHA256SUMS":
				_, _ = fmt.Fprintf(w, "%s  %s\n", tamperedHash, archiveName)
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		installDir := filepath.Join(t.TempDir(), ".local", "bin")
		out, err := runInstallSh(t, srv.URL, installDir)
		if err == nil {
			t.Fatalf("expected install.sh to fail on mismatched checksum, output:\n%s", out)
		}
		if !strings.Contains(out, "checksum mismatch") {
			t.Fatalf("expected checksum mismatch message, got:\n%s", out)
		}
		if _, statErr := os.Stat(filepath.Join(installDir, "maestria")); !os.IsNotExist(statErr) {
			t.Fatalf("expected no binary installed on checksum failure, statErr=%v", statErr)
		}
	})

	t.Run("missing SHA256SUMS entry aborts without touching install dir", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/" + archiveName:
				_, _ = w.Write(archiveBytes)
			case "/SHA256SUMS":
				_, _ = fmt.Fprintf(w, "%s  maestria_0.2.0-test_other_arch.tar.gz\n", validHash)
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		installDir := filepath.Join(t.TempDir(), ".local", "bin")
		out, err := runInstallSh(t, srv.URL, installDir)
		if err == nil {
			t.Fatalf("expected install.sh to fail on missing SHA256SUMS entry, output:\n%s", out)
		}
		if !strings.Contains(out, "does not contain an entry") {
			t.Fatalf("expected missing entry message, got:\n%s", out)
		}
		if _, statErr := os.Stat(filepath.Join(installDir, "maestria")); !os.IsNotExist(statErr) {
			t.Fatalf("expected no binary installed when checksum entry is missing, statErr=%v", statErr)
		}
	})
}
