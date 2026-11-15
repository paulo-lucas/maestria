// Package update finds, downloads, verifies and installs new Maestria releases from GitHub.
//
// It is stdlib only. The naming of release assets lives here and is shared with tools/release,
// so what the release workflow publishes is exactly what the updater (and install.sh) looks for.
package update

import (
	"fmt"
	"strings"
)

// Repo is the GitHub repository releases are published to.
const Repo = "paulo-lucas/maestria"

// ChecksumsFile is the release asset that lists the SHA-256 of every archive, in the
// `sha256sum` format: "<hex>  <archive name>" per line.
const ChecksumsFile = "SHA256SUMS"

// Binary names a release ships.
const (
	BinaryMaestria = "maestria"
	BinaryCloud    = "maestria-cloud"
)

// BinaryFile is the executable's file name on goos ("maestria.exe" on Windows).
func BinaryFile(binary, goos string) string {
	if goos == "windows" {
		return binary + ".exe"
	}
	return binary
}

// ArchiveExt is the archive extension used for goos.
func ArchiveExt(goos string) string {
	if goos == "windows" {
		return ".zip"
	}
	return ".tar.gz"
}

// CleanVersion strips a leading "v" from a tag or version.
func CleanVersion(v string) string { return strings.TrimPrefix(strings.TrimSpace(v), "v") }

// ArchiveName is the release archive for binary at version (a tag like "v1.2.3" or "1.2.3"):
// "maestria_1.2.3_linux_amd64.tar.gz", or ".zip" on Windows.
func ArchiveName(binary, version, goos, goarch string) string {
	return fmt.Sprintf("%s_%s_%s_%s%s", binary, CleanVersion(version), goos, goarch, ArchiveExt(goos))
}

// ChecksumLine formats one SHA256SUMS line.
func ChecksumLine(sha256hex, archive string) string {
	return sha256hex + "  " + archive + "\n"
}

// FindChecksum returns the lowercase SHA-256 listed for archive in a SHA256SUMS body. It accepts
// both "<hex>  name" and the binary-mode "<hex> *name" forms.
func FindChecksum(sums, archive string) (string, bool) {
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name != archive {
			continue
		}
		sum := strings.ToLower(fields[0])
		if len(sum) != 64 || strings.Trim(sum, "0123456789abcdef") != "" {
			return "", false
		}
		return sum, true
	}
	return "", false
}
