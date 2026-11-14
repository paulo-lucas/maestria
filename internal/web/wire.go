package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/maestria-dev/maestria/internal/activity"
	"github.com/maestria-dev/maestria/internal/chat"
	"github.com/maestria-dev/maestria/internal/inbox"
	"github.com/maestria-dev/maestria/internal/standby"
	"github.com/maestria-dev/maestria/internal/ui"
	"github.com/maestria-dev/maestria/internal/wire"
	"github.com/maestria-dev/maestria/internal/workspaces"
)

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// wireStatus is the link status; a server without Wire reads as not paired.
func (s *Server) wireStatus() wire.Status {
	if s.Activity == nil {
		return wire.Status{State: wire.StateDisabled}
	}
	return s.Activity.Mgr.Status()
}

// wireErr turns "no live data" into the error to explain. It is nil when the workspace is live.
func (s *Server) liveErr(wsID string) error {
	if s.Activity == nil {
		return wire.ErrNotConnected
	}
	_, err := s.Activity.Mgr.Feed(wsID)
	return err
}

// wireStateView builds the "Maestri Wire is off" panel for a failure. wsName may be empty for
// the Overview. It returns nil when err is nil.
func wireStateView(st wire.Status, err error, wsName string) *ui.WireStateView {
	if err == nil {
		return nil
	}
	v := &ui.WireStateView{State: string(st.State), Icon: "i-wire", Tone: "warn"}
	pair := []string{"maestria wire pair"}
	switch st.State {
	case wire.StateDisabled:
		v.Title = "Maestri Wire isn't connected"
		v.Text = "The panel can't see agents, floors or chat without Wire. Tasks the Maestro posts still arrive."
		if st.Message != "" {
			v.Text = st.Message
		}
		v.Heading = "Turn on Maestri Wire"
		v.Steps = "In Maestri, open Settings → Wire and turn it on, then pair it with this machine:"
		v.Commands = pair
		v.Retry = "The panel picks it up by itself; this page updates when it does."
	case wire.StateConnecting:
		v.Title = "Connecting to Maestri Wire…"
		v.Text = "Live data appears as soon as Maestri answers."
		v.Heading = "Connecting"
		v.Steps = "This usually takes a few seconds."
	case wire.StateUnreachable:
		v.Title = "Maestri Wire is off or can't be reached"
		v.Text = "The connector keeps trying and shows live data again when Maestri answers. Nothing here is stale: it is hidden until then."
		v.Heading = "Turn on Maestri Wire"
		v.Steps = "In Maestri, open Settings → Wire and make sure it is on. To check the connection from a terminal:"
		v.Commands = []string{"maestria wire status"}
		v.Retry = "This page updates by itself when Wire is back."
	case wire.StatePinMismatch:
		v.Tone = "blocked"
		v.Title = "Maestri's security key changed"
		v.Text = "Maestria refused to connect, because the host no longer matches the key it was paired with."
		v.Heading = "Pair again to trust the new key"
		v.Steps = "Only do this if you reset Wire yourself. If you didn't, something else is answering where Maestri should be."
		v.Commands = []string{"maestria wire unpair", "maestria wire pair"}
	case wire.StateRevoked:
		v.Tone = "blocked"
		v.Title = "Maestri no longer accepts this connector"
		v.Text = "Its access was revoked in Maestri (or expired after 30 idle days)."
		v.Heading = "Pair with Maestri Wire again"
		v.Steps = "Get a new pairing code in Maestri (Settings → Wire → Pairing), then run:"
		v.Commands = pair
	case wire.StateIncompatible:
		v.Title = "This Maestri Wire version isn't supported yet"
		v.Text = "Wire is in beta and its protocol changed. Maestria will not guess at it."
		v.Heading = "Update Maestria"
		v.Steps = "Install a newer Maestria that supports this version of Maestri Wire."
	case wire.StateRole:
		v.Tone = "blocked"
		v.Title = "Wire access has the wrong rights"
		v.Text = "The read-only device has more rights than expected, so Maestria stopped using it."
		v.Heading = "Pair again"
		v.Steps = "Remove the devices and pair again:"
		v.Commands = []string{"maestria wire unpair", "maestria wire pair"}
	default: // connected, but not this workspace
		name := wsName
		if name == "" {
			name = "This workspace"
		}
		switch {
		case errors.Is(err, wire.ErrUnmapped):
			v.Title = name + " isn't open in Maestri"
			v.Text = "Maestria matches Maestri workspaces by folder, then by name."
			v.Heading = "Open the workspace in Maestri"
			v.Steps = "Open this workspace's folder in Maestri to see its agents here."
		case errors.Is(err, wire.ErrLocked):
			v.Title = "Maestri locked " + name
			v.Text = "Maestri serves nothing for a locked workspace."
			v.Heading = "Unlock it in Maestri"
			v.Steps = "The agents appear here once the workspace is unlocked."
		case errors.Is(err, wire.ErrNoCapability):
			v.Title = "This Maestri doesn't share live agent feeds"
			v.Text = "Maestria only uses what the host advertises."
			v.Heading = "Update Maestri"
			v.Steps = "A newer Maestri with Wire feeds shows agents and chat here."
		default:
			v.Title = "Reconnecting to " + name + "…"
			v.Text = "The live feed dropped. It is hidden, not stale, until it is back."
			v.Heading = "Reconnecting"
			v.Steps = "This page updates by itself."
		}
	}
	return v
}

