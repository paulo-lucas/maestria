package wire_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/paulo-lucas/maestria/internal/wire"
	"github.com/paulo-lucas/maestria/internal/wire/wiretest"
)

const (
	wsWire = "11111111-1111-4111-8111-111111111111"
	termM  = "22222222-2222-4222-8222-222222222221" // Maestro
	termO  = "22222222-2222-4222-8222-222222222222" // Orin
	termV  = "22222222-2222-4222-8222-222222222223" // Vesper
	floorB = "44444444-4444-4444-8444-444444444441"
)

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

func newFake(t *testing.T) *wiretest.Server {
	t.Helper()
	f, err := wiretest.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	return f
}

func card(id, name, agent string, running bool) wire.TerminalCard {
	return wire.TerminalCard{ID: id, Name: name, AgentType: agent, IsRunning: running, Preview: []string{"$ work", "line two"}}
}

// sampleFeed is the workspace used by most tests: a Maestro on Ground, Orin waiting on a Y/n on
// the billing floor, Vesper working there.
func sampleFeed() wire.FeedSnapshot {
	maestro := card(termM, "Maestro", "claude", true)
	maestro.IsManager = true
	orin := card(termO, "Orin", "gemini", true)
	orin.FloorID, orin.FloorName, orin.NeedsAttention = floorB, "billing-webhooks", true
	vesper := card(termV, "Vesper", "claude", true)
	vesper.FloorID, vesper.FloorName = floorB, "billing-webhooks"
	p := 0.5
	return wire.FeedSnapshot{
		Workspace: wire.WorkspaceMeta{ID: wsWire, Name: "Atlas", IsLoaded: true},
		Floors: []wire.FloorMeta{
			{Name: "main", IsActive: true},
			{ID: floorB, Name: "billing-webhooks", Branch: "billing-webhooks", Status: &wire.FloorStatus{State: "working", Text: "tests", Progress: &p}},
		},
		Items: []wire.FeedItem{
			{Kind: wire.ItemTerminal, Terminal: &maestro},
			{Kind: wire.ItemPendingPrompt, Terminal: &orin, Prompt: "Install zod@3.23.8? (Y/n)"},
			{Kind: wire.ItemTerminal, Terminal: &vesper},
		},
		Epoch: "e1",
	}
}

type env struct {
	fake  *wiretest.Server
	dir   string
	path  string
	creds *wire.Credentials
	mgr   *wire.Manager

	mu     sync.Mutex
	audits []string
	feeds  int
}

func (e *env) auditEvents() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.audits...)
}

// start pairs against the fake, writes wire.json and runs a manager over the sample workspace.
func start(t *testing.T) *env {
	t.Helper()
	f := newFake(t)
	f.SetWorkspaces(wire.WorkspaceMeta{ID: wsWire, Name: "Atlas", IsLoaded: true, WorkingDirectory: "/work/atlas"})
	f.SetFeed(wsWire, sampleFeed())
	f.SetChat(termM, wire.ChatSnapshot{TerminalID: termM, Enabled: true, Status: "idle", Entries: []wire.ChatEntry{
		{ID: "e1", Role: "agent", Kind: "message", Text: "Hello"},
	}})
	f.SetPassword("hunter2")
	pin := f.Pin()
	creds, err := wire.Provision(context.Background(), f.Host, pin, "Test", "hunter2", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.ClearRequests() // the pairing calls are not what the tests inspect
	dir := t.TempDir()
	path := filepath.Join(dir, wire.FileName)
	if err := creds.Save(path); err != nil {
		t.Fatal(err)
	}
	e := &env{fake: f, dir: dir, path: path, creds: creds}
	e.mgr = wire.NewManager(wire.Options{
		CredentialsPath: path,
		Workspaces: func() []wire.WorkspaceRef {
			return []wire.WorkspaceRef{{ID: "atlas", Name: "Atlas", Path: "/work/atlas", MaestroAgent: "Maestro"}}
		},
		Audit: func(event, source string, detail any) {
			e.mu.Lock()
			e.audits = append(e.audits, event+" "+source)
			e.mu.Unlock()
		},
		PollInterval: 40 * time.Millisecond,
		MaxBackoff:   80 * time.Millisecond,
	}, wire.Handlers{Feed: func(string, *wire.FeedSnapshot) { e.mu.Lock(); e.feeds++; e.mu.Unlock() }})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.mgr.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return e
}

func (e *env) waitLive(t *testing.T) *wire.FeedSnapshot {
	t.Helper()
	var snap *wire.FeedSnapshot
	waitFor(t, "live feed", func() bool {
		s, err := e.mgr.Feed("atlas")
		snap = s
		return err == nil
	})
	return snap
}
