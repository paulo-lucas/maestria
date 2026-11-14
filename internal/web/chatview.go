package web

import (
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/maestria-dev/maestria/internal/chat"
	"github.com/maestria-dev/maestria/internal/inbox"
	"github.com/maestria-dev/maestria/internal/ui"
	"github.com/maestria-dev/maestria/internal/wire"
	"github.com/maestria-dev/maestria/internal/workspaces"
)

// threadTone maps a Maestri thread colour to a design tone (data-tone), so the swatches follow
// the tokens and stay readable in light and dark.
var threadTone = map[string]string{
	"blue": "blue", "purple": "violet", "pink": "pink", "red": "red",
	"orange": "orange", "yellow": "amber", "green": "green", "graphite": "slate",
}

func threadLabel(color string) string {
	if color == "" {
		return "Thread"
	}
	return strings.ToUpper(color[:1]) + color[1:]
}

// chatItem is one line of the conversation with the time it happened.
type chatItem struct {
	m   ui.ChatMessageView
	t   time.Time // when it happened; a Wire entry without a time inherits the previous one's
	has bool
}

// chatItems turns Wire chat entries into lines, dropping progress entries.
func chatItems(entries []wire.ChatEntry) (items []chatItem, first time.Time) {
	var carry time.Time
	for _, e := range entries {
		text := strings.TrimSpace(e.Text)
		if text == "" {
			var parts []string
			for _, sg := range e.Segments {
				parts = append(parts, sg.Text)
			}
			text = strings.TrimSpace(strings.Join(parts, ""))
		}
		m := ui.ChatMessageView{Key: e.ID, Author: "maestro"}
		switch {
		case e.Kind == "divider":
			m.Author = "system"
			if text == "" {
				text = "New conversation"
			}
			m.BodyHTML = template.HTML(template.HTMLEscapeString(text))
		case e.Kind == "progress" || text == "":
			continue
		default:
			if e.Role == "user" {
				m.Author = "human"
			}
			m.BodyHTML = template.HTML(inbox.RenderMarkdown(text))
		}
		it := chatItem{m: m, t: carry}
		if t, ok := entryTime(e.TS); ok {
			it.t, it.has, carry = t, true, t
			if first.IsZero() {
				first = t
			}
		}
		items = append(items, it)
	}
	return items, first
}

// chatMessages adds the day dividers and time labels.
func chatMessages(items []chatItem, now time.Time) []ui.ChatMessageView {
	var out []ui.ChatMessageView
	lastDay := ""
	for _, it := range items {
		m := it.m
		if it.has && m.Author != "system" {
			lt := it.t.Local()
			m.TimeLabel, m.TimeISO = lt.Format("15:04"), lt.Format(time.RFC3339)
			if day := formatDayDivider(lt, now.Local()); day != lastDay {
				m.DayLabel, lastDay = day, day
			}
		}
		out = append(out, m)
	}
	return out
}

// mergeSaid puts the messages the Maestro posted with `maestria say` in the conversation, by time.
func (s *Server) mergeSaid(wsID string, items []chatItem, first time.Time) []chatItem {
	local, err := chat.List(s.DB, wsID, 0, 200)
	if err != nil {
		return items
	}
	var said []chatItem
	for _, lm := range local {
		if lm.Sender != chat.SenderMaestro {
			continue
		}
		t := time.UnixMilli(lm.CreatedAt)
		if !first.IsZero() && t.Before(first) {
			continue
		}
		said = append(said, chatItem{m: ui.ChatMessageView{Key: "say-" + strconv.FormatInt(lm.ID, 10), Author: "maestro", Via: "maestria", BodyHTML: template.HTML(lm.BodyHTML)}, t: t, has: true})
	}
	if first.IsZero() && len(said) > 20 {
		said = said[len(said)-20:]
	}
	merged := make([]chatItem, 0, len(items)+len(said))
	i := 0
	for _, sm := range said {
		for i < len(items) && !items[i].t.After(sm.t) {
			merged = append(merged, items[i])
			i++
		}
		merged = append(merged, sm)
	}
	return append(merged, items[i:]...)
}

// threadChips builds the colour selector from the snapshot. viewSlot is the thread being shown.
// An unread count is only known for the selected thread: Wire gives none for the others.
func threadChips(th *wire.ChatThreads, viewSlot, unread int, canPreview bool) []ui.ChatThreadChip {
	if th == nil {
		return nil
	}
	chips := make([]ui.ChatThreadChip, 0, len(th.Items))
	for _, it := range th.Items {
		active := it.ID == th.ActiveSlot
		chips = append(chips, ui.ChatThreadChip{
			Slot: it.ID, Color: it.Color, Label: threadLabel(it.Color), Tone: threadTone[it.Color],
			Href:        "?thread=" + it.Color,
			Current:     it.ID == viewSlot,
			Active:      active,
			HasMessages: it.HasMessages,
			Unread:      active && unread > 0,
			Disabled:    !active && !canPreview && it.HasMessages,
		})
	}
	return chips
}

