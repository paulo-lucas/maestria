package update

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	maxSumsBytes = 1 << 20
	maxEntries   = 200
)

// Download and unpack limits; variables so tests can shrink them.
var (
	maxArchiveBytes int64 = 200 << 20
	maxBinaryBytes  int64 = 300 << 20
)

// fetchBinary downloads rel's archive for this platform, checks it against SHA256SUMS and writes
// the extracted executable next to the one it will replace (same directory, so the final rename
// is atomic). It returns the staged file's path.
func (u *Updater) fetchBinary(ctx context.Context, rel Release, dir string) (string, error) {
	archive := ArchiveName(u.Binary, rel.Tag, u.goos(), u.goarch())
	asset, ok := rel.asset(archive)
	if !ok {
		return "", fmt.Errorf("release %s has no %s (no build for %s/%s?)", rel.Tag, archive, u.goos(), u.goarch())
	}
	sumsAsset, ok := rel.asset(ChecksumsFile)
	if !ok {
		return "", fmt.Errorf("release %s has no %s; refusing to install without a checksum", rel.Tag, ChecksumsFile)
	}
	if asset.Size > maxArchiveBytes {
		return "", fmt.Errorf("%s is too large (%d bytes)", archive, asset.Size)
	}

	// Fail early, before downloading anything, if the install folder cannot be written.
	base := BinaryFile(u.Binary, u.goos())
	staged, err := os.CreateTemp(dir, "."+strings.TrimSuffix(base, filepath.Ext(base))+".new-*"+filepath.Ext(base))
	if err != nil {
		return "", fmt.Errorf("cannot write to %s (run with the permissions that installed it, e.g. sudo): %w", dir, err)
	}
	stagedPath := staged.Name()
	ok = false
	defer func() {
		if !ok {
			staged.Close()
			os.Remove(stagedPath)
		}
	}()

	sumsBody, err := u.download(ctx, sumsAsset.URL, maxSumsBytes)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", ChecksumsFile, err)
	}
	// The signature first: no line of SHA256SUMS is trusted until it has verified.
	if err := u.verifySignature(ctx, rel, sumsBody); err != nil {
		return "", err
	}
	want, found := FindChecksum(string(sumsBody), archive)
	if !found {
		return "", fmt.Errorf("%s has no valid entry for %s; refusing to install", ChecksumsFile, archive)
	}

	tmp, err := os.CreateTemp("", "maestria-update-*"+ArchiveExt(u.goos()))
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	h := sha256.New()
	u.logf("Downloading %s", archive)
	if _, err := u.downloadTo(ctx, asset.URL, maxArchiveBytes, io.MultiWriter(tmp, h)); err != nil {
		return "", fmt.Errorf("download %s: %w", archive, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return "", fmt.Errorf("checksum mismatch for %s (expected %s, got %s); refusing to install", archive, want, got)
	}
	u.logf("Verified SHA-256 %s", want[:12])

	if u.goos() == "windows" {
		err = extractZip(tmp.Name(), base, staged)
	} else {
		if _, err = tmp.Seek(0, io.SeekStart); err == nil {
			err = extractTarGz(tmp, base, staged)
		}
	}
	if err != nil {
		return "", fmt.Errorf("unpack %s: %w", archive, err)
	}
	if err := staged.Sync(); err != nil {
		return "", err
	}
	if err := staged.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(stagedPath, 0o755); err != nil {
		return "", err
	}
	ok = true
	return stagedPath, nil
}

func (u *Updater) download(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	var b strings.Builder
	if _, err := u.downloadTo(ctx, rawURL, limit, &b); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

// downloadTo streams rawURL into w and fails if the body is larger than limit.
func (u *Updater) downloadTo(ctx context.Context, rawURL string, limit int64, w io.Writer) (int64, error) {
	resp, err := u.get(ctx, rawURL)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("server answered %s", resp.Status)
	}
	if resp.ContentLength > limit {
		return 0, fmt.Errorf("download is too large (%d bytes, limit %d)", resp.ContentLength, limit)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return n, err
	}
	if n > limit {
		return n, fmt.Errorf("download is larger than the %d byte limit", limit)
	}
	return n, nil
}