func agentAvatarInitial(name string) string { return firstUpperInitial(name) }

func (s *Server) populateActivity(d *ui.Data, ws workspaces.Workspace) {
	st := s.wireStatus()
	act, err := activity.Activity{}, s.liveErr(ws.ID)
	if err == nil {
		act, err = s.Activity.Activity(ws.ID)
	}
	if err != nil {
		d.Wire = wireStateView(st, err, ws.Name)
		return
	}
	canAnswer := st.OwnerOK
	canMessage := st.OwnerOK && st.Has(wire.CapChat)
	d.CanWrite = canAnswer
	d.ActivityCounts = ui.ActivityCountsView{
		Total: act.Counts.Total, Working: act.Counts.Working, Idle: act.Counts.Idle, Needs: act.Counts.Needs, NotRunning: act.Counts.NotRunning,
	}
	for i, g := range act.Groups {
		fv := ui.AgentFloorView{
			ID: g.ID, Name: g.Name, IsGround: g.IsGround, Branch: g.Branch, Status: g.Status, StatusLabel: g.StatusLabel,
			StatusText: g.StatusText, AnchorID: "floor-" + strconv.Itoa(i),
		}
		if g.Progress != nil && *g.Progress >= 0 && *g.Progress <= 1 {
			fv.HasProgress = true
			fv.ProgressValue = strconv.FormatFloat(*g.Progress, 'f', 3, 64)
			fv.ProgressPct = int(*g.Progress*100 + 0.5)
		}
		for _, a := range g.Agents {
			fv.Agents = append(fv.Agents, ui.AgentView{
				TerminalID: a.TerminalID, Name: a.Name, Initial: agentAvatarInitial(a.Name), Tone: agentTone(a.Name),
				AgentType: titleCase(a.AgentType), IsMaestro: a.IsMaestro, State: a.State, StateLabel: a.StateLabel,
				StateTone: a.Tone, Brief: a.Brief, BriefSource: a.BriefSource, Prompt: a.Prompt, PromptHash: a.PromptHash,
				Preview: a.Preview, DisclosureID: "term-" + a.TerminalID,
				CanAnswer: canAnswer, CanMessage: canMessage && a.State != activity.StateNotRunning,
			})
		}
		d.ActivityGroups = append(d.ActivityGroups, fv)
	}
}

