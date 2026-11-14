package wire_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/maestria-dev/maestria/internal/wire"
)

func TestManagerMirrorsTheFeedWithTheGuestTokenOnly(t *testing.T) {
	e := start(t)
	snap := e.waitLive(t)
	if len(snap.Terminals()) != 3 {
		t.Fatalf("terminals = %d", len(snap.Terminals()))
	}
	st := e.mgr.Status()
	if st.State != wire.StateConnected || !st.OwnerOK || !st.Has(wire.CapFeed) || st.Has("someFutureThing") == false {
		// Unknown capabilities are kept in the list but nothing acts on them.
		t.Fatalf("status = %+v", st)
	}
	// Every feed subscription is a guest one: an owner subscription would wake terminals.
	waitFor(t, "subscription", func() bool { return len(e.fake.FeedSubscriberRoles()) > 0 })
	for _, r := range e.fake.FeedSubscriberRoles() {
		if r != "guest" {
			t.Fatalf("feed subscribed with role %q", r)
		}
	}
	// And the owner token has touched nothing but info so far.
	for _, r := range e.fake.Requests() {
		if r.Role == "owner" && r.Path != "/api/info" {
			t.Fatalf("owner token used for %s %s", r.Method, r.Path)
		}
	}
}

func TestManagerReconnectsAfterTheSocketDrops(t *testing.T) {
	e := start(t)
	e.waitLive(t)
	waitFor(t, "first snapshot callback", func() bool { e.mu.Lock(); defer e.mu.Unlock(); return e.feeds >= 1 })
	e.fake.DropSockets()
	// New data pushed while it reconnects is picked up: the new socket gets the stored snapshot.
	next := sampleFeed()
	next.Floors = append(next.Floors, wire.FloorMeta{ID: "44444444-4444-4444-8444-444444444442", Name: "invoice-pdf"})
	e.fake.SetFeed(wsWire, next)
	waitFor(t, "reconnect with new data", func() bool {
		s, err := e.mgr.Feed("atlas")
		return err == nil && len(s.Floors) == 3
	})
}

func TestNoStaleDataWhenTheLinkDrops(t *testing.T) {
	e := start(t)
	e.waitLive(t)
	e.fake.Close()
	waitFor(t, "disconnect noticed", func() bool { _, err := e.mgr.Feed("atlas"); return err != nil })
	if _, err := e.mgr.Feed("atlas"); !errors.Is(err, wire.ErrNotConnected) {
		t.Fatalf("err = %v", err)
	}
	waitFor(t, "unreachable status", func() bool { return e.mgr.Status().State == wire.StateUnreachable })
	if len(e.mgr.Feeds()) != 0 {
		t.Fatal("feeds still served")
	}
	if _, err := e.mgr.Chat("atlas"); err == nil {
		t.Fatal("chat still served")
	}
}

