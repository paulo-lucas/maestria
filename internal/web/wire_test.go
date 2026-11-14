package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maestria-dev/maestria/internal/activity"
	"github.com/maestria-dev/maestria/internal/audit"
	"github.com/maestria-dev/maestria/internal/chat"
	"github.com/maestria-dev/maestria/internal/config"
	"github.com/maestria-dev/maestria/internal/floors"
	"github.com/maestria-dev/maestria/internal/inbox"
	"github.com/maestria-dev/maestria/internal/wire"
	"github.com/maestria-dev/maestria/internal/wire/wiretest"
	"github.com/maestria-dev/maestria/internal/workspaces"
)

const (
	wWS    = "11111111-1111-4111-8111-111111111111"
	wTermM = "22222222-2222-4222-8222-222222222221"
	wTermO = "22222222-2222-4222-8222-222222222222"
	wTermV = "22222222-2222-4222-8222-222222222223"
	wFloor = "44444444-4444-4444-8444-444444444441"
)

type wireRig struct {
	*harness
	fake  *wiretest.Server
	ws    workspaces.Workspace
	creds *wire.Credentials
	csrf  string
	c     *client
}

func wTerm(id, name, agent, floorID, floorName string) wire.TerminalCard {
	return wire.TerminalCard{ID: id, Name: name, AgentType: agent, FloorID: floorID, FloorName: floorName, IsRunning: true, IsActive: true,
		Preview: []string{"$ pnpm test", "\x1b[32m✓\x1b[0m 12 passed", "<script>alert(1)</script>"}}
}

func wFeed(prompt string, maestroRunning bool) wire.FeedSnapshot {
	maestro := wTerm(wTermM, "Maestro", "claude", "", "")
	maestro.IsManager = true
	maestro.IsRunning = maestroRunning
	orin := wTerm(wTermO, "Orin", "gemini", wFloor, "billing-webhooks")
	vesper := wTerm(wTermV, "Vesper", "claude", wFloor, "billing-webhooks")
	items := []wire.FeedItem{{Kind: wire.ItemTerminal, Terminal: &maestro}, {Kind: wire.ItemTerminal, Terminal: &vesper}}
	if prompt != "" {
		items = append(items, wire.FeedItem{Kind: wire.ItemPendingPrompt, Terminal: &orin, Prompt: prompt})
	} else {
		items = append(items, wire.FeedItem{Kind: wire.ItemTerminal, Terminal: &orin})
	}
	p := 0.85
	return wire.FeedSnapshot{
		Workspace: wire.WorkspaceMeta{ID: wWS, Name: "Atlas", IsLoaded: true},
		Floors: []wire.FloorMeta{
			{Name: "main", IsActive: true},
			{ID: wFloor, Name: "billing-webhooks", Branch: "billing-webhooks", Status: &wire.FloorStatus{State: "review", Text: "PR open", Progress: &p}},
		},
		Items: items,
	}
}

