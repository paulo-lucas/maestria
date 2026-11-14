package floors

import (
	"errors"
	"strings"
	"testing"

	"github.com/maestria-dev/maestria/internal/store"
)

func openTestDB(t *testing.T) func() {
	t.Helper()
	return func() {}
}

type fakeLive struct {
	items []LiveInfo
	err   error
}

func (f fakeLive) LiveFloors(workspaceID string) ([]LiveInfo, error) {
	return f.items, f.err
}

func TestFloorLifecycleAndTransitions(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Plan two floors (Future).
	f1, err := Plan(db, "atlas", PlanInput{Name: "billing", Brief: "Add **webhooks** <script>xss()</script>"}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if f1.State != StatePlanned || f1.SortOrder != 1 || f1.PlannedAt == nil || *f1.PlannedAt != 1000 {
		t.Fatalf("unexpected planned floor: %+v", f1)
	}
	if strings.Contains(f1.BriefHTML, "<script>") || !strings.Contains(f1.BriefHTML, "<strong>webhooks</strong>") {
		t.Fatalf("unsanitized brief html: %q", f1.BriefHTML)
	}

	if _, err := Plan(db, "atlas", PlanInput{Name: "billing", Brief: "dup"}, 1001); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict for duplicate planned floor, got %v", err)
	}

	f2, err := Plan(db, "atlas", PlanInput{Name: "dep-watch", Brief: "Weekly checks", Ongoing: true}, 1002)
	if err != nil {
		t.Fatal(err)
	}
	if !f2.Ongoing || f2.SortOrder != 2 {
		t.Fatalf("unexpected ongoing planned floor: %+v", f2)
	}

	// Cannot land a planned floor without --force.
	if _, err := Land(db, "atlas", "billing", "Done early", false, 1100); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("expected ErrInvalidState landing planned floor without force, got %v", err)
	}

	// Cannot set status on a planned floor.
	if _, err := SetStatus(db, "atlas", "billing", StatusInput{Status: StatusReview}, 1100); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("expected ErrInvalidState setting status on planned floor, got %v", err)
	}

	// Start billing -> active.
	active, err := Start(db, "atlas", "billing", StartInput{}, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if active.State != StateActive || active.Status != StatusWorking || active.StartedAt == nil || *active.StartedAt != 2000 {
		t.Fatalf("unexpected active floor: %+v", active)
	}

	// Starting an already active floor fails.
	if _, err := Start(db, "atlas", "billing", StartInput{}, 2001); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("expected ErrInvalidState starting active floor, got %v", err)
	}

	// Set status, progress, text, agents.
	p, err := ParseProgress("2/5")
	if err != nil || p != 0.4 {
		t.Fatalf("ParseProgress(2/5) = %v, %v", p, err)
	}
	updated, err := SetStatus(db, "atlas", "billing", StatusInput{
		Status:      StatusReview,
		Progress:    &p,
		ProgressSet: true,
		Text:        "All tests green",
		TextSet:     true,
		Agents:      []string{"Orin", "Vesper", "Orin"},
		AgentsSet:   true,
	}, 3000)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != StatusReview || updated.Progress == nil || *updated.Progress != 0.4 ||
		updated.StatusText != "All tests green" || len(updated.Agents) != 2 {
		t.Fatalf("unexpected updated floor: %+v", updated)
	}

	// Status text > 120 chars rejected.
	if _, err := SetStatus(db, "atlas", "billing", StatusInput{
		Text:    strings.Repeat("a", MaxStatusTextChars+1),
		TextSet: true,
	}, 3100); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for long status text, got %v", err)
	}

	// Ongoing floor refuses progress.
	if _, err := Start(db, "atlas", "dep-watch", StartInput{}, 3200); err != nil {
		t.Fatal(err)
	}
	if _, err := SetStatus(db, "atlas", "dep-watch", StatusInput{
		Progress:    &p,
		ProgressSet: true,
	}, 3201); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput setting progress on ongoing floor, got %v", err)
	}

	// Land billing -> landed (Past).
	landed, err := Land(db, "atlas", "billing", "Shipped `webhooks`", false, 4000)
	if err != nil {
		t.Fatal(err)
	}
	if landed.State != StateLanded || landed.EndedAt == nil || *landed.EndedAt != 4000 || !strings.Contains(landed.OutcomeHTML, "<code>webhooks</code>") {
		t.Fatalf("unexpected landed floor: %+v", landed)
	}

	// Re-landing an already landed floor fails.
	if _, err := Land(db, "atlas", "billing", "", false, 4001); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("expected ErrInvalidState re-landing, got %v", err)
	}

	// A name from Past can be planned again later.
	if _, err := Plan(db, "atlas", PlanInput{Name: "billing", Brief: "Phase 2"}, 5000); err != nil {
		t.Fatalf("expected reusing past floor name to succeed, got %v", err)
	}
	// Cancel planned floor.
	cancelled, err := Cancel(db, "atlas", "billing", "Deferred", 5100)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.State != StateCancelled {
		t.Fatalf("expected cancelled state, got %s", cancelled.State)
	}

	// Force-land a planned floor.
	p3, err := Plan(db, "atlas", PlanInput{Name: "hotfix", Brief: "Quick fix"}, 6000)
	if err != nil {
		t.Fatal(err)
	}
	if p3.State != StatePlanned {
		t.Fatal(p3)
	}
	if _, err := Land(db, "atlas", "hotfix", "Applied manually", true, 6100); err != nil {
		t.Fatalf("expected force land to succeed: %v", err)
	}
}