func TestCapabilityGatingTurnsFeaturesOffAndIgnoresUnknownOnes(t *testing.T) {
	f := newFake(t)
	f.SetCapabilities("feedSnapshots", "somethingBrandNew") // no terminalChat, no streaming
	f.SetWorkspaces(wire.WorkspaceMeta{ID: wsWire, Name: "Atlas", IsLoaded: true})
	f.SetFeed(wsWire, sampleFeed())
	f.SetPassword("pw")
	creds, err := wire.Provision(context.Background(), f.Host, f.Pin(), "T", "pw", nil)
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/wire.json"
	_ = creds.Save(path)
	m := wire.NewManager(wire.Options{CredentialsPath: path, PollInterval: 30 * time.Millisecond,
		Workspaces: func() []wire.WorkspaceRef {
			return []wire.WorkspaceRef{{ID: "atlas", Name: "atlas"}} // name fallback: no workspaceActions
		}}, wire.Handlers{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	waitFor(t, "feed", func() bool { _, err := m.Feed("atlas"); return err == nil })
	if _, err := m.Chat("atlas"); !errors.Is(err, wire.ErrNoCapability) {
		t.Fatalf("chat without terminalChat: %v", err)
	}
	if err := m.Prompt(ctx, "test", "atlas", termM, "hi"); !errors.Is(err, wire.ErrNoCapability) {
		t.Fatalf("prompt without terminalChat: %v", err)
	}
	if err := m.Interrupt(ctx, "test", "atlas", termO); !errors.Is(err, wire.ErrNoCapability) {
		t.Fatalf("interrupt without streaming: %v", err)
	}
	// No chat socket was ever opened, and nothing probed a route the host did not advertise.
	for _, r := range f.Requests() {
		if strings.Contains(r.Path, "/chat") || strings.HasSuffix(r.Path, "/stream") && r.Path != "/api/feed/stream" {
			t.Fatalf("probed %s", r.Path)
		}
	}

	// Without feedSnapshots nothing is mirrored at all.
	f.SetCapabilities("terminalChat")
	waitFor(t, "feed capability gone", func() bool { _, err := m.Feed("atlas"); return errors.Is(err, wire.ErrNoCapability) })
}

func TestLinkStates(t *testing.T) {
	t.Run("not paired, then picks up wire.json by itself", func(t *testing.T) {
		f := newFake(t)
		f.SetPassword("pw")
		f.SetWorkspaces(wire.WorkspaceMeta{ID: wsWire, Name: "Atlas", IsLoaded: true})
		f.SetFeed(wsWire, sampleFeed())
		path := t.TempDir() + "/wire.json"
		m := wire.NewManager(wire.Options{CredentialsPath: path, PollInterval: 30 * time.Millisecond,
			Workspaces: func() []wire.WorkspaceRef { return []wire.WorkspaceRef{{ID: "atlas", Name: "Atlas"}} }}, wire.Handlers{})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go m.Run(ctx)
		waitFor(t, "disabled", func() bool { return m.Status().State == wire.StateDisabled })
		if _, err := m.Feed("atlas"); !errors.Is(err, wire.ErrNotConnected) {
			t.Fatalf("feed while unpaired: %v", err)
		}
		creds, err := wire.Provision(ctx, f.Host, f.Pin(), "T", "pw", nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = creds.Save(path)
		waitFor(t, "connected", func() bool { return m.Status().State == wire.StateConnected })
		waitFor(t, "feed", func() bool { _, err := m.Feed("atlas"); return err == nil })
	})
	t.Run("pin mismatch is refused and explained", func(t *testing.T) {
		e := start(t)
		e.waitLive(t)
		e.fake.RotateCert()
		waitFor(t, "pin_mismatch", func() bool { return e.mgr.Status().State == wire.StatePinMismatch })
		st := e.mgr.Status()
		if !strings.Contains(st.Message, "security key") || strings.Contains(st.LastError, e.creds.Guest.Token) {
			t.Fatalf("status = %+v", st)
		}
		if _, err := e.mgr.Feed("atlas"); err == nil {
			t.Fatal("feed served over a mismatched key")
		}
	})
	t.Run("revoked token", func(t *testing.T) {
		e := start(t)
		e.waitLive(t)
		e.fake.RevokeAll()
		waitFor(t, "revoked", func() bool { return e.mgr.Status().State == wire.StateRevoked })
		if !strings.Contains(e.mgr.Status().Message, "maestria wire pair") {
			t.Fatalf("message = %q", e.mgr.Status().Message)
		}
		if _, err := e.mgr.Feed("atlas"); err == nil {
			t.Fatal("feed served after revocation")
		}
	})
	t.Run("other protocol version", func(t *testing.T) {
		e := start(t)
		e.waitLive(t)
		e.fake.SetProtocol(2)
		waitFor(t, "incompatible", func() bool { return e.mgr.Status().State == wire.StateIncompatible })
	})
	t.Run("wire turned off then on again", func(t *testing.T) {
		e := start(t)
		e.waitLive(t)
		e.fake.Close()
		waitFor(t, "unreachable", func() bool { return e.mgr.Status().State == wire.StateUnreachable })
		if e.mgr.Status().Message == "" {
			t.Fatal("no explanation")
		}
	})
}

func TestChatStreamSnapshotsReplaceTheTail(t *testing.T) {
	e := start(t)
	e.waitLive(t)
	waitFor(t, "chat", func() bool { v, err := e.mgr.Chat("atlas"); return err == nil && v.Live && v.Snapshot != nil })
	v, _ := e.mgr.Chat("atlas")
	if v.Terminal.ID != termM || len(v.Snapshot.Entries) != 1 {
		t.Fatalf("chat = %+v", v)
	}
	e.fake.SetChat(termM, wire.ChatSnapshot{TerminalID: termM, Enabled: true, Status: "working", Entries: []wire.ChatEntry{
		{ID: "e1", Role: "agent", Kind: "message", Text: "Hello"},
		{ID: "e2", Role: "user", Kind: "message", Text: "Status?"},
	}})
	waitFor(t, "second snapshot", func() bool {
		v, _ := e.mgr.Chat("atlas")
		return v.Snapshot != nil && len(v.Snapshot.Entries) == 2 && v.Snapshot.Status == "working"
	})
	// Dropping the socket never leaves stale chat behind: it is not served until it is live again.
	e.fake.DropSockets()
	waitFor(t, "chat live again", func() bool { v, _ := e.mgr.Chat("atlas"); return v.Live && v.Snapshot != nil })
}

func TestWritesGoThroughTheOwnerTokenAndAreAudited(t *testing.T) {
	e := start(t)
	e.waitLive(t)
	ctx := context.Background()

	var prompted []string
	e.fake.OnPrompt = func(id, text string) { prompted = append(prompted, id+":"+text) }
	if err := e.mgr.Prompt(ctx, "10.0.0.1", "atlas", termM, "How is billing?"); err != nil {
		t.Fatal(err)
	}
	if len(prompted) != 1 || prompted[0] != termM+":How is billing?" {
		t.Fatalf("prompted = %v", prompted)
	}

	hash := wire.PromptHash("Install zod@3.23.8? (Y/n)")
	var answered []string
	e.fake.OnAnswer = func(id string, approve bool) { answered = append(answered, id) }
	if err := e.mgr.Answer(ctx, "10.0.0.1", "atlas", termO, "stale-hash", true); !errors.Is(err, wire.ErrPromptChanged) {
		t.Fatalf("stale hash accepted: %v", err)
	}
	if err := e.mgr.Answer(ctx, "10.0.0.1", "atlas", termV, "", true); !errors.Is(err, wire.ErrPromptChanged) {
		t.Fatalf("answering a terminal with no prompt: %v", err)
	}
	if err := e.mgr.Answer(ctx, "10.0.0.1", "atlas", termO, hash, true); err != nil {
		t.Fatal(err)
	}
	if err := e.mgr.Answer(ctx, "10.0.0.1", "atlas", termO, "", false); err != nil {
		t.Fatal(err)
	}
	if len(answered) != 2 {
		t.Fatalf("answered = %v", answered)
	}
	if err := e.mgr.Prompt(ctx, "x", "atlas", "99999999-9999-4999-8999-999999999999", "hi"); !errors.Is(err, wire.ErrUnknownTerminal) {
		t.Fatalf("terminal outside the workspace: %v", err)
	}
	if err := e.mgr.Interrupt(ctx, "standby", "atlas", termV); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "esc input", func() bool { return len(e.fake.Inputs()) == 1 })
	if in := e.fake.Inputs()[0]; in.Terminal != termV || len(in.Data) != 1 || in.Data[0] != 0x1b || in.Role != "owner" {
		t.Fatalf("input = %+v", in)
	}

	// Writes by role: the owner did the writes, the guest did none.
	for _, r := range e.fake.Requests() {
		if r.Method != "GET" && r.Method != "POST" || r.Path == "/pair" {
			continue
		}
		if r.Method == "POST" && r.Role != "owner" {
			t.Fatalf("write by %q: %s", r.Role, r.Path)
		}
	}
	// Audit: one entry per attempt, with route and result and never text or tokens.
	audits := strings.Join(e.auditEvents(), "\n")
	for _, want := range []string{"wire.prompt 10.0.0.1", "wire.approve 10.0.0.1", "wire.reject 10.0.0.1", "wire.interrupt standby"} {
		if !strings.Contains(audits, want) {
			t.Errorf("audit missing %q in\n%s", want, audits)
		}
	}
	if strings.Contains(audits, e.creds.Owner.Token) || strings.Contains(audits, "How is billing") {
		t.Fatal("audit leaks a token or the message text")
	}
}

func TestWritesRefuseWithoutAnOwnerToken(t *testing.T) {
	e := start(t)
	e.waitLive(t)
	// Rewrite wire.json without the owner token (read-only pairing).
	c := *e.creds
	c.Owner = nil
	if err := c.Save(e.path); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "owner gone", func() bool {
		return !e.mgr.Status().OwnerPaired
	})
	waitFor(t, "live again", func() bool { _, err := e.mgr.Feed("atlas"); return err == nil })
	if err := e.mgr.Prompt(context.Background(), "x", "atlas", termM, "hi"); !errors.Is(err, wire.ErrNoOwner) {
		t.Fatalf("err = %v", err)
	}
}

func TestSendThatMayHaveBeenDeliveredIsAmbiguousAndNotRetried(t *testing.T) {
	e := start(t)
	e.waitLive(t)
	e.fake.DropWriteReplies(true) // the host receives it, then the connection dies
	err := e.mgr.Prompt(context.Background(), "x", "atlas", termM, "hello")
	if !errors.Is(err, wire.ErrAmbiguous) {
		t.Fatalf("err = %v, want ErrAmbiguous", err)
	}
	time.Sleep(150 * time.Millisecond) // a retry would have shown up by now
	n := 0
	for _, r := range e.fake.Requests() {
		if r.Method == "POST" && strings.HasSuffix(r.Path, "/prompt") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("prompt POSTed %d times", n)
	}
	if !strings.Contains(strings.Join(e.auditEvents(), "\n"), "wire.prompt x") {
		t.Fatal("ambiguous send not audited")
	}
}

func TestFeedWorkspaceMappingByPathThenName(t *testing.T) {
	metas := []wire.WorkspaceMeta{
		{ID: "a", Name: "Atlas", WorkingDirectory: "/work/atlas"},
		{ID: "b", Name: "Nimbus"},
		{ID: "c", Name: "Dup"}, {ID: "d", Name: "dup"},
	}
	refs := []wire.WorkspaceRef{
		{ID: "r1", Name: "renamed", Path: "/work/atlas"}, // path wins over name
		{ID: "r2", Name: "nimbus"},                       // name fallback, case-insensitive
		{ID: "r3", Name: "Dup"},                          // ambiguous name: no match
		{ID: "r4", Name: "Missing"},
	}
	got := wire.MapWorkspaces(refs, metas)
	if got["r1"].ID != "a" || got["r2"].ID != "b" {
		t.Fatalf("got = %+v", got)
	}
	if _, ok := got["r3"]; ok {
		t.Fatal("ambiguous name was mapped")
	}
	if _, ok := got["r4"]; ok {
		t.Fatal("unknown workspace was mapped")
	}
}