// newWireRig builds a panel with the Wire service on top of a fake Maestri.
func newWireRig(t *testing.T) *wireRig {
	t.Helper()
	fake, err := wiretest.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	fake.SetPassword("pw")
	creds, err := wire.Provision(context.Background(), fake.Host, fake.Pin(), "T", "pw", nil)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, opts{
		mutate: func(c *config.Config) { c.MaestriDir = filepath.Join(t.TempDir(), "maestri") },
		activity: func(cfg *config.Config, db *sql.DB, aud *audit.Log) *activity.Service {
			if err := creds.Save(wire.Path(cfg.ConfigDir)); err != nil {
				t.Fatal(err)
			}
			return activity.New(activity.Options{
				DB: db, Audit: aud, ConfigDir: cfg.ConfigDir, Poll: 30 * time.Millisecond, MaxBackoff: 60 * time.Millisecond,
				Workspaces: func() []workspaces.Workspace { l, _ := workspaces.Merged(cfg.ConfigDir, cfg.MaestriDir, ""); return l },
			})
		},
	})
	ws := addTestWorkspace(t, h, "Atlas")
	fake.SetWorkspaces(wire.WorkspaceMeta{ID: wWS, Name: "Atlas", IsLoaded: true, WorkingDirectory: ws.Path})
	fake.SetFeed(wWS, wFeed("", true))
	fake.SetChat(wTermM, wire.ChatSnapshot{TerminalID: wTermM, Enabled: true, Status: "idle", Entries: []wire.ChatEntry{
		{ID: "e1", Role: "user", Kind: "message", Text: "How is billing going?", TS: json.RawMessage(`1731585000`)},
		{ID: "e2", Role: "agent", Kind: "message", Text: "**Almost done.** Tests are green.", TS: json.RawMessage(`"2026-11-14T13:52:00Z"`)},
		{ID: "e3", Role: "agent", Kind: "progress", Text: "thinking about it"},
	}})
	ctx, cancel := context.WithCancel(context.Background())
	// Let the background goroutines finish before the database and temp dirs are removed.
	t.Cleanup(func() { cancel(); time.Sleep(150 * time.Millisecond) })
	h.srv.Activity.Start(ctx)
	waitUntil(t, "wire live", func() bool { _, err := h.srv.Activity.Mgr.Feed(ws.ID); return err == nil })
	fake.ClearRequests()
	c := h.client()
	csrf := c.login()
	return &wireRig{harness: h, fake: fake, ws: ws, creds: creds, csrf: csrf, c: c}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (r *wireRig) setPrompt(prompt string) {
	r.fake.SetFeed(wWS, wFeed(prompt, true))
}

func (r *wireRig) agentPrompts() []inbox.Item {
	items, _ := inbox.List(r.db, inbox.ListOptions{WorkspaceID: r.ws.ID, Status: "open", Kind: inbox.KindAgentPrompt})
	return items
}

func (r *wireRig) auditHas(event, contains string) bool {
	entries, _ := audit.New(r.db, nil).Recent(200)
	for _, e := range entries {
		if e.Event == event && strings.Contains(e.Detail, contains) {
			return true
		}
	}
	return false
}

func TestActivityPageShowsAgentsByFloorWithBriefsAndEscapedPreviews(t *testing.T) {
	r := newWireRig(t)
	if err := activity.SetBrief(r.db, r.ws.ID, "Vesper", "Writing retry tests", 0); err != nil {
		t.Fatal(err)
	}
	r.setPrompt("Install zod@3.23.8? (Y/n)")
	waitUntil(t, "prompt mirrored", func() bool { return len(r.agentPrompts()) == 1 })

	page := r.c.get("/workspaces/" + r.ws.ID + "/activity")
	if page.Code != 200 {
		t.Fatalf("status %d", page.Code)
	}
	for _, want := range []string{
		"billing-webhooks", "Review", "Maestro", "Orin", "Vesper", "Writing retry tests",
		"Waiting on Y/n", "Install zod@3.23.8? (Y/n)", `data-action="agent-answer"`, "Terminal preview", "12 passed",
		"Default floor", "Open chat", `data-filter="agents"`,
	} {
		if !strings.Contains(page.body, want) {
			t.Errorf("activity page missing %q", want)
		}
	}
	// The floor that needs you is listed before the default floor.
	if strings.Index(page.body, "billing-webhooks") > strings.Index(page.body, "Default floor") {
		t.Error("floor needing you must come first")
	}
	// Terminal output is escaped and escape codes are gone.
	if strings.Contains(page.body, "<script>alert(1)") || strings.Contains(page.body, "\x1b[") {
		t.Fatal("terminal preview was not sanitized")
	}
	if !strings.Contains(page.body, "&lt;script&gt;alert(1)") {
		t.Error("preview should show the escaped text")
	}
	// JSON twin.
	api := r.c.get("/api/workspaces/" + r.ws.ID + "/activity")
	if api.Code != 200 || !strings.Contains(api.body, `"promptHash"`) {
		t.Fatalf("api = %d %s", api.Code, api.body)
	}
}

