package activity

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paulo-lucas/maestria/internal/floors"
	"github.com/paulo-lucas/maestria/internal/store"
	"github.com/paulo-lucas/maestria/internal/wire"
	"github.com/paulo-lucas/maestria/internal/workspaces"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// repoWithFloors builds: main with a commit; "merged" (committed, merged with a merge commit);
// "ff" (fast-forwarded into main); "open" (an unmerged commit); "gone" (merged then deleted).
func repoWithFloors(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run(t, dir, "init", "-q", "-b", "main")
	run(t, dir, "commit", "-q", "--allow-empty", "-m", "root")
	for _, b := range []string{"merged", "open", "gone"} {
		run(t, dir, "checkout", "-q", "-b", b, "main")
		run(t, dir, "commit", "-q", "--allow-empty", "-m", b+" work")
		run(t, dir, "checkout", "-q", "main")
	}
	run(t, dir, "merge", "-q", "--no-ff", "-m", "land merged", "merged")
	run(t, dir, "merge", "-q", "--no-ff", "-m", "land gone", "gone")
	run(t, dir, "branch", "-q", "-D", "gone")
	run(t, dir, "checkout", "-q", "-b", "ff", "main")
	run(t, dir, "commit", "-q", "--allow-empty", "-m", "ff work")
	run(t, dir, "checkout", "-q", "main")
	run(t, dir, "merge", "-q", "--ff-only", "ff")
	return dir
}

func TestBranchLandedUsesGitAncestry(t *testing.T) {
	dir := repoWithFloors(t)
	for branch, want := range map[string]bool{
		"merged": true,  // merge commit
		"ff":     true,  // fast-forward
		"open":   false, // commits main does not have
		"gone":   false, // branch deleted: cannot tell
		"nope":   false,
		"-D":     false, // never an option
		"a..b":   false,
		"":       false,
	} {
		if got := BranchLanded(dir, branch); got != want {
			t.Errorf("BranchLanded(%q) = %v, want %v", branch, got, want)
		}
	}
	if BranchLanded(t.TempDir(), "main") || BranchLanded("", "merged") {
		t.Error("a directory that is not a repository must read as not landed")
	}
	// The branch that is the default itself is never "landed into" itself.
	if BranchLanded(dir, "main") {
		t.Error("main landed into main")
	}
}

func TestBranchLandedFallsBackToTheCheckedOutBranch(t *testing.T) {
	dir := repoWithFloors(t)
	run(t, dir, "branch", "-m", "main", "trunk")
	// No main or master any more and no remote: the checked-out branch is the default.
	if !BranchLanded(dir, "merged") {
		t.Error("checked-out branch should be the default")
	}
}

func TestBranchTipAndCommitLanded(t *testing.T) {
	dir := repoWithFloors(t)
	tip := BranchTip(dir, "open")
	if tip == "" || len(tip) != 40 {
		t.Fatalf("BranchTip(open) = %q", tip)
	}
	if BranchTip(dir, "nope") != "" || BranchTip(dir, "-D") != "" || BranchTip("", "open") != "" || BranchTip(t.TempDir(), "main") != "" {
		t.Error("a missing branch, an option-like name or a non-repository has no tip")
	}
	if CommitLanded(dir, tip) {
		t.Error("an unmerged commit is not landed")
	}
	merged := BranchTip(dir, "merged")
	if !CommitLanded(dir, merged) {
		t.Error("a merged commit is landed")
	}
	for _, bad := range []string{"", "HEAD", "main", "--all", merged[:10], strings.Repeat("0", 40)} {
		if CommitLanded(dir, bad) {
			t.Errorf("CommitLanded(%q) should be false", bad)
		}
	}
}

