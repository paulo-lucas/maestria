package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/paulo-lucas/maestria/internal/insights"
)

func TestInsightsPageAndAPI(t *testing.T) {
	h := newHarness(t)
	addTestWorkspace(t, h, "Atlas")
	start := h.clock.now().Add(-24 * time.Hour).UnixMilli()
	for _, name := range []string{"a", "b", "c"} {
		if _, err := h.db.Exec(`INSERT INTO floors (workspace_id, name, state, started_at, ended_at, updated_at) VALUES ('atlas', ?, 'landed', ?, ?, ?)`,
			name, start, start+2*3600*1000, start); err != nil {
			t.Fatal(err)
		}
	}

	// Signed out: no page, no data.
	if r := h.client().get("/insights"); r.Code == http.StatusOK {
		t.Fatalf("insights served without a session")
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

	// The usage page links here, so the page is reachable on a phone (no bottom-nav slot).
	if !strings.Contains(c.get("/usage").body, `href="/insights"`) {
		t.Error("usage page does not link to insights")
	}
}
