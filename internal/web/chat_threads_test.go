package web

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/maestria-dev/maestria/internal/chat"
	"github.com/maestria-dev/maestria/internal/wire"
)

// assertChatNeedsWire checks the Chat page of a workspace whose live chat is unavailable: the
// notice (why, what to do), a composer that is off, and no live conversation.
func assertChatNeedsWire(t *testing.T, page resp, wants ...string) {
	t.Helper()
	if page.Code != http.StatusOK {
		t.Fatalf("chat page: %d", page.Code)
	}
	for _, want := range append([]string{`class="banner"`, "disabled", "Chat is off until Maestri Wire is connected"}, wants...) {
		if !strings.Contains(page.body, want) {
			t.Errorf("chat page missing %q", want)
		}
	}
	for _, bad := range []string{"data-chat-form", `id="chat-log"`, `class="threads"`, "Message the Maestro\"></textarea>"} {
		if strings.Contains(page.body, bad) {
			t.Errorf("chat page still has %q while Wire is unavailable", bad)
		}
	}
	// Every textarea and send button of the page is disabled.
	for _, tag := range regexp.MustCompile(`<(textarea|button class="btn btn--primary btn--icon")[^>]*>`).FindAllString(page.body, -1) {
		if !strings.Contains(tag, "disabled") {
			t.Errorf("composer control is enabled: %s", tag)
		}
	}
}

func chatThreadsSnapshot(active int, unread int, entries ...wire.ChatEntry) wire.ChatSnapshot {
	items := []wire.ChatThreadItem{
		{ID: 0, Color: "blue", HasMessages: true}, {ID: 1, Color: "purple", HasMessages: true}, {ID: 2, Color: "pink"},
		{ID: 3, Color: "red"}, {ID: 4, Color: "orange"}, {ID: 5, Color: "yellow"}, {ID: 6, Color: "green"},
	}
	return wire.ChatSnapshot{TerminalID: wTermM, ConversationID: "conv-" + items[active].Color, Enabled: true, Status: "idle",
		Entries: entries, UnreadCount: unread, Threads: &wire.ChatThreads{ActiveSlot: active, CanSelect: true, Items: items}}
}

func threadRig(t *testing.T) *wireRig {
	r := newWireRig(t)
	r.fake.SetThreads(wTermM, wire.ChatThreadPreviews{Threads: []wire.ChatThreadPreview{
		{ID: 1, ConversationID: "conv-purple", Entries: []wire.ChatEntry{
			{ID: "p1", Role: "user", Kind: "message", Text: "Purple question"},
			{ID: "p2", Role: "agent", Kind: "message", Text: "Purple **answer**"},
		}},
	}})
	r.fake.SetChat(wTermM, chatThreadsSnapshot(0, 2, wire.ChatEntry{ID: "b1", Role: "agent", Kind: "message", Text: "Blue answer"}))
	waitUntil(t, "threads in chat", func() bool {
		return strings.Contains(r.c.get("/workspaces/"+r.ws.ID+"/chat").body, `class="threads"`)
	})
	return r
}

func TestChatShowsOneSwatchPerThreadAndTheActiveOneByDefault(t *testing.T) {
	r := threadRig(t)
	page := r.c.get("/workspaces/" + r.ws.ID + "/chat").body
	for _, color := range []string{"blue", "purple", "pink", "red", "orange", "yellow", "green"} {
		if !strings.Contains(page, `href="?thread=`+color+`"`) {
			t.Errorf("no selector link for %s", color)
		}
	}
	if n := strings.Count(page, `class="thread"`); n != 7 {
		t.Errorf("%d swatches, want 7", n)
	}
	// Tones come from the design tokens, not from inline colours.
	for _, tone := range []string{`data-tone="blue"`, `data-tone="violet"`, `data-tone="rose"`, `data-tone="amber"`} {
		if !strings.Contains(page, tone) {
			t.Errorf("missing %s", tone)
		}
	}
	// The thread selected in Maestri (blue) is shown and is the current one; it has unread messages.
	blue := regexp.MustCompile(`<a class="thread"[^>]*href="\?thread=blue"[^>]*>`).FindString(page)
	for _, want := range []string{`aria-current="true"`, "data-has", "data-active", "data-unread"} {
		if !strings.Contains(blue, want) {
			t.Errorf("blue swatch missing %s: %s", want, blue)
		}
	}
	pink := regexp.MustCompile(`<a class="thread"[^>]*href="\?thread=pink"[^>]*>`).FindString(page)
	if strings.Contains(pink, "data-has") || strings.Contains(pink, "aria-current") || strings.Contains(pink, "data-unread") {
		t.Errorf("pink is empty and not shown: %s", pink)
	}
	purple := regexp.MustCompile(`<a class="thread"[^>]*href="\?thread=purple"[^>]*>`).FindString(page)
	if !strings.Contains(purple, "data-has") || strings.Contains(purple, "aria-current") {
		t.Errorf("purple has messages but is not shown: %s", purple)
	}
	if !strings.Contains(page, "Blue answer") || strings.Contains(page, "Purple answer") {
		t.Error("default view is not the active thread")
	}
	if !strings.Contains(page, `data-thread="blue"`) || strings.Contains(page, "Reading the") {
		t.Error("composer is not tied to the active thread, or says it is read-only")
	}
}

