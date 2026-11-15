package web

import (
	"net/http"
	"strings"
	"testing"

	"github.com/paulo-lucas/maestria/internal/update"
)

// The favicon set and the manifest are requested by the browser before anyone logs in, so they
// must pass the Host and auth gates, and the manifest must carry its own media type.
func TestFaviconAndManifestAreServedBeforeLogin(t *testing.T) {
	h := newHarness(t)
	anon := h.client()
	for name, ct := range map[string]string{
		"favicon.svg":          "image/svg+xml",
		"favicon-32.png":       "image/png",
		"apple-touch-icon.png": "image/png",
		"icon-192.png":         "image/png",
		"icon-512.png":         "image/png",
		"manifest.webmanifest": "application/manifest+json",
		"sidebar.js":           "text/javascript",
	} {
		r := anon.get("/static/" + name)
		if r.Code != http.StatusOK || len(r.body) == 0 {
			t.Errorf("%s: %d", name, r.Code)
			continue
		}
		if got := r.Header().Get("Content-Type"); !strings.HasPrefix(got, ct) {
			t.Errorf("%s content type %q, want %q", name, got, ct)
		}
		if got := r.Header().Get("Content-Security-Policy"); got != CSP {
			t.Errorf("%s CSP %q", name, got)
		}
	}

	page := anon.get("/login").body
	for _, want := range []string{
		`rel="icon" href="/static/favicon.svg?v=`,
		`rel="icon" href="/static/favicon-32.png?v=`,
		`rel="apple-touch-icon" href="/static/apple-touch-icon.png?v=`,
		`rel="manifest" href="/static/manifest.webmanifest?v=`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("login page misses %q", want)
		}
	}
}

func TestSidebarFooterShowsTheVersionAndAnUpdateHint(t *testing.T) {
	const versionHTML = `<span>maestria v1.1.0` // the test client drops every closing </span>

	h := newHarness(t, opts{version: "1.1.0"})
	c := h.client()
	c.login()
	page := c.get("/security").body
	if !strings.Contains(page, versionHTML) {
		t.Fatalf("version missing from the footer:\n%s", page)
	}
	if strings.Contains(page, `class="ver-dot"`) {
		t.Fatal("no update is known, so no hint is expected")
	}
	for _, want := range []string{`data-sidebar-resizer`, `role="separator"`, `/static/sidebar.js?v=`} {
		if !strings.Contains(page, want) {
			t.Errorf("shell misses %q", want)
		}
	}

	fu := &fakeUpdates{notice: &update.Notice{Version: "1.2.0"}}
	h = newHarness(t, opts{version: "1.1.0", updates: fu})
	c = h.client()
	c.login()
	// The hint is on every page, not only on Security.
	page = c.get("/usage").body
	if !strings.Contains(page, `class="ver-dot"`) || !strings.Contains(page, `title="v1.2.0 is available"`) {
		t.Fatalf("update hint missing:\n%s", page)
	}

	// A development build says so and still renders.
	h = newHarness(t, opts{version: "dev"})
	c = h.client()
	c.login()
	if page = c.get("/usage").body; !strings.Contains(page, "<span>maestria dev") {
		t.Fatal("dev build not shown")
	}
}
