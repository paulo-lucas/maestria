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
	funcs  template.FuncMap
	base   *template.Template
}

// Pages that can be rendered.
const (
	PageLogin      = "login"
	PageTwoFA      = "2fa"
	PageOverview   = "overview"
	PageTasks      = "workspace-tasks"
	PageChat       = "workspace-chat"
	PageFloors     = "workspace-floors"
	PageDocs       = "workspace-docs"
	PageUsage      = "usage"
	PageSecurity   = "security"
	PageComingSoon = "coming-soon"
	PageActivity   = "workspace-activity"
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
		"plural": func(n int, one, many string) string {
			if n == 1 {
				return one
			}
			return many
		},
		"markClass": func(step int) string {
			if step >= 50 && step <= 100 && step%5 == 0 {
				return fmt.Sprintf("mark-%d", step)
			}
			return ""
		},
		"taskIcon": func(kind string) string {
			switch kind {
			case "approve":
				return "i-approve"
			case "choice":
				return "i-choice"
			case "input":
				return "i-question"
			case "notice":
				return "i-bell"
			case "agent_prompt":
				return "i-terminal"
			default:
				return "i-inbox"
			}
		},
		"taskKindLabel": func(kind string) string {
			switch kind {
			case "approve":
				return "Approval"
			case "choice":
				return "Choice"
			case "input":
				return "Question"
			case "notice":
				return "Update"
			case "agent_prompt":
				return "Agent prompt"
			default:
				return "Task"
			}
		},
	}
	base, err := template.New("layout").Funcs(funcs).ParseFS(files, "templates/layout.html")
	if err != nil {
		return nil, err
	}
	u.funcs, u.base = funcs, base
	for _, name := range []string{PageLogin, PageTwoFA, PageOverview, PageTasks, PageChat, PageFloors, PageDocs, PageUsage, PageSecurity, PageComingSoon, PageActivity} {
		t, err := template.Must(base.Clone()).ParseFS(files, "templates/"+name+".html")
		if err != nil {
			return nil, err
		}
		u.pages[name] = t
	}
	return u, nil
}

// Funcs returns the template functions (asset, icon, sprite, ...), for servers that render their
// own pages with the same design system.
func (u *UI) Funcs() template.FuncMap { return u.funcs }

// Layout returns a fresh copy of the shared layout templates (head, brandmark, banner, auth...).
// A caller may redefine any of them in its own copy.
func (u *UI) Layout() (*template.Template, error) { return u.base.Clone() }

