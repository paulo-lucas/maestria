package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paulo-lucas/maestria/internal/chat"
	"github.com/paulo-lucas/maestria/internal/config"
	"github.com/paulo-lucas/maestria/internal/events"
	"github.com/paulo-lucas/maestria/internal/inbox"
	"github.com/paulo-lucas/maestria/internal/workspaces"
)

func addTestWorkspace(t *testing.T, h *harness, name string) workspaces.Workspace {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ws, err := workspaces.Add(h.srv.Config.ConfigDir, workspaces.AddInput{
		Name:            name,
		Path:            dir,
		MaestroAgent:    "Maestro",
		MaestroProvider: "claude",
	})
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func writeTestUsage(t *testing.T, h *harness, fiveHourPct, weeklyPct float64) {
	t.Helper()
	pdir := filepath.Join(h.srv.Config.MaestriDir, "usage", "providers")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{
		"updatedAt": h.clock.now().Format(time.RFC3339),
		"providers": map[string]any{
			"claude": map[string]any{
				"state": "ready",
				"plan":  "max",
				"fiveHour": map[string]any{
					"usedPercent": fiveHourPct,
					"resetsAt":    h.clock.now().Add(2 * time.Hour).Format(time.RFC3339),
				},
				"sevenDay": map[string]any{
					"usedPercent": weeklyPct,
					"resetsAt":    h.clock.now().Add(48 * time.Hour).Format(time.RFC3339),
				},
			},
		},
	}
	b, _ := json.Marshal(payload)
	if err := os.WriteFile(filepath.Join(pdir, ".status.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (c *client) postJSON(path string, payload any, csrf string) resp {
	c.h.t.Helper()
	var body []byte
	if payload != nil {
		body, _ = json.Marshal(payload)
	}
	hdr := map[string]string{"Content-Type": "application/json"}
	if csrf != "" {
		hdr["X-CSRF-Token"] = csrf
	}
	return c.do(http.MethodPost, path, bytes.NewReader(body), hdr)
}

func TestConnectorAPIAuthAndCSRFEnforced(t *testing.T) {
	h := newHarness(t, opts{mutate: func(c *config.Config) {
		c.MaestriDir = filepath.Join(t.TempDir(), "maestri")
	}})
	ws := addTestWorkspace(t, h, "Atlas")
	anon := h.client()

	for _, ep := range []string{
		"/api/workspaces",
		"/api/inbox",
		"/api/workspaces/" + ws.ID + "/inbox",
		"/api/workspaces/" + ws.ID + "/chat",
		"/api/usage",
		"/api/stream",
	} {
		if r := anon.get(ep); r.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated GET %s = %d, want 401", ep, r.Code)
		}
	}

	writeRoutes := []string{
		"/api/inbox/dismiss-notices",
		"/api/workspaces/" + ws.ID + "/inbox/dismiss-notices",
		"/api/workspaces/" + ws.ID + "/inbox/1/respond",
		"/api/workspaces/" + ws.ID + "/inbox/1/dismiss",
		"/api/workspaces/" + ws.ID + "/inbox/1/postpone",
		"/api/workspaces/" + ws.ID + "/chat",
		"/api/standby/config",
		"/api/standby/resume",
	}
	for _, ep := range writeRoutes {
		if r := anon.postJSON(ep, map[string]any{}, ""); r.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated POST %s = %d, want 401", ep, r.Code)
		}
	}

	// Authenticated client without/with wrong CSRF must get 403 on every new write route.
	c := h.client()
	c.login()
	for _, ep := range writeRoutes {
		if r := c.postJSON(ep, map[string]any{}, ""); r.Code != http.StatusForbidden {
			t.Errorf("missing CSRF POST %s = %d, want 403", ep, r.Code)
		}
		if r := c.postJSON(ep, map[string]any{}, "wrong-token"); r.Code != http.StatusForbidden {
			t.Errorf("bad CSRF POST %s = %d, want 403", ep, r.Code)
		}
	}
}

func TestConnectorAPIIndexInboxChatUsageAndStandby(t *testing.T) {
	h := newHarness(t, opts{mutate: func(c *config.Config) {
		c.MaestriDir = filepath.Join(t.TempDir(), "maestri")
	}})
	h.srv.env = config.Env{Home: t.TempDir(), Getenv: func(string) string { return "" }, GOOS: "linux"}
	ws := addTestWorkspace(t, h, "Atlas")
	writeTestUsage(t, h, 96, 40)

	c := h.client()
	csrf := c.login()

	// Seed inbox items.
	nowMs := h.clock.now().UnixMilli()
	appItem, err := inbox.Post(h.db, ws.ID, inbox.PostInput{Kind: inbox.KindApprove, Body: "Land billing?", Author: "Maestro"}, nowMs)
	if err != nil {
		t.Fatal(err)
	}
	choiceItem, err := inbox.Post(h.db, ws.ID, inbox.PostInput{Kind: inbox.KindChoice, Body: "Paper size?", Options: []string{"A4", "Letter"}}, nowMs+1)
	if err != nil {
		t.Fatal(err)
	}
	noticeItem, err := inbox.Post(h.db, ws.ID, inbox.PostInput{Kind: inbox.KindNotice, Body: "Tests green"}, nowMs+2)
	if err != nil {
		t.Fatal(err)
	}

	// GET /api/workspaces
	wsResp := c.get("/api/workspaces")
	if wsResp.Code != 200 || !strings.Contains(wsResp.body, `"needsYou":2`) {
		t.Fatalf("GET /api/workspaces=%d %s", wsResp.Code, wsResp.body)
	}

	// Postpone choiceItem -> moves into postponed bucket.
	pResp := c.postJSON("/api/workspaces/"+ws.ID+"/inbox/"+itoa(choiceItem.ID)+"/postpone", map[string]any{"preset": "1h"}, csrf)
	if pResp.Code != 200 || !strings.Contains(pResp.body, `"postponed":true`) {
		t.Fatalf("postpone=%d %s", pResp.Code, pResp.body)
	}

	inResp := c.get("/api/workspaces/" + ws.ID + "/inbox")
	if inResp.Code != 200 {
		t.Fatalf("GET workspace inbox=%d %s", inResp.Code, inResp.body)
	}
	var inParsed struct {
		NeedsYou     []apiInboxItem `json:"needsYou"`
		Postponed    []apiInboxItem `json:"postponed"`
		Updates      []apiInboxItem `json:"updates"`
		HistoryCount int            `json:"historyCount"`
	}
	if err := json.Unmarshal([]byte(inResp.body), &inParsed); err != nil {
		t.Fatal(err)
	}
	if len(inParsed.NeedsYou) != 1 || len(inParsed.Postponed) != 1 || len(inParsed.Updates) != 1 {
		t.Fatalf("unexpected inbox buckets: %+v", inParsed)
	}

	// Approve with optional comment.
	rResp := c.postJSON("/api/workspaces/"+ws.ID+"/inbox/"+itoa(appItem.ID)+"/respond", map[string]any{
		"decision": "approved",
		"response": "Ship it!",
	}, csrf)
	if rResp.Code != 200 || !strings.Contains(rResp.body, "Yes — Ship it!") {
		t.Fatalf("respond approve=%d %s", rResp.Code, rResp.body)
	}

	// Dismiss all notices.
	dResp := c.postJSON("/api/workspaces/"+ws.ID+"/inbox/dismiss-notices", map[string]any{}, csrf)
	if dResp.Code != 200 || !strings.Contains(dResp.body, `"dismissed":1`) {
		t.Fatalf("dismiss notices=%d %s", dResp.Code, dResp.body)
	}
	_ = noticeItem

	// Chat send & list.
	chatPost := c.postJSON("/api/workspaces/"+ws.ID+"/chat", map[string]any{"body": "Status check?"}, csrf)
	if chatPost.Code != 200 || !strings.Contains(chatPost.body, "Status check?") {
		t.Fatalf("chat post=%d %s", chatPost.Code, chatPost.body)
	}
	if _, _, err := chat.Send(h.db, ws.ID, chat.SenderMaestro, "All **good**", nowMs+10); err != nil {
		t.Fatal(err)
	}
	chatGet := c.get("/api/workspaces/" + ws.ID + "/chat")
	if chatGet.Code != 200 {
		t.Fatalf("chat get=%d %s", chatGet.Code, chatGet.body)
	}
	var chatParsed struct {
		Messages []chat.Message `json:"messages"`
	}
	if err := json.Unmarshal([]byte(chatGet.body), &chatParsed); err != nil || len(chatParsed.Messages) != 2 || !strings.Contains(chatParsed.Messages[1].BodyHTML, "<strong>good</strong>") {
		t.Fatalf("chat get parsed=%+v err=%v", chatParsed, err)
	}

	// GET /api/usage triggers standby (96% >= default 95%).
	uResp := c.get("/api/usage")
	if uResp.Code != 200 || !strings.Contains(uResp.body, `"active":true`) {
		t.Fatalf("GET /api/usage=%d %s", uResp.Code, uResp.body)
	}

	// POST /api/standby/config updates threshold and action, persists config.json.
	cfgResp := c.postJSON("/api/standby/config", map[string]any{
		"thresholds":  map[string]map[string]int{"claude:max": {"session": 80, "weekly": 90}},
		"actions":     map[string]map[string]string{"claude:max": {"session": "interrupt"}},
		"wakeOnReset": true,
	}, csrf)
	if cfgResp.Code != 200 || !strings.Contains(cfgResp.body, `"action":"interrupt"`) {
		t.Fatalf("standby config=%d %s", cfgResp.Code, cfgResp.body)
	}

	// Invalid standby config (threshold > 100) rejected cleanly with 400.
	badCfg := c.postJSON("/api/standby/config", map[string]any{
		"thresholds": map[string]map[string]int{"claude": {"session": 150}},
	}, csrf)
	if badCfg.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on invalid threshold, got %d %s", badCfg.Code, badCfg.body)
	}

	// Manual resume clears active guard.
	resResp := c.postJSON("/api/standby/resume", map[string]any{}, csrf)
	if resResp.Code != 200 || !strings.Contains(resResp.body, `"active":false`) {
		t.Fatalf("standby resume=%d %s", resResp.Code, resResp.body)
	}

	// Verify events emitted include inbox_response, inbox_dismiss, chat, standby, resume.
	evs, err := events.Since(h.db, ws.ID, 0, 50)
	if err != nil || len(evs) < 5 {
		t.Fatalf("expected >=5 events, got %d err=%v", len(evs), err)
	}

	// Verify audit recorded writes.
	audResp := c.get("/api/audit?limit=20")
	if audResp.Code != 200 || !strings.Contains(audResp.body, "/api/standby/config") {
		t.Fatalf("audit=%d %s", audResp.Code, audResp.body)
	}
}

