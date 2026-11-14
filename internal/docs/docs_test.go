package docs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDocsOrderingFallbackLinksSanitizationAndCache(t *testing.T) {
	ws := t.TempDir()
	// No docs/README.md initially: fall back to root README.md.
	writeFile(t, filepath.Join(ws, "README.md"), "# Root Project\n\nSee [Arch](docs/architecture.md) and ![Logo](docs/img/logo.png).\n")
	writeFile(t, filepath.Join(ws, "docs", "zebra.md"), "# Zebra\n")
	writeFile(t, filepath.Join(ws, "docs", "architecture.md"), "# Arch\n\n| A | B |\n| - | - |\n| 1 | 2 |\n")
	writeFile(t, filepath.Join(ws, "docs", "decisions", "ulids.md"), "# ULIDs\n\nBack to [Arch](../architecture.md#top) and [Root](../README.md).\n\nBad: <script>alert(1)</script> ![xss](javascript:alert(1)) <img src=x onerror=alert(1)>\n\n[Escape](../../secret.md)\n")
	writeFile(t, filepath.Join(ws, "docs", "img", "logo.png"), "PNGDATA")
	writeFile(t, filepath.Join(ws, "docs", "img", "bad.svg"), "<svg onload=alert(1)/>")
	writeFile(t, filepath.Join(ws, "docs", "notes.txt"), "not markdown")
	writeFile(t, filepath.Join(ws, "docs", ".hidden.md"), "# Hidden")

	v := NewViewer()
	snap, err := v.View("atlas", ws, "")
	if err != nil {
		t.Fatal(err)
	}
	if !snap.HasDocs || snap.Doc == nil || snap.Doc.Path != "README.md" || snap.Doc.DisplayPath != "README.md" {
		t.Fatalf("expected fallback README.md first, got %+v", snap)
	}
	// Tree order: README.md, architecture.md, zebra.md, decisions/
	if len(snap.Tree) != 4 || snap.Tree[0].Name != "README.md" || snap.Tree[1].Name != "architecture.md" || snap.Tree[2].Name != "zebra.md" || !snap.Tree[3].IsDir || snap.Tree[3].Name != "decisions" {
		t.Fatalf("unexpected tree order: %+v", snap.Tree)
	}
	if !strings.Contains(snap.Doc.HTML, `href="/workspaces/atlas/docs?doc=architecture.md"`) {
		t.Fatalf("expected rewritten docs link from fallback README, got %s", snap.Doc.HTML)
	}
	if !strings.Contains(snap.Doc.HTML, `src="/workspaces/atlas/docs/asset?path=img%2Flogo.png"`) {
		t.Fatalf("expected rewritten image asset src, got %s", snap.Doc.HTML)
	}

	// Render nested doc with relative links and XSS attempts.
	nested, err := v.View("atlas", ws, "decisions/ulids.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(nested.Doc.HTML, `href="/workspaces/atlas/docs?doc=architecture.md#top"`) {
		t.Fatalf("expected relative ../architecture.md#top rewrite, got %s", nested.Doc.HTML)
	}
	if !strings.Contains(nested.Doc.HTML, `href="/workspaces/atlas/docs?doc=README.md"`) {
		t.Fatalf("expected relative ../README.md rewrite, got %s", nested.Doc.HTML)
	}
	if strings.Contains(nested.Doc.HTML, "<script") || strings.Contains(nested.Doc.HTML, "onerror") || strings.Contains(nested.Doc.HTML, "javascript:") || strings.Contains(nested.Doc.HTML, "secret.md") {
		t.Fatalf("sanitization failed: %s", nested.Doc.HTML)
	}
	// Active folder should be marked Open.
	if !nested.Tree[3].Open || !nested.Tree[3].Children[0].Active {
		t.Fatalf("expected decisions dir open and ulids.md active: %+v", nested.Tree[3])
	}

	// GFM table rendering.
	arch, err := v.View("atlas", ws, "architecture.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(arch.Doc.HTML, "<table>") || !strings.Contains(arch.Doc.HTML, "<th>A</th>") {
		t.Fatalf("expected GFM table, got %s", arch.Doc.HTML)
	}

	// Asset serving allows png and refuses svg.
	asset, err := v.ReadAsset(ws, "img/logo.png")
	if err != nil || asset.ContentType != "image/png" || string(asset.Data) != "PNGDATA" {
		t.Fatalf("ReadAsset png = %+v err=%v", asset, err)
	}
	if _, err := v.ReadAsset(ws, "img/bad.svg"); !errors.Is(err, ErrNotImage) {
		t.Fatalf("expected ErrNotImage for svg, got %v", err)
	}

	// ETag updates when file changes on disk.
	oldETag := arch.ETag
	time.Sleep(15 * time.Millisecond)
	writeFile(t, filepath.Join(ws, "docs", "architecture.md"), "# Arch v2\n")
	arch2, err := v.View("atlas", ws, "architecture.md")
	if err != nil {
		t.Fatal(err)
	}
	if arch2.ETag == oldETag || !strings.Contains(arch2.Doc.HTML, "Arch v2") {
		t.Fatalf("expected live reload & new ETag, old=%s new=%s html=%s", oldETag, arch2.ETag, arch2.Doc.HTML)
	}
}

