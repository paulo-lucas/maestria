// Command release cross-compiles maestria (and maestria-cloud when present), writes
// .tar.gz / .zip release archives, generates SHA256SUMS, and optionally fills the Homebrew formula.
//
// Usage:
//
//	go run ./tools/release -out dist/release -version v0.2.0
package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/paulo-lucas/maestria/internal/update"
)

type target struct {
	OS   string
	Arch string
}

var defaultTargets = []target{
	{OS: "darwin", Arch: "arm64"},
	{OS: "darwin", Arch: "amd64"},
	{OS: "linux", Arch: "amd64"},
	{OS: "linux", Arch: "arm64"},
	{OS: "windows", Arch: "amd64"},
}

type releaseOpts struct {
	OutDir   string
	RepoRoot string
	Version  string
	Commit   string
	Date     string
	Targets  []target
	// Binaries limits the build to these cmd/ names (default: every binary found).
	Binaries         []string
	HomebrewTemplate string
	HomebrewOut      string
}

type artifact struct {
	Binary string
	Target target
	Name   string
	Path   string
	SHA256 string
}

func main() {
	outDir := flag.String("out", "dist/release", "output directory for release archives and SHA256SUMS")
	repoRoot := flag.String("repo-root", ".", "repository root")
	version := flag.String("version", "", "release version (defaults to git tag or dev)")
	commit := flag.String("commit", "", "git commit SHA (defaults to git rev-parse --short HEAD)")
	date := flag.String("date", "", "build timestamp in RFC3339 UTC (defaults to current UTC time)")
	targetsRaw := flag.String("targets", "", "comma-separated os/arch targets (defaults to all 5 release targets)")
	binariesRaw := flag.String("binaries", "", "comma-separated cmd/ names to build (defaults to maestria and maestria-cloud)")
	hbTemplate := flag.String("homebrew-template", "", "optional path to Homebrew formula template")
	hbOut := flag.String("homebrew-out", "", "optional output path for rendered Homebrew formula")
	flag.Parse()

	targets, err := parseTargets(*targetsRaw)
	if err != nil {
		fatal("%v", err)
	}
	opts := releaseOpts{
		OutDir:           *outDir,
		RepoRoot:         *repoRoot,
		Version:          *version,
		Commit:           *commit,
		Date:             *date,
		Targets:          targets,
		Binaries:         splitList(*binariesRaw),
		HomebrewTemplate: *hbTemplate,
		HomebrewOut:      *hbOut,
	}
	if err := runRelease(opts, os.Stdout); err != nil {
		fatal("%v", err)
	}
}

func splitList(raw string) []string {
	var out []string
	for _, tok := range strings.Split(raw, ",") {
		if tok = strings.TrimSpace(tok); tok != "" {
			out = append(out, tok)
		}
	}
	return out
}

func parseTargets(raw string) ([]target, error) {
	if strings.TrimSpace(raw) == "" {
		return defaultTargets, nil
	}
	var out []target
	for _, tok := range strings.Split(raw, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		parts := strings.Split(tok, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("invalid target %q (expected os/arch)", tok)
		}
		out = append(out, target{OS: parts[0], Arch: parts[1]})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no targets specified")
	}
	return out, nil
}