func TestOverviewAgentsAtWorkListsWorkingAndWaitingAgents(t *testing.T) {
	r := newWireRig(t)
	r.setPrompt("Run it? (Y/n)")
	waitUntil(t, "prompt", func() bool { return len(r.agentPrompts()) == 1 })
	page := r.c.get("/")
	for _, want := range []string{"Agents at work", "Orin", "Vesper", "Atlas", "billing-webhooks", "1 waiting", "Waiting on Y/n"} {
		if !strings.Contains(page.body, want) {
			t.Errorf("overview missing %q", want)
		}
	}
	if strings.Contains(page.body, "Live agent stream comes with Wire") {
		t.Error("placeholder still there")
	}
	// The Needs you list carries the Agent prompt.
	if !strings.Contains(page.body, "Agent prompt") || !strings.Contains(page.body, "Run it? (Y/n)") {
		t.Errorf("Needs you is missing the agent prompt")
	}
}

func TestApproveAndRejectNeedCSRFAndAreAudited(t *testing.T) {
	r := newWireRig(t)
	r.setPrompt("Install zod? (Y/n)")
	waitUntil(t, "prompt", func() bool { return len(r.agentPrompts()) == 1 })
	hash := wire.PromptHash("Install zod? (Y/n)")
	url := "/api/workspaces/" + r.ws.ID + "/agents/" + wTermO + "/approve"

	var mu sync.Mutex
	var answers []bool
	r.fake.OnAnswer = func(id string, approve bool) { mu.Lock(); answers = append(answers, approve); mu.Unlock() }

	// No CSRF token, a wrong one, and a foreign Origin are all refused before Wire is touched.
	if got := r.c.postJSON(url, map[string]string{"promptHash": hash}, ""); got.Code != http.StatusForbidden {
		t.Fatalf("no csrf: %d", got.Code)
	}
	if got := r.c.postJSON(url, map[string]string{"promptHash": hash}, "wrong"); got.Code != http.StatusForbidden {
		t.Fatalf("wrong csrf: %d", got.Code)
	}
	cross := r.c.do(http.MethodPost, url, strings.NewReader(`{"promptHash":"`+hash+`"}`), map[string]string{
		"Content-Type": "application/json", "X-CSRF-Token": r.csrf, "Origin": "http://evil.example", "Sec-Fetch-Site": "cross-site",
	})
	if cross.Code != http.StatusForbidden {
		t.Fatalf("cross origin: %d", cross.Code)
	}
	// An unauthenticated client gets nothing.
	if got := r.h().client().postJSON(url, map[string]string{"promptHash": hash}, "x"); got.Code != http.StatusUnauthorized && got.Code != http.StatusForbidden {
		t.Fatalf("anonymous: %d", got.Code)
	}
	for _, q := range r.fake.Requests() {
		if strings.HasSuffix(q.Path, "/approve") {
			t.Fatal("a refused request reached Maestri")
		}
	}

	// A click must name the prompt it saw.
	if got := r.c.postJSON(url, map[string]string{}, r.csrf); got.Code != http.StatusBadRequest {
		t.Fatalf("no hash: %d %s", got.Code, got.body)
	}
	// A stale prompt is refused with an explanation.
	stale := r.c.postJSON(url, map[string]string{"promptHash": "deadbeef"}, r.csrf)
	if stale.Code != http.StatusConflict || !strings.Contains(stale.body, "prompt_changed") {
		t.Fatalf("stale: %d %s", stale.Code, stale.body)
	}
	// The real thing.
	ok := r.c.postJSON(url, map[string]string{"promptHash": hash}, r.csrf)
	if ok.Code != 200 {
		t.Fatalf("approve: %d %s", ok.Code, ok.body)
	}
	rej := r.c.postJSON("/api/workspaces/"+r.ws.ID+"/agents/"+wTermO+"/reject", map[string]string{"promptHash": hash}, r.csrf)
	if rej.Code != 200 {
		t.Fatalf("reject: %d %s", rej.Code, rej.body)
	}
	mu.Lock()
	if len(answers) != 2 || !answers[0] || answers[1] {
		t.Fatalf("answers = %v", answers)
	}
	mu.Unlock()
	// Owner token only, on exactly the allowlisted routes.
	for _, q := range r.fake.Requests() {
		if q.Method == http.MethodPost && q.Role != "owner" {
			t.Fatalf("write by %q", q.Role)
		}
	}
	// Audited twice over: the panel's write log and the Wire write itself.
	if !r.auditHas("write", "/agents/"+wTermO+"/approve") || !r.auditHas("wire.approve", wTermO) || !r.auditHas("wire.reject", `"result":"ok"`) {
		t.Fatal("approve/reject not audited")
	}
	// Terminal ids that are not UUIDs never reach the Wire client.
	if got := r.c.postJSON("/api/workspaces/"+r.ws.ID+"/agents/..%2F..%2Fkill/approve", map[string]string{"promptHash": hash}, r.csrf); got.Code == 200 {
		t.Fatal("path trick accepted")
	}
}

