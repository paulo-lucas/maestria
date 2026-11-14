package inbox

import (
	"errors"
	"strings"
	"testing"

	"github.com/paulo-lucas/maestria/internal/events"
	"github.com/paulo-lucas/maestria/internal/store"
)

func TestRenderMarkdownSanitization(t *testing.T) {
	attacks := []struct {
		name string
		src  string
		bad  string
	}{
		{"script", "Hello <script>alert(1)</script> world", "<script"},
		{"onerror", `<img src="x" onerror="alert(1)">`, "onerror"},
		{"iframe", `<iframe src="https://evil.example"></iframe>`, "<iframe"},
		{"javascript-url", `[click](javascript:alert(1))`, "javascript:"},
		{"data-url", `[data](data:text/html,<script>alert(1)</script>)`, "data:text/html"},
		{"inline-style", `<p style="color:red">hi</p>`, "style="},
	}
	for _, tc := range attacks {
		got := RenderMarkdown(tc.src)
		if strings.Contains(strings.ToLower(got), tc.bad) {
			t.Fatalf("%s leaked %q in %q", tc.name, tc.bad, got)
		}
	}

	safe := RenderMarkdown("# Deploy Ready\n\nSee [docs](https://example.com) and `go test`.\n\n| A | B |\n| - | - |\n| 1 | 2 |")
	if !strings.Contains(safe, "<h1>Deploy Ready</h1>") ||
		!strings.Contains(safe, "<code>go test</code>") ||
		!strings.Contains(safe, "<table>") ||
		!strings.Contains(safe, `rel="`) ||
		!strings.Contains(safe, "noopener") ||
		!strings.Contains(safe, "noreferrer") {
		t.Fatalf("safe markdown unexpected output: %s", safe)
	}
}

func TestPostRespondAnswersDismissPostponeAndCounts(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Validation limits.
	if _, err := Post(db, "ws", PostInput{Kind: KindNotice, Body: strings.Repeat("a", MaxBodyChars+1)}, 100); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for long body, got %v", err)
	}
	if _, err := Post(db, "ws", PostInput{Kind: KindChoice, Body: "Pick", Options: []string{"OnlyOne"}}, 100); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for 1 option, got %v", err)
	}
	if _, err := Post(db, "ws", PostInput{Kind: KindChoice, Body: "Pick", Options: []string{"a", "b", "c", "d", "e"}}, 100); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for 5 options, got %v", err)
	}
	if _, err := Post(db, "ws", PostInput{Kind: KindInput, Body: "Q", Author: strings.Repeat("x", MaxAuthorChars+1)}, 100); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for long author, got %v", err)
	}

	notice, err := Post(db, "ws", PostInput{Kind: KindNotice, Body: "Build finished\nAll tests passed.", Author: "Builder"}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if notice.Title != "Build finished" {
		t.Fatalf("title mismatch: %q", notice.Title)
	}
	approve, err := Post(db, "ws", PostInput{Kind: KindApprove, Body: "**Deploy** to staging?", Author: "Maestro"}, 1001)
	if err != nil {
		t.Fatal(err)
	}
	choice, err := Post(db, "ws", PostInput{Kind: KindChoice, Body: "Pick DB", Options: []string{"SQLite", "Postgres"}}, 1002)
	if err != nil {
		t.Fatal(err)
	}
	input, err := Post(db, "ws", PostInput{Kind: KindInput, Body: "Release tag?"}, 1003)
	if err != nil {
		t.Fatal(err)
	}

	// Postpone choice until t=5000.
	if postponed, err := Postpone(db, "ws", choice.ID, 5000, 2000); err != nil || !postponed.Postponed {
		t.Fatalf("postpone failed: %+v err=%v", postponed, err)
	}

	counts, err := CountsByWorkspace(db, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if counts["ws"] != (WorkspaceCounts{NeedsYou: 2, Postponed: 1, Updates: 1, History: 0}) {
		t.Fatalf("counts at 2000: %+v", counts["ws"])
	}

	// Un-postpone when now >= 5000.
	countsAfter, _ := CountsByWorkspace(db, 5001)
	if countsAfter["ws"].NeedsYou != 3 || countsAfter["ws"].Postponed != 0 {
		t.Fatalf("counts at 5001: %+v", countsAfter["ws"])
	}

	// Cannot respond to notice.
	if _, _, err := Respond(db, "ws", notice.ID, "ok", 2100); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("responding to notice must fail: %v", err)
	}
	// Approve normalizes "approve" -> "Yes".
	if _, ev, err := Respond(db, "ws", approve.ID, "approve", 2200); err != nil || ev.Kind != events.KindInboxResponse {
		t.Fatalf("approve respond: %+v err=%v", ev, err)
	}
	// Choice rejects unknown option and accepts exact match.
	if _, _, err := Respond(db, "ws", choice.ID, "MySQL", 2300); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid choice should fail: %v", err)
	}
	if _, _, err := Respond(db, "ws", choice.ID, "Postgres", 2301); err != nil {
		t.Fatalf("valid choice failed: %v", err)
	}
	// Input bounds and answer.
	if _, _, err := Respond(db, "ws", input.ID, "v2.0.0", 2400); err != nil {
		t.Fatalf("input failed: %v", err)
	}

	// Answers without ack keeps them; with ack marks them acknowledged.
	unacked, err := Answers(db, "ws", false, 2500)
	if err != nil || len(unacked) != 3 {
		t.Fatalf("unacked=%d err=%v", len(unacked), err)
	}
	acked, err := Answers(db, "ws", true, 2501)
	if err != nil || len(acked) != 3 {
		t.Fatalf("acked=%d err=%v", len(acked), err)
	}
	again, err := Answers(db, "ws", false, 2502)
	if err != nil || len(again) != 0 {
		t.Fatalf("expected 0 after ack, got %d err=%v", len(again), err)
	}

	// Dismiss notices.
	nDismissed, err := DismissNotices(db, "ws", 2600)
	if err != nil || nDismissed != 1 {
		t.Fatalf("DismissNotices=%d err=%v", nDismissed, err)
	}
	evs, err := events.Since(db, "ws", 0, 50)
	if err != nil || len(evs) != 4 {
		t.Fatalf("expected 4 events (3 respond + 1 dismiss), got %d err=%v", len(evs), err)
	}
}
