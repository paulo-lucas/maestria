package web

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/maestria-dev/maestria/internal/chat"
	"github.com/maestria-dev/maestria/internal/config"
	"github.com/maestria-dev/maestria/internal/inbox"
)

func TestConnectorPagesRequireAuthAndStrictCSP(t *testing.T) {
	h := newHarness(t, opts{mutate: func(c *config.Config) {
		c.MaestriDir = filepath.Join(t.TempDir(), "maestri")
	}})
	ws := addTestWorkspace(t, h, "Atlas")

	routes := []string{
		"/",
		"/usage",
		"/security",
		"/workspaces/" + ws.ID + "/tasks",
		"/workspaces/" + ws.ID + "/chat",
		"/workspaces/" + ws.ID + "/activity",
		"/workspaces/" + ws.ID + "/floors",
		"/workspaces/" + ws.ID + "/docs",
	}

	// Unauthenticated HTML requests redirect to /login.
	anon := h.client()
	for _, p := range routes {
		res := anon.get(p)
		if res.Code != http.StatusSeeOther || res.Header().Get("Location") != "/login" {
			t.Fatalf("unauth GET %s: got %d location %q", p, res.Code, res.Header().Get("Location"))
		}
	}

	authed := h.client()
	authed.login()
	inlineScript := regexp.MustCompile(`(?is)<script\b[^>]*>[^<]+</script>`)
	inlineStyle := regexp.MustCompile(`(?is)\sstyle\s*=`)

	for _, p := range routes {
		res := authed.get(p)
		if res.Code != http.StatusOK {
			t.Fatalf("auth GET %s: got %d", p, res.Code)
		}
		if got := res.Header().Get("Content-Security-Policy"); got != CSP {
			t.Fatalf("GET %s CSP = %q, want %q", p, got, CSP)
		}
		if !strings.Contains(res.body, `/static/panel.js?v=`) {
			t.Fatalf("GET %s missing versioned panel.js script tag", p)
		}
		if inlineScript.MatchString(res.body) {
			t.Fatalf("GET %s contains inline <script> under strict CSP", p)
		}
		if inlineStyle.MatchString(res.body) {
			t.Fatalf("GET %s contains inline style= attribute under strict CSP", p)
		}
	}

	// Missing workspace returns 404.
	if res := authed.get("/workspaces/nonexistent/tasks"); res.Code != http.StatusNotFound {
		t.Fatalf("missing workspace status = %d, want 404", res.Code)
	}
}