func (r *wireRig) h() *harness { return r.harness }

func TestAnsweringFromNeedsYouSendsTheAnswerThroughWire(t *testing.T) {
	r := newWireRig(t)
	var answered []string
	r.fake.OnAnswer = func(id string, approve bool) {
		answered = append(answered, id)
		r.fake.SetFeed(wWS, wFeed("", true)) // the agent moves on
	}
	r.setPrompt("Delete the cache? (Y/n)")
	waitUntil(t, "prompt", func() bool { return len(r.agentPrompts()) == 1 })
	item := r.agentPrompts()[0]

	resp := r.c.postJSON("/api/workspaces/"+r.ws.ID+"/inbox/"+itoa(item.ID)+"/respond", map[string]string{"response": "approve"}, r.csrf)
	if resp.Code != 200 || !strings.Contains(resp.body, "Approved") {
		t.Fatalf("respond: %d %s", resp.Code, resp.body)
	}
	if len(answered) != 1 || answered[0] != wTermO {
		t.Fatalf("answered = %v", answered)
	}
	got, _ := inbox.Get(r.db, item.ID, 0)
	if got.Status != inbox.StatusAnswered {
		t.Fatalf("item = %+v", got)
	}
	// Nothing for the Maestro to wait on: the agent got its answer in the terminal.
	var n int
	_ = r.db.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = 'inbox_response'`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d inbox_response events", n)
	}
	// A bad answer is refused.
	if bad := r.c.postJSON("/api/workspaces/"+r.ws.ID+"/inbox/"+itoa(item.ID)+"/respond", map[string]string{"response": "maybe"}, r.csrf); bad.Code == 200 {
		t.Fatal("garbage answer accepted")
	}
}

func TestPromptingAnAgentFromActivity(t *testing.T) {
	r := newWireRig(t)
	var got []string
	r.fake.OnPrompt = func(id, text string) { got = append(got, id+"|"+text) }
	url := "/api/workspaces/" + r.ws.ID + "/agents/" + wTermV + "/prompt"
	if resp := r.c.postJSON(url, map[string]string{"body": "Please also cover dead letters"}, ""); resp.Code != http.StatusForbidden {
		t.Fatalf("no csrf: %d", resp.Code)
	}
	if resp := r.c.postJSON(url, map[string]string{"body": "  "}, r.csrf); resp.Code != http.StatusBadRequest {
		t.Fatalf("empty: %d", resp.Code)
	}
	resp := r.c.postJSON(url, map[string]string{"body": "Please also cover dead letters"}, r.csrf)
	if resp.Code != 200 || len(got) != 1 || got[0] != wTermV+"|Please also cover dead letters" {
		t.Fatalf("prompt: %d %s %v", resp.Code, resp.body, got)
	}
	if !r.auditHas("wire.prompt", wTermV) {
		t.Fatal("prompt not audited")
	}
	// A terminal that is not in this workspace is refused.
	other := "22222222-2222-4222-8222-2222222222ff"
	if resp := r.c.postJSON("/api/workspaces/"+r.ws.ID+"/agents/"+other+"/prompt", map[string]string{"body": "hi"}, r.csrf); resp.Code != http.StatusNotFound {
		t.Fatalf("foreign terminal: %d %s", resp.Code, resp.body)
	}
}

func TestChatUsesTheMaestrosTerminalWhenWireIsLive(t *testing.T) {
	r := newWireRig(t)
	// Messages posted with `maestria say` join the conversation by time, with a badge; one from
	// before the Wire tail starts is left out.
	at := func(iso string) int64 { ts, _ := time.Parse(time.RFC3339, iso); return ts.UnixMilli() }
	for _, m := range []struct {
		body string
		ms   int64
	}{
		{"ancient local reply", at("2020-01-01T00:00:00Z")},
		{"said in between", at("2026-11-14T13:00:00Z")},
		{"said last", at("2026-11-14T14:00:00Z")},
	} {
		if _, _, err := chat.Send(r.db, r.ws.ID, chat.SenderMaestro, m.body, m.ms); err != nil {
			t.Fatal(err)
		}
	}
	waitUntil(t, "chat live", func() bool { v, err := r.srv.Activity.Mgr.Chat(r.ws.ID); return err == nil && v.Live })
	page := r.c.get("/workspaces/" + r.ws.ID + "/chat")
	for _, want := range []string{"How is billing going?", "<strong>Almost done.</strong>", "Sent through your connector", "said in between", "said last", "via maestria"} {
		if !strings.Contains(page.body, want) {
			t.Errorf("chat missing %q", want)
		}
	}
	if strings.Contains(page.body, "ancient local reply") || strings.Contains(page.body, "thinking about it") {
		t.Error("old local message or progress entry shown in Wire mode")
	}
	i1, i2, i3, i4 := strings.Index(page.body, "How is billing going?"), strings.Index(page.body, "said in between"), strings.Index(page.body, "Almost done."), strings.Index(page.body, "said last")
	if !(i1 < i2 && i2 < i3 && i3 < i4) {
		t.Errorf("messages are not interleaved by time: %d %d %d %d", i1, i2, i3, i4)
	}
	if strings.Count(page.body, "via maestria") != 2 {
		t.Errorf("badge count = %d, want 2 (only the say messages)", strings.Count(page.body, "via maestria"))
	}
	// Working: header chip and typing indicator.
	r.fake.SetChat(wTermM, wire.ChatSnapshot{TerminalID: wTermM, Enabled: true, Status: "working", Entries: []wire.ChatEntry{{ID: "e1", Role: "user", Kind: "message", Text: "Go"}}})
	waitUntil(t, "working", func() bool {
		return strings.Contains(r.c.get("/workspaces/"+r.ws.ID+"/chat").body, "Maestro working")
	})

	// Sending goes through /prompt to the Maestro terminal only; the local log is untouched.
	var prompts []string
	r.fake.OnPrompt = func(id, text string) { prompts = append(prompts, id+"|"+text) }
	chatURL := "/api/workspaces/" + r.ws.ID + "/chat"
	if resp := r.c.postJSON(chatURL, map[string]string{"body": "hello maestro"}, ""); resp.Code != http.StatusForbidden {
		t.Fatalf("csrf: %d", resp.Code)
	}
	resp := r.c.postJSON(chatURL, map[string]string{"body": "hello maestro"}, r.csrf)
	if resp.Code != 200 || len(prompts) != 1 || prompts[0] != wTermM+"|hello maestro" {
		t.Fatalf("send: %d %s %v", resp.Code, resp.body, prompts)
	}
	msgs, _ := chat.List(r.db, r.ws.ID, 0, 50)
	for _, m := range msgs {
		if m.Sender == chat.SenderHuman {
			t.Fatal("Wire-mode message also written to the local log")
		}
	}
	var n int
	_ = r.db.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = 'chat'`).Scan(&n)
	if n != 0 {
		t.Fatal("a chat event was emitted for a message already delivered to the terminal")
	}

	// Ambiguous delivery: a clear error, one attempt, text kept by the UI (no retry here).
	r.fake.ClearRequests()
	r.fake.DropWriteReplies(true)
	amb := r.c.postJSON(chatURL, map[string]string{"body": "again"}, r.csrf)
	if amb.Code != http.StatusBadGateway || !strings.Contains(amb.body, `"ambiguous":true`) || !strings.Contains(amb.body, "not retried") {
		t.Fatalf("ambiguous: %d %s", amb.Code, amb.body)
	}
	time.Sleep(100 * time.Millisecond)
	posts := 0
	for _, q := range r.fake.Requests() {
		if q.Method == http.MethodPost && strings.HasSuffix(q.Path, "/prompt") {
			posts++
		}
	}
	if posts != 1 {
		t.Fatalf("%d attempts", posts)
	}
}

