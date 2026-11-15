package cloud

import (
	"encoding/json"
	"strings"
	"testing"
)

type fakeReleases string

func (f fakeReleases) LatestKnown() string { return string(f) }

// The server says which machines are behind the newest release it knows, from its cache: the
// browser makes no request for it. Dev builds and machines already on the release get no hint.
func TestMachinesBehindTheLatestReleaseAreFlagged(t *testing.T) {
	f := newFixture(t) // both machines report 9.9.9-test
	f.b.srv.version = "1.0.0"

	// No release is known: nothing is flagged.
	var list struct{ Connectors []connectorView }
	if err := json.Unmarshal([]byte(f.br.api("GET", "/api/connectors", nil).body), &list); err != nil {
		t.Fatal(err)
	}
	for _, c := range list.Connectors {
		if c.UpdateAvailable != "" {
			t.Fatalf("%s flagged without a known release: %q", c.Name, c.UpdateAvailable)
		}
	}
	if page := f.br.get("/connectors").body; strings.Contains(page, "ver-hint") {
		t.Fatal("hint shown without a known release")
	}

	f.b.srv.releases = fakeReleases("10.0.0")
	list.Connectors = nil
	if err := json.Unmarshal([]byte(f.br.api("GET", "/api/connectors", nil).body), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Connectors) != 2 {
		t.Fatalf("connectors: %+v", list.Connectors)
	}
	for _, c := range list.Connectors {
		if c.UpdateAvailable != "v10.0.0" {
			t.Fatalf("%s: updateAvailable %q", c.Name, c.UpdateAvailable)
		}
	}
	page := f.br.get("/connectors").body
	if !strings.Contains(page, "v10.0.0 available") {
		t.Fatalf("connectors page misses the hint:\n%s", page)
	}
	// The footer names the server's own version, and flags it when it is behind too.
	if !strings.Contains(page, "<span>Server v1.0.0") || !strings.Contains(page, `title="v10.0.0 is available"`) {
		t.Fatal("server version or its hint missing from the footer")
	}
	// The phone has no sidebar footer: the scope menu carries it too, and hosted.js reads the
	// data attributes to add it to Security's About line.
	if !strings.Contains(page, `data-version="Server v1.0.0" data-update="v10.0.0"`) ||
		!strings.Contains(page, `<div class="menu__meta"><span>Server v1.0.0</span><a class="ver-dot"`) {
		t.Fatalf("server version missing from the scope menu:\n%s", page)
	}

	// A machine on the latest release is not flagged.
	f.b.srv.releases = fakeReleases("9.9.9-test")
	list.Connectors = nil
	_ = json.Unmarshal([]byte(f.br.api("GET", "/api/connectors", nil).body), &list)
	for _, c := range list.Connectors {
		if c.UpdateAvailable != "" {
			t.Fatalf("%s on the latest release flagged: %q", c.Name, c.UpdateAvailable)
		}
	}
}

func TestFaviconAndManifestArePublicOnTheCloud(t *testing.T) {
	b := newBed(t)
	for name, ct := range map[string]string{
		"favicon.svg":          "image/svg+xml",
		"favicon-32.png":       "image/png",
		"apple-touch-icon.png": "image/png",
		"manifest.webmanifest": "application/manifest+json",
		"sidebar.js":           "text/javascript",
	} {
		r := b.browser("10.0.0.9").get("/static/" + name)
		if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), ct) {
			t.Errorf("%s: %d %q", name, r.StatusCode, r.Header.Get("Content-Type"))
		}
	}
	page := b.browser("10.0.0.9").get("/login").body
	if !strings.Contains(page, `rel="manifest" href="/static/manifest.webmanifest?v=`) {
		t.Fatal("login page does not link the manifest")
	}
}