func TestDocsSecurityTraversalNonMDAndSizeLimit(t *testing.T) {
	ws := t.TempDir()
	outsideDir := t.TempDir()
	outsideSecret := filepath.Join(outsideDir, "secret.md")
	writeFile(t, outsideSecret, "# Secret")
	writeFile(t, filepath.Join(ws, "docs", "README.md"), "# Docs")
	writeFile(t, filepath.Join(ws, "docs", "notes.txt"), "plain")

	// Symlink escaping docs/.
	symPath := filepath.Join(ws, "docs", "escape.md")
	if err := os.Symlink(outsideSecret, symPath); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	outsideImg := filepath.Join(outsideDir, "secret.png")
	writeFile(t, outsideImg, "PNG")
	symImg := filepath.Join(ws, "docs", "escape.png")
	if err := os.Symlink(outsideImg, symImg); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	// Oversized file > 1 MiB.
	bigPath := filepath.Join(ws, "docs", "big.md")
	writeFile(t, bigPath, strings.Repeat("x", MaxFileBytes+10))

	v := NewViewer()

	for _, bad := range []string{
		"../README.md",
		"%2e%2e/README.md",
		"%2e%2e%2fsecret.md",
		"decisions/../../secret.md",
		"/etc/passwd.md",
		`..\secret.md`,
		"C:\\secret.md",
		".hidden.md",
		"escape.md",
	} {
		if _, err := v.View("atlas", ws, bad); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("View(%q) err = %v, want ErrInvalidPath", bad, err)
		}
		if _, err := v.ReadAsset(ws, strings.TrimSuffix(bad, ".md")+".png"); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("ReadAsset(%q) err = %v, want ErrInvalidPath", bad, err)
		}
	}

	if _, err := v.View("atlas", ws, "notes.txt"); !errors.Is(err, ErrNotMarkdown) {
		t.Errorf("View(notes.txt) err = %v, want ErrNotMarkdown", err)
	}
	if _, err := v.View("atlas", ws, "big.md"); !errors.Is(err, ErrTooLarge) {
		t.Errorf("View(big.md) err = %v, want ErrTooLarge", err)
	}

	// Symlink escape must not appear in tree either.
	snap, err := v.View("atlas", ws, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range snap.Tree {
		if n.Name == "escape.md" {
			t.Fatalf("symlink escape appeared in tree: %+v", n)
		}
	}
}

func TestDocsTreeSizeCap(t *testing.T) {
	ws := t.TempDir()
	docsDir := filepath.Join(ws, "docs")
	if err := os.MkdirAll(docsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxTreeFiles+10; i++ {
		name := filepath.Join(docsDir, strings.ReplaceAll(time.Unix(0, int64(i)).Format("doc-150405.000000000"), ".", "-")+".md")
		if err := os.WriteFile(name, []byte("# doc"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	v := NewViewer()
	snap, err := v.View("atlas", ws, "")
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Truncated {
		t.Fatalf("expected Truncated=true when exceeding %d files", MaxTreeFiles)
	}
}