func TestChatShowsMaestroNotRunning(t *testing.T) {
	r := newWireRig(t)
	r.fake.SetFeed(wWS, wFeed("", false)) // the Maestro terminal closed
	waitUntil(t, "maestro closed", func() bool {
		return strings.Contains(r.c.get("/workspaces/"+r.ws.ID+"/chat").body, "Maestro not running")
	})
	page := r.c.get("/workspaces/" + r.ws.ID + "/chat").body
	for _, want := range []string{"The Maestro isn&#39;t running", "Open it in Maestri", `placeholder="Maestro not running"`, "disabled"} {
		if !strings.Contains(page, want) {
			t.Errorf("missing %q", want)
		}
	}
	if resp := r.c.postJSON("/api/workspaces/"+r.ws.ID+"/chat", map[string]string{"body": "hi"}, r.csrf); resp.Code != http.StatusConflict {
		t.Fatalf("send while closed: %d %s", resp.Code, resp.body)
	}
}

func TestFloorsPageShowsLiveFloors(t *testing.T) {
	r := newWireRig(t)
	if _, err := floors.Start(r.db, r.ws.ID, "billing-webhooks", floors.StartInput{Brief: "Webhook delivery"}, 0); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "live floor", func() bool {
		return strings.Contains(r.c.get("/workspaces/"+r.ws.ID+"/floors").body, "PR open")
	})
	page := r.c.get("/workspaces/" + r.ws.ID + "/floors").body
	for _, want := range []string{"Review", "PR open", "billing-webhooks", "Webhook delivery", "85%"} {
		if !strings.Contains(page, want) {
			t.Errorf("floors missing %q", want)
		}
	}
	// The floor leaves Maestri: history is recorded by itself.
	f := wFeed("", true)
	f.Floors = []wire.FloorMeta{{Name: "main", IsActive: true}}
	f.Items = f.Items[:1]
	r.fake.SetFeed(wWS, f)
	waitUntil(t, "history", func() bool {
		past, _ := floors.List(r.db, floors.ListOptions{WorkspaceID: r.ws.ID, IncludePast: true})
		return len(past) == 1 && past[0].State != floors.StateActive
	})
}