func TestChatReadsAnotherThreadWithoutSwitchingTheApp(t *testing.T) {
	r := threadRig(t)
	r.fake.ClearRequests()
	page := r.c.get("/workspaces/" + r.ws.ID + "/chat?thread=purple").body
	for _, want := range []string{"Purple question", "<strong>answer</strong>", "Reading the Purple thread. Maestri has Blue selected.", "Messages go to the thread selected in Maestri (Blue)"} {
		if !strings.Contains(page, want) {
			t.Errorf("purple view missing %q", want)
		}
	}
	if strings.Contains(page, "Blue answer") {
		t.Error("the active thread leaked into another thread's view")
	}
	purple := regexp.MustCompile(`<a class="thread"[^>]*href="\?thread=purple"[^>]*>`).FindString(page)
	if !strings.Contains(purple, `aria-current="true"`) {
		t.Errorf("purple is not marked as shown: %s", purple)
	}
	// Reading is GET only: no thread switch, nothing marked read, nothing sent.
	for _, q := range r.fake.Requests() {
		if q.Method != http.MethodGet {
			t.Errorf("reading a thread made %s %s", q.Method, q.Path)
		}
		if strings.HasSuffix(q.Path, "/chat/thread") || strings.HasSuffix(q.Path, "/chat/read") || strings.HasSuffix(q.Path, "/prompt") {
			t.Errorf("reading a thread called %s", q.Path)
		}
	}
	// Sending is off on a thread that is not the one selected in Maestri.
	if !regexp.MustCompile(`<textarea[^>]*disabled`).MatchString(page) {
		t.Error("composer is enabled on a non-active thread")
	}
	// An empty thread says so; an unknown colour falls back to the active thread.
	if empty := r.c.get("/workspaces/" + r.ws.ID + "/chat?thread=pink").body; !strings.Contains(empty, "No messages in the Pink thread") {
		t.Errorf("empty thread:\n%s", empty)
	}
	if odd := r.c.get("/workspaces/" + r.ws.ID + "/chat?thread=%3Cscript%3E").body; !strings.Contains(odd, "Blue answer") || strings.Contains(odd, "<script>") {
		t.Error("unknown thread did not fall back to the active one")
	}
}

func TestChatNeverSendsToAThreadThatIsNotSelectedInMaestri(t *testing.T) {
	r := threadRig(t)
	url := "/api/workspaces/" + r.ws.ID + "/chat"
	r.fake.ClearRequests()
	// The page showed purple, but Maestri has blue selected: refused, nothing delivered.
	resp := r.c.postJSON(url, map[string]string{"body": "hello", "thread": "purple"}, r.csrf)
	if resp.Code != http.StatusConflict || !strings.Contains(resp.body, "thread_changed") {
		t.Fatalf("send to a non-active thread: %d %s", resp.Code, resp.body)
	}
	for _, q := range r.fake.Requests() {
		if q.Method != http.MethodGet {
			t.Fatalf("a write went out: %s %s", q.Method, q.Path)
		}
	}
	// The thread that is selected in Maestri takes it, and it is the Maestro's prompt route.
	ok := r.c.postJSON(url, map[string]string{"body": "hello", "thread": "blue"}, r.csrf)
	if ok.Code != http.StatusOK || !strings.Contains(ok.body, `"sent"`) {
		t.Fatalf("send to the active thread: %d %s", ok.Code, ok.body)
	}
	// A page that never knew about threads (older client) is unchanged.
	if old := r.c.postJSON(url, map[string]string{"body": "again"}, r.csrf); old.Code != http.StatusOK {
		t.Fatalf("send without thread: %d %s", old.Code, old.body)
	}
	// When Maestri moves to purple the guard follows it.
	r.fake.SetChat(wTermM, chatThreadsSnapshot(1, 0, wire.ChatEntry{ID: "p1", Role: "user", Kind: "message", Text: "Purple question"}))
	waitUntil(t, "purple active", func() bool {
		v, err := r.srv.Activity.Mgr.Chat(r.ws.ID)
		return err == nil && v.Snapshot != nil && v.Snapshot.Threads != nil && v.Snapshot.Threads.ActiveSlot == 1
	})
	if stale := r.c.postJSON(url, map[string]string{"body": "late", "thread": "blue"}, r.csrf); stale.Code != http.StatusConflict {
		t.Fatalf("stale thread: %d %s", stale.Code, stale.body)
	}
}

func TestChatThreadsDegradeWhereMaestriCannotShowThem(t *testing.T) {
	r := threadRig(t)
	// A host without chatThreadPreviews: other threads with messages are listed but not selectable.
	r.fake.SetCapabilities("feedSnapshots", "terminalChat", "workspaceActions", "terminalStreaming", "terminalInputBytes")
	waitUntil(t, "capability gone", func() bool {
		return strings.Contains(r.c.get("/workspaces/"+r.ws.ID+"/chat").body, `aria-disabled="true" title="Purple`)
	})
	// And a host without threads at all shows no selector.
	snap := chatThreadsSnapshot(0, 0, wire.ChatEntry{ID: "b1", Role: "agent", Kind: "message", Text: "Only conversation"})
	snap.Threads = nil
	r.fake.SetChat(wTermM, snap)
	waitUntil(t, "no threads", func() bool {
		b := r.c.get("/workspaces/" + r.ws.ID + "/chat").body
		return strings.Contains(b, "Only conversation") && !strings.Contains(b, `class="threads"`)
	})
}

func TestChatKeepsTheSavedLogReadableWithoutWire(t *testing.T) {
	h := newHarness(t)
	ws := addTestWorkspace(t, h, "Solo")
	if _, _, err := chat.Send(h.db, ws.ID, chat.SenderMaestro, "Deploy finished, **all green**.", h.srv.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	c := h.client()
	c.login()
	page := c.get("/workspaces/" + ws.ID + "/chat")
	assertChatNeedsWire(t, page, "maestria wire pair", "Saved here", "<strong>all green</strong>", "maestria say", "maestria wait")
}