func TestConnectorPagesRenderOverviewTasksChatAndUsage(t *testing.T) {
	h := newHarness(t, opts{mutate: func(c *config.Config) {
		c.MaestriDir = filepath.Join(t.TempDir(), "maestri")
	}})
	ws := addTestWorkspace(t, h, "Atlas")
	other := addTestWorkspace(t, h, "Nimbus")

	nowMs := h.clock.now().UnixMilli()
	if _, err := inbox.Post(h.srv.DB, ws.ID, inbox.PostInput{
		Kind: inbox.KindApprove,
		Body: "Land `billing-webhooks` into main?\nAll **214 tests** pass <script>xss()</script>.",
	}, nowMs-60_000); err != nil {
		t.Fatal(err)
	}
	if _, err := inbox.Post(h.srv.DB, ws.ID, inbox.PostInput{
		Kind:    inbox.KindChoice,
		Body:    "Pick default format",
		Options: []string{"JSON", "CSV"},
	}, nowMs-50_000); err != nil {
		t.Fatal(err)
	}
	if _, err := inbox.Post(h.srv.DB, ws.ID, inbox.PostInput{
		Kind: inbox.KindInput,
		Body: "Staging base URL?",
	}, nowMs-40_000); err != nil {
		t.Fatal(err)
	}
	laterItem, err := inbox.Post(h.srv.DB, ws.ID, inbox.PostInput{
		Kind: inbox.KindApprove,
		Body: "Rotate staging API key?",
	}, nowMs-30_000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inbox.Postpone(h.srv.DB, ws.ID, laterItem.ID, nowMs+3600_000, nowMs); err != nil {
		t.Fatal(err)
	}
	if _, err := inbox.Post(h.srv.DB, ws.ID, inbox.PostInput{
		Kind: inbox.KindNotice,
		Body: "PDF exporter ready",
	}, nowMs-20_000); err != nil {
		t.Fatal(err)
	}
	histItem, err := inbox.Post(h.srv.DB, ws.ID, inbox.PostInput{
		Kind: inbox.KindApprove,
		Body: "Archive old snapshots?",
	}, nowMs-90_000)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := inbox.RespondWithComment(h.srv.DB, ws.ID, histItem.ID, "approve", "Keep 7 days", nowMs-10_000); err != nil {
		t.Fatal(err)
	}
	if _, err := inbox.Post(h.srv.DB, other.ID, inbox.PostInput{
		Kind: inbox.KindInput,
		Body: "Release tag?",
	}, nowMs-10_000); err != nil {
		t.Fatal(err)
	}

	if _, _, err := chat.Send(h.srv.DB, ws.ID, chat.SenderMaestro, "Ready for **review** <script>bad()</script>", nowMs-15_000); err != nil {
		t.Fatal(err)
	}
	if _, _, err := chat.Send(h.srv.DB, ws.ID, chat.SenderHuman, "Looks good, carry on.", nowMs-5_000); err != nil {
		t.Fatal(err)
	}

	// Write usage file with an armed threshold that trips standby plus one hidden errored provider.
	pdir := filepath.Join(h.srv.Config.MaestriDir, "usage", "providers")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{
		"updatedAt": h.clock.now().Format(time.RFC3339),
		"providers": map[string]any{
			"claude": map[string]any{
				"state": "ready",
				"plan":  "Max",
				"fiveHour": map[string]any{
					"usedPercent": 93,
					"resetsAt":    h.clock.now().Add(2 * time.Hour).Format(time.RFC3339),
				},
				"sevenDay": map[string]any{
					"usedPercent": 41,
					"resetsAt":    h.clock.now().Add(48 * time.Hour).Format(time.RFC3339),
				},
			},
			"copilot": map[string]any{
				"state": "error",
				"error": "token expired",
			},
		},
	}
	b, _ := json.Marshal(payload)
	if err := os.WriteFile(filepath.Join(pdir, ".status.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	h.srv.cfgMu.Lock()
	h.srv.Config.Standby = config.Standby{
		Thresholds:  map[string]map[string]int{"claude": {"session": 90}},
		Actions:     map[string]map[string]string{"claude": {"session": "tell"}},
		WakeOnReset: true,
	}
	h.srv.cfgMu.Unlock()

	authed := h.client()
	authed.login()

	// 1. Overview
	ovRes := authed.get("/")
	if ovRes.Code != http.StatusOK {
		t.Fatalf("GET / status = %d", ovRes.Code)
	}
	for _, want := range []string{
		"Atlas", "Nimbus",
		"Land <code>billing-webhooks</code> into main?",
		"<strong>214 tests</strong>",
		"Pick default format", "Staging base URL?",
		"Live agent stream comes with Wire",
		"Floor status comes with Wire",
		"Claude", "93%", "mark-90",
		"1 provider hidden: no data (error)",
	} {
		if !strings.Contains(ovRes.body, want) {
			t.Fatalf("GET / missing %q", want)
		}
	}
	if strings.Contains(ovRes.body, "<script>xss()") {
		t.Fatalf("GET / leaked raw script tag from markdown")
	}

	// 2. Workspace Human tasks
	taskRes := authed.get("/workspaces/" + ws.ID + "/tasks")
	if taskRes.Code != http.StatusOK {
		t.Fatalf("GET tasks status = %d", taskRes.Code)
	}
	for _, want := range []string{
		"Land <code>billing-webhooks</code> into main?",
		"In 1 hour", "This evening, 18:00", "Tomorrow, 09:00",
		"Postponed", "Rotate staging API key?", "Bring back",
		"PDF exporter ready", "Mark all as read", "Got it",
		"History", "Archive old snapshots?", "Keep 7 days",
	} {
		if !strings.Contains(taskRes.body, want) {
			t.Fatalf("GET tasks missing %q", want)
		}
	}

	// 3. Workspace Chat
	chatRes := authed.get("/workspaces/" + ws.ID + "/chat")
	if chatRes.Code != http.StatusOK {
		t.Fatalf("GET chat status = %d", chatRes.Code)
	}
	for _, want := range []string{
		"Today", "<strong>review</strong>", "Looks good, carry on.", `data-chat-form`,
	} {
		if !strings.Contains(chatRes.body, want) {
			t.Fatalf("GET chat missing %q", want)
		}
	}
	if strings.Contains(chatRes.body, "<script>bad()") {
		t.Fatalf("GET chat leaked raw script tag from markdown")
	}

	// 4. Usage & standby
	usageRes := authed.get("/usage")
	if usageRes.Code != http.StatusOK {
		t.Fatalf("GET /usage status = %d", usageRes.Code)
	}
	for _, want := range []string{
		"Standby", "Resume now",
		"Claude", "Session", "Weekly", "mark-90",
		"Hidden providers", "Copilot", "token expired",
	} {
		if !strings.Contains(usageRes.body, want) {
			t.Fatalf("GET /usage missing %q", want)
		}
	}

	// 5. Coming-soon placeholder preserves nav and workspace scope.
	actRes := authed.get("/workspaces/" + ws.ID + "/activity")
	if actRes.Code != http.StatusOK {
		t.Fatalf("GET activity status = %d", actRes.Code)
	}
	if !strings.Contains(actRes.body, "Coming soon") {
		t.Fatalf("GET activity missing Coming soon placeholder")
	}

	// 6. Verify embedded panel.js serves with JS content-type.
	jsRes := authed.get("/static/panel.js")
	if jsRes.Code != http.StatusOK || !strings.Contains(jsRes.Header().Get("Content-Type"), "javascript") {
		t.Fatalf("GET /static/panel.js = %d ct=%q", jsRes.Code, jsRes.Header().Get("Content-Type"))
	}
}