func TestNoTokensInPagesAPIsOrAudit(t *testing.T) {
	r := newWireRig(t)
	r.setPrompt("Go? (Y/n)")
	waitUntil(t, "prompt", func() bool { return len(r.agentPrompts()) == 1 })
	_ = r.c.postJSON("/api/workspaces/"+r.ws.ID+"/agents/"+wTermO+"/approve", map[string]string{"promptHash": wire.PromptHash("Go? (Y/n)")}, r.csrf)
	secrets := []string{r.creds.Guest.Token, r.creds.Owner.Token, r.creds.Guest.DeviceID, r.creds.Owner.DeviceID, "pw"}
	paths := []string{
		"/", "/workspaces/" + r.ws.ID + "/activity", "/workspaces/" + r.ws.ID + "/chat", "/workspaces/" + r.ws.ID + "/tasks",
		"/workspaces/" + r.ws.ID + "/floors", "/security", "/api/wire", "/api/workspaces", "/api/inbox", "/api/audit", "/api/floors",
		"/api/workspaces/" + r.ws.ID + "/activity", "/api/workspaces/" + r.ws.ID + "/chat", "/api/usage", "/api/session",
	}
	for _, p := range paths {
		resp := r.c.get(p)
		for _, s := range secrets {
			if strings.Contains(resp.body, s) {
				t.Errorf("%s leaks a Wire secret", p)
			}
		}
		for k, v := range resp.Header() {
			for _, s := range secrets {
				if strings.Contains(strings.Join(v, ","), s) {
					t.Errorf("%s header %s leaks a secret", p, k)
				}
			}
		}
	}
	// And the status API says nothing a browser should not have.
	var st map[string]any
	_ = json.Unmarshal([]byte(r.c.get("/api/wire").body), &st)
	for _, banned := range []string{"token", "pin", "host", "guest", "owner"} {
		if _, ok := st[banned]; ok {
			t.Errorf("/api/wire exposes %q", banned)
		}
	}
	if st["state"] != "connected" || st["ownerOk"] != true {
		t.Fatalf("status = %v", st)
	}
}