func titleCase(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// populateAgentsAtWork fills the Overview panel: working agents and agents that need you, across
// every workspace that is live.
func (s *Server) populateAgentsAtWork(d *ui.Data, list []workspaces.Workspace) {
	st := s.wireStatus()
	if !st.Connected() {
		d.Wire = wireStateView(st, wire.ErrNotConnected, "")
		return
	}
	type line struct {
		v    ui.AgentLineView
		rank int
	}
	var lines []line
	for _, ws := range list {
		act, err := s.Activity.Activity(ws.ID)
		if err != nil {
			continue
		}
		for _, g := range act.Groups {
			for _, a := range g.Agents {
				if a.State != activity.StateWorking && a.State != activity.StateYN && a.State != activity.StateNeeds {
					continue
				}
				attn := a.State == activity.StateYN || a.State == activity.StateNeeds
				brief := a.Brief
				if brief == "" {
					brief = a.StateLabel
				}
				l := line{v: ui.AgentLineView{
					Name: a.Name, WorkspaceID: ws.ID, WorkspaceName: ws.Name, WorkspaceTone: ws.Tone, Floor: g.Name,
					Brief: brief, StateTone: a.Tone, Attn: attn,
				}}
				if attn {
					l.rank = 0
					d.AgentsNeeds++
				} else {
					l.rank = 1
				}
				lines = append(lines, l)
			}
		}
	}
	for rank := 0; rank < 2; rank++ {
		for _, l := range lines {
			if l.rank == rank {
				d.AgentLines = append(d.AgentLines, l.v)
			}
		}
	}
}

// ---- chat ----

// entryTime reads a Wire chat timestamp: an ISO string, or a number of seconds or milliseconds.
func entryTime(raw json.RawMessage) (time.Time, bool) {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" || s == "null" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return time.Time{}, false
	}
	switch {
	case f > 1e12:
		return time.UnixMilli(int64(f)), true
	case f > 1e9:
		return time.Unix(int64(f), 0), true
	default: // seconds since 2001-01-01, Apple's reference date
		return time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(f * float64(time.Second))), true
	}
}

// wireChatView returns the live chat of a workspace's Maestro, or nil when the local chat log
// should be shown instead (Wire off, workspace not open in Maestri, or no chat capability).
func (s *Server) wireChatView(ws workspaces.Workspace, now time.Time) *ui.WireChatView {
	if s.Activity == nil {
		return nil
	}
	cv, err := s.Activity.Mgr.Chat(ws.ID)
	if err != nil {
		return nil
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
		v.StatusTone, v.StatusLabel, v.Typing = "working", "Maestro working", true
	case status == "needsAttention":
		v.StatusTone, v.StatusLabel = "needs", "Maestro needs you"
	}
	v.CanSend = v.CanSend && running
	if !running {
		v.SendNote = ""
	} else if !st.OwnerOK {
		v.SendNote = "Sending is off: this connector only has read access to Maestri. Run `maestria wire pair` to allow it."
	}
	if cv.Snapshot == nil || !cv.Live {
		v.Connecting = true
		return v
	}
	type item struct {
		m   ui.ChatMessageView
		t   time.Time // when it happened; a Wire entry without a time inherits the previous one's
		has bool
	}
	var items []item
	var carry time.Time
	var first time.Time // time of the oldest dated Wire entry: say messages before it are not shown
	for _, e := range cv.Snapshot.Entries {
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
		it := item{m: m, t: carry}
		if t, ok := entryTime(e.TS); ok {
			it.t, it.has, carry = t, true, t
			if first.IsZero() {
				first = t
			}
		}
		items = append(items, it)
	}
	// Messages the Maestro posted with `maestria say` go in the same conversation, by time.
	if local, err := chat.List(s.DB, ws.ID, 0, 200); err == nil {
		var said []item
		for _, lm := range local {
			if lm.Sender != chat.SenderMaestro {
				continue
			}
			t := time.UnixMilli(lm.CreatedAt)
			if !first.IsZero() && t.Before(first) {
				continue
			}
			said = append(said, item{m: ui.ChatMessageView{Key: "say-" + strconv.FormatInt(lm.ID, 10), Author: "maestro", Via: "maestria", BodyHTML: template.HTML(lm.BodyHTML)}, t: t, has: true})
		}
		if first.IsZero() && len(said) > 20 {
			said = said[len(said)-20:]
		}
		merged := make([]item, 0, len(items)+len(said))
		i := 0
		for _, sm := range said {
			for i < len(items) && !items[i].t.After(sm.t) {
				merged = append(merged, items[i])
				i++
			}
			merged = append(merged, sm)
		}
		items = append(merged, items[i:]...)
	}
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
		v.Messages = append(v.Messages, m)
	}
	return v
}

// ---- pages ----

