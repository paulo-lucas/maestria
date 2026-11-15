package insights

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paulo-lucas/maestria/internal/store"
	"github.com/paulo-lucas/maestria/internal/usage"
)

var now = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.OpenPath(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func addFloor(t *testing.T, db *sql.DB, name, state string, start, end time.Time) {
	t.Helper()
	var endMs any
	if !end.IsZero() {
		endMs = ms(end)
	}
	if _, err := db.Exec(`INSERT INTO floors (workspace_id, name, state, started_at, ended_at, updated_at) VALUES ('ws', ?, ?, ?, ?, ?)`,
		name, state, ms(start), endMs, ms(start)); err != nil {
		t.Fatal(err)
	}
}

func addTask(t *testing.T, db *sql.DB, kind, response string, created time.Time, wait time.Duration) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO inbox (workspace_id, kind, body, response, status, created_at, answered_at) VALUES ('ws', ?, 'b', ?, 'answered', ?, ?)`,
		kind, response, ms(created), ms(created.Add(wait))); err != nil {
		t.Fatal(err)
	}
}

func area(t *testing.T, rep Report, id string) Area {
	t.Helper()
	for _, a := range rep.Areas {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("area %s missing", id)
	return Area{}
}

func TestScale(t *testing.T) {
	for _, c := range []struct {
		s    scale
		v    float64
		want int
	}{
		{leadScale, 3, 7}, {leadScale, 4, 7}, {leadScale, 5, 6}, {leadScale, 168, 2}, {leadScale, 200, 1},
		{landedScale, 100, 7}, {landedScale, 70, 4}, {landedScale, 49, 1},
	} {
		if got := c.s.level(c.v); got != c.want {
			t.Errorf("level(%v) = %d, want %d", c.v, got, c.want)
		}
	}
	if got := leadScale.next(1); got != "Level 2 at ≤ 7 days" {
		t.Errorf("next(1) = %q", got)
	}
	if got := landedScale.next(4); got != "Level 5 at ≥ 80%" {
		t.Errorf("next(4) = %q", got)
	}
	if got := leadScale.next(MaxLevel); got != "" {
		t.Errorf("next(7) = %q, want empty", got)
	}
}

func TestEmptyMeansNotEnoughData(t *testing.T) {
	rep, err := Compute(openDB(t), now)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Level != 0 || rep.Focus != "" || len(rep.Areas) != 4 {
		t.Fatalf("report = %+v", rep)
	}
	for _, a := range rep.Areas {
		if a.Level != 0 || a.Missing == "" || a.Tip != "" {
			t.Errorf("area %s = %+v, want no level and a Missing reason", a.ID, a)
		}
	}
}

func TestDelivery(t *testing.T) {
	db := openDB(t)
	day := now.Add(-48 * time.Hour)
	addFloor(t, db, "a", "landed", day, day.Add(2*time.Hour))
	addFloor(t, db, "b", "landed", day, day.Add(10*time.Hour))
	addFloor(t, db, "c", "landed", day, day.Add(30*time.Hour))
	addFloor(t, db, "vanished", "landed", day, day)                                        // no real duration
	addFloor(t, db, "old", "landed", now.Add(-60*24*time.Hour), now.Add(-40*24*time.Hour)) // before the period
	addFloor(t, db, "dropped", "cancelled", day, day.Add(time.Hour))                       // not landed

	rep, err := Compute(db, now)
	if err != nil {
		t.Fatal(err)
	}
	a := area(t, rep, AreaDelivery)
	if a.Level != 5 || !strings.Contains(a.Value, "10 h") || !strings.Contains(a.Value, "3 floors") {
		t.Fatalf("delivery = %+v, want level 5 for a 10 h median over 3 floors", a)
	}
	if a.Next != "Level 6 at ≤ 8 h" || a.Tip == "" {
		t.Errorf("delivery next/tip = %q / %q", a.Next, a.Tip)
	}
}

func TestQualityAveragesFloorsAndApprovals(t *testing.T) {
	db := openDB(t)
	day := now.Add(-24 * time.Hour)
	for _, n := range []string{"a", "b", "c"} {
		addFloor(t, db, n, "landed", day, day.Add(time.Hour))
	}
	addFloor(t, db, "d", "cancelled", day, day.Add(time.Hour)) // 75% landed: level 4
	for i := range 9 {
		addTask(t, db, "approve", "Yes", day.Add(time.Duration(i)*time.Minute), time.Minute)
	}
	addTask(t, db, "approve", "No", day, time.Minute) // 10% rejected: level 5

	rep, err := Compute(db, now)
	if err != nil {
		t.Fatal(err)
	}
	a := area(t, rep, AreaQuality)
	if a.Level != 5 { // round((4+5)/2)
		t.Fatalf("quality = %+v, want level 5", a)
	}
	if !strings.Contains(a.Value, "3 of 4 floors landed") || !strings.Contains(a.Value, "1 of 10 approvals rejected") {
		t.Errorf("quality value = %q", a.Value)
	}
	if a.Next != "Higher at ≥ 80% landed · ≤ 5% rejected" {
		t.Errorf("quality next = %q", a.Next)
	}
	if !strings.Contains(a.Tip, "Cancelled") {
		t.Errorf("tip should target the weaker part (cancelled floors): %q", a.Tip)
	}
}

func TestResponseTime(t *testing.T) {
	db := openDB(t)
	day := now.Add(-24 * time.Hour)
	addTask(t, db, "input", "x", day, 2*time.Minute)
	addTask(t, db, "choice", "A", day, 10*time.Minute)
	addTask(t, db, "agent_prompt", "Yes", day, 20*time.Minute)
	addTask(t, db, "notice", "", day, 900*time.Minute) // notices wait on no one

	rep, err := Compute(db, now)
	if err != nil {
		t.Fatal(err)
	}
	a := area(t, rep, AreaResponse)
	if a.Level != 6 || !strings.Contains(a.Value, "10 min") {
		t.Fatalf("response = %+v, want level 6 for a 10 min median", a)
	}
	// One rated area is not enough for an overall level.
	if rep.Level != 0 {
		t.Errorf("overall level = %d with one rated area, want 0", rep.Level)
	}
}

func TestSpend(t *testing.T) {
	db := openDB(t)
	t0 := now.Add(-24 * time.Hour)
	addFloor(t, db, "shipped", "landed", t0, t0.Add(60*time.Minute))
	// Samples every 5 minutes from 30 min before the floor to its end, one point of usage each:
	// 6 intervals with no floor (overhead) and 12 while it ran (delivered).
	used := 10.0
	for at := t0.Add(-30 * time.Minute); !at.After(t0.Add(60 * time.Minute)); at = at.Add(5 * time.Minute) {
		if _, err := db.Exec(`INSERT INTO usage_samples (provider, account, window_id, used_pct, at) VALUES ('claude', 'claude:a', 'session', ?, ?)`, used, ms(at)); err != nil {
			t.Fatal(err)
		}
		used++
	}
	// A weekly window of the same account is ignored in favour of the session window.
	if _, err := db.Exec(`INSERT INTO usage_samples (provider, account, window_id, used_pct, at) VALUES ('claude', 'claude:a', 'weekly', 1, ?), ('claude', 'claude:a', 'weekly', 90, ?)`,
		ms(t0.Add(-30*time.Minute)), ms(t0.Add(-25*time.Minute))); err != nil {
		t.Fatal(err)
	}

	rep, err := Compute(db, now)
	if err != nil {
		t.Fatal(err)
	}
	a := area(t, rep, AreaSpend)
	if a.Level != 4 || !strings.Contains(a.Value, "67%") { // 12 of 18 points
		t.Fatalf("spend = %+v, want level 4 at 67%%", a)
	}
	if !strings.HasPrefix(a.Note, "About 12% of a Claude session window per landed floor (1 floor)") {
		t.Errorf("estimate = %q", a.Note)
	}
}

func TestConsumed(t *testing.T) {
	min5 := 5 * time.Minute.Milliseconds()
	for _, c := range []struct {
		name      string
		prev, cur point
		want      float64
	}{
		{"growth", point{used: 10, at: 0}, point{used: 13, at: min5}, 3},
		{"flat", point{used: 10, at: 0}, point{used: 10, at: min5}, 0},
		{"reset by time", point{used: 80, resetAt: 1000, at: 0}, point{used: 85, resetAt: 1000 + 5*3600*1000, at: min5}, 85},
		{"reset by drop", point{used: 80, at: 0}, point{used: 4, at: min5}, 4},
		{"gap", point{used: 10, at: 0}, point{used: 60, at: maxGap.Milliseconds() + 1}, 0},
	} {
		if got := consumed(c.prev, c.cur); got != c.want {
			t.Errorf("%s: consumed = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestClassify(t *testing.T) {
	ivs := []interval{
		{start: 100, end: 200, state: "landed"},
		{start: 220, end: 280, state: "cancelled"},
		{start: 300, end: 0, state: "active"}, // still open
	}
	for _, c := range []struct {
		t    int64
		want int
	}{{150, spentDelivered}, {250, spentOverhead}, {290, spentOverhead}, {350, spentUndecided}, {50, spentOverhead}} {
		if got := classify(ivs, c.t); got != c.want {
			t.Errorf("classify(%d) = %d, want %d", c.t, got, c.want)
		}
	}
}

func TestOverallLevelAndFocus(t *testing.T) {
	db := openDB(t)
	day := now.Add(-24 * time.Hour)
	for _, n := range []string{"a", "b", "c"} {
		addFloor(t, db, n, "landed", day, day.Add(2*time.Hour)) // delivery 7, quality 7 (floors only)
	}
	for i := range 3 {
		addTask(t, db, "input", "x", day.Add(time.Duration(i)*time.Hour), 2*time.Hour) // response 3
	}
	rep, err := Compute(db, now)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Level != 6 || rep.Focus != AreaResponse { // round((7+7+3)/3) = round(5.67)
		t.Fatalf("overall = %d, focus = %q; want 6 and response", rep.Level, rep.Focus)
	}
}

func TestRecordUsage(t *testing.T) {
	db := openDB(t)
	reset := ms(now.Add(3 * time.Hour))
	snap := usage.Snapshot{Available: true, Accounts: []usage.AccountGroup{
		{Key: "claude:a", Provider: "claude", Windows: []usage.Window{{ID: "session", UsedPercent: 42, ResetAt: &reset}, {ID: "weekly", UsedPercent: 61}}},
		{Key: "gemini:b", Provider: "gemini", Stale: true, Windows: []usage.Window{{ID: "session", UsedPercent: 5}}},
		{Key: "codex:c", Provider: "codex", Error: "unauthenticated", Windows: []usage.Window{{ID: "session", UsedPercent: 5}}},
	}}
	if _, err := db.Exec(`INSERT INTO usage_samples (provider, account, window_id, used_pct, at) VALUES ('claude', 'claude:a', 'session', 1, ?)`, ms(now.Add(-KeepSamples-time.Hour))); err != nil {
		t.Fatal(err)
	}
	count := func() (n int) {
		if err := db.QueryRow(`SELECT COUNT(*) FROM usage_samples`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, at := range []time.Time{now, now.Add(time.Minute)} {
		if err := RecordUsage(db, snap, at); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(); n != 2 { // two windows once; the old sample pruned; stale/failing skipped
		t.Fatalf("samples = %d, want 2", n)
	}
	if err := RecordUsage(db, snap, now.Add(SampleEvery+time.Second)); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 4 {
		t.Fatalf("samples after %s = %d, want 4", SampleEvery, n)
	}
	if err := RecordUsage(db, usage.Snapshot{}, now.Add(time.Hour)); err != nil || count() != 4 {
		t.Fatalf("an unavailable snapshot must record nothing (err %v)", err)
	}
}
