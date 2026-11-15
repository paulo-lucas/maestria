package activity_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paulo-lucas/maestria/internal/activity"
	"github.com/paulo-lucas/maestria/internal/audit"
	"github.com/paulo-lucas/maestria/internal/config"
	"github.com/paulo-lucas/maestria/internal/events"
	"github.com/paulo-lucas/maestria/internal/floors"
	"github.com/paulo-lucas/maestria/internal/inbox"
	"github.com/paulo-lucas/maestria/internal/store"
	"github.com/paulo-lucas/maestria/internal/wire"
	"github.com/paulo-lucas/maestria/internal/wire/wiretest"
	"github.com/paulo-lucas/maestria/internal/workspaces"
)

const (
	wsWire = "11111111-1111-4111-8111-111111111111"
	termM  = "22222222-2222-4222-8222-222222222221"
	termO  = "22222222-2222-4222-8222-222222222222"
	termV  = "22222222-2222-4222-8222-222222222223"
	termG  = "22222222-2222-4222-8222-222222222224"
	floorB = "44444444-4444-4444-8444-444444444441"
	floorC = "44444444-4444-4444-8444-444444444442"
)

func testConfig(t *testing.T, dir string) *config.Config {
	t.Helper()
	cfg, err := config.Defaults(dir, config.Env{Getenv: func(string) string { return "" }, GOOS: "linux", Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func waitFor(t *testing.T, what string, cond func() bool) {
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

func card(id, name, agent, floorID, floorName string) wire.TerminalCard {
	return wire.TerminalCard{ID: id, Name: name, AgentType: agent, FloorID: floorID, FloorName: floorName, IsRunning: true, IsActive: true, Preview: []string{"working"}}
}

type feedOpts struct {
	orinPrompt string // "" = no Y/n
	floors     []wire.FloorMeta
}

func feed(o feedOpts) wire.FeedSnapshot {
	maestro := card(termM, "Maestro", "claude", "", "")
	maestro.IsManager = true
	orin := card(termO, "Orin", "gemini", floorB, "billing")
	vesper := card(termV, "Vesper", "claude", floorB, "billing")
	idle := card(termG, "Wren", "claude", floorB, "billing")
	idle.IsActive = false
	items := []wire.FeedItem{{Kind: wire.ItemTerminal, Terminal: &maestro}, {Kind: wire.ItemTerminal, Terminal: &vesper}, {Kind: wire.ItemTerminal, Terminal: &idle}}
	if o.orinPrompt != "" {
		items = append(items, wire.FeedItem{Kind: wire.ItemPendingPrompt, Terminal: &orin, Prompt: o.orinPrompt})
	} else {
		items = append(items, wire.FeedItem{Kind: wire.ItemTerminal, Terminal: &orin})
	}
	fl := o.floors
	if fl == nil {
		fl = []wire.FloorMeta{{Name: "main", IsActive: true}, {ID: floorB, Name: "billing", Branch: "billing", Status: &wire.FloorStatus{State: "working", Text: "tests"}}}
	}
	return wire.FeedSnapshot{Workspace: wire.WorkspaceMeta{ID: wsWire, Name: "Atlas", IsLoaded: true}, Floors: fl, Items: items}
}

type rig struct {
	fake   *wiretest.Server
	svc    *activity.Service
	sqlDB  *sql.DB
	aud    *audit.Log
	mu     sync.Mutex
	change []string
}

func start(t *testing.T, grace time.Duration) *rig {
	t.Helper()
	fake, err := wiretest.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	fake.SetPassword("pw")
	fake.SetWorkspaces(wire.WorkspaceMeta{ID: wsWire, Name: "Atlas", IsLoaded: true, WorkingDirectory: "/work/atlas"})
	fake.SetFeed(wsWire, feed(feedOpts{}))
	fake.SetChat(termM, wire.ChatSnapshot{TerminalID: termM, Enabled: true, Status: "idle"})

	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	creds, err := wire.Provision(context.Background(), fake.Host, fake.Pin(), "T", "pw", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := creds.Save(wire.Path(dir)); err != nil {
		t.Fatal(err)
	}
	r := &rig{fake: fake, sqlDB: db, aud: audit.New(db, nil)}
	r.svc = activity.New(activity.Options{
		DB: db, Audit: r.aud, ConfigDir: dir, Grace: grace,
		Poll: 30 * time.Millisecond, MaxBackoff: 60 * time.Millisecond,
		Workspaces: func() []workspaces.Workspace {
			return []workspaces.Workspace{{ID: "atlas", Name: "Atlas", Path: "/work/atlas", MaestroAgent: "Maestro"}}
		},
		OnChange: func(k string) { r.mu.Lock(); r.change = append(r.change, k); r.mu.Unlock() },
	})
	ctx, cancel := context.WithCancel(context.Background())
	// Let the background goroutines finish before the database and temp dirs are removed.
	t.Cleanup(func() { cancel(); time.Sleep(150 * time.Millisecond) })
	r.svc.Start(ctx)
	waitFor(t, "live", func() bool { _, err := r.svc.Mgr.Feed("atlas"); return err == nil })
	fake.ClearRequests()
	return r
}

func (r *rig) openPrompts(t *testing.T) []inbox.Item {
	t.Helper()
	items, err := inbox.List(r.sqlDB, inbox.ListOptions{WorkspaceID: "atlas", Status: "open", Kind: inbox.KindAgentPrompt})
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func TestYesNoPromptAppearsInNeedsYouAndResolvesItselfWhenItLeavesTheFeed(t *testing.T) {
	r := start(t, time.Minute)
	r.fake.SetFeed(wsWire, feed(feedOpts{orinPrompt: "Install zod@3.23.8? (Y/n)"}))
	waitFor(t, "agent prompt", func() bool { return len(r.openPrompts(t)) == 1 })
	it := r.openPrompts(t)[0]
	if it.Kind != "agent_prompt" || it.Author == nil || *it.Author != "Orin" || !strings.Contains(it.Body, "Install zod@3.23.8? (Y/n)") || !strings.Contains(it.Body, "billing") {
		t.Fatalf("item = %+v", it)
	}
	counts, _ := inbox.CountsByWorkspace(r.sqlDB, 0)
	if counts["atlas"].NeedsYou != 1 {
		t.Fatalf("Needs you = %+v", counts["atlas"])
	}
	// The same prompt in a second snapshot does not duplicate it.
	r.fake.SetFeed(wsWire, feed(feedOpts{orinPrompt: "Install zod@3.23.8? (Y/n)", floors: []wire.FloorMeta{{Name: "main"}, {ID: floorB, Name: "billing"}}}))
	time.Sleep(100 * time.Millisecond)
	if n := len(r.openPrompts(t)); n != 1 {
		t.Fatalf("%d open prompts", n)
	}
	// Answered in Maestri: it disappears from the feed and the item closes by itself.
	r.fake.SetFeed(wsWire, feed(feedOpts{}))
	waitFor(t, "auto-resolve", func() bool { return len(r.openPrompts(t)) == 0 })
	closed, _ := inbox.List(r.sqlDB, inbox.ListOptions{WorkspaceID: "atlas", Status: "history"})
	if len(closed) != 1 || closed[0].Status != inbox.StatusDismissed {
		t.Fatalf("history = %+v", closed)
	}
}

func TestAnswerFromNeedsYouGoesThroughWireAndClosesTheItem(t *testing.T) {
	r := start(t, time.Minute)
	var answers []string
	r.fake.OnAnswer = func(id string, approve bool) {
		answers = append(answers, id)
		r.fake.SetFeed(wsWire, feed(feedOpts{})) // the agent moves on
	}
	r.fake.SetFeed(wsWire, feed(feedOpts{orinPrompt: "Run rm? (Y/n)"}))
	waitFor(t, "agent prompt", func() bool { return len(r.openPrompts(t)) == 1 })
	id := r.openPrompts(t)[0].ID

	it, err := r.svc.AnswerInboxPrompt(context.Background(), "127.0.0.1", "atlas", id, true)
	if err != nil || it.Status != inbox.StatusAnswered || it.Response == nil || *it.Response != "Approved" {
		t.Fatalf("it=%+v err=%v", it, err)
	}
	if len(answers) != 1 || answers[0] != termO {
		t.Fatalf("answers = %v", answers)
	}
	// No Maestro event: the agent got the answer in its own terminal.
	evs, _ := events.Since(r.sqlDB, "atlas", 0, 10)
	if len(evs) != 0 {
		t.Fatalf("events = %+v", evs)
	}
	// The write used the owner token and was audited.
	for _, q := range r.fake.Requests() {
		if q.Method == "POST" && q.Role != "owner" {
			t.Fatalf("write with %q", q.Role)
		}
	}
	entries, _ := r.aud.Recent(20)
	found := false
	for _, e := range entries {
		if e.Event == "wire.approve" && strings.Contains(e.Detail, termO) && strings.Contains(e.Detail, `"result":"ok"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no wire.approve audit: %+v", entries)
	}
}

func TestAnswerRefusedWhenThePromptChangedUnderTheClick(t *testing.T) {
	r := start(t, time.Minute)
	r.fake.SetFeed(wsWire, feed(feedOpts{orinPrompt: "First? (Y/n)"}))
	waitFor(t, "prompt", func() bool { return len(r.openPrompts(t)) == 1 })
	id := r.openPrompts(t)[0].ID
	// Wire moves to a different prompt but our sync has not run yet: simulate by changing the
	// fake's feed and answering immediately; the hash check must refuse and the item refresh.
	r.fake.SetFeed(wsWire, feed(feedOpts{orinPrompt: "Second? (Y/n)"}))
	waitFor(t, "second prompt mirrored", func() bool {
		items := r.openPrompts(t)
		return len(items) == 1 && strings.Contains(items[0].Body, "Second")
	})
	if _, err := r.svc.AnswerInboxPrompt(context.Background(), "x", "atlas", id, true); err == nil {
		t.Fatal("answered an item whose prompt is gone")
	}
	for _, q := range r.fake.Requests() {
		if strings.HasSuffix(q.Path, "/approve") {
			t.Fatalf("approve sent for the wrong prompt")
		}
	}
}

func TestPromptsLeaveNeedsYouAfterTheGracePeriodWithoutWire(t *testing.T) {
	r := start(t, 150*time.Millisecond)
	r.fake.SetFeed(wsWire, feed(feedOpts{orinPrompt: "Go? (Y/n)"}))
	waitFor(t, "prompt", func() bool { return len(r.openPrompts(t)) == 1 })
	r.fake.Close()
	waitFor(t, "prompts dismissed", func() bool { return len(r.openPrompts(t)) == 0 })
}

func TestLiveFloorsFromTheFeed(t *testing.T) {
	r := start(t, time.Minute)
	p := 0.4
	r.fake.SetFeed(wsWire, feed(feedOpts{orinPrompt: "x? (Y/n)", floors: []wire.FloorMeta{
		{Name: "main"},
		{ID: floorB, Name: "billing", Branch: "feat/billing", Status: &wire.FloorStatus{State: "review", Text: "PR open", Progress: &p}},
	}}))
	waitFor(t, "floors", func() bool {
		li, err := r.svc.LiveFloors("atlas")
		return err == nil && len(li) == 1 && li[0].Status == "review"
	})
	li, _ := r.svc.LiveFloors("atlas")
	f := li[0]
	if f.Name != "billing" || f.Branch != "feat/billing" || f.StatusText != "PR open" || *f.Progress != 0.4 || f.Attention != 1 || len(f.Agents) != 3 {
		t.Fatalf("live floor = %+v", f)
	}
	// Merged into stored floors by the existing floors package.
	if _, err := floors.Start(r.sqlDB, "atlas", "billing", floors.StartInput{Brief: "Billing webhooks"}, 1); err != nil {
		t.Fatal(err)
	}
	list, err := floors.ListWithLive(r.sqlDB, "atlas", false, r.svc)
	if err != nil || len(list) != 1 || list[0].Status != "review" || list[0].Branch != "feat/billing" || list[0].Brief != "Billing webhooks" {
		t.Fatalf("merged = %+v %v", list, err)
	}
	// With Wire down, the live source errors and the stored data is served unchanged.
	r.fake.Close()
	waitFor(t, "down", func() bool { _, err := r.svc.LiveFloors("atlas"); return err != nil })
	list, _ = floors.ListWithLive(r.sqlDB, "atlas", false, r.svc)
	if len(list) != 1 || list[0].Live || list[0].Status != "working" {
		t.Fatalf("stale live data after disconnect: %+v", list)
	}
}

func TestFloorDisappearingFromWireRecordsHistory(t *testing.T) {
	r := start(t, time.Minute)
	two := func(withB, withC bool, bState string) feedOpts {
		fl := []wire.FloorMeta{{Name: "main"}}
		if withB {
			fl = append(fl, wire.FloorMeta{ID: floorB, Name: "billing", Status: &wire.FloorStatus{State: bState}})
		}
		if withC {
			fl = append(fl, wire.FloorMeta{ID: floorC, Name: "scratch", Status: &wire.FloorStatus{State: "working"}})
		}
		return feedOpts{floors: fl}
	}
	// The Maestro tracks "billing"; "scratch" it never told Maestria about.
	if _, err := floors.Start(r.sqlDB, "atlas", "billing", floors.StartInput{Brief: "Billing webhooks"}, 1); err != nil {
		t.Fatal(err)
	}
	r.fake.SetFeed(wsWire, feed(two(true, true, "working")))
	waitFor(t, "seen", func() bool { li, _ := r.svc.LiveFloors("atlas"); return len(li) == 2 })
	// billing reaches done, then both floors vanish: billing landed, scratch cancelled.
	r.fake.SetFeed(wsWire, feed(two(true, true, "done")))
	waitFor(t, "done seen", func() bool { li, _ := r.svc.LiveFloors("atlas"); return len(li) == 2 && li[0].Status == "done" })
	r.fake.SetFeed(wsWire, feed(two(false, false, "")))
	waitFor(t, "history", func() bool {
		past, _ := floors.List(r.sqlDB, floors.ListOptions{WorkspaceID: "atlas", IncludePast: true})
		n := 0
		for _, f := range past {
			if f.State == floors.StateLanded || f.State == floors.StateCancelled {
				n++
			}
		}
		return n == 2
	})
	past, _ := floors.List(r.sqlDB, floors.ListOptions{WorkspaceID: "atlas", IncludePast: true})
	got := map[string]floors.Floor{}
	for _, f := range past {
		got[f.Name] = f
	}
	if got["billing"].State != floors.StateLanded || got["billing"].Brief != "Billing webhooks" || got["billing"].Outcome != "" {
		t.Fatalf("billing = %+v", got["billing"])
	}
	if got["scratch"].State != floors.StateCancelled || !strings.Contains(got["scratch"].Brief, "no brief") {
		t.Fatalf("scratch = %+v", got["scratch"])
	}
	// Recorded from when it was first seen, not as a floor that started when it ended.
	if s := got["scratch"]; s.StartedAt == nil || s.EndedAt == nil || *s.StartedAt >= *s.EndedAt {
		t.Fatalf("scratch started %v, ended %v: want a real duration", s.StartedAt, s.EndedAt)
	}
	// The outcome is editable afterwards; and nothing is recorded twice.
	r.fake.SetFeed(wsWire, feed(two(false, false, "")))
	time.Sleep(100 * time.Millisecond)
	past, _ = floors.List(r.sqlDB, floors.ListOptions{WorkspaceID: "atlas", IncludePast: true})
	if len(past) != 2 {
		t.Fatalf("recorded again: %d", len(past))
	}
}

func TestIncompleteSnapshotNeverRecordsHistory(t *testing.T) {
	r := start(t, time.Minute)
	r.fake.SetFeed(wsWire, feed(feedOpts{}))
	waitFor(t, "seen", func() bool { li, _ := r.svc.LiveFloors("atlas"); return len(li) == 1 })
	s := feed(feedOpts{})
	s.Floors = nil // floors unavailable
	r.fake.SetFeed(wsWire, s)
	s2 := feed(feedOpts{floors: []wire.FloorMeta{{Name: "main"}}})
	s2.Workspace.IsLoaded = false // workspace not loaded in Maestri
	r.fake.SetFeed(wsWire, s2)
	time.Sleep(150 * time.Millisecond)
	if past, _ := floors.List(r.sqlDB, floors.ListOptions{WorkspaceID: "atlas", IncludePast: true}); len(past) != 0 {
		t.Fatalf("history recorded from an incomplete snapshot: %+v", past)
	}
}

func ev(kind, ws string, payload map[string]any) events.Event {
	b, _ := json.Marshal(payload)
	return events.Event{Kind: kind, WorkspaceID: ws, Payload: b}
}

func TestStandbyInterruptsWorkingAgentsOfTheProviderOnly(t *testing.T) {
	r := start(t, time.Minute)
	cfg := testConfig(t, t.TempDir())
	res := r.svc.HandleStandby(context.Background(), []events.Event{
		ev(events.KindStandby, "atlas", map[string]any{"provider": "claude", "action": "interrupt"}),
		ev(events.KindStandby, "other", map[string]any{"provider": "claude", "action": "interrupt"}), // same provider: once
	}, cfg)
	if res.Interrupted != 1 || len(res.Errors) != 0 {
		t.Fatalf("res = %+v", res)
	}
	waitFor(t, "esc", func() bool { return len(r.fake.Inputs()) == 1 })
	in := r.fake.Inputs()[0]
	// Vesper (claude, working). Not Maestro (manager), not Orin (gemini), not Wren (claude, idle).
	if in.Terminal != termV || len(in.Data) != 1 || in.Data[0] != 0x1b {
		t.Fatalf("input = %+v", in)
	}
	// "tell" does nothing on Wire.
	r.fake.ClearRequests()
	res = r.svc.HandleStandby(context.Background(), []events.Event{ev(events.KindStandby, "atlas", map[string]any{"provider": "claude", "action": "tell"})}, cfg)
	if res.Interrupted != 0 || len(r.fake.Inputs()) != 0 {
		t.Fatalf("tell interrupted agents: %+v", res)
	}
}

func TestStandbyWakesTheMaestroOnResetWhenEnabled(t *testing.T) {
	r := start(t, time.Minute)
	var mu sync.Mutex
	var prompts []string
	r.fake.OnPrompt = func(id, text string) { mu.Lock(); prompts = append(prompts, id+"|"+text); mu.Unlock() }
	cfg := testConfig(t, t.TempDir())
	cfg.Standby.WakeOnReset = true
	res := r.svc.HandleStandby(context.Background(), []events.Event{
		ev(events.KindResume, "atlas", map[string]any{"provider": "claude", "reason": "reset"}),
		ev(events.KindResume, "atlas", map[string]any{"provider": "claude", "reason": "manual"}), // not a reset
	}, cfg)
	if len(res.Woken) != 1 || len(res.Errors) != 0 {
		t.Fatalf("res = %+v", res)
	}
	mu.Lock()
	if len(prompts) != 1 || !strings.HasPrefix(prompts[0], termM+"|Maestria: the claude usage window has reset") {
		t.Fatalf("prompts = %v", prompts)
	}
	mu.Unlock()
	cfg.Standby.WakeOnReset = false
	res = r.svc.HandleStandby(context.Background(), []events.Event{ev(events.KindResume, "atlas", map[string]any{"provider": "claude", "reason": "reset"})}, cfg)
	if len(res.Woken) != 0 {
		t.Fatal("woke the Maestro with wake-on-reset off")
	}
}

func TestBriefsAreOneLineAndOverrideTheWirePreview(t *testing.T) {
	r := start(t, time.Minute)
	if err := activity.SetBrief(r.sqlDB, "atlas", "Vesper", "  Writing   retry\ttests\nfor webhooks ", 0); err != nil {
		t.Fatal(err)
	}
	if err := activity.SetBrief(r.sqlDB, "atlas", "Vesper", strings.Repeat("x", 161), 0); !errors.Is(err, activity.ErrInvalidBrief) {
		t.Fatalf("long brief: %v", err)
	}
	if err := activity.SetBrief(r.sqlDB, "atlas", "Vesper", "   ", 0); !errors.Is(err, activity.ErrInvalidBrief) {
		t.Fatalf("empty brief: %v", err)
	}
	act, err := r.svc.Activity("atlas")
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range act.Groups {
		for _, a := range g.Agents {
			switch a.Name {
			case "Vesper":
				if a.Brief != "Writing retry tests for webhooks" || a.BriefSource != "maestro" {
					t.Fatalf("vesper = %+v", a)
				}
			case "Orin":
				if a.BriefSource != "wire" || a.Brief != "working" {
					t.Fatalf("orin = %+v", a)
				}
			}
		}
	}
	if ok, _ := activity.ClearBrief(r.sqlDB, "atlas", "vesper"); !ok {
		t.Fatal("clear did nothing")
	}
	if ok, _ := activity.ClearBrief(r.sqlDB, "atlas", "vesper"); ok {
		t.Fatal("cleared twice")
	}
}

func TestFloorAlreadyClosedByTheMaestroIsNotRecordedAgain(t *testing.T) {
	r := start(t, time.Minute)
	withB := feed(feedOpts{})
	r.fake.SetFeed(wsWire, withB)
	waitUntil2(t, func() bool { li, _ := r.svc.LiveFloors("atlas"); return len(li) == 1 })
	if _, err := floors.Start(r.sqlDB, "atlas", "billing", floors.StartInput{Brief: "Billing"}, 1); err != nil {
		t.Fatal(err)
	}
	// The Maestro lands it with the CLI, then lands it in Maestri.
	if _, err := floors.Land(r.sqlDB, "atlas", "billing", "Shipped", false, time.Now().UnixMilli()+10); err != nil {
		t.Fatal(err)
	}
	r.fake.SetFeed(wsWire, feed(feedOpts{floors: []wire.FloorMeta{{Name: "main"}}}))
	time.Sleep(200 * time.Millisecond)
	past, _ := floors.List(r.sqlDB, floors.ListOptions{WorkspaceID: "atlas", IncludePast: true})
	if len(past) != 1 || past[0].Outcome != "Shipped" {
		t.Fatalf("history duplicated or overwritten: %+v", past)
	}
}

func waitUntil2(t *testing.T, cond func() bool) { t.Helper(); waitFor(t, "condition", cond) }