func (s *Server) getWorkspaceActivity(w http.ResponseWriter, r *http.Request) {
	wsID := r.PathValue("id")
	d := s.appData(r, "Activity", "activity")
	list, _ := s.populateShell(d, wsID)
	ws, ok := s.findWorkspace(list, wsID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	d.Title = ws.Name + " · Activity"
	s.populateActivity(d, ws)
	s.render(w, ui.PageActivity, d)
}

// ---- API ----

type wireErrorBody struct {
	Error     string `json:"error"`
	Code      string `json:"code"`
	Ambiguous bool   `json:"ambiguous,omitempty"`
}

// writeWireError answers a failed Wire action with a sentence the person can act on.
func writeWireError(w http.ResponseWriter, err error) {
	var ae *wire.APIError
	body := wireErrorBody{Code: "wire_error"}
	status := http.StatusBadGateway
	switch {
	case errors.Is(err, wire.ErrAmbiguous):
		body.Error, body.Code, body.Ambiguous = "Not sure it was delivered: the connection broke while sending. Check the terminal before sending again; it was not retried.", "ambiguous", true
	case errors.Is(err, wire.ErrNotConnected), errors.Is(err, wire.ErrUnmapped), errors.Is(err, wire.ErrLocked), errors.Is(err, wire.ErrNoCapability):
		status, body.Code = http.StatusServiceUnavailable, "wire_unavailable"
		body.Error = "Maestri Wire isn't connected, so nothing was sent."
	case errors.Is(err, wire.ErrNoOwner):
		status, body.Code = http.StatusConflict, "read_only"
		body.Error = "Sending is off: this connector only has read access to Maestri. Run maestria wire pair to allow it."
	case errors.Is(err, wire.ErrUnknownTerminal):
		status, body.Code = http.StatusNotFound, "unknown_agent"
		body.Error = "That agent isn't in this workspace any more."
	case errors.Is(err, wire.ErrPromptChanged):
		status, body.Code = http.StatusConflict, "prompt_changed"
		body.Error = "That prompt changed or was already answered. Look at the current one."
	case errors.Is(err, activity.ErrMaestroNotRunning):
		status, body.Code = http.StatusConflict, "not_running"
		body.Error = "The Maestro isn't running."
	case errors.As(err, &ae):
		if ae.Status == http.StatusConflict {
			status, body.Code = http.StatusConflict, "not_running"
			body.Error = "The agent isn't running, or has no pending prompt."
		} else {
			body.Error = fmt.Sprintf("Maestri refused it (%d).", ae.Status)
		}
	default:
		body.Error = "Could not reach Maestri Wire."
	}
	writeJSON(w, status, body)
}

func wireContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 25*time.Second)
}

func (s *Server) apiWire(w http.ResponseWriter, r *http.Request) {
	st := s.wireStatus()
	// Status only: no tokens, no pin, no host details beyond the name Maestri gave itself.
	writeJSON(w, http.StatusOK, map[string]any{
		"state": st.State, "message": st.Message, "hostName": st.HostName,
		"guestPaired": st.GuestPaired, "ownerPaired": st.OwnerPaired, "ownerOk": st.OwnerOK,
		"capabilities": st.Capabilities, "since": st.Since,
	})
}

func (s *Server) apiWorkspaceActivity(w http.ResponseWriter, r *http.Request) {
	list, err := s.mergedWorkspaces()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "workspaces_load_failed"})
		return
	}
	ws, ok := s.findWorkspace(list, r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "workspace_not_found"})
		return
	}
	if err := s.liveErr(ws.ID); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "wire_unavailable", "state": s.wireStatus().State})
		return
	}
	act, err := s.Activity.Activity(ws.ID)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "wire_unavailable", "state": s.wireStatus().State})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspace": ws.ID, "activity": act})
}

// agentTarget resolves the workspace and validates the terminal id of an agent route.
func (s *Server) agentTarget(w http.ResponseWriter, r *http.Request) (workspaces.Workspace, string, bool) {
	if s.Activity == nil {
		writeWireError(w, wire.ErrNotConnected)
		return workspaces.Workspace{}, "", false
	}
	list, err := s.mergedWorkspaces()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "workspaces_load_failed"})
		return workspaces.Workspace{}, "", false
	}
	ws, ok := s.findWorkspace(list, r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "workspace_not_found"})
		return workspaces.Workspace{}, "", false
	}
	term := r.PathValue("terminal")
	if !uuidRE.MatchString(term) {
		writeWireError(w, wire.ErrUnknownTerminal)
		return workspaces.Workspace{}, "", false
	}
	return ws, term, true
}

