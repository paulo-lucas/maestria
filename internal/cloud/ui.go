package cloud

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"strings"

	"rsc.io/qr"

	"github.com/paulo-lucas/maestria/internal/ui"
)

//go:embed templates static
var files embed.FS

type cloudUI struct {
	*ui.UI
	pages map[string]*template.Template
}

func newCloudUI() (*cloudUI, error) {
	base, err := ui.New()
	if err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(files, "static")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		body, err := files.ReadFile("static/" + e.Name())
		if err != nil {
			return nil, err
		}
		base.AddAsset(e.Name(), body)
	}
	u := &cloudUI{UI: base, pages: map[string]*template.Template{}}
	tpls, err := fs.ReadDir(files, "templates")
	if err != nil {
		return nil, err
	}
	for _, e := range tpls {
		name := strings.TrimSuffix(e.Name(), ".html")
		if name == "layout" {
			continue
		}
		t, err := base.Layout()
		if err != nil {
			return nil, err
		}
		if _, err := t.ParseFS(files, "templates/layout.html", "templates/"+e.Name()); err != nil {
			return nil, fmt.Errorf("template %s: %w", e.Name(), err)
		}
		u.pages[name] = t
	}
	return u, nil
}

// Render executes a page into w (buffered: a template error never sends half a page).
func (u *cloudUI) Render(w io.Writer, page string, d *pageData) error {
	t, ok := u.pages[page]
	if !ok {
		return fmt.Errorf("cloud ui: unknown page %q", page)
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "page", d); err != nil {
		return err
	}
	_, err := buf.WriteTo(w)
	return err
}

// RenderContent renders only a page's content block (client-side navigation).
func (u *cloudUI) RenderContent(w io.Writer, page string, d *pageData) error {
	t, ok := u.pages[page]
	if !ok {
		return fmt.Errorf("cloud ui: unknown page %q", page)
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "content", d); err != nil {
		return err
	}
	_, err := buf.WriteTo(w)
	return err
}

// ServeStatic serves assets from both the shared design and the cloud's own files.
func (u *cloudUI) ServeStatic(w http.ResponseWriter, r *http.Request, name string) {
	u.UI.ServeStatic(w, r, name)
}

// pageData is the data for every cloud page.
type pageData struct {
	Title   string
	CSRF    string
	Nav     string
	Email   string
	Initial string
	Scripts []string
	Error   string
	Info    string
	Token   string

	Heading                       string
	PrimaryHref, PrimaryLabel     string
	SecondaryHref, SecondaryLabel string

	QRPath      string
	QRSize      int
	Secret      string
	Codes       []string
	UseRecovery bool

	Connectors []connectorPage
	PublicURL  string
}

type connectorPage struct {
	ID         string
	Name       string
	Online     bool
	Version    string
	System     string
	LastSeen   string
	Connected  string
	KillSwitch bool
	Linked     string
}

// qrPath draws text as one SVG path of unit squares, with a 2-module quiet zone.
func qrPath(text string) (path string, size int, err error) {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", 0, err
	}
	const quiet = 2
	size = code.Size + 2*quiet
	var sb strings.Builder
	// A white backing square, then the dark modules.
	for y := 0; y < code.Size; y++ {
		x := 0
		for x < code.Size {
			if !code.Black(x, y) {
				x++
				continue
			}
			start := x
			for x < code.Size && code.Black(x, y) {
				x++
			}
			fmt.Fprintf(&sb, "M%d %dh%dv1h-%dz", start+quiet, y+quiet, x-start, x-start)
		}
	}
	return sb.String(), size, nil
}