// A floor that lands and has its branch deleted right away is recorded as landed, from the tip
// the panel saw while the floor was alive; a deleted floor that never merged is cancelled.
func TestFloorLandedAndBranchDeletedAtOnceIsRecordedAsLanded(t *testing.T) {
	dir := repoWithFloors(t)
	for _, b := range []string{"late", "abandoned"} {
		run(t, dir, "checkout", "-q", "-b", b, "main")
		run(t, dir, "commit", "-q", "--allow-empty", "-m", b+" work")
		run(t, dir, "checkout", "-q", "main")
	}
	db, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := New(Options{
		DB: db, ConfigDir: t.TempDir(), Now: time.Now,
		Workspaces: func() []workspaces.Workspace { return []workspaces.Workspace{{ID: "atlas", Name: "Atlas", Path: dir}} },
	})
	snap := func(fl ...wire.FloorMeta) *wire.FeedSnapshot {
		return &wire.FeedSnapshot{Workspace: wire.WorkspaceMeta{IsLoaded: true}, Floors: append([]wire.FloorMeta{{Name: "main"}}, fl...)}
	}
	work := &wire.FloorStatus{State: "working"}
	// Alive: the tips are recorded. (A second snapshot with no change must keep them.)
	alive := snap(
		wire.FloorMeta{ID: "f1", Name: "late", Branch: "late", Status: work},
		wire.FloorMeta{ID: "f2", Name: "abandoned", Branch: "abandoned", Status: work},
	)
	s.trackFloors("atlas", alive)
	s.trackFloors("atlas", alive)

	// Land "late" and delete both branches in one go, before the panel looks again.
	run(t, dir, "merge", "-q", "--no-ff", "-m", "land late", "late")
	run(t, dir, "branch", "-q", "-D", "late")
	run(t, dir, "branch", "-q", "-D", "abandoned")
	if BranchLanded(dir, "late") {
		t.Fatal("the branch should be gone")
	}
	s.trackFloors("atlas", snap())

	past, _ := floors.List(db, floors.ListOptions{WorkspaceID: "atlas", IncludePast: true})
	got := map[string]string{}
	for _, f := range past {
		got[f.Name] = f.State
	}
	if got["late"] != floors.StateLanded {
		t.Errorf("late recorded as %q, want landed (all: %v)", got["late"], got)
	}
	if got["abandoned"] != floors.StateCancelled {
		t.Errorf("abandoned recorded as %q, want cancelled (all: %v)", got["abandoned"], got)
	}
}

func TestDisappearedFloorIsLandedWhenItsBranchIsInTheDefaultBranch(t *testing.T) {
	dir := repoWithFloors(t)
	db, err := store.OpenPath(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := New(Options{
		DB: db, ConfigDir: t.TempDir(), Now: time.Now,
		Workspaces: func() []workspaces.Workspace { return []workspaces.Workspace{{ID: "atlas", Name: "Atlas", Path: dir}} },
	})
	snap := func(fl ...wire.FloorMeta) *wire.FeedSnapshot {
		return &wire.FeedSnapshot{Workspace: wire.WorkspaceMeta{IsLoaded: true}, Floors: append([]wire.FloorMeta{{Name: "main"}}, fl...)}
	}
	// Wire last showed every floor as "working": only git can tell which ones landed.
	work := &wire.FloorStatus{State: "working"}
	s.trackFloors("atlas", snap(
		wire.FloorMeta{ID: "f1", Name: "merged", Branch: "merged", Status: work},
		wire.FloorMeta{ID: "f2", Name: "open", Branch: "open", Status: work},
		wire.FloorMeta{ID: "f3", Name: "ff", Branch: "ff", Status: work},
		wire.FloorMeta{ID: "f4", Name: "gone", Branch: "gone", Status: work},
	))
	s.trackFloors("atlas", snap())
	past, _ := floors.List(db, floors.ListOptions{WorkspaceID: "atlas", IncludePast: true})
	got := map[string]string{}
	for _, f := range past {
		got[f.Name] = f.State
	}
	want := map[string]string{"merged": floors.StateLanded, "ff": floors.StateLanded, "open": floors.StateCancelled, "gone": floors.StateCancelled}
	for name, state := range want {
		if got[name] != state {
			t.Errorf("%s recorded as %q, want %q (all: %v)", name, got[name], state, got)
		}
	}
}
