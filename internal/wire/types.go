package wire

import (
	"encoding/json"
	"strings"
)

// ProtocolVersion is the only Wire protocol version this client speaks.
const ProtocolVersion = 1

// Capability names this client uses. Unknown capabilities are ignored.
const (
	CapFeed       = "feedSnapshots"
	CapChat       = "terminalChat"
	CapStream     = "terminalStreaming"
	CapInputBytes = "terminalInputBytes"
	CapFloors     = "floorStatus"
	CapActions    = "workspaceActions" // adds workingDirectory to WorkspaceMeta
)

// Info is GET /api/info.
type Info struct {
	Name             string   `json:"name"`
	ProtocolVersion  int      `json:"protocolVersion"`
	Capabilities     []string `json:"capabilities"`
	RequiresPassword bool     `json:"requiresPassword"`
	Role             string   `json:"role"`
}

// Has reports whether the host advertises capability c.
func (i Info) Has(c string) bool {
	for _, x := range i.Capabilities {
		if x == c {
			return true
		}
	}
	return false
}

// FloorStatus is a floor's status report.
type FloorStatus struct {
	State    string   `json:"state"`
	Text     string   `json:"text"`
	Progress *float64 `json:"progress,omitempty"`
	Author   string   `json:"author,omitempty"`
}

// FloorPullRequest is the part of a floor's pull request this client reads.
type FloorPullRequest struct {
	State  string `json:"state,omitempty"` // open | merged | closed
	Number int    `json:"number,omitempty"`
}

// FloorMeta describes one floor. Ground has no ID.
type FloorMeta struct {
	ID             string            `json:"id,omitempty"`
	Name           string            `json:"name"`
	IsActive       bool              `json:"isActive"`
	Branch         string            `json:"branch,omitempty"`
	Status         *FloorStatus      `json:"status,omitempty"`
	PullRequest    *FloorPullRequest `json:"pullRequest,omitempty"`
	AttentionCount int               `json:"attentionCount,omitempty"`
	HasActivity    bool              `json:"hasActivity,omitempty"`
	IsRemoving     bool              `json:"isRemoving,omitempty"`
}

// WorkspaceMeta describes a Maestri workspace.
type WorkspaceMeta struct {
	ID               string      `json:"id"`
	Name             string      `json:"name"`
	WorkingDirectory string      `json:"workingDirectory,omitempty"`
	IsLoaded         bool        `json:"isLoaded"`
	IsLocked         bool        `json:"isLocked,omitempty"`
	AttentionCount   int         `json:"attentionCount,omitempty"`
	Floors           []FloorMeta `json:"floors,omitempty"`
}

// TerminalCard is one terminal in the feed.
type TerminalCard struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	AgentType      string          `json:"agentType"`
	Status         json.RawMessage `json:"status,omitempty"`
	FloorID        string          `json:"floorId,omitempty"`
	FloorName      string          `json:"floorName,omitempty"`
	IsRunning      bool            `json:"isRunning"`
	Preview        []string        `json:"preview"`
	IsManager      bool            `json:"isManager"`
	NeedsAttention bool            `json:"needsAttention"`
	IsActive       bool            `json:"isActive,omitempty"`
	IsUnloaded     bool            `json:"isUnloaded,omitempty"`
	RoleName       string          `json:"roleName,omitempty"`
	AccountName    string          `json:"accountName,omitempty"`
}

// StatusText returns the terminal status as a lowercase word. Wire does not enumerate the values,
// so this accepts a string or an object carrying one.
func (t TerminalCard) StatusText() string {
	if len(t.Status) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(t.Status, &s) == nil {
		return strings.ToLower(strings.TrimSpace(s))
	}
	var o map[string]any
	if json.Unmarshal(t.Status, &o) == nil {
		for _, k := range []string{"state", "kind", "status", "name"} {
			if v, ok := o[k].(string); ok && v != "" {
				return strings.ToLower(strings.TrimSpace(v))
			}
		}
	}
	return ""
}

// Feed item kinds.
const (
	ItemTerminal      = "terminal"
	ItemPendingPrompt = "pendingPrompt"
)

// FeedItem is one entry of a feed: a terminal, or a terminal waiting on a Y/n.
type FeedItem struct {
	Kind     string        `json:"kind"`
	Terminal *TerminalCard `json:"terminal,omitempty"`
	Prompt   string        `json:"prompt,omitempty"`
}

// FeedSnapshot is the full state of a workspace feed. It is replaced wholesale and never mutated
// after it is published, so readers may keep a pointer to one.
type FeedSnapshot struct {
	Workspace WorkspaceMeta `json:"workspace"`
	Floors    []FloorMeta   `json:"floors"`
	Items     []FeedItem    `json:"items"`
	Epoch     string        `json:"epoch,omitempty"`
}

// Terminals returns every terminal in the feed once, with the Y/n prompt text it waits on, if any.
func (f *FeedSnapshot) Terminals() []FeedTerminal {
	if f == nil {
		return nil
	}
	byID := map[string]int{}
	var out []FeedTerminal
	for _, it := range f.Items {
		if it.Terminal == nil || it.Terminal.ID == "" {
			continue
		}
		if it.Kind != ItemTerminal && it.Kind != ItemPendingPrompt {
			continue
		}
		i, ok := byID[it.Terminal.ID]
		if !ok {
			byID[it.Terminal.ID] = len(out)
			out = append(out, FeedTerminal{TerminalCard: *it.Terminal})
			i = len(out) - 1
		}
		if it.Kind == ItemPendingPrompt {
			out[i].TerminalCard = *it.Terminal
			out[i].Pending = true
			out[i].Prompt = it.Prompt
		}
	}
	return out
}

// Terminal looks up one terminal by id.
func (f *FeedSnapshot) Terminal(id string) (FeedTerminal, bool) {
	for _, t := range f.Terminals() {
		if t.ID == id {
			return t, true
		}
	}
	return FeedTerminal{}, false
}

// FeedTerminal is a terminal card plus the Y/n prompt it waits on.
type FeedTerminal struct {
	TerminalCard
	Pending bool
	Prompt  string
}

// ChatSegment is one piece of a chat entry.
type ChatSegment struct {
	Text string `json:"text,omitempty"`
}

// ChatEntry is one chat line.
type ChatEntry struct {
	ID         string        `json:"id"`
	Role       string        `json:"role"` // user | agent
	Kind       string        `json:"kind"` // message | progress | final | divider
	Text       string        `json:"text"`
	Segments   []ChatSegment `json:"segments,omitempty"`
	AuthorName string        `json:"authorName,omitempty"`
	TS         json.Number   `json:"ts,omitempty"`
}

// ChatSnapshot is the replacement tail of a terminal chat.
type ChatSnapshot struct {
	TerminalID  string      `json:"terminalId"`
	Enabled     bool        `json:"enabled"`
	Status      string      `json:"status"` // idle | working | needsAttention | notRunning | unloaded
	Entries     []ChatEntry `json:"entries"`
	UnreadCount int         `json:"unreadCount,omitempty"`
}
