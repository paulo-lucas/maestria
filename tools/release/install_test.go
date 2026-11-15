package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

// installEnv is one install.sh run: a "release" on disk (served with file://, since the installer
// only talks https to the network), the PATH it sees, and extra environment.
type installEnv struct {
	releaseDir string
	installDir string
	path       string
	extra      []string
}

func runInstallSh(t *testing.T, e installEnv) (string, error) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", filepath.Join(root, "install.sh"))
	cmd.Env = append(os.Environ(),
		"MAESTRIA_VERSION=v0.2.0-test",
		"MAESTRIA_BASE_URL=file://"+e.releaseDir,
		"MAESTRIA_INSTALL_DIR="+e.installDir,
		"PATH="+e.path,
	)
	cmd.Env = append(cmd.Env, e.extra...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// pathWithout returns a PATH holding links to the tools install.sh needs and nothing else, so a
// cosign installed on the machine running the tests cannot change the outcome.
func pathWithout(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, tool := range []string{"uname", "mktemp", "awk", "sed", "head", "tar", "cp", "chmod", "mkdir", "rm", "sha256sum", "shasum", "openssl", "gzip"} {
		if p, err := exec.LookPath(tool); err == nil {
			if err := os.Symlink(p, filepath.Join(dir, tool)); err != nil {
				t.Fatal(err)
			}
		}
	}
	return dir
}

// withFakeCosign returns a PATH like pathWithout plus a cosign that records its arguments in
// argsFile and exits with the status in exit.
func withFakeCosign(t *testing.T, exit int) (path, argsFile string) {
	t.Helper()
	path = pathWithout(t)
	argsFile = filepath.Join(t.TempDir(), "cosign-args")
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = version ]; then echo "GitVersion: v0.0.0-fake"; exit 0; fi
for a in "$@"; do printf '%%s\n' "$a"; done > %q
exit %d
`, argsFile, exit)
	if err := os.WriteFile(filepath.Join(path, "cosign"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, argsFile
}

func TestInstallShVerification(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh targets POSIX shells")
	}

	binaryPayload := []byte("#!/bin/sh\necho maestria-test\n")
	archiveBytes := buildTestTarGz(t, binaryPayload)
	sumBytes := sha256.Sum256(archiveBytes)
	validHash := hex.EncodeToString(sumBytes[:])
	archiveName := fmt.Sprintf("maestria_0.2.0-test_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)

	// release writes the files a release page would hold.
	release := func(t *testing.T, sums string, withBundle bool) installEnv {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, archiveName), archiveBytes, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(sums), 0o644); err != nil {
			t.Fatal(err)
		}
		if withBundle {
			if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS.sigstore.json"), []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return installEnv{
			releaseDir: dir,
			installDir: filepath.Join(t.TempDir(), ".local", "bin"),
			path:       pathWithout(t),
		}
	}
	goodSums := fmt.Sprintf("%s  %s\n", validHash, archiveName)
	notInstalled := func(t *testing.T, e installEnv) {
		t.Helper()
		if _, err := os.Stat(filepath.Join(e.installDir, "maestria")); !os.IsNotExist(err) {
			t.Fatalf("expected nothing installed, stat err=%v", err)
		}
	}

	t.Run("no cosign: warns, then the checksum installs the binary", func(t *testing.T) {
		e := release(t, goodSums, false)
		out, err := runInstallSh(t, e)
		if err != nil {
			t.Fatalf("install.sh failed: %v\noutput:\n%s", err, out)
		}
		for _, want := range []string{"cosign was not found", "NOT checked", "gh attestation verify", "Verified SHA256 checksum"} {
			if !strings.Contains(out, want) {
				t.Fatalf("expected %q in output:\n%s", want, out)
			}
		}
		got, err := os.ReadFile(filepath.Join(e.installDir, "maestria"))
		if err != nil || !bytes.Equal(got, binaryPayload) {
			t.Fatalf("installed binary mismatch: %v", err)
		}
	})

	t.Run("no cosign and a signature required: refuses", func(t *testing.T) {
		e := release(t, goodSums, false)
		e.extra = []string{"MAESTRIA_REQUIRE_SIGNATURE=1"}
		out, err := runInstallSh(t, e)
		if err == nil || !strings.Contains(out, "cosign is required") {
			t.Fatalf("expected refusal, err=%v output:\n%s", err, out)
		}
		notInstalled(t, e)
	})

	t.Run("cosign present: verifies the exact identity, then installs", func(t *testing.T) {
		e := release(t, goodSums, true)
		var argsFile string
		e.path, argsFile = withFakeCosign(t, 0)
		out, err := runInstallSh(t, e)
		if err != nil {
			t.Fatalf("install.sh failed: %v\noutput:\n%s", err, out)
		}
		args, err := os.ReadFile(argsFile)
		if err != nil {
			t.Fatalf("cosign was not run: %v", err)
		}
		lines := strings.Split(strings.TrimSpace(string(args)), "\n")
		want := []string{
			"verify-blob",
			"--bundle", "SHA256SUMS.sigstore.json",
			"--certificate-identity", "https://github.com/paulo-lucas/maestria/.github/workflows/release.yml@refs/tags/v0.2.0-test",
			"--certificate-oidc-issuer", "https://token.actions.githubusercontent.com",
			"--certificate-github-workflow-repository", "paulo-lucas/maestria",
			"--certificate-github-workflow-trigger", "push",
		}
		if len(lines) != len(want)+1 { // the arguments above, then the blob
			t.Fatalf("unexpected cosign arguments:\n%s", args)
		}
		for i, w := range want {
			if i == 2 {
				if !strings.HasSuffix(lines[i], w) {
					t.Fatalf("bundle argument %q", lines[i])
				}
				continue
			}
			if lines[i] != w {
				t.Fatalf("cosign argument %d = %q, want %q\nall:\n%s", i, lines[i], w, args)
			}
		}
		if !strings.HasSuffix(lines[len(lines)-1], "SHA256SUMS") {
			t.Fatalf("the blob verified must be SHA256SUMS, got %q", lines[len(lines)-1])
		}
		if !strings.Contains(out, "Verified the release signature") {
			t.Fatalf("expected signature message:\n%s", out)
		}
	})

	t.Run("cosign present and the signature fails: nothing is read from SHA256SUMS or installed", func(t *testing.T) {
		// The checksum is wrong too: if it were looked at first, the error would be about it.
		e := release(t, fmt.Sprintf("%s  %s\n", strings.Repeat("0", 64), archiveName), true)
		e.path, _ = withFakeCosign(t, 1)
		out, err := runInstallSh(t, e)
		if err == nil {
			t.Fatalf("expected failure:\n%s", out)
		}
		if !strings.Contains(out, "did not verify") || !strings.Contains(out, "v0.0.0-fake") || strings.Contains(out, "checksum mismatch") {
			t.Fatalf("unexpected output:\n%s", out)
		}
		notInstalled(t, e)
	})

	t.Run("cosign present and the signature asset is missing: fails closed", func(t *testing.T) {
		e := release(t, goodSums, false)
		e.path, _ = withFakeCosign(t, 0)
		out, err := runInstallSh(t, e)
		if err == nil || !strings.Contains(out, "failed to read") {
			t.Fatalf("expected failure on a missing bundle, err=%v output:\n%s", err, out)
		}
		notInstalled(t, e)
	})

	t.Run("an http base URL is refused", func(t *testing.T) {
		e := release(t, goodSums, false)
		e.releaseDir = ""
		e.extra = []string{"MAESTRIA_BASE_URL=http://127.0.0.1:1"}
		out, err := runInstallSh(t, e)
		if err == nil || !strings.Contains(out, "non-https") {
			t.Fatalf("expected refusal, err=%v output:\n%s", err, out)
		}
		notInstalled(t, e)
	})

	t.Run("a repository that is not owner/name is refused", func(t *testing.T) {
		for _, repo := range []string{"a/b/c", "evil", "a/b;c", "/a/b", "a/b/"} {
			e := release(t, goodSums, false)
			e.extra = []string{"MAESTRIA_REPO=" + repo}
			out, err := runInstallSh(t, e)
			if err == nil || !strings.Contains(out, "owner/name") {
				t.Fatalf("repo %q: expected refusal, err=%v output:\n%s", repo, err, out)
			}
		}
	})

	t.Run("tampered checksum aborts without touching install dir", func(t *testing.T) {
		e := release(t, fmt.Sprintf("%s  %s\n", strings.Repeat("0", 64), archiveName), false)
		out, err := runInstallSh(t, e)
		if err == nil || !strings.Contains(out, "checksum mismatch") {
			t.Fatalf("expected checksum mismatch, err=%v output:\n%s", err, out)
		}
		notInstalled(t, e)
	})

	t.Run("missing SHA256SUMS entry aborts without touching install dir", func(t *testing.T) {
		e := release(t, fmt.Sprintf("%s  maestria_0.2.0-test_other_arch.tar.gz\n", validHash), false)
		out, err := runInstallSh(t, e)
		if err == nil || !strings.Contains(out, "does not contain an entry") {
			t.Fatalf("expected missing entry message, err=%v output:\n%s", err, out)
		}
		notInstalled(t, e)
	})
}

// TestInstallShRunsNothingWhenTruncated cuts the script anywhere before its last line: the
// shell must execute nothing (no output, no files), whatever the cut.
func TestInstallShRunsNothingWhenTruncated(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh targets POSIX shells")
	}
	script, err := os.ReadFile(filepath.Join("..", "..", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(script), "\n"), "\n")
	last := lines[len(lines)-1]
	if last != `main "$@"` {
		t.Fatalf("the last line must be the call to main, got %q", last)
	}
	for _, cut := range []int{len(lines) - 1, len(lines) - 5, len(lines) / 2, len(lines) / 4} {
		dir := t.TempDir()
		cmd := exec.Command("sh")
		cmd.Stdin = strings.NewReader(strings.Join(lines[:cut], "\n"))
		cmd.Env = append(os.Environ(), "MAESTRIA_INSTALL_DIR="+filepath.Join(dir, "bin"), "MAESTRIA_BASE_URL=file://"+dir, "MAESTRIA_VERSION=v0.0.0")
		out, _ := cmd.CombinedOutput()
		if strings.Contains(string(out), "Downloading") || strings.Contains(string(out), "Installed") {
			t.Fatalf("cut at line %d ran part of the installer:\n%s", cut, out)
		}
		if _, err := os.Stat(filepath.Join(dir, "bin")); !os.IsNotExist(err) {
			t.Fatalf("cut at line %d created the install dir", cut)
		}
	}
}
