package ui

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func normalize(b []byte) []byte { return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")) }

// design/ is the source of truth for the CSS and icons: the embedded copies must match it.
func TestStaticAssetsMatchDesign(t *testing.T) {
	design := filepath.Join("..", "..", "design")
	if _, err := os.Stat(design); err != nil {
		t.Fatalf("design/ not found: %v", err)
	}
	for name, rel := range DesignAssets {
		want, err := os.ReadFile(filepath.Join(design, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		have, err := os.ReadFile(filepath.Join("static", name))
		if err != nil {
			t.Fatalf("static/%s: %v (run: go generate ./internal/ui)", name, err)
		}
		if !bytes.Equal(normalize(have), normalize(want)) {
			t.Errorf("static/%s differs from design/%s: run `go generate ./internal/ui`", name, rel)
		}
	}
}

// Every design asset must be embedded and served, and app.js (ours, not the design's) must not be
// in the sync list, or a regenerate would overwrite it.
func TestDesignAssetListIsConsistent(t *testing.T) {
	u, err := New()
	if err != nil {
		t.Fatal(err)
	}
	for name := range DesignAssets {
		if _, ok := u.assets[name]; !ok {
			t.Errorf("%s is listed in DesignAssets but not embedded", name)
		}
	}
	if _, ok := DesignAssets["app.js"]; ok {
		t.Fatal("app.js is the panel's own script and must not be synced from design/")
	}
	for _, f := range []string{"tokens.css", "base.css", "components.css", "shell.css"} {
		if _, ok := DesignAssets[f]; !ok {
			t.Errorf("%s is referenced by the layout but not synced", f)
		}
	}
}