// wireChatView returns the live chat of a workspace's Maestro. It returns an error, and no view,
// when Wire has no live data for the workspace (off, not paired, revoked, unreachable, workspace
// not open in Maestri, or no chat capability): the page then explains why instead of offering
// a composer.
func (s *Server) wireChatView(r *http.Request, ws workspaces.Workspace, now time.Time) (*ui.WireChatView, error) {
	if s.Activity == nil {
		return nil, wire.ErrNotConnected
	}
	cv, err := s.Activity.Mgr.Chat(ws.ID)
	if err != nil {
		return nil, err
	}
	st := s.Activity.Mgr.Status()
	v := &ui.WireChatView{CanSend: st.OwnerOK}
	running := cv.Terminal.ID != "" && cv.Terminal.IsRunning && !cv.Terminal.IsUnloaded
	status := ""
	if cv.Snapshot != nil {
		status = cv.Snapshot.Status
		if status == "notRunning" || status == "unloaded" {
			running = false
		}
	}

	// Which thread is shown: ?thread=<colour>, else the one selected in Maestri.
	var th *wire.ChatThreads
	if cv.Snapshot != nil {
		th = cv.Snapshot.Threads
	}
	viewActive := true
	viewSlot := -1
	if th != nil && len(th.Items) > 0 {
		viewSlot = th.ActiveSlot
		want := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("thread")))
		for _, it := range th.Items {
			if want != "" && it.Color == want {
				viewSlot = it.ID
			}
		}
		viewActive = viewSlot == th.ActiveSlot
		unread := 0
		if cv.Snapshot != nil {
			unread = cv.Snapshot.UnreadCount
		}
		v.Threads = threadChips(th, viewSlot, unread, st.Has(wire.CapThreads))
		for _, c := range v.Threads {
			if c.Current {
				v.ThreadSlot, v.ThreadColor, v.ThreadLabel = c.Slot, c.Color, c.Label
			}
			if c.Active {
				v.ActiveLabel = c.Label
			}
		}
	}
	v.ViewingActive = viewActive

	switch {
	case cv.Terminal.ID == "":
		v.NotRunning = true
		v.StatusTone, v.StatusLabel = "offline", "Maestro not running"
		v.OffTitle = "The Maestro isn't running"
		v.OffText = "There is no Maestro terminal in the " + ws.Name + " workspace in Maestri. Open one there to chat again."
	case !running:
		v.NotRunning = true
		v.StatusTone, v.StatusLabel = "offline", "Maestro not running"
		v.OffTitle = "The Maestro isn't running"
		v.OffText = "Its terminal in the " + ws.Name + " workspace is closed. Open it in Maestri to chat again."
	case status == "working":
		v.StatusTone, v.StatusLabel, v.Typing = "working", "Maestro working", viewActive
	case status == "needsAttention":
		v.StatusTone, v.StatusLabel = "needs", "Maestro needs you"
	}
	v.CanSend = v.CanSend && running
	switch {
	case !running:
		v.SendNote = ""
	case !st.OwnerOK:
		v.SendNote = "Sending is off: this connector only has read access to Maestri. Run `maestria wire pair` to allow it."
	case !viewActive:
		// Wire's prompt route has no thread field: it always writes to the thread selected in
		// Maestri, and switching that thread is the owner's call, never the panel's.
		v.CanSend = false
		v.SendNote = "Messages go to the thread selected in Maestri (" + v.ActiveLabel + "). Open " + v.ActiveLabel + " here to send."
	}
	if cv.Snapshot == nil || !cv.Live {
		v.Connecting = true
		return v, nil
	}

	var items []chatItem
	var first time.Time
	if viewActive {
		items, first = chatItems(cv.Snapshot.Entries)
		// Messages the Maestro posted with `maestria say` belong to the live conversation.
		items = s.mergeSaid(ws.ID, items, first)
	} else {
		ctx, cancel := wireContext(r)
		defer cancel()
		prev, perr := s.Activity.Mgr.ChatThreads(ctx, ws.ID)
		if perr != nil {
			v.ThreadError = "Couldn't read the " + v.ThreadLabel + " thread from Maestri. It tries again on the next update."
		}
		for _, p := range prev.Threads {
			if p.ID == viewSlot {
				items, _ = chatItems(p.Entries)
			}
		}
	}
	v.Messages = chatMessages(items, now)
	return v, nil
}

// localChat lists the local log of `maestria say` and panel messages, read-only, for the page
// that explains why Wire is not available.
func (s *Server) localChat(ws workspaces.Workspace, now time.Time) []ui.ChatMessageView {
	msgs, err := chat.List(s.DB, ws.ID, 0, 200)
	if err != nil {
		return nil
	}
	var out []ui.ChatMessageView
	lastDay := ""
	for _, m := range msgs {
		t := time.UnixMilli(m.CreatedAt).Local()
		day := formatDayDivider(t, now.Local())
		dayLabel := ""
		if day != lastDay {
			dayLabel = day
			lastDay = day
		}
		out = append(out, ui.ChatMessageView{
			ID: m.ID, Author: m.Sender, BodyHTML: template.HTML(m.BodyHTML),
			TimeLabel: t.Format("15:04"), TimeISO: t.Format(time.RFC3339), DayLabel: dayLabel,
		})
	}
	return out
}
