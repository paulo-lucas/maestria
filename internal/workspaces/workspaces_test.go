package workspaces

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestAddListRemoveAndResolve(t *testing.T) {
	cfgDir := t.TempDir()
	root := t.TempDir()
	wsDir := filepath.Join(root, "alpha")
	subDir := filepath.Join(wsDir, "packages", "core")
	nestedDir := filepath.Join(wsDir, "nested-app")
	for _, d := range []string{subDir, nestedDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	ws1, err := Add(cfgDir, AddInput{Path: wsDir, Name: "Alpha App", MaestroProvider: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if ws1.ID != "alpha-app" || ws1.MaestroAgent != "Maestro" || ws1.Color == "" || ws1.Tone == "" {
		t.Fatalf("unexpected ws1: %+v", ws1)
	}

	// Duplicate path must fail.
	if _, err := Add(cfgDir, AddInput{Path: wsDir, Name: "Other"}); err == nil {
		t.Fatal("duplicate path should fail")
	}

	// Same name on another path gets unique slug ID.
	ws2, err := Add(cfgDir, AddInput{Path: nestedDir, Name: "Alpha App", MaestroAgent: "Conduct"})
	if err != nil {
		t.Fatal(err)
	}
	if ws2.ID != "alpha-app-2" {
		t.Fatalf("expected alpha-app-2, got %q", ws2.ID)
	}

	list, err := Load(cfgDir)
	if err != nil || len(list) != 2 {
		t.Fatalf("load len=%d err=%v", len(list), err)
	}

	// Resolve by id, case-insensitive name, path, and deepest cwd match.
	if got, err := Resolve(list, "alpha-app", ""); err != nil || got.ID != ws1.ID {
		t.Fatalf("resolve by id: %+v %v", got, err)
	}
	if got, err := Resolve(list, "ALPHA APP", ""); err != nil || got.ID != ws1.ID {
		t.Fatalf("resolve by name: %+v %v", got, err)
	}
	if got, err := Resolve(list, nestedDir, ""); err != nil || got.ID != ws2.ID {
		t.Fatalf("resolve by path: %+v %v", got, err)
	}
	if got, err := Resolve(list, "", subDir); err != nil || got.ID != ws1.ID {
		t.Fatalf("resolve by parent cwd: %+v %v", got, err)
	}
	if got, err := Resolve(list, "", nestedDir); err != nil || got.ID != ws2.ID {
		t.Fatalf("resolve deepest nested cwd: %+v %v", got, err)
	}

	// Outside any workspace must fail.
	if _, err := Resolve(list, "", t.TempDir()); err == nil {
		t.Fatal("outside cwd must fail")
	}

	removed, err := Remove(cfgDir, "alpha-app-2")
	if err != nil || removed.ID != ws2.ID {
		t.Fatalf("remove: %+v %v", removed, err)
	}
	list, _ = Load(cfgDir)
	if len(list) != 1 {
		t.Fatalf("after remove len=%d", len(list))
	}
}

func TestV1CompatibilityPreserved(t *testing.T) {
	cfgDir := t.TempDir()
	raw := `[{"id":"app","name":"My App","path":"/tmp/app","maestroAgent":"Maestro","roadmapPath":"/tmp/app/ROADMAP.md","color":"#0ea5e9"}]`
	if err := os.WriteFile(FilePath(cfgDir), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := Load(cfgDir)
	if err != nil || len(list) != 1 {
		t.Fatalf("load v1: %v %+v", err, list)
	}
	if list[0].RoadmapPath != "/tmp/app/ROADMAP.md" || list[0].Tone != "sky" {
		t.Fatalf("v1 fields mismatch: %+v", list[0])
	}
}

func TestDiscoveryAndMerged(t *testing.T) {
	cfgDir := t.TempDir()
	homeDir := t.TempDir()
	maestriDir := filepath.Join(homeDir, ".maestri")
	wsExplicit := filepath.Join(homeDir, "src", "one")
	wsManifest := filepath.Join(homeDir, "src", "two")
	wsDirOnly := filepath.Join(homeDir, "src", "three")
	for _, d := range []string{wsExplicit, wsManifest, wsDirOnly, filepath.Join(maestriDir, "workspaces", "three-ws")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := Add(cfgDir, AddInput{Path: wsExplicit, Name: "Explicit One", Color: "#ef4444"}); err != nil {
		t.Fatal(err)
	}
	manifest := `{"workspaces":[{"id":"one-dup","name":"One From Manifest","path":"~/src/one"},{"id":"two","name":"Two From Manifest","path":"~/src/two","agents":[{"name":"Maestro","provider":"codex","isMaestro":true}]}]}`
	if err := os.WriteFile(filepath.Join(maestriDir, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	// Marshal rather than concatenate: Windows paths hold backslashes that must be escaped in JSON.
	wsJSON, err := json.Marshal(map[string]string{"name": "Three Room", "path": wsDirOnly, "maestro": "Orchestrator"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(maestriDir, "workspaces", "three-ws", "workspace.json"), wsJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	merged, err := Merged(cfgDir, maestriDir, homeDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged) != 3 {
		t.Fatalf("expected 3 merged workspaces, got %d: %+v", len(merged), merged)
	}
	if merged[0].Name != "Explicit One" || merged[0].Discovered {
		t.Fatalf("explicit workspace should win and not be marked discovered: %+v", merged[0])
	}
	if merged[1].ID != "two" || merged[1].MaestroProvider != "codex" || !merged[1].Discovered {
		t.Fatalf("manifest workspace mismatch: %+v", merged[1])
	}
	if merged[2].ID != "three-ws" || merged[2].MaestroAgent != "Orchestrator" || !merged[2].Discovered {
		t.Fatalf("workspace.json discovery mismatch: %+v", merged[2])
	}
}

func TestColorToneAndInitials(t *testing.T) {
	if got := ColorTone("#4f46e5"); got != "blue" {
		t.Fatalf("#4f46e5 -> %q", got)
	}
	if got := ColorTone("#f59e0b"); got != "amber" {
		t.Fatalf("#f59e0b -> %q", got)
	}
	if got := ColorTone("#ff6600"); got != "orange" {
		t.Fatalf("#ff6600 -> %q", got)
	}
	if got := Initials("Maestria Control"); got != "MC" {
		t.Fatalf("Initials -> %q", got)
	}
	if got := Initials("Atlas"); got != "AT" {
		t.Fatalf("Initials -> %q", got)
	}
}

func TestDiscoverPayloadWorkingDirectoryAndRoadmap(t *testing.T) {
	home := t.TempDir()
	maestriDir := filepath.Join(home, ".maestri")
	wsAlpha := filepath.Join(home, "proj-alpha")
	wsBeta := filepath.Join(home, "proj-beta")
	for _, d := range []string{
		filepath.Join(wsAlpha, "docs"),
		wsBeta,
		filepath.Join(maestriDir, "workspaces", "W1"),
		filepath.Join(maestriDir, "workspaces", "W2"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(wsAlpha, "docs", "roadmap.md"), []byte("# Alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(maestriDir, "manifest.json"), []byte(`{"workspaces":[{"id":"W1","name":"Alpha","path":"workspaces/W1/workspace.json"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(maestriDir, "workspaces", "W1", "workspace.json"), []byte(`{"schemaVersion":2,"payload":{"name":"Alpha","workingDirectory":"`+filepath.ToSlash(wsAlpha)+`","color":"#2563EB"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(maestriDir, "workspaces", "W2", "workspace.json"), []byte(`{"schemaVersion":2,"payload":{"name":"Beta","workingDirectory":"`+filepath.ToSlash(wsBeta)+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	found, err := Discover(maestriDir, home)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 {
		t.Fatalf("expected 2 workspaces, got %d: %+v", len(found), found)
	}
	if found[0].Name != "Alpha" || found[0].RoadmapPath != "docs/roadmap.md" || found[0].Color != "#2563eb" {
		t.Fatalf("unexpected alpha: %+v", found[0])
	}
	if found[1].Name != "Beta" || found[1].RoadmapPath != "" {
		t.Fatalf("unexpected beta: %+v", found[1])
	}
}