func runRelease(opts releaseOpts, stdout io.Writer) error {
	root, err := filepath.Abs(opts.RepoRoot)
	if err != nil {
		return err
	}
	outDir, err := filepath.Abs(opts.OutDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}

	ver := strings.TrimSpace(opts.Version)
	if ver == "" {
		ver = gitOutput(root, "describe", "--tags", "--exact-match")
		if ver == "" {
			ver = "0.0.0-dev"
		}
	}
	verClean := strings.TrimPrefix(ver, "v")
	commit := strings.TrimSpace(opts.Commit)
	if commit == "" {
		commit = gitOutput(root, "rev-parse", "--short", "HEAD")
		if commit == "" {
			commit = "unknown"
		}
	}
	date := strings.TrimSpace(opts.Date)
	if date == "" {
		date = time.Now().UTC().Format(time.RFC3339)
	}
	modTime := time.Now().UTC()
	if parsed, err := time.Parse(time.RFC3339, date); err == nil {
		modTime = parsed.UTC()
	}

	binaries := discoverBinaries(root)
	if len(opts.Binaries) > 0 {
		found := map[string]bool{}
		for _, b := range binaries {
			found[b] = true
		}
		binaries = nil
		for _, want := range opts.Binaries {
			if !found[want] {
				return fmt.Errorf("binary %q not found under %s/cmd", want, root)
			}
			binaries = append(binaries, want)
		}
	}
	if len(binaries) == 0 {
		return fmt.Errorf("no binaries found under %s/cmd", root)
	}

	stageDir, err := os.MkdirTemp("", "maestria-release-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stageDir)

	ldflags := fmt.Sprintf("-s -w -X main.version=%s -X main.commit=%s -X main.date=%s", verClean, commit, date)
	var extraFiles []string
	for _, doc := range []string{"LICENSE", "README.md"} {
		if info, err := os.Stat(filepath.Join(root, doc)); err == nil && info.Mode().IsRegular() {
			extraFiles = append(extraFiles, doc)
		}
	}

	var artifacts []artifact
	for _, bin := range binaries {
		for _, tgt := range opts.Targets {
			binName := update.BinaryFile(bin, tgt.OS)
			stagedBin := filepath.Join(stageDir, fmt.Sprintf("%s_%s_%s", bin, tgt.OS, tgt.Arch), binName)
			if err := os.MkdirAll(filepath.Dir(stagedBin), 0o755); err != nil {
				return err
			}

			cmd := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags, "-o", stagedBin, "./cmd/"+bin)
			cmd.Dir = root
			cmd.Env = append(os.Environ(),
				"CGO_ENABLED=0",
				"GOOS="+tgt.OS,
				"GOARCH="+tgt.Arch,
			)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("build %s for %s/%s: %w\n%s", bin, tgt.OS, tgt.Arch, err, stderr.String())
			}

			// The names come from internal/update: what is published is what the updater looks for.
			archiveName := update.ArchiveName(bin, verClean, tgt.OS, tgt.Arch)
			archivePath := filepath.Join(outDir, archiveName)
			if tgt.OS == "windows" {
				err = writeZipArchive(archivePath, stagedBin, binName, root, extraFiles, modTime)
			} else {
				err = writeTarGzArchive(archivePath, stagedBin, binName, root, extraFiles, modTime)
			}
			if err != nil {
				return fmt.Errorf("archive %s: %w", archiveName, err)
			}
			sum, err := fileSHA256(archivePath)
			if err != nil {
				return err
			}
			artifacts = append(artifacts, artifact{
				Binary: bin,
				Target: tgt,
				Name:   archiveName,
				Path:   archivePath,
				SHA256: sum,
			})
			fmt.Fprintf(stdout, "built %s (%s)\n", archiveName, sum[:12])
		}
	}

	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Name < artifacts[j].Name })
	var sumsBuf bytes.Buffer
	for _, a := range artifacts {
		sumsBuf.WriteString(update.ChecksumLine(a.SHA256, a.Name))
	}
	sumsPath := filepath.Join(outDir, update.ChecksumsFile)
	if err := os.WriteFile(sumsPath, sumsBuf.Bytes(), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "wrote %s\n", sumsPath)

	if opts.HomebrewTemplate != "" && opts.HomebrewOut != "" {
		if err := renderHomebrew(opts.HomebrewTemplate, opts.HomebrewOut, verClean, artifacts); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "wrote %s\n", opts.HomebrewOut)
	}
	return nil
}

func discoverBinaries(root string) []string {
	var out []string
	for _, name := range []string{"maestria", "maestria-cloud"} {
		dir := filepath.Join(root, "cmd", name)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		hasGo := false
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
				hasGo = true
				break
			}
		}
		if hasGo {
			out = append(out, name)
		}
	}
	return out
}

func writeTarGzArchive(dst, binPath, binArchiveName, root string, extraFiles []string, modTime time.Time) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	add := func(src, name string, mode int64) error {
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		hdr := &tar.Header{
			Name:    name,
			Mode:    mode,
			Size:    int64(len(data)),
			ModTime: modTime,
			Format:  tar.FormatUSTAR,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err = tw.Write(data)
		return err
	}

	if err := add(binPath, binArchiveName, 0o755); err != nil {
		return err
	}
	for _, rel := range extraFiles {
		if err := add(filepath.Join(root, rel), rel, 0o644); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gw.Close()
}

func writeZipArchive(dst, binPath, binArchiveName, root string, extraFiles []string, modTime time.Time) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	add := func(src, name string, mode os.FileMode) error {
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		hdr := &zip.FileHeader{
			Name:     name,
			Method:   zip.Deflate,
			Modified: modTime,
		}
		hdr.SetMode(mode)
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}

	if err := add(binPath, binArchiveName, 0o755); err != nil {
		return err
	}
	for _, rel := range extraFiles {
		if err := add(filepath.Join(root, rel), rel, 0o644); err != nil {
			return err
		}
	}
	return zw.Close()
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func renderHomebrew(tmplPath, outPath, version string, artifacts []artifact) error {
	raw, err := os.ReadFile(tmplPath)
	if err != nil {
		return err
	}
	byTarget := map[string]string{}
	for _, a := range artifacts {
		if a.Binary == "maestria" {
			byTarget[a.Target.OS+"/"+a.Target.Arch] = a.SHA256
		}
	}
	replacer := strings.NewReplacer(
		"{{VERSION}}", version,
		"{{SHA256_DARWIN_ARM64}}", byTarget["darwin/arm64"],
		"{{SHA256_DARWIN_AMD64}}", byTarget["darwin/amd64"],
		"{{SHA256_LINUX_AMD64}}", byTarget["linux/amd64"],
		"{{SHA256_LINUX_ARM64}}", byTarget["linux/arm64"],
	)
	rendered := replacer.Replace(string(raw))
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(outPath, []byte(rendered), 0o644)
}

func gitOutput(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "release: "+format+"\n", args...)
	os.Exit(1)
}