// AddAsset registers another static file (served by ServeStatic and returned by AssetURL).
func (u *UI) AddAsset(name string, body []byte) {
	sum := sha256.Sum256(body)
	ct := mime.TypeByExtension(path.Ext(name))
	switch path.Ext(name) {
	case ".css":
		ct = "text/css; charset=utf-8"
	case ".js":
		ct = "text/javascript; charset=utf-8"
	case ".svg":
		ct = "image/svg+xml"
	}
	u.assets[name] = asset{body: body, hash: hex.EncodeToString(sum[:])[:10], contentType: ct}
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

// WorkspaceNav represents a workspace entry in the shell navigation and overview strip.
type WorkspaceNav struct {
	ID             string
	Name           string
	Tone           string
	Initials       string
	Machine        string
	NeedsCount     int
	UpdatesCount   int
	PostponedCount int
	TotalOpenCount int
	Selected       bool
}

// TaskView represents a human task (open, postponed, update, or history) in templates.
type TaskView struct {
	ID            int64
	WorkspaceID   string
	WorkspaceName string
	WorkspaceTone string
	Kind          string
	KindLabel     string
	KindIcon      string
	FloorName     string
	By            string
	Title         string
	BodyHTML      template.HTML // sanitized via bluemonday before passing into TaskView
	Placeholder   string
	Options       []string
	Status        string
	Response      string
	Comment       string
	StatusTone    string
	StatusLabel   string
	PostponeLabel string
	CreatedRel    string
	CreatedISO    string
	ResolvedLabel string
}

// ChatMessageView renders one workspace chat entry.
type ChatMessageView struct {
	ID        int64
	Key       string // stable id of a Wire chat entry
	Via       string // "maestria" for a message posted with `maestria say`
	Author    string
	BodyHTML  template.HTML // sanitized via bluemonday before passing into ChatMessageView
	TimeLabel string
	TimeISO   string
	DayLabel  string
}

// ThresholdOption represents one dropdown option for a quota window threshold.
type ThresholdOption struct {
	Value    int
	Label    string
	Selected bool
}

// UsageWindowView represents one quota meter and its standby controls.
type UsageWindowView struct {
	AccountKey     string
	ID             string
	Label          string
	UsedPct        int
	ResetLabel     string
	Threshold      int
	ThresholdValue int
	MarkClass      string
	Action         string
	Armed          bool
	Tripped        bool
	WakeOnReset    bool
	StatusTone     string
	FootRight      string
	SwitchID       string
	SelectID       string
	RadioName      string
	ThresholdOpts  []ThresholdOption
}

// UsageAccountView represents one provider account card.
type UsageAccountView struct {
	Key           string
	Provider      string
	ProviderLabel string
	AccountLabel  string
	Plan          string
	Subline       string
	State         string
	Error         string
	Stale         bool
	StatusTone    string
	StatusLabel   string
	StatusBadge   bool
	Windows       []UsageWindowView
}

// HiddenProviderView lists a provider hidden from the overview after errors without fresh data.
type HiddenProviderView struct {
	Key           string
	Provider      string
	ProviderLabel string
	State         string
	Error         string
}

// StandbyBannerView describes an active standby banner across the usage page.
type StandbyBannerView struct {
	Active   bool
	GuardKey string
	Title    string
	Text     string
}

// FloorAgentView renders an agent avatar and name on a floor card.
type FloorAgentView struct {
	Name    string
	Initial string
	Tone    string
}

// FloorView renders a Present, Future, Past, or Overview floor entry.
type FloorView struct {
	ID            int64
	WorkspaceID   string
	WorkspaceName string
	WorkspaceTone string
	Name          string
	BriefHTML     template.HTML // sanitized via bluemonday before passing into FloorView
	State         string
	Ongoing       bool
	HasProgress   bool
	ProgressValue string
	ProgressPct   int
	Status        string
	StatusLabel   string
	StatusText    string
	Agents        []FloorAgentView
	AgentNames    string
	Branch        string
	Attention     int
	OutcomeHTML   template.HTML // sanitized via bluemonday before passing into FloorView
	EndedLabel    string
	Next          bool
}

// DocTreeNode renders a file or directory node in the workspace Docs sidebar.
type DocTreeNode struct {
	WSID     string
	Name     string
	Path     string
	URLPath  string
	IsDir    bool
	Active   bool
	Open     bool
	Children []*DocTreeNode
}

// Data is the template data for every page.
type Data struct {
	Title  string
	CSRF   string
	Nav    string
	Scope  string
	Banner *Banner

	// Auth pages.
	Error  string // error under the main field
	Locked bool   // hide the form (kill switch)

	// App shell & navigation.
	Workspaces []WorkspaceNav
	CurrentWS  *WorkspaceNav
	TotalNeeds int
	SubTitle   string
	NowISO     string
	NowLabel   string

	// Security page.
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

	// Tasks / Overview pages.
	NeedsItems     []TaskView
	PostponedItems []TaskView
	UpdatesItems   []TaskView
	HistoryItems   []TaskView

	// Chat page.
	ChatMessages []ChatMessageView
	WireChat     *WireChatView

	// Maestri Wire: Activity page, Overview "Agents at work", and the state shown when Wire is
	// off, unreachable, unpaired or refused.
	Wire           *WireStateView
	ActivityGroups []AgentFloorView
	ActivityCounts ActivityCountsView
	AgentLines     []AgentLineView
	AgentsLive     bool
	AgentsNeeds    int
	CanWrite       bool

	// Overview & Workspace Floors.
	ActiveFloors  []FloorView
	PresentFloors []FloorView
	FutureFloors  []FloorView
	PastFloors    []FloorView

	// Workspace Docs.
	DocsAvailable bool
	DocsTruncated bool
	DocsTree      []*DocTreeNode
	DocMetaPath   string
	DocUpdated    string
	DocHTML       template.HTML // sanitized via bluemonday before passing into Data

	// Usage & standby page + overview card.
	UsageAvailable    bool
	UsageIssueTitle   string
	UsageIssueMessage string
	UsageAccounts     []UsageAccountView
	UsageHidden       []HiddenProviderView
	UsageHiddenCount  int
	StandbyBanner     *StandbyBannerView
	StandbyActive     bool
	WakeOnReset       bool

	// Coming-soon placeholder page.
	ComingSoonTitle string
	ComingSoonText  string
	ComingSoonIcon  string
}

// WireStateView explains why live data is missing and what to do. It is the "Maestri Wire is off"
// design, with a variant per cause.
type WireStateView struct {
	State    string // wire link state, or "unmapped", "locked", "limited"
	Icon     string
	Tone     string // banner tone
	Title    string
	Text     string
	Heading  string // empty-state title
	Steps    string // empty-state text (safe HTML-free text)
	Commands []string
	Retry    string
}

// AgentView is one agent on the Activity page.
type AgentView struct {
	TerminalID   string
	Name         string
	Initial      string
	Tone         string // avatar tone
	AgentType    string
	IsMaestro    bool
	State        string
	StateLabel   string
	StateTone    string // data-status
	Brief        string
	BriefSource  string
	Prompt       string
	PromptHash   string
	Preview      []string
	DisclosureID string
	CanAnswer    bool
	CanMessage   bool
}

// AgentFloorView is a floor and its agents on the Activity page.
type AgentFloorView struct {
	ID            string
	Name          string
	AnchorID      string
	IsGround      bool
	Branch        string
	Status        string
	StatusLabel   string
	StatusText    string
	HasProgress   bool
	ProgressValue string
	ProgressPct   int
	Agents        []AgentView
}

// ActivityCountsView counts agents by state for the filter chips.
type ActivityCountsView struct {
	Total, Working, Idle, Needs, NotRunning int
}

// AgentLineView is one row of the Overview "Agents at work" panel.
type AgentLineView struct {
	Name          string
	WorkspaceID   string
	WorkspaceName string
	WorkspaceTone string
	Floor         string
	Brief         string
	StateTone     string
	Attn          bool
}

// WireChatView is the Maestro's live terminal chat.
type WireChatView struct {
	StatusTone  string // data-status for the header chip ("" = no chip)
	StatusLabel string
	Typing      bool
	Connecting  bool // chat socket not live yet
	NotRunning  bool
	OffTitle    string
	OffText     string
	CanSend     bool
	SendNote    string // why sending is off, when it is
	Messages    []ChatMessageView
}