// safeEntry reports whether an archive entry name stays inside the archive's root.
func safeEntry(name string) bool {
	if name == "" || strings.Contains(name, `\`) || strings.ContainsRune(name, 0) || path.IsAbs(name) {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

func copyLimited(dst io.Writer, src io.Reader) error {
	n, err := io.Copy(dst, io.LimitReader(src, maxBinaryBytes+1))
	if err != nil {
		return err
	}
	if n > maxBinaryBytes {
		return fmt.Errorf("the binary is larger than %d bytes", maxBinaryBytes)
	}
	if n == 0 {
		return errors.New("the binary is empty")
	}
	return nil
}

// extractTarGz copies the single regular file called want out of a .tar.gz into dst. Entry names
// are only compared, never used as paths, and any entry that could escape the archive is refused.
func extractTarGz(src io.Reader, want string, dst io.Writer) error {
	gz, err := gzip.NewReader(src)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	found := false
	for i := 0; ; i++ {
		if i > maxEntries {
			return errors.New("too many entries")
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if !safeEntry(hdr.Name) {
			return fmt.Errorf("unsafe entry %q in the archive", hdr.Name)
		}
		if path.Clean(hdr.Name) != want {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return fmt.Errorf("%q in the archive is not a regular file", hdr.Name)
		}
		if found {
			return fmt.Errorf("%q appears twice in the archive", want)
		}
		if err := copyLimited(dst, tr); err != nil {
			return err
		}
		found = true
	}
	if !found {
		return fmt.Errorf("the archive does not contain %s", want)
	}
	return nil
}

// extractZip is extractTarGz for a .zip file on disk.
func extractZip(srcPath, want string, dst io.Writer) error {
	zr, err := zip.OpenReader(srcPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	if len(zr.File) > maxEntries {
		return errors.New("too many entries")
	}
	found := false
	for _, f := range zr.File {
		if !safeEntry(f.Name) {
			return fmt.Errorf("unsafe entry %q in the archive", f.Name)
		}
		if path.Clean(f.Name) != want {
			continue
		}
		if !f.Mode().IsRegular() {
			return fmt.Errorf("%q in the archive is not a regular file", f.Name)
		}
		if f.UncompressedSize64 > uint64(maxBinaryBytes) {
			return fmt.Errorf("the binary is larger than %d bytes", maxBinaryBytes)
		}
		if found {
			return fmt.Errorf("%q appears twice in the archive", want)
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = copyLimited(dst, rc)
		rc.Close()
		if err != nil {
			return err
		}
		found = true
	}
	if !found {
		return fmt.Errorf("the archive does not contain %s", want)
	}
	return nil
}

// swapIn makes staged the executable at exe and keeps the old one as exe+".previous". On Unix the
// old file is hard-linked (or copied) to .previous first, then staged is renamed over exe: one
// atomic step, so exe is never missing or half written. Windows cannot replace a running exe but
// can rename it, so the old one is moved aside and staged takes its place.
func swapIn(exe, staged, goos string) (string, error) {
	prev := exe + ".previous"
	if goos == "windows" {
		removeStale(exe)
		if err := clearForRename(exe, prev); err != nil {
			return "", fmt.Errorf("move the previous binary out of the way: %w", err)
		}
		if err := os.Rename(exe, prev); err != nil {
			return "", fmt.Errorf("move the running binary aside: %w (is an antivirus or another program holding %s?)", err, filepath.Base(exe))
		}
		if err := os.Rename(staged, exe); err != nil {
			_ = os.Rename(prev, exe)
			return "", fmt.Errorf("put the new binary in place: %w", err)
		}
		return prev, nil
	}
	_ = os.Remove(prev)
	if err := os.Link(exe, prev); err != nil {
		if err := copyFile(exe, prev); err != nil {
			return "", fmt.Errorf("keep the previous binary: %w", err)
		}
	}
	if err := os.Rename(staged, exe); err != nil {
		return "", fmt.Errorf("put the new binary in place: %w", err)
	}
	syncDir(filepath.Dir(exe))
	return prev, nil
}

// removeFile is os.Remove; tests replace it to act like Windows with a file in use.
var removeFile = os.Remove

// clearForRename makes way for a rename to target on Windows. A process still running from
// target (a "maestria wait" or "serve" started before the last update) keeps it from being
// removed or replaced, but not from being renamed, so it moves to a unique stale name that a
// later update removes.
func clearForRename(exe, target string) error {
	err := removeFile(target)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	stale := fmt.Sprintf("%s.stale-%d", exe, time.Now().UnixNano())
	if rerr := os.Rename(target, stale); rerr != nil {
		return fmt.Errorf("%w (a program may still be running from %s: close it and try again)", err, filepath.Base(target))
	}
	return nil
}

// removeStale deletes what clearForRename moved aside, once nothing runs from it.
func removeStale(exe string) {
	matches, _ := filepath.Glob(exe + ".stale-*")
	for _, m := range matches {
		_ = removeFile(m)
	}
}

// restorePrevious puts prev back as exe.
func restorePrevious(exe, prev, goos string) error {
	if goos == "windows" {
		failed := exe + ".failed"
		if err := clearForRename(exe, failed); err != nil {
			return err
		}
		if err := os.Rename(exe, failed); err != nil {
			return err
		}
		if err := os.Rename(prev, exe); err != nil {
			_ = os.Rename(failed, exe)
			return err
		}
		_ = os.Remove(failed)
		return nil
	}
	if err := os.Rename(prev, exe); err != nil {
		return err
	}
	syncDir(filepath.Dir(exe))
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}

// syncDir flushes a directory entry change; it is best effort (not supported everywhere).
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	d.Close()
}
