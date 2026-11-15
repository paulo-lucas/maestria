package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestSignerIdentityAndArgs(t *testing.T) {
	if got, want := SignerIdentity("paulo-lucas/maestria", "v1.2.0"), "https://github.com/paulo-lucas/maestria/.github/workflows/release.yml@refs/tags/v1.2.0"; got != want {
		t.Fatalf("identity = %q, want %q", got, want)
	}
	args := CosignArgs("o/r", "v1.2.0", "B", "S")
	want := []string{
		"verify-blob", "--bundle", "B",
		"--certificate-identity", "https://github.com/o/r/.github/workflows/release.yml@refs/tags/v1.2.0",
		"--certificate-oidc-issuer", "https://token.actions.githubusercontent.com",
		"--certificate-github-workflow-repository", "o/r",
		"--certificate-github-workflow-trigger", "push",
		"S",
	}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %q\nwant %q", args, want)
	}
}

// signedUpdater is an updater whose cosign (found, or not) is fake.
func signedUpdater(t *testing.T, withBundle bool, run func(args []string) ([]byte, error)) (*Updater, installed) {
	t.Helper()
	g := newFakeGitHub(t)
	rel := release(t, "maestria", "v1.2.0", "linux", "amd64", "NEW BINARY")
	if withBundle {
		rel.Files[SignatureFile] = []byte(`{"bundle":true}`)
	}
	g.add(rel)
	ins := install(t, "maestria", "OLD BINARY")
	u := updater(g, ins, "1.0.0")
	if run != nil {
		u.LookPath = func(string) (string, error) { return "/fake/cosign", nil }
		u.RunCosign = func(_ context.Context, bin string, args []string) ([]byte, error) {
			if bin != "/fake/cosign" {
				t.Errorf("ran %q", bin)
			}
			return run(args)
		}
	}
	return u, ins
}

func TestApplyVerifiesSignatureWithCosign(t *testing.T) {
	var got []string
	u, ins := signedUpdater(t, true, func(args []string) ([]byte, error) {
		got = args
		// The files cosign is given exist and hold what was downloaded.
		sums, err := os.ReadFile(args[len(args)-1])
		if err != nil || !strings.Contains(string(sums), "maestria_1.2.0_linux_amd64.tar.gz") {
			t.Errorf("SHA256SUMS passed to cosign: %q %v", sums, err)
		}
		if b, err := os.ReadFile(args[2]); err != nil || string(b) != `{"bundle":true}` {
			t.Errorf("bundle passed to cosign: %q %v", b, err)
		}
		return nil, nil
	})
	mustApply(t, u, latest(t, u, ChannelStable))
	if len(got) == 0 {
		t.Fatal("cosign was not run")
	}
	if got[4] != SignerIdentity(Repo, "v1.2.0") {
		t.Fatalf("identity = %q", got[4])
	}
	if ins.read(t) != "NEW BINARY" {
		t.Fatal("binary not installed")
	}
}

func TestApplyFailsClosedWithCosign(t *testing.T) {
	t.Run("signature does not verify", func(t *testing.T) {
		u, ins := signedUpdater(t, true, func([]string) ([]byte, error) {
			return []byte("Error: none of the expected identities matched\x1b[2J\x07"), errors.New("exit status 1")
		})
		_, err := u.Apply(context.Background(), latest(t, u, ChannelStable))
		if err == nil || !strings.Contains(err.Error(), "did not verify") || !strings.Contains(err.Error(), "none of the expected identities") {
			t.Fatalf("err = %v", err)
		}
		if strings.ContainsAny(err.Error(), "\x1b\x07") {
			t.Fatalf("control characters from cosign reached the error: %q", err)
		}
		if ins.read(t) != "OLD BINARY" {
			t.Fatal("binary replaced despite a failed signature")
		}
	})
	t.Run("signature asset missing", func(t *testing.T) {
		ran := false
		u, ins := signedUpdater(t, false, func([]string) ([]byte, error) { ran = true; return nil, nil })
		_, err := u.Apply(context.Background(), latest(t, u, ChannelStable))
		if err == nil || !strings.Contains(err.Error(), SignatureFile) || ran {
			t.Fatalf("err = %v, cosign ran = %v", err, ran)
		}
		if ins.read(t) != "OLD BINARY" {
			t.Fatal("binary replaced despite a missing signature")
		}
	})
	t.Run("the signature is checked before the checksum is read", func(t *testing.T) {
		g := newFakeGitHub(t)
		rel := release(t, "maestria", "v1.2.0", "linux", "amd64", "NEW")
		rel.Files[ChecksumsFile] = []byte(ChecksumLine(strings.Repeat("a", 64), ArchiveName("maestria", "v1.2.0", "linux", "amd64")))
		rel.Files[SignatureFile] = []byte("{}")
		g.add(rel)
		u := updater(g, install(t, "maestria", "OLD"), "1.0.0")
		u.LookPath = func(string) (string, error) { return "/fake/cosign", nil }
		u.RunCosign = func(context.Context, string, []string) ([]byte, error) { return nil, errors.New("bad") }
		_, err := u.Apply(context.Background(), latest(t, u, ChannelStable))
		if err == nil || strings.Contains(err.Error(), "checksum mismatch") || !strings.Contains(err.Error(), "did not verify") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestApplyWithoutCosign(t *testing.T) {
	t.Run("warns and relies on the checksum", func(t *testing.T) {
		u, ins := signedUpdater(t, false, nil)
		var logs []string
		u.Log = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
		mustApply(t, u, latest(t, u, ChannelStable))
		if ins.read(t) != "NEW BINARY" {
			t.Fatal("not installed")
		}
		if !strings.Contains(strings.Join(logs, "\n"), "NOT checked") {
			t.Fatalf("expected a warning, logs: %q", logs)
		}
	})
	t.Run("a required signature refuses", func(t *testing.T) {
		u, ins := signedUpdater(t, false, nil)
		u.RequireSignature = true
		u.LookPath = func(string) (string, error) { return "", exec.ErrNotFound }
		_, err := u.Apply(context.Background(), latest(t, u, ChannelStable))
		if err == nil || !strings.Contains(err.Error(), "required") {
			t.Fatalf("err = %v", err)
		}
		if ins.read(t) != "OLD BINARY" {
			t.Fatal("binary replaced")
		}
	})
}
