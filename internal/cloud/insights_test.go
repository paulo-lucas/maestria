package cloud

import (
	"strings"
	"testing"
)

// A cold load of the Insights page (before hosted.js runs) already has its own title and the
// Insights item selected, for /insights and for one machine's /c/{id}/insights.
func TestInsightsShellOnColdLoad(t *testing.T) {
	_, admin, _ := adminBed(t)
	for _, path := range []string{"/insights", "/c/cn_" + strings.Repeat("a", 32) + "/insights"} {
		r := admin.get(path)
		if r.StatusCode != 200 {
			t.Fatalf("GET %s = %d", path, r.StatusCode)
		}
		if !strings.Contains(r.body, "<title>Insights · Maestria</title>") {
			t.Errorf("GET %s: title is not Insights", path)
		}
		if !strings.Contains(r.body, `data-nav="insights" aria-current="page"`) || strings.Contains(r.body, `data-nav="overview" aria-current="page"`) {
			t.Errorf("GET %s: Insights is not the selected nav item", path)
		}
	}
	// A workspace that happens to be called "insights" is still a workspace page.
	if r := admin.get("/c/cn_" + strings.Repeat("a", 32) + "/workspaces/insights/tasks"); strings.Contains(r.body, "<title>Insights · Maestria</title>") {
		t.Error("a workspace page was taken for the Insights page")
	}
}
