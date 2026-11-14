package usage

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/maestria-dev/maestria/internal/config"
	"github.com/maestria-dev/maestria/internal/store"
)

func testCfg(t *testing.T, maestriDir string) *config.Config {
	t.Helper()
	cfg, err := config.Defaults(maestriDir, config.Env{Home: maestriDir})
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaestriDir = maestriDir
	return cfg
}

func TestSetupIssues(t *testing.T) {
	base := t.TempDir()
	cfg := testCfg(t, filepath.Join(base, "missing-maestri"))
	if snap := ReadSnapshot(nil, cfg, time.Now()); snap.Available || snap.Issue == nil || snap.Issue.Code != "missing_maestri_dir" {
		t.Fatalf("missing maestri dir: %+v", snap)
	}

	md := filepath.Join(base, "m")
	if err := os.MkdirAll(md, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg = testCfg(t, md)
	if snap := ReadSnapshot(nil, cfg, time.Now()); snap.Available || snap.Issue == nil || snap.Issue.Code != "missing_providers_dir" {
		t.Fatalf("missing providers dir: %+v", snap)
	}

	pdir := filepath.Join(md, "usage", "providers")
	if err := os.MkdirAll(pdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if snap := ReadSnapshot(nil, cfg, time.Now()); snap.Available || snap.Issue == nil || snap.Issue.Code != "missing_status_file" {
		t.Fatalf("missing status file: %+v", snap)
	}

	if err := os.WriteFile(filepath.Join(pdir, ".status.json"), []byte("{invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if snap := ReadSnapshot(nil, cfg, time.Now()); snap.Available || snap.Issue == nil || snap.Issue.Code != "invalid_status_json" {
		t.Fatalf("invalid json: %+v", snap)
	}
}

func TestReadSnapshotGroupingErrorHidingAnd24hCache(t *testing.T) {
	base := t.TempDir()
	db, err := store.Open(filepath.Join(base, "cfg"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	md := filepath.Join(base, "maestri")
	pdir := filepath.Join(md, "usage", "providers")
	if err := os.MkdirAll(pdir, 0o700); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	statusPath := filepath.Join(pdir, ".status.json")
	payload1 := `{
		"updatedAt": "2026-11-14T11:55:00Z",
		"providers": {
			"claude": {
				"state": "ready",
				"plan": "pro",
				"fiveHour": {"usedPercent": 46.2, "resetsAt": "2026-11-14T14:00:00Z"},
				"sevenDay": {"usedPercent": 92.0, "resetsAt": "2026-11-18T00:00:00Z"}
			},
			"codex": {
				"state": "executableNotFound",
				"error": "codex CLI missing"
			},
			"antigravity": {
				"state": "ready",
				"meters": [
					{"label": "Gemini Models", "usedPercent": 12.5, "account": "work@example.com"},
					{"label": "Claude and GPT models", "usedPercent": 64.0, "account": "personal@example.com"}
				]
			}
		}
	}`
	if err := os.WriteFile(statusPath, []byte(payload1), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := testCfg(t, md)
	snap := ReadSnapshot(db, cfg, now)
	if !snap.Available || snap.Issue != nil {
		t.Fatalf("expected available snapshot: %+v", snap)
	}
	// codex without <24h success is hidden.
	if snap.HiddenCount != 1 {
		t.Fatalf("expected HiddenCount=1 for codex, got %d", snap.HiddenCount)
	}
	// claude:pro + 2 antigravity accounts = 3 account groups.
	if len(snap.Accounts) != 3 {
		t.Fatalf("expected 3 account groups, got %d: %+v", len(snap.Accounts), snap.Accounts)
	}

	// Verify claude weekly threshold (90 default) tripped!
	var foundWeekly bool
	for _, g := range snap.Accounts {
		if g.Provider == "claude" {
			if g.AccountLabel != "Claude · pro" {
				t.Fatalf("claude account label: %q", g.AccountLabel)
			}
			for _, w := range g.Windows {
				if w.ID == "weekly" {
					foundWeekly = true
					if !w.Tripped || w.Threshold != 90 || w.MarkStep != 90 {
						t.Fatalf("unexpected weekly annotation: %+v", w)
					}
				}
			}
		}
	}
	if !foundWeekly {
		t.Fatal("claude weekly window missing")
	}

	// Now claude errors out 2 hours later -> must still appear as stale from 24h cache.
	payload2 := `{
		"updatedAt": "2026-11-14T14:00:00Z",
		"providers": {
			"claude": {"state": "error", "error": "rate check failed"},
			"codex": {"state": "executableNotFound", "error": "codex CLI missing"}
		}
	}`
	if err := os.WriteFile(statusPath, []byte(payload2), 0o600); err != nil {
		t.Fatal(err)
	}
	snap2 := ReadSnapshot(db, cfg, now.Add(2*time.Hour))
	if len(snap2.Accounts) != 1 || !snap2.Accounts[0].Stale || snap2.Accounts[0].Error != "rate check failed" {
		t.Fatalf("expected stale cached claude, got %+v", snap2.Accounts)
	}

	// After 25h without success, claude must be hidden too.
	snap3 := ReadSnapshot(db, cfg, now.Add(25*time.Hour))
	if len(snap3.Accounts) != 0 || snap3.HiddenCount != 2 {
		t.Fatalf("expected 0 accounts and HiddenCount=2 after 25h, got %+v", snap3)
	}

	// Read-only invariant: .status.json bytes untouched.
	after, _ := os.ReadFile(statusPath)
	if string(after) != payload2 {
		t.Fatal(".status.json must never be modified")
	}
}

func TestReadSnapshotSupportsV1ProvidersArrayAndNestedWindows(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	maestriDir := t.TempDir()
	providersDir := filepath.Join(maestriDir, "usage", "providers")
	if err := os.MkdirAll(providersDir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := `{
		"updatedAt": "2026-11-14T11:58:00Z",
		"providers": [
			{
				"id": "claude",
				"name": "Claude",
				"enabled": true,
				"state": "ready",
				"plan": "Max",
				"meters": [
					{
						"id": "claude",
						"label": "Claude",
						"windows": [
							{"id": "five_hour", "title": "Session (5h)", "usedPercent": 42, "resetsAt": "2026-11-14T15:00:00Z", "ring": "session"},
							{"id": "seven_day", "title": "Weekly", "usedPercent": 61, "resetsAt": "2026-11-18T00:00:00Z", "ring": "weekly"}
						]
					}
				]
			},
			{
				"id": "multi",
				"name": "MultiAcct",
				"enabled": true,
				"state": "ready",
				"meters": [
					{"id": "acct-a", "label": "Work", "windows": [{"id": "primary", "title": "5h", "usedPercent": 15, "ring": "session"}]},
					{"id": "acct-b", "label": "Personal", "windows": [{"id": "primary", "title": "5h", "usedPercent": 55, "ring": "session"}]}
				]
			},
			{
				"id": "disabled",
				"name": "Disabled",
				"enabled": false,
				"state": "off"
			},
			{
				"id": "copilot",
				"name": "Copilot",
				"enabled": true,
				"state": "failed",
				"error": "auth expired"
			}
		]
	}`
	if err := os.WriteFile(filepath.Join(providersDir, ".status.json"), []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		MaestriDir: maestriDir,
		Usage:      config.Usage{StaleMinutes: 15},
		Standby: config.Standby{
			Thresholds: map[string]map[string]int{"claude": {"weekly": 60}},
		},
	}
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snap := ReadSnapshot(db, cfg, now)
	if !snap.Available || snap.Issue != nil {
		t.Fatalf("unexpected issue: %+v", snap.Issue)
	}
	if len(snap.Accounts) != 3 {
		t.Fatalf("expected 3 account groups (1 claude + 2 multi, skipping disabled/failed), got %d: %+v", len(snap.Accounts), snap.Accounts)
	}
	if snap.HiddenCount != 1 || len(snap.Hidden) != 1 || snap.Hidden[0].Provider != "copilot" {
		t.Fatalf("expected 1 hidden copilot provider, got %+v", snap.Hidden)
	}
	var claudeFound bool
	for _, a := range snap.Accounts {
		if a.Provider == "claude" {
			claudeFound = true
			if len(a.Windows) != 2 || a.Windows[0].ID != "session" || a.Windows[1].ID != "weekly" || !a.Windows[1].Tripped {
				t.Fatalf("unexpected claude windows: %+v", a.Windows)
			}
		}
	}
	if !claudeFound {
		t.Fatal("claude account missing from V1 array parse")
	}
}
