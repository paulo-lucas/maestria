package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/paulo-lucas/maestria/internal/update"
)

type fakeUpdates struct {
	notice    *update.Notice
	dismissed []string
}

func (f *fakeUpdates) Cached() *update.Notice { return f.notice }
func (f *fakeUpdates) Dismiss(v string)       { f.dismissed = append(f.dismissed, v); f.notice = nil }

func TestSecurityPageShowsAndDismissesTheUpdateNote(t *testing.T) {
	fu := &fakeUpdates{notice: &update.Notice{Version: "1.2.0", URL: "https://github.com/paulo-lucas/maestria/releases/tag/v1.2.0"}}
	h := newHarness(t, opts{updates: fu})
	c := h.client()
	c.login()

	page := c.get("/security")
	if !strings.Contains(page.body, "Maestria 1.2.0 is available") || !strings.Contains(page.body, "maestria update") ||
		!strings.Contains(page.body, "releases/tag/v1.2.0") {
		t.Fatalf("note missing:\n%s", page.body)
	}

	// A write: no CSRF token, no dismissal.
	if r := c.postForm("/updates/dismiss", url.Values{"version": {"1.2.0"}}, nil); r.Code != http.StatusForbidden || len(fu.dismissed) != 0 {
		t.Fatalf("without a token: %d dismissed=%v", r.Code, fu.dismissed)
	}
	r := c.postForm("/updates/dismiss", url.Values{"_csrf": {csrfFrom(t, page)}, "version": {"1.2.0"}}, nil)
	if r.Code != http.StatusSeeOther || len(fu.dismissed) != 1 || fu.dismissed[0] != "1.2.0" {
		t.Fatalf("dismiss: %d %v", r.Code, fu.dismissed)
	}
	if strings.Contains(c.get("/security").body, "is available") {
		t.Error("the note should be gone after dismissing")
	}
	entries, err := h.srv.Audit.Recent(10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		found = found || e.Event == "update_dismiss"
	}
	if !found {
		t.Error("the dismissal was not audited")
	}
}

func TestUpdateNoteNeedsASessionAndIgnoresForeignLinks(t *testing.T) {
	fu := &fakeUpdates{notice: &update.Notice{Version: "1.2.0", URL: "https://evil.example/x"}}
	h := newHarness(t, opts{updates: fu})
	anon := h.client()
	if r := anon.postForm("/updates/dismiss", url.Values{"version": {"1.2.0"}}, nil); r.Code == http.StatusSeeOther && len(fu.dismissed) != 0 {
		t.Fatalf("anonymous dismissal worked: %d", r.Code)
	}
	if len(fu.dismissed) != 0 {
		t.Fatal("an anonymous request dismissed the note")
	}
	c := h.client()
	c.login()
	body := c.get("/security").body
	if strings.Contains(body, "evil.example") || !strings.Contains(body, "is available") {
		t.Errorf("only github.com release links may be shown:\n%s", body)
	}
}

func TestNoUpdateNoteByDefault(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	c.login()
	if strings.Contains(c.get("/security").body, "is available") {
		t.Error("no note expected")
	}
}