func (s *Server) apiAgentAnswer(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ws, term, ok := s.agentTarget(w, r)
		if !ok {
			return
		}
		var body struct {
			PromptHash string `json:"promptHash"`
		}
		if err := decodeJSONBody(r, &body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
			return
		}
		if body.PromptHash == "" {
			// A click must name the prompt it answers.
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "promptHash is required"})
			return
		}
		ctx, cancel := wireContext(r)
		defer cancel()
		if err := s.Activity.Mgr.Answer(ctx, ipOf(r), ws.ID, term, body.PromptHash, approve); err != nil {
			writeWireError(w, err)
			return
		}
		s.notifyStream("inbox")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func (s *Server) apiAgentPrompt(w http.ResponseWriter, r *http.Request) {
	ws, term, ok := s.agentTarget(w, r)
	if !ok {
		return
	}
	var body struct {
		Body string `json:"body"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	text := strings.TrimSpace(body.Body)
	if text == "" || len([]rune(text)) > 4000 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Write 1 to 4000 characters."})
		return
	}
	ctx, cancel := wireContext(r)
	defer cancel()
	if err := s.Activity.Mgr.Prompt(ctx, ipOf(r), ws.ID, term, text); err != nil {
		writeWireError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// sendWireChat sends a Chat message to the Maestro's terminal. It reports whether Wire handled
// the request; false means the local chat log should take it.
func (s *Server) sendWireChat(w http.ResponseWriter, r *http.Request, ws workspaces.Workspace, text string) bool {
	if s.Activity == nil {
		return false
	}
	if _, err := s.Activity.Mgr.Chat(ws.ID); err != nil {
		return false
	}
	text = strings.TrimSpace(text)
	if text == "" || len([]rune(text)) > chat.MaxBodyChars {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("Write 1 to %d characters.", chat.MaxBodyChars)})
		return true
	}
	ctx, cancel := wireContext(r)
	defer cancel()
	// No retry on failure: when delivery is ambiguous the person decides whether to send again.
	if err := s.Activity.PromptMaestro(ctx, ipOf(r), ws.ID, text); err != nil {
		writeWireError(w, err)
		return true
	}
	s.notifyStream("chat")
	writeJSON(w, http.StatusOK, map[string]any{"sent": true})
	return true
}

// answerAgentPromptItem handles Respond on an agent_prompt inbox item.
func (s *Server) answerAgentPromptItem(w http.ResponseWriter, r *http.Request, ws workspaces.Workspace, itemID int64, decision string) (inbox.Item, bool) {
	resp, ok := inbox.ParseAgentDecision(decision)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Answer approve or reject."})
		return inbox.Item{}, false
	}
	if s.Activity == nil {
		writeWireError(w, wire.ErrNotConnected)
		return inbox.Item{}, false
	}
	ctx, cancel := wireContext(r)
	defer cancel()
	it, err := s.Activity.AnswerInboxPrompt(ctx, ipOf(r), ws.ID, itemID, resp == inbox.ResponseApproved)
	if err != nil {
		if errors.Is(err, inbox.ErrNotFound) || errors.Is(err, inbox.ErrNotOpen) || errors.Is(err, inbox.ErrInvalidInput) {
			writeInboxError(w, err)
		} else {
			writeWireError(w, err)
		}
		return inbox.Item{}, false
	}
	return it, true
}

// handleStandbyTransition runs the Wire side of a standby change without holding up the request.
func (s *Server) handleStandbyTransition(tr standby.Transition) {
	if s.Activity == nil || len(tr.Events) == 0 {
		return
	}
	cfg := s.configCopy()
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		res := s.Activity.HandleStandby(ctx, tr.Events, cfg)
		if len(res.Errors) > 0 && s.Audit != nil {
			_ = s.Audit.Write("standby.wire_errors", "standby", map[string]any{"errors": res.Errors, "interrupted": res.Interrupted})
		}
	}()
}

// WaitBackground blocks until the standby actions started by requests have finished (tests).
func (s *Server) WaitBackground() { s.bg.Wait() }

// forwardWireChanges tells browsers to refresh when the Wire mirror changes, at most once a second.
func (s *Server) forwardWireChanges(ctx context.Context) {
	ch, cancel := s.Activity.Mgr.Subscribe()
	defer cancel()
	var last time.Time
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	pending := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			if !pending {
				wait := time.Second - s.Now().Sub(last)
				if wait < 50*time.Millisecond {
					wait = 50 * time.Millisecond
				}
				timer.Reset(wait)
				pending = true
			}
		case <-timer.C:
			pending = false
			last = s.Now()
			s.notifyStream("wire")
		}
	}
}
