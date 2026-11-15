package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/paulo-lucas/maestria/internal/insights"
	"github.com/paulo-lucas/maestria/internal/usage"
)

func addLandedFloors(t *testing.T, h *harness, names ...string) {
	t.Helper()
	start := h.clock.now().Add(-24 * time.Hour).UnixMilli()
	for _, name := range names {
		if _, err := h.db.Exec(`INSERT INTO floors (workspace_id, name, state, started_at, ended_at, updated_at) VALUES ('atlas', ?, 'landed', ?, ?, ?)`,
			name, start, start+2*3600*1000, start); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInsightsPageAndAPI(t *testing.T) {
	h := newHarness(t)
	addTestWorkspace(t, h, "Atlas")
	addLandedFloors(t, h, "a", "b", "c")

	// Signed out: the page sends you to the login, the API refuses.
	out := h.client()
	if r := out.get("/insights"); r.Code != http.StatusSeeOther || r.Header().Get("Location") != "/login" {
		t.Fatalf("signed-out GET /insights = %d to %q, want 303 to /login", r.Code, r.Header().Get("Location"))
	}
	if r := out.get("/api/insights"); r.Code != http.StatusUnauthorized {
		t.Fatalf("signed-out GET /api/insights = %d, want 401", r.Code)
	}

	c := h.client()
	c.login()
	page := c.get("/insights")
	if page.Code != http.StatusOK {
		t.Fatalf("GET /insights = %d", page.Code)
	}
	for _, want := range []string{`href="/insights" aria-current="page"`, "Delivery speed", "7 of 7", "Your response time", "Not rated yet", "docs/insights.md"} {
		if !strings.Contains(page.body, want) {
			t.Errorf("insights page lacks %q", want)
		}
	}

	api := c.get("/api/insights")
	var rep insights.Report
	if api.Code != http.StatusOK || json.Unmarshal([]byte(api.body), &rep) != nil || len(rep.Areas) != 4 {
		t.Fatalf("GET /api/insights = %d %.200s", api.Code, api.body)
	}
	if rep.Areas[0].ID != insights.AreaDelivery || rep.Areas[0].Level != 7 {
		t.Errorf("delivery = %+v, want level 7 for three 2 h floors", rep.Areas[0])
	}
}

// Usage & standby and Insights share an underline tab bar, each page marking itself; the bottom
// nav keeps Usage selected on both.
func TestUsageAndInsightsTabs(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	c.login()
	for _, p := range []struct{ path, current, other string }{
		{"/usage", `<a class="tab" href="/usage" aria-current="page">`, `<a class="tab" href="/insights">`},
		{"/insights", `<a class="tab" href="/insights" aria-current="page">`, `<a class="tab" href="/usage">`},
	} {
		body := c.get(p.path).body
		if !strings.Contains(body, `<nav class="tabs"`) || !strings.Contains(body, p.current) || !strings.Contains(body, p.other) {
			t.Errorf("%s: tab bar missing or wrong tab selected", p.path)
		}
		if !strings.Contains(body, `class="bottomnav__item" href="/usage" aria-current="page"`) {
			t.Errorf("%s: Usage is not selected in the bottom nav", p.path)
		}
	}
	if strings.Contains(c.get("/usage").body, `class="btn btn--sm" href="/insights"`) {
		t.Error("the old Insights button is still in the Usage header")
	}
}

// The report is worked out at most once a minute.
func TestInsightsReportIsCachedForAMinute(t *testing.T) {
	h := newHarness(t)
	addTestWorkspace(t, h, "Atlas")
	first, err := h.srv.insightsReport()
	if err != nil || first.Areas[0].Level != 0 {
		t.Fatalf("first report: %+v, %v", first.Areas[0], err)
	}
	addLandedFloors(t, h, "a", "b", "c")
	if again, _ := h.srv.insightsReport(); again.Areas[0].Level != 0 {
		t.Fatal("report recomputed within the minute")
	}
	h.clock.advance(insightsCacheFor + time.Second)
	if later, _ := h.srv.insightsReport(); later.Areas[0].Level != 7 {
		t.Fatalf("report not refreshed after a minute: %+v", later.Areas[0])
	}
}

// A failing usage sample is logged, but at most once an hour.
func TestUsageSampleFailureIsLoggedOnceAnHour(t *testing.T) {
	h := newHarness(t)
	var log bytes.Buffer
	h.srv.Log = &log
	if _, err := h.db.Exec(`DROP TABLE usage_samples`); err != nil {
		t.Fatal(err)
	}
	snap := usage.Snapshot{Available: true, Accounts: []usage.AccountGroup{{Key: "k", Provider: "claude", Windows: []usage.Window{{ID: "session", UsedPercent: 1}}}}}
	h.srv.recordUsage(snap)
	h.clock.advance(10 * time.Minute)
	h.srv.recordUsage(snap)
	if n := strings.Count(log.String(), "Insights could not record a usage sample"); n != 1 {
		t.Fatalf("logged %d times within the hour, want 1:\n%s", n, log.String())
	}
	h.clock.advance(time.Hour)
	h.srv.recordUsage(snap)
	if n := strings.Count(log.String(), "Insights could not record a usage sample"); n != 2 {
		t.Fatalf("logged %d times after an hour, want 2", n)
	}
	// A clock that steps back does not mute the warning until it catches up.
	h.clock.advance(-3 * time.Hour)
	h.srv.recordUsage(snap)
	if n := strings.Count(log.String(), "Insights could not record a usage sample"); n != 3 {
		t.Fatalf("logged %d times after the clock stepped back, want 3", n)
	}
}
