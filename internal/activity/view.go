package activity

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/paulo-lucas/maestria/internal/wire"
)

// Agent states.
const (
	StateWorking    = "working"
	StateIdle       = "idle"
	StateNeeds      = "needs"      // needs the human, no Y/n
	StateYN         = "yn"         // waiting on a Y/n
	StateNotRunning = "notrunning" // terminal closed or unloaded
)

// Agent is one terminal as the Activity page shows it.
type Agent struct {
	TerminalID  string   `json:"terminalId"`
	Name        string   `json:"name"`
	AgentType   string   `json:"agentType,omitempty"`
	Floor       string   `json:"floor"`
	IsMaestro   bool     `json:"isMaestro"`
	State       string   `json:"state"`
	StateLabel  string   `json:"stateLabel"`
	Tone        string   `json:"tone"` // data-status value: working | idle | needs | offline
	Brief       string   `json:"brief"`
	BriefSource string   `json:"briefSource,omitempty"` // "maestro" or "wire"
	Prompt      string   `json:"prompt,omitempty"`
	PromptHash  string   `json:"promptHash,omitempty"`
	Preview     []string `json:"preview,omitempty"`
}

// FloorGroup is a floor and the agents on it.
type FloorGroup struct {
	ID          string   `json:"id,omitempty"`
	Name        string   `json:"name"`
	IsGround    bool     `json:"isGround"`
	Branch      string   `json:"branch,omitempty"`
	Status      string   `json:"status,omitempty"`
	StatusLabel string   `json:"statusLabel,omitempty"`
	StatusText  string   `json:"statusText,omitempty"`
	Progress    *float64 `json:"progress,omitempty"`
	Agents      []Agent  `json:"agents"`
}

// Counts summarize a workspace's agents.
type Counts struct {
	Total      int `json:"total"`
	Working    int `json:"working"`
	Idle       int `json:"idle"`
	Needs      int `json:"needs"` // needs you, including Y/n
	NotRunning int `json:"notRunning"`
}

// Activity is the live agent picture of one workspace.
type Activity struct {
	Groups []FloorGroup `json:"groups"`
	Counts Counts       `json:"counts"`
}

// ClassifyAgent maps a terminal card to one of the agent states. Wire does not enumerate the
// terminal status values, so anything unknown falls back to isActive.
func ClassifyAgent(t wire.FeedTerminal) string {
	switch {
	case !t.IsRunning || t.IsUnloaded:
		return StateNotRunning
	case t.Pending:
		return StateYN
	case t.NeedsAttention:
		return StateNeeds
	}
	switch s := t.StatusText(); {
	case strings.Contains(s, "attention"):
		return StateNeeds
	case strings.Contains(s, "work"), strings.Contains(s, "busy"), strings.Contains(s, "think"), strings.Contains(s, "run"):
		return StateWorking
	case strings.Contains(s, "idle"), strings.Contains(s, "ready"), strings.Contains(s, "wait"):
		return StateIdle
	}
	if t.IsActive {
		return StateWorking
	}
	return StateIdle
}

func stateMeta(state string) (label, tone string) {
	switch state {
	case StateWorking:
		return "Working", "working"
	case StateYN:
		return "Waiting on Y/n", "needs"
	case StateNeeds:
		return "Needs you", "needs"
	case StateNotRunning:
		return "Not running", "offline"
	}
	return "Idle", "idle"
}

var ansiRE = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]|\x1b\\][^\x07]*(\x07|\x1b\\\\)|\x1b[@-_]")

