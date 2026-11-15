package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- a fake GitHub ----

type fakeRelease struct {
	Tag        string
	Prerelease bool
	Draft      bool
	// Files are the release assets by name; the server serves them under /dl/<tag>/<name>.
	Files map[string][]byte
}

type fakeGitHub struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	releases []*fakeRelease // newest first, like GitHub's list
	limited  bool
	hits     map[string]int
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	g := &fakeGitHub{t: t, hits: map[string]int{}}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGitHub) releaseJSON(r *fakeRelease) string {
	var assets []string
	for name, data := range r.Files {
		assets = append(assets, fmt.Sprintf(`{"name":%q,"size":%d,"browser_download_url":%q}`,
			name, len(data), g.srv.URL+"/dl/"+r.Tag+"/"+name))
	}
	return fmt.Sprintf(`{"tag_name":%q,"prerelease":%t,"draft":%t,"html_url":%q,"assets":[%s]}`,
		r.Tag, r.Prerelease, r.Draft, g.srv.URL+"/releases/"+r.Tag, strings.Join(assets, ","))
}

func (g *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.hits[r.URL.Path]++
	if g.limited && strings.HasPrefix(r.URL.Path, "/repos/") {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "2000000000")
		http.Error(w, "rate limit", http.StatusForbidden)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/repos/"+Repo)
	switch {
	case p == "/releases/latest":
		for _, rel := range g.releases {
			if !rel.Prerelease && !rel.Draft {
				fmt.Fprint(w, g.releaseJSON(rel))
				return
			}
		}
		http.NotFound(w, r)
	case p == "/releases":
		var parts []string
		for _, rel := range g.releases {
			parts = append(parts, g.releaseJSON(rel))
		}
		fmt.Fprintf(w, "[%s]", strings.Join(parts, ","))
	case strings.HasPrefix(p, "/releases/tags/"):
		tag := strings.TrimPrefix(p, "/releases/tags/")
		for _, rel := range g.releases {
			if rel.Tag == tag {
				fmt.Fprint(w, g.releaseJSON(rel))
				return
			}
		}
		http.NotFound(w, r)
	case strings.HasPrefix(r.URL.Path, "/dl/"):
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/dl/"), "/", 2)
		for _, rel := range g.releases {
			if rel.Tag == parts[0] {
				if data, ok := rel.Files[parts[1]]; ok {
					w.Write(data)
					return
				}
			}
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (g *fakeGitHub) add(r *fakeRelease) *fakeRelease {
	g.releases = append(g.releases, r)
	return r
}

// ---- archives ----

func tarGz(t *testing.T, entries map[string]string, order ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if len(order) == 0 {
		for name := range entries {
			order = append(order, name)
		}
	}
	for _, name := range order {
		body := entries[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func zipFile(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	zw.Close()
	return buf.Bytes()
}

func sumOf(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// release builds a release whose archive for goos/goarch holds one binary with content, with a
// matching SHA256SUMS.
func release(t *testing.T, binary, tag, goos, goarch, content string) *fakeRelease {
	t.Helper()
	name := BinaryFile(binary, goos)
	var archive []byte
	if goos == "windows" {
		archive = zipFile(t, map[string]string{name: content, "LICENSE": "MIT"})
	} else {
		archive = tarGz(t, map[string]string{name: content, "LICENSE": "MIT"}, name, "LICENSE")
	}
	an := ArchiveName(binary, tag, goos, goarch)
	return &fakeRelease{Tag: tag, Files: map[string][]byte{
		an:            archive,
		ChecksumsFile: []byte(ChecksumLine(sumOf(archive), an) + ChecksumLine(strings.Repeat("0", 64), "other.tar.gz")),
	}}
}

// ---- harness ----

type installed struct {
	dir, exe string
}

func install(t *testing.T, name, content string) installed {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, name)
	if err := os.WriteFile(exe, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return installed{dir: dir, exe: exe}
}

func (i installed) read(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(i.exe)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// leftovers lists files in the install folder other than the binary and its .previous.
func (i installed) leftovers(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(i.dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if n := e.Name(); n != filepath.Base(i.exe) && n != filepath.Base(i.exe)+".previous" {
			out = append(out, n)
		}
	}
	return out
}

func updater(g *fakeGitHub, ins installed, current string) *Updater {
	return &Updater{
		Binary: BinaryMaestria, Current: current, APIBase: g.srv.URL, Insecure: true,
		GOOS: "linux", GOARCH: "amd64", ExePath: ins.exe,
		LookPath:      func(string) (string, error) { return "", exec.ErrNotFound },
		HealthTimeout: 300 * time.Millisecond, HealthPoll: 20 * time.Millisecond,
	}
}

func mustApply(t *testing.T, u *Updater, rel Release) Result {
	t.Helper()
	res, err := u.Apply(context.Background(), rel)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return res
}

func latest(t *testing.T, u *Updater, ch Channel) Release {
	t.Helper()
	rel, err := u.Latest(context.Background(), ch)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	return rel
}

// ---- tests ----

func TestVersionCompare(t *testing.T) {
	order := []string{"0.9.0", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2",
		"1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1", "1.2.0", "2.0.0", "10.0.0"}
	for i := range order {
		for j := range order {
			a, err := ParseVersion(order[i])
			if err != nil {
				t.Fatal(err)
			}
			b, _ := ParseVersion("v" + order[j])
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got := a.Compare(b); got != want {
				t.Errorf("%s vs %s = %d, want %d", order[i], order[j], got, want)
			}
		}
	}
	a, _ := ParseVersion("1.0.0+abc")
	b, _ := ParseVersion("1.0.0+def")
	if a.Compare(b) != 0 {
		t.Error("build metadata must not affect precedence")
	}
	for _, bad := range []string{"", "1", "1.2", "1.2.3.4", "01.2.3", "1.2.x", "1.2.3-", "1.2.3-a..b", "1.2.3+"} {
		if _, err := ParseVersion(bad); err == nil {
			t.Errorf("ParseVersion(%q) should fail", bad)
		}
	}
}

func TestIsDevBuild(t *testing.T) {
	for v, want := range map[string]bool{
		"": true, "dev": true, "0.0.0-dev": true, "2.0.0-dev+abc1234": true, "2.0.0+abc": true, "garbage": true,
		"2.0.0": false, "v2.0.0": false, "2.1.0-rc.1": false,
	} {
		if got := IsDevBuild(v); got != want {
			t.Errorf("IsDevBuild(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestNamingAndChecksums(t *testing.T) {
	if got := ArchiveName("maestria", "v1.2.3", "linux", "amd64"); got != "maestria_1.2.3_linux_amd64.tar.gz" {
		t.Errorf("tar name: %s", got)
	}
	if got := ArchiveName("maestria-cloud", "1.2.3", "windows", "amd64"); got != "maestria-cloud_1.2.3_windows_amd64.zip" {
		t.Errorf("zip name: %s", got)
	}
	if BinaryFile("maestria", "windows") != "maestria.exe" || BinaryFile("maestria", "darwin") != "maestria" {
		t.Error("BinaryFile")
	}
	h := strings.Repeat("ab", 32)
	sums := ChecksumLine(h, "a.tar.gz") + strings.ToUpper(h) + " *b.tar.gz\n" + "short  c.tar.gz\n" + "bad\n"
	if got, ok := FindChecksum(sums, "a.tar.gz"); !ok || got != h {
		t.Errorf("a: %q %v", got, ok)
	}
	if got, ok := FindChecksum(sums, "b.tar.gz"); !ok || got != h {
		t.Errorf("b (binary mode, upper case): %q %v", got, ok)
	}
	for _, name := range []string{"c.tar.gz", "d.tar.gz", "tar.gz"} {
		if _, ok := FindChecksum(sums, name); ok {
			t.Errorf("%s should not be found", name)
		}
	}
}

func TestLatestChannels(t *testing.T) {
	g := newFakeGitHub(t)
	g.add(&fakeRelease{Tag: "v1.3.0-rc.1", Prerelease: true})
	g.add(&fakeRelease{Tag: "v9.9.9", Draft: true})
	g.add(&fakeRelease{Tag: "v1.2.0"})
	g.add(&fakeRelease{Tag: "v1.1.0"})
	u := &Updater{Binary: "maestria", Current: "1.0.0", APIBase: g.srv.URL, Insecure: true}

	if got := latest(t, u, ChannelStable).Tag; got != "v1.2.0" {
		t.Errorf("stable = %s", got)
	}
	if got := latest(t, u, ChannelPrerelease).Tag; got != "v1.3.0-rc.1" {
		t.Errorf("prerelease = %s", got)
	}
	// Once the stable release overtakes the prerelease, that channel follows it.
	g.add(&fakeRelease{Tag: "v1.3.0"})
	if got := latest(t, u, ChannelPrerelease).Tag; got != "v1.3.0" {
		t.Errorf("prerelease after 1.3.0 = %s", got)
	}
	if rel, err := u.ByTag(context.Background(), "1.1.0"); err != nil || rel.Tag != "v1.1.0" {
		t.Errorf("ByTag: %v %v", rel.Tag, err)
	}
	if _, err := u.ByTag(context.Background(), "v0.0.1"); err == nil {
		t.Error("ByTag of a missing tag should fail")
	}
	if _, err := u.ByTag(context.Background(), "v9.9.9"); err == nil {
		t.Error("ByTag must not return a draft")
	}
	if _, err := ParseChannel("nightly"); err == nil {
		t.Error("unknown channel accepted")
	}
}

func TestLatestNoReleaseAndRateLimit(t *testing.T) {
	g := newFakeGitHub(t)
	u := &Updater{Binary: "maestria", Current: "1.0.0", APIBase: g.srv.URL, Insecure: true}
	if _, err := u.Latest(context.Background(), ChannelStable); !errors.Is(err, ErrNoRelease) {
		t.Errorf("empty repo: %v", err)
	}
	g.limited = true
	_, err := u.Latest(context.Background(), ChannelStable)
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.Reset.Unix() != 2000000000 {
		t.Errorf("rate limit: %v", err)
	}
}

func TestHTTPSRequired(t *testing.T) {
	g := newFakeGitHub(t)
	g.add(&fakeRelease{Tag: "v1.2.0"})
	u := &Updater{Binary: "maestria", Current: "1.0.0", APIBase: g.srv.URL} // Insecure unset
	if _, err := u.Latest(context.Background(), ChannelStable); err == nil || !strings.Contains(err.Error(), "non-https") {
		t.Errorf("plain http must be refused: %v", err)
	}
}

func TestCompare(t *testing.T) {
	for _, tc := range []struct {
		cur, tag string
		want     Status
	}{
		{"1.0.0", "v1.1.0", StatusAvailable},
		{"1.0.0", "v1.0.0", StatusUpToDate},
		{"1.1.0", "v1.0.0", StatusAhead},
		{"1.0.0", "v1.0.1-rc.1", StatusAvailable},
		{"1.0.1-rc.1", "v1.0.0", StatusAhead},
		{"1.0.1-rc.1", "v1.0.1", StatusAvailable},
		{"2.0.0-dev+abc1234", "v9.0.0", StatusDev},
		{"dev", "v9.0.0", StatusDev},
		{"", "v9.0.0", StatusDev},
	} {
		u := &Updater{Current: tc.cur}
		got, err := u.Compare(Release{Tag: tc.tag})
		if err != nil || got != tc.want {
			t.Errorf("%q vs %s = %v (%v), want %v", tc.cur, tc.tag, got, err, tc.want)
		}
	}
	if _, err := (&Updater{Current: "1.0.0"}).Compare(Release{Tag: "nightly"}); err == nil {
		t.Error("a non-version tag should be an error")
	}
}

func TestCheckEndToEnd(t *testing.T) {
	g := newFakeGitHub(t)
	g.add(&fakeRelease{Tag: "v1.2.0"})
	u := &Updater{Binary: "maestria", Current: "1.0.0", APIBase: g.srv.URL, Insecure: true}
	c, err := u.Check(context.Background(), ChannelStable)
	if err != nil || c.Status != StatusAvailable || c.Release.Tag != "v1.2.0" {
		t.Fatalf("check: %+v %v", c, err)
	}
	u.Current = "2.0.0-dev+abc"
	if c, _ := u.Check(context.Background(), ChannelStable); c.Status != StatusDev {
		t.Errorf("dev build status = %v", c.Status)
	}
}

func TestApplyTarGz(t *testing.T) {
	g := newFakeGitHub(t)
	g.add(release(t, "maestria", "v1.2.0", "linux", "amd64", "NEW BINARY"))
	ins := install(t, "maestria", "OLD BINARY")
	u := updater(g, ins, "1.0.0")
	var logs []string
	u.Log = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }

	res := mustApply(t, u, latest(t, u, ChannelStable))
	if got := ins.read(t); got != "NEW BINARY" {
		t.Fatalf("binary = %q", got)
	}
	prev, err := os.ReadFile(ins.exe + ".previous")
	if err != nil || string(prev) != "OLD BINARY" {
		t.Fatalf("previous = %q %v", prev, err)
	}
	if res.From != "1.0.0" || res.To != "1.2.0" || res.Previous != ins.exe+".previous" || res.Restarted {
		t.Errorf("result: %+v", res)
	}
	info, err := os.Stat(ins.exe)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("installed binary: %v %v", info, err)
	}
	// Unix mode bits mean nothing on Windows (executability comes from the .exe name).
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v", info.Mode())
	}
	if l := ins.leftovers(t); len(l) != 0 {
		t.Errorf("leftover files: %v", l)
	}
	if len(logs) == 0 {
		t.Error("expected progress lines")
	}
}

func TestApplyZipOnWindowsNaming(t *testing.T) {
	g := newFakeGitHub(t)
	g.add(release(t, "maestria", "v1.2.0", "windows", "amd64", "NEW EXE"))
	ins := install(t, "maestria.exe", "OLD EXE")
	u := updater(g, ins, "1.0.0")
	u.GOOS = "windows"
	mustApply(t, u, latest(t, u, ChannelStable))
	if got := ins.read(t); got != "NEW EXE" {
		t.Fatalf("binary = %q", got)
	}
	if prev, _ := os.ReadFile(ins.exe + ".previous"); string(prev) != "OLD EXE" {
		t.Fatalf("previous = %q", prev)
	}
	if l := ins.leftovers(t); len(l) != 0 {
		t.Errorf("leftover files: %v", l)
	}
}

func TestApplyCloudBinary(t *testing.T) {
	g := newFakeGitHub(t)
	rel := release(t, "maestria-cloud", "v1.2.0", "linux", "arm64", "CLOUD NEW")
	g.add(rel)
	ins := install(t, "maestria-cloud", "CLOUD OLD")
	u := updater(g, ins, "1.0.0")
	u.Binary, u.GOARCH = BinaryCloud, "arm64"
	mustApply(t, u, latest(t, u, ChannelStable))
	if got := ins.read(t); got != "CLOUD NEW" {
		t.Fatalf("binary = %q", got)
	}
}

func TestApplyRefusals(t *testing.T) {
	good := release(t, "maestria", "v1.2.0", "linux", "amd64", "NEW")
	archive := ArchiveName("maestria", "v1.2.0", "linux", "amd64")
	for _, tc := range []struct {
		name    string
		mutate  func(r *fakeRelease)
		wantErr string
	}{
		{"bad checksum", func(r *fakeRelease) {
			r.Files[ChecksumsFile] = []byte(ChecksumLine(strings.Repeat("a", 64), archive))
		}, "checksum mismatch"},
		{"tampered archive", func(r *fakeRelease) {
			r.Files[archive] = tarGz(t, map[string]string{"maestria": "EVIL"})
		}, "checksum mismatch"},
		{"missing entry", func(r *fakeRelease) {
			r.Files[ChecksumsFile] = []byte(ChecksumLine(strings.Repeat("a", 64), "something-else.tar.gz"))
		}, "no valid entry"},
		{"empty sums", func(r *fakeRelease) { r.Files[ChecksumsFile] = nil }, "no valid entry"},
		{"no SHA256SUMS asset", func(r *fakeRelease) { delete(r.Files, ChecksumsFile) }, "without a checksum"},
		{"no archive for this platform", func(r *fakeRelease) { delete(r.Files, archive) }, "no build for linux/amd64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newFakeGitHub(t)
			rel := &fakeRelease{Tag: good.Tag, Files: map[string][]byte{}}
			for k, v := range good.Files {
				rel.Files[k] = v
			}
			tc.mutate(rel)
			g.add(rel)
			ins := install(t, "maestria", "OLD")
			u := updater(g, ins, "1.0.0")
			_, err := u.Apply(context.Background(), latest(t, u, ChannelStable))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if got := ins.read(t); got != "OLD" {
				t.Errorf("binary changed to %q", got)
			}
			if _, err := os.Stat(ins.exe + ".previous"); err == nil {
				t.Error("a failed update must not leave a .previous")
			}
			if l := ins.leftovers(t); len(l) != 0 {
				t.Errorf("leftover files: %v", l)
			}
		})
	}
}

func TestApplyRejectsHostileArchives(t *testing.T) {
	const an = "maestria_1.2.0_linux_amd64.tar.gz"
	for name, archive := range map[string][]byte{
		"parent traversal":        tarGz(t, map[string]string{"../maestria-evil": "EVIL", "maestria": "NEW"}, "../maestria-evil", "maestria"),
		"absolute path":           tarGz(t, map[string]string{"/etc/maestria": "EVIL", "maestria": "NEW"}, "/etc/maestria", "maestria"),
		"nested traversal":        tarGz(t, map[string]string{"a/../../x": "EVIL", "maestria": "NEW"}, "a/../../x", "maestria"),
		"backslash":               tarGz(t, map[string]string{`..\maestria`: "EVIL", "maestria": "NEW"}, `..\maestria`, "maestria"),
		"binary only in a folder": tarGz(t, map[string]string{"dir/maestria": "NEW"}),
		"no binary":               tarGz(t, map[string]string{"LICENSE": "MIT"}),
		"empty binary":            tarGz(t, map[string]string{"maestria": ""}),
		"not a gzip":              []byte("this is not an archive"),
	} {
		t.Run(name, func(t *testing.T) {
			g := newFakeGitHub(t)
			g.add(&fakeRelease{Tag: "v1.2.0", Files: map[string][]byte{
				an: archive, ChecksumsFile: []byte(ChecksumLine(sumOf(archive), an)),
			}})
			ins := install(t, "maestria", "OLD")
			u := updater(g, ins, "1.0.0")
			if _, err := u.Apply(context.Background(), latest(t, u, ChannelStable)); err == nil {
				t.Fatal("expected an error")
			}
			if got := ins.read(t); got != "OLD" {
				t.Errorf("binary changed to %q", got)
			}
			if l := ins.leftovers(t); len(l) != 0 {
				t.Errorf("leftover files: %v", l)
			}
			// Nothing may have been written outside the install folder either.
			parent := filepath.Dir(ins.dir)
			for _, n := range []string{"maestria-evil", "x"} {
				if _, err := os.Stat(filepath.Join(parent, n)); err == nil {
					t.Errorf("%s escaped the install folder", n)
				}
			}
		})
	}
}

func TestApplyRejectsNonRegularAndHostileZip(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "maestria", Typeflag: tar.TypeSymlink, Linkname: "/bin/sh"})
	tw.Close()
	var gzBuf bytes.Buffer
	gz := gzip.NewWriter(&gzBuf)
	gz.Write(buf.Bytes())
	gz.Close()

	cases := []struct {
		goos, an string
		archive  []byte
	}{
		{"linux", "maestria_1.2.0_linux_amd64.tar.gz", gzBuf.Bytes()},
		{"windows", "maestria_1.2.0_windows_amd64.zip", zipFile(t, map[string]string{"..\\maestria.exe": "EVIL", "maestria.exe": "NEW"})},
		{"windows", "maestria_1.2.0_windows_amd64.zip", zipFile(t, map[string]string{"../maestria.exe": "EVIL", "maestria.exe": "NEW"})},
		{"windows", "maestria_1.2.0_windows_amd64.zip", zipFile(t, map[string]string{"LICENSE": "MIT"})},
	}
	for i, tc := range cases {
		g := newFakeGitHub(t)
		g.add(&fakeRelease{Tag: "v1.2.0", Files: map[string][]byte{
			tc.an: tc.archive, ChecksumsFile: []byte(ChecksumLine(sumOf(tc.archive), tc.an)),
		}})
		name := BinaryFile("maestria", tc.goos)
		ins := install(t, name, "OLD")
		u := updater(g, ins, "1.0.0")
		u.GOOS = tc.goos
		if _, err := u.Apply(context.Background(), latest(t, u, ChannelStable)); err == nil {
			t.Errorf("case %d: expected an error", i)
		}
		if got := ins.read(t); got != "OLD" {
			t.Errorf("case %d: binary changed to %q", i, got)
		}
	}
}

func TestApplyOversized(t *testing.T) {
	oldA, oldB := maxArchiveBytes, maxBinaryBytes
	t.Cleanup(func() { maxArchiveBytes, maxBinaryBytes = oldA, oldB })

	t.Run("archive", func(t *testing.T) {
		g := newFakeGitHub(t)
		g.add(release(t, "maestria", "v1.2.0", "linux", "amd64", strings.Repeat("x", 4096)))
		ins := install(t, "maestria", "OLD")
		u := updater(g, ins, "1.0.0")
		maxArchiveBytes, maxBinaryBytes = 100, oldB
		rel := latest(t, u, ChannelStable)
		_, err := u.Apply(context.Background(), rel)
		if err == nil || !strings.Contains(err.Error(), "too large") {
			t.Fatalf("err = %v", err)
		}
		// A server that lies about the size is still cut off while streaming.
		for i := range rel.Assets {
			rel.Assets[i].Size = 1
		}
		if _, err := u.Apply(context.Background(), rel); err == nil || !strings.Contains(err.Error(), "limit") && !strings.Contains(err.Error(), "too large") {
			t.Fatalf("streamed err = %v", err)
		}
		if got := ins.read(t); got != "OLD" {
			t.Errorf("binary changed to %q", got)
		}
		if l := ins.leftovers(t); len(l) != 0 {
			t.Errorf("leftover files: %v", l)
		}
	})
	t.Run("binary", func(t *testing.T) {
		g := newFakeGitHub(t)
		g.add(release(t, "maestria", "v1.2.0", "linux", "amd64", strings.Repeat("x", 4096)))
		ins := install(t, "maestria", "OLD")
		u := updater(g, ins, "1.0.0")
		maxArchiveBytes, maxBinaryBytes = oldA, 1024
		if _, err := u.Apply(context.Background(), latest(t, u, ChannelStable)); err == nil || !strings.Contains(err.Error(), "larger than") {
			t.Fatalf("err = %v", err)
		}
		if got := ins.read(t); got != "OLD" {
			t.Errorf("binary changed to %q", got)
		}
		if l := ins.leftovers(t); len(l) != 0 {
			t.Errorf("leftover files: %v", l)
		}
	})
}

func TestApplyUnwritableFolder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a folder cannot be made unwritable with chmod on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	g := newFakeGitHub(t)
	g.add(release(t, "maestria", "v1.2.0", "linux", "amd64", "NEW"))
	ins := install(t, "maestria", "OLD")
	if err := os.Chmod(ins.dir, 0o555); err != nil {
		t.Skip("cannot make the folder read-only here")
	}
	t.Cleanup(func() { os.Chmod(ins.dir, 0o755) })
	u := updater(g, ins, "1.0.0")
	if _, err := u.Apply(context.Background(), latest(t, u, ChannelStable)); err == nil || !strings.Contains(err.Error(), "cannot write") {
		t.Fatalf("err = %v", err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for p := range g.hits {
		if strings.HasPrefix(p, "/dl/") {
			t.Errorf("downloaded %s although the folder is not writable", p)
		}
	}
}

func TestVerifyBlocksTheSwap(t *testing.T) {
	g := newFakeGitHub(t)
	g.add(release(t, "maestria", "v1.2.0", "linux", "amd64", "NEW"))
	ins := install(t, "maestria", "OLD")
	u := updater(g, ins, "1.0.0")
	var staged string
	u.Verify = func(_ context.Context, p string) error {
		staged = p
		if b, _ := os.ReadFile(p); string(b) != "NEW" {
			t.Errorf("verify saw %q", b)
		}
		return errors.New("exec format error")
	}
	if _, err := u.Apply(context.Background(), latest(t, u, ChannelStable)); err == nil || !strings.Contains(err.Error(), "does not run here") {
		t.Fatalf("err = %v", err)
	}
	if ins.read(t) != "OLD" || staged == "" {
		t.Error("the old binary must stay")
	}
	if l := ins.leftovers(t); len(l) != 0 {
		t.Errorf("leftover files: %v", l)
	}
}

func TestRestartAndHealthy(t *testing.T) {
	g := newFakeGitHub(t)
	g.add(release(t, "maestria", "v1.2.0", "linux", "amd64", "NEW"))
	ins := install(t, "maestria", "OLD")
	u := updater(g, ins, "1.0.0")
	restarts, checks := 0, 0
	u.Restart = func(context.Context) error {
		restarts++
		if got := ins.read(t); got != "NEW" {
			t.Errorf("restarted while the binary is %q", got)
		}
		return nil
	}
	u.Health = func(context.Context) error {
		if checks++; checks < 3 {
			return errors.New("starting")
		}
		return nil
	}
	res := mustApply(t, u, latest(t, u, ChannelStable))
	if !res.Restarted || restarts != 1 || checks != 3 {
		t.Errorf("restarted=%v restarts=%d checks=%d", res.Restarted, restarts, checks)
	}
	if ins.read(t) != "NEW" {
		t.Error("binary should be the new one")
	}
}

func TestRollbackWhenUnhealthy(t *testing.T) {
	g := newFakeGitHub(t)
	g.add(release(t, "maestria", "v1.2.0", "linux", "amd64", "NEW"))
	ins := install(t, "maestria", "OLD")
	u := updater(g, ins, "1.0.0")
	var seenAtRestart []string
	u.Restart = func(context.Context) error { seenAtRestart = append(seenAtRestart, ins.read(t)); return nil }
	u.Health = func(context.Context) error { return errors.New("connection refused") }

	_, err := u.Apply(context.Background(), latest(t, u, ChannelStable))
	var rb *ErrRolledBack
	if !errors.As(err, &rb) {
		t.Fatalf("err = %v, want ErrRolledBack", err)
	}
	if got := ins.read(t); got != "OLD" {
		t.Fatalf("binary after rollback = %q", got)
	}
	if strings.Join(seenAtRestart, ",") != "NEW,OLD" {
		t.Errorf("restarts saw %v, want the new then the old binary", seenAtRestart)
	}
	if l := ins.leftovers(t); len(l) != 0 {
		t.Errorf("leftover files: %v", l)
	}
}

func TestRollbackWhenRestartFails(t *testing.T) {
	g := newFakeGitHub(t)
	g.add(release(t, "maestria", "v1.2.0", "linux", "amd64", "NEW"))
	ins := install(t, "maestria", "OLD")
	u := updater(g, ins, "1.0.0")
	calls := 0
	u.Restart = func(context.Context) error {
		if calls++; calls == 1 {
			return errors.New("systemctl: unit failed")
		}
		return nil
	}
	_, err := u.Apply(context.Background(), latest(t, u, ChannelStable))
	var rb *ErrRolledBack
	if !errors.As(err, &rb) || ins.read(t) != "OLD" || calls != 2 {
		t.Fatalf("err=%v binary=%q restarts=%d", err, ins.read(t), calls)
	}
}

func TestRollbackOnWindowsLayout(t *testing.T) {
	g := newFakeGitHub(t)
	g.add(release(t, "maestria", "v1.2.0", "windows", "amd64", "NEW EXE"))
	ins := install(t, "maestria.exe", "OLD EXE")
	u := updater(g, ins, "1.0.0")
	u.GOOS = "windows"
	u.Restart = func(context.Context) error { return nil }
	u.Health = func(context.Context) error { return errors.New("down") }
	if _, err := u.Apply(context.Background(), latest(t, u, ChannelStable)); err == nil {
		t.Fatal("expected a rollback error")
	}
	if got := ins.read(t); got != "OLD EXE" {
		t.Fatalf("binary = %q", got)
	}
	if l := ins.leftovers(t); len(l) != 0 {
		t.Errorf("leftover files: %v", l)
	}
}

func TestManualRollback(t *testing.T) {
	g := newFakeGitHub(t)
	g.add(release(t, "maestria", "v1.2.0", "linux", "amd64", "NEW"))
	ins := install(t, "maestria", "OLD")
	u := updater(g, ins, "1.0.0")
	mustApply(t, u, latest(t, u, ChannelStable))
	if err := Rollback(ins.exe); err != nil {
		t.Fatal(err)
	}
	if ins.read(t) != "OLD" {
		t.Error("rollback did not restore the old binary")
	}
	if err := Rollback(ins.exe); err == nil {
		t.Error("a second rollback has nothing to restore")
	}
}

func TestSecondUpdateReplacesPrevious(t *testing.T) {
	g := newFakeGitHub(t)
	g.add(release(t, "maestria", "v1.3.0", "linux", "amd64", "V3"))
	g.add(release(t, "maestria", "v1.2.0", "linux", "amd64", "V2"))
	ins := install(t, "maestria", "V1")
	u := updater(g, ins, "1.0.0")
	rel2, err := u.ByTag(context.Background(), "v1.2.0")
	if err != nil {
		t.Fatal(err)
	}
	mustApply(t, u, rel2)
	mustApply(t, u, latest(t, u, ChannelStable))
	if ins.read(t) != "V3" {
		t.Error("binary should be V3")
	}
	if prev, _ := os.ReadFile(ins.exe + ".previous"); string(prev) != "V2" {
		t.Errorf("previous = %q, want V2", prev)
	}
}

func TestBeforeSwapRunsFirstAndCanStopTheUpdate(t *testing.T) {
	g := newFakeGitHub(t)
	g.add(release(t, "maestria", "v1.2.0", "linux", "amd64", "NEW"))
	ins := install(t, "maestria", "OLD")
	u := updater(g, ins, "1.0.0")
	var sawBinary string
	u.BeforeSwap = func(context.Context) error { sawBinary = ins.read(t); return nil }
	mustApply(t, u, latest(t, u, ChannelStable))
	if sawBinary != "OLD" {
		t.Errorf("BeforeSwap saw %q, want the old binary still in place", sawBinary)
	}

	ins2 := install(t, "maestria", "OLD")
	u2 := updater(g, ins2, "1.0.0")
	u2.BeforeSwap = func(context.Context) error { return errors.New("backup failed") }
	if _, err := u2.Apply(context.Background(), latest(t, u2, ChannelStable)); err == nil || !strings.Contains(err.Error(), "backup failed") {
		t.Fatalf("err = %v", err)
	}
	if ins2.read(t) != "OLD" || len(ins2.leftovers(t)) != 0 {
		t.Error("a failed BeforeSwap must leave the install untouched")
	}
}
