package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/paulo-lucas/maestria/internal/config"
	"github.com/paulo-lucas/maestria/internal/floors"
)

type stubLiveFloors struct {
	byWorkspace map[string][]floors.LiveInfo
}

func (s stubLiveFloors) LiveFloors(workspaceID string) ([]floors.LiveInfo, error) {
	return s.byWorkspace[workspaceID], nil
}

func TestFloorsAndDocsAuthGates(t *testing.T) {
	h := newHarness(t, opts{mutate: func(c *config.Config) {
		c.MaestriDir = filepath.Join(t.TempDir(), "maestri")
	}})
	ws := addTestWorkspace(t, h, "Atlas")

	anon := h.client()
	for _, p := range []string{
		"/workspaces/" + ws.ID + "/floors",
		"/workspaces/" + ws.ID + "/docs",
		"/workspaces/" + ws.ID + "/docs/asset?path=img/arch.png",
	} {
		res := anon.get(p)
		if res.Code != http.StatusSeeOther || res.Header().Get("Location") != "/login" {
			t.Fatalf("unauth GET %s: got %d location %q", p, res.Code, res.Header().Get("Location"))
		}
	}

	for _, ep := range []string{
		"/api/floors",
		"/api/floors?workspace=" + ws.ID,
		"/api/workspaces/" + ws.ID + "/floors",
		"/api/workspaces/" + ws.ID + "/docs",
	} {
		res := anon.get(ep)
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("unauth GET %s: got %d, want 401", ep, res.Code)
		}
	}

	authed := h.client()
	csrf := authed.login()
	if res := authed.postJSON("/api/workspaces/"+ws.ID+"/chat", map[string]string{"body": "hi"}, ""); res.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF got %d, want 403", res.Code)
	}
	if res := authed.postJSON("/api/workspaces/"+ws.ID+"/chat", map[string]string{"body": "hi"}, csrf+"-bad"); res.Code != http.StatusForbidden {
		t.Fatalf("bad CSRF got %d, want 403", res.Code)
	}
}

