package web

import (
	"strings"
	"testing"

	"github.com/paulo-lucas/maestria/internal/inbox"
)

// The overview lists every workspace's notices under "Updates", newest first, each with "Got it",
// and a filter chip per workspace that has some.
func TestOverviewListsUpdatesFromAllWorkspaces(t *testing.T) {
	h := newHarness(t)
	atlas := addTestWorkspace(t, h, "Atlas")
	borealis := addTestWorkspace(t, h, "Borealis")
	c := h.client()
	c.login()
	nowMs := h.clock.now().UnixMilli()

	if _, err := inbox.Post(h.db, atlas.ID, inbox.PostInput{Kind: inbox.KindNotice, Body: "Older atlas notice"}, nowMs); err != nil {
		t.Fatal(err)
	}
	if _, err := inbox.Post(h.db, borealis.ID, inbox.PostInput{Kind: inbox.KindNotice, Body: "Newer borealis notice"}, nowMs+10); err != nil {
		t.Fatal(err)
	}
	if _, err := inbox.Post(h.db, atlas.ID, inbox.PostInput{Kind: inbox.KindApprove, Body: "Ship it?"}, nowMs+5); err != nil {
		t.Fatal(err)
	}

	page := c.get("/").body
	i := strings.Index(page, `id="updates-list"`)
	if i < 0 {
		t.Fatalf("no updates list on the overview")
	}
	list := page[i:]
	if end := strings.Index(list, "</ul>"); end >= 0 {
		list = list[:end]
	}
	newer, older := strings.Index(list, "Newer borealis notice"), strings.Index(list, "Older atlas notice")
	if newer < 0 || older < 0 || newer > older {
		t.Fatalf("notices missing or not newest first (newer@%d older@%d)", newer, older)
	}
	if strings.Contains(list, "Ship it?") {
		t.Error("an approval is a task, not an update")
	}
	if got := strings.Count(list, `data-action="dismiss"`); got != 2 {
		t.Errorf("%d Got it buttons, want 2", got)
	}
	for _, want := range []string{`data-filter="updates-list"`, `data-action="dismiss-updates"`, `data-key="` + atlas.ID + `"`, `data-key="` + borealis.ID + `"`} {
		if !strings.Contains(page, want) {
			t.Errorf("overview lacks %s", want)
		}
	}
	// The approval still goes to Needs you only.
	needs := page[strings.Index(page, `id="needs-list"`):i]
	if !strings.Contains(needs, "Ship it?") || strings.Contains(needs, "notice") {
		t.Errorf("Needs you should hold the approval and no notice")
	}
}