// cleanText removes terminal escape sequences and control characters from a preview line.
func cleanText(s string) string {
	s = ansiRE.ReplaceAllString(s, "")
	var b strings.Builder
	for _, r := range s {
		if r == '\t' {
			b.WriteString("  ")
		} else if !unicode.IsControl(r) {
			b.WriteRune(r)
		}
	}
	return strings.TrimRight(b.String(), " ")
}

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// previewLines cleans the last lines of a terminal preview.
func previewLines(raw []string) []string {
	var out []string
	for _, l := range raw {
		out = append(out, clipRunes(cleanText(l), 240))
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	if len(out) > 14 {
		out = out[len(out)-14:]
	}
	return out
}

// lastLine is the last line with something readable in it (not a bare prompt character).
func lastLine(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		s := strings.TrimSpace(lines[i])
		if strings.IndexFunc(s, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) >= 0 {
			return s
		}
	}
	return ""
}

// Build groups a live feed's terminals by floor and attaches briefs. briefs is keyed by lowercase
// agent name. Floors with no terminals are left out.
func Build(snap *wire.FeedSnapshot, briefs map[string]string) Activity {
	act := Activity{Groups: []FloorGroup{}}
	if snap == nil {
		return act
	}
	byID := map[string]*FloorGroup{}
	var order []string
	ensure := func(id, name string, meta *wire.FloorMeta) *FloorGroup {
		if g, ok := byID[id]; ok {
			return g
		}
		g := &FloorGroup{ID: id, Name: name, IsGround: id == "", Agents: []Agent{}}
		if meta != nil {
			g.Branch = meta.Branch
			if meta.Status != nil {
				g.Status, g.StatusText, g.Progress = meta.Status.State, meta.Status.Text, meta.Status.Progress
				g.StatusLabel = statusLabel(meta.Status.State)
			}
		}
		byID[id] = g
		order = append(order, id)
		return g
	}
	floors := snap.Floors
	if floors == nil {
		floors = snap.Workspace.Floors
	}
	names := map[string]*wire.FloorMeta{}
	for i := range floors {
		names[floors[i].ID] = &floors[i]
	}
	for _, t := range snap.Terminals() {
		meta := names[t.FloorID]
		name := t.FloorName
		if meta != nil && meta.Name != "" {
			name = meta.Name
		}
		if name == "" {
			name = "main"
		}
		g := ensure(t.FloorID, name, meta)
		state := ClassifyAgent(t)
		label, tone := stateMeta(state)
		prev := previewLines(t.Preview)
		a := Agent{
			TerminalID: t.ID, Name: t.Name, AgentType: t.AgentType, Floor: name, IsMaestro: t.IsManager,
			State: state, StateLabel: label, Tone: tone, Preview: prev,
		}
		if b := briefs[strings.ToLower(strings.TrimSpace(t.Name))]; b != "" {
			a.Brief, a.BriefSource = b, "maestro"
		} else if l := lastLine(prev); l != "" && state != StateNotRunning {
			a.Brief, a.BriefSource = clipRunes(l, 160), "wire"
		}
		if t.Pending {
			a.Prompt = clipRunes(cleanText(t.Prompt), 400)
			a.PromptHash = wire.PromptHash(t.Prompt)
			if a.Brief == "" || a.BriefSource == "wire" {
				a.Brief, a.BriefSource = "Waiting on Y/n: "+clipRunes(a.Prompt, 100), "wire"
			}
		}
		g.Agents = append(g.Agents, a)
		act.Counts.Total++
		switch state {
		case StateWorking:
			act.Counts.Working++
		case StateIdle:
			act.Counts.Idle++
		case StateYN, StateNeeds:
			act.Counts.Needs++
		case StateNotRunning:
			act.Counts.NotRunning++
		}
	}
	// Floors that need the human come first; otherwise the host's order is kept.
	for pass := 0; pass < 2; pass++ {
		for _, id := range order {
			g := byID[id]
			needs := false
			for _, a := range g.Agents {
				if a.State == StateYN || a.State == StateNeeds {
					needs = true
				}
			}
			if needs == (pass == 0) {
				act.Groups = append(act.Groups, *g)
			}
		}
	}
	return act
}

func statusLabel(state string) string {
	switch state {
	case "blocked":
		return "Blocked"
	case "review":
		return "Review"
	case "done":
		return "Done"
	case "working":
		return "Working"
	}
	return ""
}