func TestDisabledStates(t *testing.T) {
	t.Run("wire turned off: nothing stale, explained", func(t *testing.T) {
		r := newWireRig(t)
		r.setPrompt("Go? (Y/n)")
		waitUntil(t, "prompt", func() bool { return len(r.agentPrompts()) == 1 })
		r.fake.Close()
		waitUntil(t, "unreachable", func() bool { return r.srv.Activity.Mgr.Status().State == wire.StateUnreachable })
		for _, p := range []string{"/workspaces/" + r.ws.ID + "/activity", "/"} {
			body := r.c.get(p).body
			wants := []string{"Maestri Wire is off or can&#39;t be reached", "Settings → Wire"}
			if p == "/" {
				wants = []string{"Turn on Maestri Wire", "Settings → Wire"} // the Overview shows the compact state
			}
			for _, want := range wants {
				if !strings.Contains(body, want) {
					t.Errorf("%s missing %q", p, want)
				}
			}
			for _, stale := range []string{"Vesper", "12 passed", "Orin"} {
				if strings.Contains(body, stale) {
					t.Errorf("%s still shows %q after Wire went away", p, stale)
				}
			}
		}
		// Chat falls back to the local log, which keeps working.
		if page := r.c.get("/workspaces/" + r.ws.ID + "/chat"); page.Code != 200 || !strings.Contains(page.body, "maestria wait") {
			t.Fatalf("local chat fallback: %d", page.Code)
		}
		local := r.c.postJSON("/api/workspaces/"+r.ws.ID+"/chat", map[string]string{"body": "still works"}, r.csrf)
		if local.Code != 200 || !strings.Contains(local.body, `"message"`) {
			t.Fatalf("local send: %d %s", local.Code, local.body)
		}
		// Approving something that can no longer be reached fails cleanly.
		if resp := r.c.postJSON("/api/workspaces/"+r.ws.ID+"/agents/"+wTermO+"/approve", map[string]string{"promptHash": "x"}, r.csrf); resp.Code != http.StatusServiceUnavailable {
			t.Fatalf("approve while down: %d %s", resp.Code, resp.body)
		}
		if resp := r.c.get("/api/workspaces/" + r.ws.ID + "/activity"); resp.Code != http.StatusServiceUnavailable {
			t.Fatalf("activity api while down: %d", resp.Code)
		}
	})
	t.Run("security key changed", func(t *testing.T) {
		r := newWireRig(t)
		r.fake.RotateCert()
		waitUntil(t, "pin mismatch", func() bool { return r.srv.Activity.Mgr.Status().State == wire.StatePinMismatch })
		body := r.c.get("/workspaces/" + r.ws.ID + "/activity").body
		for _, want := range []string{"security key changed", "maestria wire unpair", "maestria wire pair"} {
			if !strings.Contains(body, want) {
				t.Errorf("missing %q", want)
			}
		}
	})
	t.Run("access revoked", func(t *testing.T) {
		r := newWireRig(t)
		r.fake.RevokeAll()
		waitUntil(t, "revoked", func() bool { return r.srv.Activity.Mgr.Status().State == wire.StateRevoked })
		body := r.c.get("/workspaces/" + r.ws.ID + "/activity").body
		if !strings.Contains(body, "no longer accepts") || !strings.Contains(body, "maestria wire pair") {
			t.Errorf("revoked page:\n%s", body)
		}
	})
	t.Run("workspace not open in Maestri", func(t *testing.T) {
		r := newWireRig(t)
		r.fake.SetWorkspaces(wire.WorkspaceMeta{ID: wWS, Name: "Something else", IsLoaded: true, WorkingDirectory: "/elsewhere"})
		waitUntil(t, "unmapped", func() bool { _, err := r.srv.Activity.Mgr.Feed(r.ws.ID); return err != nil })
		body := r.c.get("/workspaces/" + r.ws.ID + "/activity").body
		if !strings.Contains(body, "isn&#39;t open in Maestri") {
			t.Errorf("unmapped page:\n%s", body)
		}
	})
	t.Run("no Wire service at all", func(t *testing.T) {
		h := newHarness(t)
		ws := addTestWorkspace(t, h, "Solo")
		c := h.client()
		c.login()
		if body := c.get("/workspaces/" + ws.ID + "/activity").body; !strings.Contains(body, "maestria wire pair") {
			t.Errorf("not paired page:\n%s", body)
		}
		if resp := c.get("/api/wire"); resp.Code != 200 || !strings.Contains(resp.body, `"disabled"`) {
			t.Errorf("api/wire: %d %s", resp.Code, resp.body)
		}
	})
}

func TestStandbyInterruptAndWakeThroughTheWebServer(t *testing.T) {
	r := newWireRig(t)
	r.srv.Config.Standby = config.Standby{
		Thresholds:  map[string]map[string]int{"claude": {"*": 90}},
		Actions:     map[string]map[string]string{"claude": {"*": "interrupt"}},
		WakeOnReset: true,
	}
	writeTestUsage(t, r.harness, 97, 30)
	var mu sync.Mutex
	var prompts []string
	r.fake.OnPrompt = func(id, text string) { mu.Lock(); prompts = append(prompts, id+"|"+text); mu.Unlock() }

	r.c.get("/api/usage") // evaluates: the guard trips and the interrupt goes out
	r.srv.WaitBackground()
	waitUntil(t, "esc", func() bool { return len(r.fake.Inputs()) >= 1 })
	in := r.fake.Inputs()[0]
	if in.Terminal != wTermV || len(in.Data) != 1 || in.Data[0] != 0x1b {
		t.Fatalf("interrupt input = %+v", in) // Vesper: claude, working. Not Orin (gemini), not the Maestro.
	}
	if !r.auditHas("wire.interrupt", wTermV) {
		t.Fatal("interrupt not audited")
	}
	_ = prompts
}