func TestFloorEditReorderAndLiveMerge(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, name := range []string{"alpha", "beta", "gamma"} {
		if _, err := Plan(db, "atlas", PlanInput{Name: name, Brief: "Brief for " + name}, 1000); err != nil {
			t.Fatal(err)
		}
	}

	// Move gamma before alpha -> gamma, alpha, beta.
	reordered, err := Reorder(db, "atlas", "gamma", "alpha", true, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if len(reordered) != 3 || reordered[0].Name != "gamma" || reordered[1].Name != "alpha" || reordered[2].Name != "beta" {
		t.Fatalf("unexpected order after --before: %+v", reordered)
	}

	// Move alpha after beta -> gamma, beta, alpha.
	reordered, err = Reorder(db, "atlas", "alpha", "beta", false, 2001)
	if err != nil {
		t.Fatal(err)
	}
	if reordered[0].Name != "gamma" || reordered[1].Name != "beta" || reordered[2].Name != "alpha" {
		t.Fatalf("unexpected order after --after: %+v", reordered)
	}

	// Edit rename + brief.
	edited, err := Edit(db, "atlas", "beta", EditInput{
		Rename:   "beta-v2",
		Brief:    "Updated brief",
		BriefSet: true,
	}, 2100)
	if err != nil {
		t.Fatal(err)
	}
	if edited.Name != "beta-v2" || edited.Brief != "Updated brief" {
		t.Fatalf("unexpected edited floor: %+v", edited)
	}

	// Start gamma and merge live source info.
	if _, err := Start(db, "atlas", "gamma", StartInput{}, 2200); err != nil {
		t.Fatal(err)
	}
	liveProg := 0.75
	list, err := ListWithLive(db, "atlas", true, fakeLive{items: []LiveInfo{
		{Name: "gamma", Status: StatusBlocked, StatusText: "Needs key", Progress: &liveProg, Agents: []string{"Kestrel"}},
		{Name: "wire-only", Status: StatusWorking, StatusText: "Running", Agents: []string{"Corvo"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 4 {
		t.Fatalf("expected 4 floors after live merge, got %d: %+v", len(list), list)
	}
	if list[0].Name != "gamma" || list[0].Status != StatusBlocked || list[0].Progress == nil || *list[0].Progress != 0.75 || list[0].Agents[0] != "Kestrel" {
		t.Fatalf("live override failed: %+v", list[0])
	}
	if list[1].Name != "wire-only" || list[1].State != StateActive {
		t.Fatalf("synthesized live floor missing: %+v", list[1])
	}
}

func TestParseProgress(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    float64
		wantErr bool
	}{
		{"0.4", 0.4, false},
		{"0", 0, false},
		{"1", 1, false},
		{"40%", 0.4, false},
		{"2/5", 0.4, false},
		{"0/3", 0, false},
		{"3/3", 1, false},
		{"-0.1", 0, true},
		{"1.2", 0, true},
		{"110%", 0, true},
		{"4/3", 0, true},
		{"1/0", 0, true},
		{"abc", 0, true},
	} {
		got, err := ParseProgress(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseProgress(%q) expected error, got %v", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ParseProgress(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
}