func TestSSEStreamDeliversLiveAndOutOfBandUpdates(t *testing.T) {
	h := newHarness(t, opts{mutate: func(c *config.Config) {
		c.MaestriDir = filepath.Join(t.TempDir(), "maestri")
	}})
	h.srv.ssePollInterval = 40 * time.Millisecond
	ws := addTestWorkspace(t, h, "Atlas")

	c := h.client()
	c.login()

	// Start SSE request against the mux using httptest.NewRecorder in a goroutine + pipe or ResponseRecorder.
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

	// Out-of-band SQLite write (simulating CLI `maestria post`) triggers SSE update!
	if _, err := inbox.Post(h.db, ws.ID, inbox.PostInput{Kind: inbox.KindNotice, Body: "CLI ping"}, h.clock.now().UnixMilli()); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(10 * time.Second)
	foundUpdate := false
	for !foundUpdate {
		lineCh := make(chan string, 1)
		go func() {
			s, _ := reader.ReadString('\n')
			lineCh <- s
		}()
		select {
		case <-deadline:
			t.Fatal("timed out waiting for SSE update event")
		case l := <-lineCh:
			if strings.Contains(l, "event: update") {
				foundUpdate = true
			}
		}
	}

	cancel()
	_ = pw.Close()
	<-done
}

func itoa(n int64) string {
	return json.Number(strings.TrimSpace(func() string {
		b, _ := json.Marshal(n)
		return string(b)
	}())).String()
}

// pipeResponseWriter bridges Server.ServeHTTP SSE output to a streaming reader without opening a TCP socket.
type pipeResponseWriter struct {
	header http.Header
	status int
	r      *os.File
	w      *os.File
}

func newPipeResponseWriter() (*os.File, *pipeResponseWriter) {
	r, w, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	return r, &pipeResponseWriter{header: make(http.Header), r: r, w: w}
}

func (p *pipeResponseWriter) Header() http.Header         { return p.header }
func (p *pipeResponseWriter) WriteHeader(code int)        { p.status = code }
func (p *pipeResponseWriter) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *pipeResponseWriter) Flush()                      {}
func (p *pipeResponseWriter) Close() error                { _ = p.w.Close(); return p.r.Close() }