func TestFloorsPagesOverviewAndAPI(t *testing.T) {
	h := newHarness(t, opts{mutate: func(c *config.Config) {
		c.MaestriDir = filepath.Join(t.TempDir(), "maestri")
	}})
	ws := addTestWorkspace(t, h, "Atlas")
	other := addTestWorkspace(t, h, "Nimbus")

	now := h.clock.now().UnixMilli()
	p05 := 0.5
	if _, err := floors.Plan(h.db, ws.ID, floors.PlanInput{
		Name:    "billing-webhooks",
		Brief:   "Ship webhooks with **tests** and <script>alert(1)</script>.",
		Ongoing: false,
	}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := floors.Start(h.db, ws.ID, "billing-webhooks", floors.StartInput{}, now+1000); err != nil {
		t.Fatal(err)
	}
	if _, err := floors.SetStatus(h.db, ws.ID, "billing-webhooks", floors.StatusInput{
		Status:      floors.StatusReview,
		Text:        "Waiting for contract test signoff",
		TextSet:     true,
		Progress:    &p05,
		ProgressSet: true,
		Agents:      []string{"Klawf", "Vex"},
		AgentsSet:   true,
	}, now+2000); err != nil {
		t.Fatal(err)
	}

	if _, err := floors.Plan(h.db, ws.ID, floors.PlanInput{
		Name:    "continuous-triage",
		Brief:   "Ongoing triage loop.",
		Ongoing: true,
	}, now+3000); err != nil {
		t.Fatal(err)
	}
	if _, err := floors.Start(h.db, ws.ID, "continuous-triage", floors.StartInput{}, now+4000); err != nil {
		t.Fatal(err)
	}

	// Future planned floors, reordered so second comes first ("Next").
	if _, err := floors.Plan(h.db, ws.ID, floors.PlanInput{Name: "plan-alpha", Brief: "First planned."}, now+5000); err != nil {
		t.Fatal(err)
	}
	if _, err := floors.Plan(h.db, ws.ID, floors.PlanInput{Name: "plan-beta", Brief: "Second planned."}, now+6000); err != nil {
		t.Fatal(err)
	}
	if _, err := floors.Reorder(h.db, ws.ID, "plan-beta", "plan-alpha", true, now+7000); err != nil {
		t.Fatal(err)
	}

	// Past landed and cancelled floors.
	if _, err := floors.Plan(h.db, ws.ID, floors.PlanInput{Name: "old-landed", Brief: "Old landed."}, now+8000); err != nil {
		t.Fatal(err)
	}
	if _, err := floors.Start(h.db, ws.ID, "old-landed", floors.StartInput{}, now+8500); err != nil {
		t.Fatal(err)
	}
	if _, err := floors.Land(h.db, ws.ID, "old-landed", "Merged cleanly with <img src=x onerror=alert(1)> zero regressions.", false, now+9000); err != nil {
		t.Fatal(err)
	}

	if _, err := floors.Plan(h.db, ws.ID, floors.PlanInput{Name: "old-cancelled", Brief: "Cancelled."}, now+9500); err != nil {
		t.Fatal(err)
	}
	if _, err := floors.Cancel(h.db, ws.ID, "old-cancelled", "Superseded by v2 design.", now+10000); err != nil {
		t.Fatal(err)
	}

	// Wire live override on continuous-triage + another workspace active floor.
	p09 := 0.9
	if _, err := floors.Plan(h.db, other.ID, floors.PlanInput{Name: "docs-sync", Brief: "Sync docs."}, now+11000); err != nil {
		t.Fatal(err)
	}
	if _, err := floors.Start(h.db, other.ID, "docs-sync", floors.StartInput{}, now+12000); err != nil {
		t.Fatal(err)
	}
	if _, err := floors.SetStatus(h.db, other.ID, "docs-sync", floors.StatusInput{
		Agents:    []string{"Mira"},
		AgentsSet: true,
	}, now+12500); err != nil {
		t.Fatal(err)
	}
	h.srv.LiveFloors = stubLiveFloors{byWorkspace: map[string][]floors.LiveInfo{
		ws.ID: {
			{
				Name:       "continuous-triage",
				Status:     floors.StatusBlocked,
				StatusText: "Live Wire status",
				Agents:     []string{"LiveAgent"},
				Progress:   &p09,
			},
		},
	}}

	c := h.client()
	c.login()

	// Overview includes Active floors across workspaces without placeholder text.
	ov := c.get("/")
	if ov.Code != http.StatusOK {
		t.Fatalf("GET / = %d", ov.Code)
	}
	for _, want := range []string{
		"Active floors",
		"billing-webhooks",
		"continuous-triage",
		"Live Wire status",
		"docs-sync",
		"Review",
		`data-status="blocked"`,
		"Ongoing",
	} {
		if !strings.Contains(ov.body, want) {
			t.Fatalf("overview missing %q", want)
		}
	}
	if strings.Contains(ov.body, "No active floors") {
		t.Fatalf("overview unexpectedly rendered empty active-floors message")
	}

	// Workspace Floors page renders Present, Future in order, Past collapsed, and strict CSP.
	flRes := c.get("/workspaces/" + ws.ID + "/floors")
	if flRes.Code != http.StatusOK {
		t.Fatalf("GET floors = %d", flRes.Code)
	}
	inlineScript := regexp.MustCompile(`(?is)<script\b[^>]*>[^<]+</script>`)
	inlineStyle := regexp.MustCompile(`(?is)\sstyle\s*=`)
	if inlineScript.MatchString(flRes.body) || inlineStyle.MatchString(flRes.body) {
		t.Fatalf("floors page violated strict CSP with inline script or style")
	}
	for _, want := range []string{
		"Planned by the Maestro",
		"billing-webhooks",
		"Waiting for contract test signoff",
		"50%",
		"continuous-triage",
		"Live Wire status",
		"plan-beta",
		"plan-alpha",
		"Next",
		"Planned",
		"old-landed",
		"Landed",
		"Merged cleanly with",
		"old-cancelled",
		"Cancelled",
		"Superseded by v2 design.",
	} {
		if !strings.Contains(flRes.body, want) {
			t.Fatalf("floors page missing %q", want)
		}
	}
	for _, forbidden := range []string{"<script>alert(1)</script>", "onerror=alert(1)"} {
		if strings.Contains(flRes.body, forbidden) {
			t.Fatalf("floors page leaked unsanitized markup %q", forbidden)
		}
	}
	// Ongoing floor renders ongoing progress indicator instead of percentage bar even if LiveInfo carried progress.
	if strings.Contains(flRes.body, "90%") {
		t.Fatalf("ongoing floor should not render bounded percentage progress")
	}
	// Reordered Future floor order: plan-beta must appear before plan-alpha.
	if idxBeta, idxAlpha := strings.Index(flRes.body, "plan-beta"), strings.Index(flRes.body, "plan-alpha"); idxBeta < 0 || idxAlpha < 0 || idxBeta > idxAlpha {
		t.Fatalf("future floor order wrong: plan-beta=%d plan-alpha=%d", idxBeta, idxAlpha)
	}

	// API endpoints return live-merged JSON.
	apiAll := c.get("/api/floors")
	if apiAll.Code != http.StatusOK {
		t.Fatalf("GET /api/floors = %d", apiAll.Code)
	}
	var allPayload struct {
		Floors []struct {
			Name          string `json:"name"`
			WorkspaceName string `json:"workspace_name"`
			StatusText    string `json:"statusText"`
		} `json:"floors"`
	}
	if err := json.Unmarshal([]byte(apiAll.body), &allPayload); err != nil {
		t.Fatal(err)
	}
	if len(allPayload.Floors) != 3 {
		t.Fatalf("expected 3 active floors across workspaces, got %d (%+v)", len(allPayload.Floors), allPayload.Floors)
	}

	apiWS := c.get("/api/workspaces/" + ws.ID + "/floors")
	if apiWS.Code != http.StatusOK {
		t.Fatalf("GET /api/workspaces/%s/floors = %d", ws.ID, apiWS.Code)
	}
	var wsPayload struct {
		Present []floors.Floor `json:"present"`
		Future  []floors.Floor `json:"future"`
		Past    []floors.Floor `json:"past"`
	}
	if err := json.Unmarshal([]byte(apiWS.body), &wsPayload); err != nil {
		t.Fatal(err)
	}
	if len(wsPayload.Present) != 2 || len(wsPayload.Future) != 2 || len(wsPayload.Past) != 2 {
		t.Fatalf("unexpected counts present=%d future=%d past=%d", len(wsPayload.Present), len(wsPayload.Future), len(wsPayload.Past))
	}
	if wsPayload.Future[0].Name != "plan-beta" || wsPayload.Future[1].Name != "plan-alpha" {
		t.Fatalf("API future order = %v, %v", wsPayload.Future[0].Name, wsPayload.Future[1].Name)
	}

	// Missing workspace returns 404.
	if res := c.get("/api/workspaces/does-not-exist/floors"); res.Code != http.StatusNotFound {
		t.Fatalf("missing workspace API = %d, want 404", res.Code)
	}
}

func TestSSEStreamDeliversFloorUpdates(t *testing.T) {
	h := newHarness(t, opts{mutate: func(c *config.Config) {
		c.MaestriDir = filepath.Join(t.TempDir(), "maestri")
	}})
	h.srv.ssePollInterval = 40 * time.Millisecond
	ws := addTestWorkspace(t, h, "Atlas")

	c := h.client()
	c.login()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pr, pw := newPipeResponseWriter()
	req := httptest.NewRequest(http.MethodGet, "http://"+testHost+"/api/stream", nil).WithContext(ctx)
	req.Host = testHost
	req.RemoteAddr = c.ip
	for _, ck := range c.jar.Cookies(req.URL) {
		req.AddCookie(ck)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.srv.ServeHTTP(pw, req)
	}()

	reader := bufio.NewReader(pr)
	line1, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(line1, "event: ready") {
		t.Fatalf("expected event: ready, got %q err=%v", line1, err)
	}

	// Out-of-band floor lifecycle writes (even with frozen clock!) trigger SSE updates.
	now := h.clock.now().UnixMilli()
	if _, err := floors.Plan(h.db, ws.ID, floors.PlanInput{Name: "sse-floor", Brief: "Watch SSE."}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := floors.Start(h.db, ws.ID, "sse-floor", floors.StartInput{}, now); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(2 * time.Second)
	foundUpdate := false
	for !foundUpdate {
		lineCh := make(chan string, 1)
		go func() {
			s, _ := reader.ReadString('\n')
			lineCh <- s
		}()
		select {
		case <-deadline:
			t.Fatal("timed out waiting for SSE floor creation update event")
		case l := <-lineCh:
			if strings.Contains(l, "event: update") {
				foundUpdate = true
			}
		}
	}

	// Subsequent update with identical frozen timestamp still triggers SSE via rev increment.
	if _, err := floors.SetStatus(h.db, ws.ID, "sse-floor", floors.StatusInput{
		Status:  floors.StatusReview,
		Text:    "Ready to verify",
		TextSet: true,
	}, now); err != nil {
		t.Fatal(err)
	}
	deadline2 := time.After(2 * time.Second)
	foundRevUpdate := false
	for !foundRevUpdate {
		lineCh := make(chan string, 1)
		go func() {
			s, _ := reader.ReadString('\n')
			lineCh <- s
		}()
		select {
		case <-deadline2:
			t.Fatal("timed out waiting for SSE floor rev update event")
		case l := <-lineCh:
			if strings.Contains(l, "event: update") {
				foundRevUpdate = true
			}
		}
	}

	cancel()
	_ = pw.Close()
	<-done
}

func TestDocsPagesAPIAndSecurity(t *testing.T) {
	h := newHarness(t, opts{mutate: func(c *config.Config) {
		c.MaestriDir = filepath.Join(t.TempDir(), "maestri")
	}})
	ws := addTestWorkspace(t, h, "Atlas")

	// Fallback README at workspace root, docs/ subfiles & image asset, plus outside secret for symlink escape test.
	if err := os.WriteFile(filepath.Join(ws.Path, "README.md"), []byte("# Root Overview\n\nSee [Guide](docs/guide.md) and ![Diagram](docs/img/arch.png).\n\n| Col | Val |\n| --- | --- |\n| a | 1 |\n\n<script>alert(1)</script><img src=x onerror=\"alert(2)\" style=\"color:red\">\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws.Path, "docs", "img"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Path, "docs", "guide.md"), []byte("# Guide\n\nBody text.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 1x1 PNG bytes.
	pngBytes := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 13, 'I', 'H', 'D', 'R', 0, 0, 0, 1, 0, 0, 0, 1, 8, 2, 0, 0, 0, 0x90, 0x77, 0x53, 0xde}
	if err := os.WriteFile(filepath.Join(ws.Path, "docs", "img", "arch.png"), pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Path, "docs", "img", "bad.svg"), []byte("<svg onload=alert(1)></svg>"), 0o644); err != nil {
		t.Fatal(err)
	}
	outsideFile := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outsideFile, []byte("# Secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(ws.Path, "docs", "escape.md")); err != nil {
		t.Fatal(err)
	}

	c := h.client()
	c.login()

	// Docs HTML page renders README fallback, GFM table, rewritten link & asset URL, and keeps strict CSP.
	res := c.get("/workspaces/" + ws.ID + "/docs")
	if res.Code != http.StatusOK {
		t.Fatalf("GET docs = %d", res.Code)
	}
	inlineScript := regexp.MustCompile(`(?is)<script\b[^>]*>[^<]+</script>`)
	inlineStyle := regexp.MustCompile(`(?is)\sstyle\s*=`)
	if inlineScript.MatchString(res.body) || inlineStyle.MatchString(res.body) {
		t.Fatalf("docs page violated strict CSP with inline script or style")
	}
	for _, want := range []string{
		"Managed by the Maestro",
		"README.md",
		"guide.md",
		"Root Overview",
		"<table>",
		"/workspaces/" + ws.ID + "/docs?doc=guide.md",
		"/workspaces/" + ws.ID + "/docs/asset?path=img%2Farch.png",
	} {
		if !strings.Contains(res.body, want) {
			t.Fatalf("docs page missing %q", want)
		}
	}
	for _, forbidden := range []string{"<script>", "onerror=", "secret.md", "escape.md"} {
		if strings.Contains(res.body, forbidden) {
			t.Fatalf("docs page leaked %q", forbidden)
		}
	}

	// Docs JSON API & ETag 304.
	apiRes := c.get("/api/workspaces/" + ws.ID + "/docs?doc=guide.md")
	if apiRes.Code != http.StatusOK {
		t.Fatalf("GET /api/workspaces/%s/docs = %d", ws.ID, apiRes.Code)
	}
	etag := apiRes.Header().Get("ETag")
	if etag == "" {
		t.Fatalf("expected non-empty ETag header on docs API")
	}
	apiNotMod := c.do(http.MethodGet, "/api/workspaces/"+ws.ID+"/docs?doc=guide.md", nil, map[string]string{
		"If-None-Match": etag,
	})
	if apiNotMod.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match got %d, want 304", apiNotMod.Code)
	}

	// Image asset endpoint serves allowlisted PNG with ETag / 304.
	assetRes := c.get("/workspaces/" + ws.ID + "/docs/asset?path=img%2Farch.png")
	if assetRes.Code != http.StatusOK {
		t.Fatalf("GET asset = %d", assetRes.Code)
	}
	assetETag := assetRes.Header().Get("ETag")
	if assetETag == "" {
		t.Fatalf("expected ETag on docs asset")
	}
	asset304 := c.do(http.MethodGet, "/workspaces/"+ws.ID+"/docs/asset?path=img%2Farch.png", nil, map[string]string{
		"If-None-Match": assetETag,
	})
	if asset304.Code != http.StatusNotModified {
		t.Fatalf("asset If-None-Match got %d, want 304", asset304.Code)
	}

	// Security rejections: .svg blocked, path traversal & encoded traversal blocked, symlink escape blocked, non-.md doc blocked.
	for _, bad := range []struct {
		url  string
		want int
	}{
		{"/workspaces/" + ws.ID + "/docs/asset?path=img%2Fbad.svg", http.StatusBadRequest},
		{"/workspaces/" + ws.ID + "/docs/asset?path=../secret.png", http.StatusBadRequest},
		{"/workspaces/" + ws.ID + "/docs/asset?path=%2e%2e/secret.png", http.StatusBadRequest},
		{"/api/workspaces/" + ws.ID + "/docs?doc=../secret.md", http.StatusBadRequest},
		{"/api/workspaces/" + ws.ID + "/docs?doc=%2e%2e%2fsecret.md", http.StatusBadRequest},
		{"/api/workspaces/" + ws.ID + "/docs?doc=img/arch.png", http.StatusBadRequest},
		{"/api/workspaces/" + ws.ID + "/docs?doc=escape.md", http.StatusBadRequest},
	} {
		r := c.get(bad.url)
		if r.Code != bad.want {
			t.Fatalf("GET %s = %d, want %d (body=%s)", bad.url, r.Code, bad.want, r.body)
		}
	}
}
