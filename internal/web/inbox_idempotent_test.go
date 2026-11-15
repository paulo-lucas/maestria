package web

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/paulo-lucas/maestria/internal/events"
	"github.com/paulo-lucas/maestria/internal/inbox"
)

func countEvents(t *testing.T, h *harness, wsID, kind string) int {
	t.Helper()
	evs, err := events.Since(h.db, wsID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range evs {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// A double click (or a retry after a lost answer) must not answer the same item twice: the second
// request is refused with 409 and no second event is emitted.
func TestInboxWritesAreIdempotent(t *testing.T) {
	h := newHarness(t)
	ws := addTestWorkspace(t, h, "Atlas")
	c := h.client()
	csrf := c.login()
	nowMs := h.clock.now().UnixMilli()

	approve, err := inbox.Post(h.db, ws.ID, inbox.PostInput{Kind: inbox.KindApprove, Body: "Ship it?"}, nowMs)
	if err != nil {
		t.Fatal(err)
	}
	notice, err := inbox.Post(h.db, ws.ID, inbox.PostInput{Kind: inbox.KindNotice, Body: "Done"}, nowMs+1)
	if err != nil {
		t.Fatal(err)
	}
	approvePath := "/api/workspaces/" + ws.ID + "/inbox/" + itoa(approve.ID)
	noticePath := "/api/workspaces/" + ws.ID + "/inbox/" + itoa(notice.ID)

	if r := c.postJSON(approvePath+"/respond", map[string]any{"response": "approve"}, csrf); r.Code != 200 {
		t.Fatalf("first respond=%d %s", r.Code, r.body)
	}
	r := c.postJSON(approvePath+"/respond", map[string]any{"response": "reject"}, csrf)
	if r.Code != 409 || !strings.Contains(r.body, "item_already_closed") {
		t.Fatalf("second respond=%d %s, want 409 item_already_closed", r.Code, r.body)
	}
	if n := countEvents(t, h, ws.ID, events.KindInboxResponse); n != 1 {
		t.Fatalf("inbox_response events=%d, want 1", n)
	}
	got, err := inbox.Get(h.db, approve.ID, nowMs)
	if err != nil {
		t.Fatal(err)
	}
	if got.Response == nil || !strings.HasPrefix(*got.Response, "Yes") {
		t.Fatalf("the second answer overwrote the first: %v", got.Response)
	}
	// A closed item can no longer be postponed either.
	if r := c.postJSON(approvePath+"/postpone", map[string]any{"preset": "1h"}, csrf); r.Code != 409 {
		t.Fatalf("postpone after answer=%d %s, want 409", r.Code, r.body)
	}

	if r := c.postJSON(noticePath+"/dismiss", map[string]any{}, csrf); r.Code != 200 {
		t.Fatalf("first dismiss=%d %s", r.Code, r.body)
	}
	if r := c.postJSON(noticePath+"/dismiss", map[string]any{}, csrf); r.Code != 409 {
		t.Fatalf("second dismiss=%d %s, want 409", r.Code, r.body)
	}
	if n := countEvents(t, h, ws.ID, events.KindInboxDismiss); n != 1 {
		t.Fatalf("inbox_dismiss events=%d, want 1", n)
	}
}

// Requests that really overlap: exactly one wins, whatever the interleaving.
func TestInboxConcurrentRespondAppliesOnce(t *testing.T) {
	h := newHarness(t)
	ws := addTestWorkspace(t, h, "Atlas")
	nowMs := h.clock.now().UnixMilli()
	it, err := inbox.Post(h.db, ws.ID, inbox.PostInput{Kind: inbox.KindChoice, Body: "Env?", Options: []string{"Staging", "Prod"}}, nowMs)
	if err != nil {
		t.Fatal(err)
	}

	var ok, closed int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := inbox.Respond(h.db, ws.ID, it.ID, "Staging", nowMs)
			switch err {
			case nil:
				atomic.AddInt32(&ok, 1)
			case inbox.ErrNotOpen:
				atomic.AddInt32(&closed, 1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok != 1 || closed != 7 {
		t.Fatalf("applied %d times, refused %d times; want 1 and 7", ok, closed)
	}
	if n := countEvents(t, h, ws.ID, events.KindInboxResponse); n != 1 {
		t.Fatalf("inbox_response events=%d, want 1", n)
	}
}
