// Package ui holds the embedded panel UI: the design system's CSS and icons, a small vanilla JS
// file, and the HTML templates. Everything works under a strict CSP: no inline script or style.
package ui

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

//go:embed static templates
var files embed.FS

type asset struct {
	body        []byte
	hash        string
	contentType string
}

// UI renders pages and serves static assets.
type UI struct {
	assets map[string]asset
	pages  map[string]*template.Template
	icons  template.HTML
}

// Pages that can be rendered.
const (
	PageLogin    = "login"
	PageTwoFA    = "2fa"
	PageOverview = "overview"
	PageSecurity = "security"
)

// New loads the embedded assets and parses the templates.
func New() (*UI, error) {
	u := &UI{assets: map[string]asset{}, pages: map[string]*template.Template{}}
	entries, err := fs.ReadDir(files, "static")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		body, err := files.ReadFile("static/" + e.Name())
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(body)
		ct := mime.TypeByExtension(path.Ext(e.Name()))
		switch path.Ext(e.Name()) {
		case ".css":
			ct = "text/css; charset=utf-8"
		case ".js":
			ct = "text/javascript; charset=utf-8"
		case ".svg":
			ct = "image/svg+xml"
		}
		u.assets[e.Name()] = asset{body: body, hash: hex.EncodeToString(sum[:])[:10], contentType: ct}
	}
	sprite, ok := u.assets["icons.svg"]
	if !ok {
		return nil, fmt.Errorf("ui: icons.svg missing")
	}
	u.icons = template.HTML(bytes.TrimSpace(sprite.body)) // embedded, trusted

	funcs := template.FuncMap{
		"asset": u.AssetURL,
		"icon": func(id string, class ...string) template.HTML {
			cls := "icon"
			if len(class) > 0 && class[0] != "" {
				cls += " " + class[0]
			}
			return template.HTML(`<svg class="` + template.HTMLEscapeString(cls) + `" aria-hidden="true"><use href="#` + template.HTMLEscapeString(id) + `"/></svg>`)
		},
		"sprite": func() template.HTML { return u.icons },
	}
	base, err := template.New("layout").Funcs(funcs).ParseFS(files, "templates/layout.html")
	if err != nil {
		return nil, err
	}
	for _, name := range []string{PageLogin, PageTwoFA, PageOverview, PageSecurity} {
		t, err := template.Must(base.Clone()).ParseFS(files, "templates/"+name+".html")
		if err != nil {
			return nil, err
		}
		u.pages[name] = t
	}
	return u, nil
}

// AssetURL returns the cache-busted URL of a static asset.
func (u *UI) AssetURL(name string) string {
	a, ok := u.assets[name]
	if !ok {
		panic("ui: unknown asset " + name)
	}
	return "/static/" + name + "?v=" + a.hash
}

// Render executes a page template into w (buffered, so a template error never sends half a page).
func (u *UI) Render(w io.Writer, page string, data *Data) error {
	t, ok := u.pages[page]
	if !ok {
		return fmt.Errorf("ui: unknown page %q", page)
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "page", data); err != nil {
		return err
	}
	_, err := buf.WriteTo(w)
	return err
}

// ServeStatic serves an embedded asset by name. Unknown names are 404s (strict whitelist).
// A request carrying the current content hash is cached for a year; anything else must revalidate.
func (u *UI) ServeStatic(w http.ResponseWriter, r *http.Request, name string) {
	a, ok := u.assets[name]
	if !ok || strings.Contains(name, "/") {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	if r.URL.Query().Get("v") == a.hash {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	w.Header().Set("Content-Type", a.contentType)
	w.Header().Set("ETag", `"`+a.hash+`"`)
	if match := r.Header.Get("If-None-Match"); match == `"`+a.hash+`"` {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(a.body)
}

// Banner is a status banner (data-status tone, title, text).
type Banner struct {
	Status string
	Icon   string
	Title  string
	Text   string
}

// AuditRow is one audit entry shown on the security page.
type AuditRow struct {
	When   string
	ISO    string
	Event  string
	Status string // optional status chip tone
	Label  string // optional status chip text
	IP     string
}

// Data is the template data for every page.
type Data struct {
	Title  string
	CSRF   string
	Nav    string
	Banner *Banner

	// Auth pages.
	Error  string // error under the main field
	Locked bool   // hide the form (kill switch)

	// App pages.
	Listen            string
	SessionExpires    string
	IdleMinutes       int
	AbsoluteHours     int
	SessionCount      int
	RecoveryLeft      int
	Killed            bool
	TunnelStop        bool
	TunnelStopCommand string
	Audit             []AuditRow
}
