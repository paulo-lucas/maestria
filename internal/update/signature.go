package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// SignatureFile is the release asset holding the cosign (sigstore) bundle for SHA256SUMS:
// signature, Fulcio certificate and transparency-log proof, made keyless by the release workflow.
const SignatureFile = "SHA256SUMS.sigstore.json"

// OIDCIssuer is who issued the identity the release workflow signs with.
const OIDCIssuer = "https://token.actions.githubusercontent.com"

const maxSignatureBytes = 1 << 20

// SignerIdentity is the exact certificate identity a release's SHA256SUMS must carry: this
// repository's release workflow, run for that very tag. The tag is part of it so another
// release's signed SHA256SUMS cannot be swapped in.
func SignerIdentity(repo, tag string) string {
	return "https://github.com/" + repo + "/.github/workflows/release.yml@refs/tags/" + tag
}

// CosignArgs is the `cosign verify-blob` command line that checks sums against bundle for tag of
// repo. The installers (install.sh, install.ps1) run the same checks.
func CosignArgs(repo, tag, bundle, sums string) []string {
	return []string{
		"verify-blob",
		"--bundle", bundle,
		"--certificate-identity", SignerIdentity(repo, tag),
		"--certificate-oidc-issuer", OIDCIssuer,
		"--certificate-github-workflow-repository", repo,
		"--certificate-github-workflow-trigger", "push",
		sums,
	}
}

// verifySignature checks the signature on SHA256SUMS with cosign, when cosign is installed.
//
// The updater does not verify sigstore bundles itself: that would mean carrying Fulcio and Rekor
// trust roots (and their rotation) and a good deal of careful crypto in this binary. So the check
// is opt-in by installing cosign, and then it is strict: a missing signature asset or a failing
// verification stops the update, it never falls back to the checksum. Without cosign the checksum
// is the only check (see docs/security.md for what that does and does not prove).
func (u *Updater) verifySignature(ctx context.Context, rel Release, sums []byte) error {
	look := u.LookPath
	if look == nil {
		look = exec.LookPath
	}
	cosign, err := look("cosign")
	if err != nil {
		if u.RequireSignature {
			return errors.New("a release signature is required (MAESTRIA_REQUIRE_SIGNATURE=1) but cosign was not found on PATH")
		}
		u.logf("Warning: cosign was not found, so the release signature was NOT checked; only the checksum is (install cosign to check who built the release)")
		return nil
	}

	asset, ok := rel.asset(SignatureFile)
	if !ok {
		return fmt.Errorf("release %s has no %s, and cosign is installed, so the signature is required; refusing to install", rel.Tag, SignatureFile)
	}
	bundle, err := u.download(ctx, asset.URL, maxSignatureBytes)
	if err != nil {
		return fmt.Errorf("download %s: %w", SignatureFile, err)
	}

	dir, err := os.MkdirTemp("", "maestria-sig-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	sumsPath := filepath.Join(dir, ChecksumsFile)
	bundlePath := filepath.Join(dir, SignatureFile)
	if err := os.WriteFile(sumsPath, sums, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(bundlePath, bundle, 0o600); err != nil {
		return err
	}

	u.logf("Verifying the release signature with cosign")
	run := u.RunCosign
	if run == nil {
		run = func(ctx context.Context, bin string, args []string) ([]byte, error) {
			return exec.CommandContext(ctx, bin, args...).CombinedOutput()
		}
	}
	out, err := run(ctx, cosign, CosignArgs(u.repo(), rel.Tag, bundlePath, sumsPath))
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 2000 {
			msg = msg[len(msg)-2000:]
		}
		return fmt.Errorf("the signature on %s for %s did not verify (%v); refusing to install. If cosign reports an unreadable bundle, upgrade cosign (releases are signed with cosign v3). %s", ChecksumsFile, rel.Tag, err, msg)
	}
	u.logf("Verified the release signature (%s)", SignerIdentity(u.repo(), rel.Tag))
	return nil
}

// RequireSignatureFromEnv reports whether MAESTRIA_REQUIRE_SIGNATURE=1 asks for a signature
// check even when cosign is not installed. It only ever makes the update stricter.
func RequireSignatureFromEnv() bool { return os.Getenv("MAESTRIA_REQUIRE_SIGNATURE") == "1" }
