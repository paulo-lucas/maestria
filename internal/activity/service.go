package activity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/paulo-lucas/maestria/internal/audit"
	"github.com/paulo-lucas/maestria/internal/config"
	"github.com/paulo-lucas/maestria/internal/events"
	"github.com/paulo-lucas/maestria/internal/floors"
	"github.com/paulo-lucas/maestria/internal/inbox"
	"github.com/paulo-lucas/maestria/internal/store"
	"github.com/paulo-lucas/maestria/internal/wire"
	"github.com/paulo-lucas/maestria/internal/workspaces"
)

const (
	floorsSeenKey = "wire.floors_seen_v1"
	// StatusKey holds the last link status, so `maestria wire status` can show what the panel saw.
	StatusKey = "wire.status_v1"
)

// Options configure a Service.
type Options struct {
	DB         *sql.DB
	Audit      *audit.Log
	ConfigDir  string
	Workspaces func() []workspaces.Workspace
	Now        func() time.Time
	// Grace is how long a workspace may be unreachable before its Y/n prompts leave Needs you.
	Grace time.Duration
	// Poll and MaxBackoff tune the Wire manager (tests shorten them).
	Poll, MaxBackoff time.Duration
	// OnChange is told what changed: "inbox", "floors", "wire".
	OnChange func(kind string)
	Logf     func(format string, args ...any)
}

// Service owns the Wire manager and everything the panel derives from it.
type Service struct {
	opts Options
	Mgr  *wire.Manager

	mu        sync.Mutex // guards floor tracking
	downSince map[string]time.Time

	wsMu   sync.Mutex
	wsList []workspaces.Workspace
	wsAt   time.Time
}

// New builds the service and its Wire manager. Call Start to run it.
func New(o Options) *Service {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Grace <= 0 {
		o.Grace = 30 * time.Second
	}
	if o.OnChange == nil {
		o.OnChange = func(string) {}
	}
	s := &Service{opts: o, downSince: map[string]time.Time{}}
	s.Mgr = wire.NewManager(wire.Options{
		CredentialsPath: wire.Path(o.ConfigDir),
		Workspaces:      s.refs,
		Audit: func(event, source string, detail any) {
			if o.Audit != nil {
				_ = o.Audit.Write(event, source, detail)
			}
		},
		PollInterval: o.Poll,
		MaxBackoff:   o.MaxBackoff,
		Now:          o.Now,
		Logf:         o.Logf,
	}, wire.Handlers{Feed: s.onFeed, Status: s.onStatus})
	return s
}

// workspaces returns the registered workspaces, re-read at most every few seconds.
func (s *Service) workspaces() []workspaces.Workspace {
	if s.opts.Workspaces == nil {
		return nil
	}
	s.wsMu.Lock()
	defer s.wsMu.Unlock()
	if s.wsList == nil || s.opts.Now().Sub(s.wsAt) > 3*time.Second {
		s.wsList, s.wsAt = s.opts.Workspaces(), s.opts.Now()
		if s.wsList == nil {
			s.wsList = []workspaces.Workspace{}
		}
	}
	return s.wsList
}

func (s *Service) refs() []wire.WorkspaceRef {
	var out []wire.WorkspaceRef
	for _, w := range s.workspaces() {
		out = append(out, wire.WorkspaceRef{ID: w.ID, Name: w.Name, Path: w.Path, MaestroAgent: w.MaestroAgent})
	}
	return out
}

// SetOnChange sets the change callback (the web server's SSE broker). Call it before Start.
func (s *Service) SetOnChange(f func(kind string)) {
	if f == nil {
		f = func(string) {}
	}
	s.opts.OnChange = f
}

// Start runs the Wire manager and the housekeeping loop until ctx ends.
func (s *Service) Start(ctx context.Context) {
	go s.Mgr.Run(ctx)
	go func() {
		t := time.NewTicker(min(2*time.Second, s.opts.Grace/2))
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.expirePrompts()
			}
		}
	}()
}

func (s *Service) onStatus(st wire.Status) {
	raw, _ := json.Marshal(map[string]any{
		"state": st.State, "message": st.Message, "lastError": st.LastError, "lastErrorAt": st.LastErrorAt,
		"since": st.Since, "at": s.opts.Now().UnixMilli(),
	})
	_ = store.SetKV(s.opts.DB, StatusKey, string(raw))
	s.opts.OnChange("wire")
}

// StoredStatus is the last link status the panel recorded.
type StoredStatus struct {
	State       string `json:"state"`
	Message     string `json:"message"`
	LastError   string `json:"lastError"`
	LastErrorAt int64  `json:"lastErrorAt"`
	At          int64  `json:"at"`
}

// LoadStoredStatus reads the status the running panel last recorded (ok false when it never ran).
func LoadStoredStatus(db *sql.DB) (StoredStatus, bool) {
	raw, ok, err := store.GetKV(db, StatusKey)
	if err != nil || !ok {
		return StoredStatus{}, false
	}
	var st StoredStatus
	if json.Unmarshal([]byte(raw), &st) != nil {
		return StoredStatus{}, false
	}
	return st, true
}

// ---- feed handling ----

func (s *Service) onFeed(wsID string, snap *wire.FeedSnapshot) {
	s.syncPrompts(wsID, snap)
	s.trackFloors(wsID, snap)
}

func (s *Service) syncPrompts(wsID string, snap *wire.FeedSnapshot) {
	var present []inbox.AgentPrompt
	for _, t := range snap.Terminals() {
		if !t.Pending {
			continue
		}
		body := strings.TrimSpace(cleanText(t.Prompt))
		if t.FloorName != "" {
			body += "\n\nFloor: `" + strings.ReplaceAll(t.FloorName, "`", "'") + "`"
		}
		present = append(present, inbox.AgentPrompt{
			Ref: t.ID + ":" + wire.PromptHash(t.Prompt), Agent: t.Name, Floor: t.FloorName, Prompt: body,
		})
	}
	changed, err := inbox.SyncAgentPrompts(s.opts.DB, wsID, present, s.opts.Now().UnixMilli())
	if err == nil && changed {
		s.opts.OnChange("inbox")
	}
}

// expirePrompts removes the Y/n prompts of workspaces that stayed unreachable past the grace
// period: Needs you must never offer a prompt that cannot be answered.
func (s *Service) expirePrompts() {
	now := s.opts.Now()
	changed := false
	for _, w := range s.workspaces() {
		if _, err := s.Mgr.Feed(w.ID); err == nil {
			s.mu.Lock()
			delete(s.downSince, w.ID)
			s.mu.Unlock()
			continue
		}
		s.mu.Lock()
		since, ok := s.downSince[w.ID]
		if !ok {
			s.downSince[w.ID] = now
			since = now
		}
		s.mu.Unlock()
		if now.Sub(since) >= s.opts.Grace {
			if c, err := inbox.SyncAgentPrompts(s.opts.DB, w.ID, nil, now.UnixMilli()); err == nil && c {
				changed = true
			}
		}
	}
	if changed {
		s.opts.OnChange("inbox")
	}
}

// workspacePath is the folder of a registered workspace ("" when unknown).
func (s *Service) workspacePath(wsID string) string {
	for _, w := range s.workspaces() {
		if w.ID == wsID {
			return w.Path
		}
	}
	return ""
}

type seenFloor struct {
	Name   string `json:"name"`
	Branch string `json:"branch,omitempty"`
	// Tip is the commit the branch pointed at when last seen. A floor that lands and has its branch
	// deleted at once leaves nothing but this commit to tell it was merged.
	Tip  string `json:"tip,omitempty"`
	Done bool   `json:"done,omitempty"` // last seen done or merged: it landed rather than being cancelled
	// First is when the floor was first seen (epoch ms): history recorded since then, for example
	// by the Maestro running `maestria floor land`, is not recorded a second time.
	First int64 `json:"first,omitempty"`
}

func (s *Service) loadSeen() map[string]map[string]seenFloor {
	out := map[string]map[string]seenFloor{}
	if raw, ok, err := store.GetKV(s.opts.DB, floorsSeenKey); err == nil && ok {
		_ = json.Unmarshal([]byte(raw), &out)
	}
	return out
}

// trackFloors remembers the floors Wire shows and records history when one disappears: landed if
// it was done or its pull request merged, cancelled otherwise. The outcome stays empty; the
// Maestro or the human fills it with `maestria floor edit`.
func (s *Service) trackFloors(wsID string, snap *wire.FeedSnapshot) {
	floorsNow := snap.Floors
	if floorsNow == nil || !snap.Workspace.IsLoaded {
		return // an incomplete snapshot never proves a floor is gone
	}
	// The tip of each live floor branch, read before the lock is taken (git can be slow).
	tips := map[string]string{}
	if dir := s.workspacePath(wsID); dir != "" {
		for _, f := range floorsNow {
			if f.ID != "" && f.Branch != "" {
				tips[f.ID] = BranchTip(dir, f.Branch)
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := s.loadSeen()
	prev := seen[wsID]
	cur := map[string]seenFloor{}
	for _, f := range floorsNow {
		if f.ID == "" {
			continue
		}
		done := (f.Status != nil && f.Status.State == "done") || (f.PullRequest != nil && f.PullRequest.State == "merged")
		first := s.opts.Now().UnixMilli()
		if p, ok := prev[f.ID]; ok && p.First > 0 {
			first = p.First
		}
		// Keep the last known tip when the branch cannot be read this time.
		tip := tips[f.ID]
		if tip == "" {
			tip = prev[f.ID].Tip
		}
		cur[f.ID] = seenFloor{Name: f.Name, Branch: f.Branch, Tip: tip, Done: done, First: first}
	}
	changed := len(prev) != len(cur)
	for id, p := range prev {
		if _, ok := cur[id]; ok {
			continue
		}
		s.recordGone(wsID, p)
		changed = true
	}
	if !changed {
		for id, c := range cur {
			if prev[id] != c {
				changed = true
			}
		}
	}
	if changed {
		seen[wsID] = cur
		raw, _ := json.Marshal(seen)
		_ = store.SetKV(s.opts.DB, floorsSeenKey, string(raw))
	}
}

const unrecordedBrief = "Created in Maestri; no brief was recorded. Add one with `maestria floor edit`."

func (s *Service) recordGone(wsID string, f seenFloor) {
	nowMs := s.opts.Now().UnixMilli()
	existing, err := floors.List(s.opts.DB, floors.ListOptions{WorkspaceID: wsID})
	if err != nil {
		return
	}
	known := false
	for _, e := range existing {
		if e.Name == f.Name && (e.State == floors.StateActive || e.State == floors.StatePlanned) {
			known = true
		}
	}
	if !known {
		// Already closed by the Maestro (`maestria floor land|cancel`) since we first saw it?
		past, err := floors.List(s.opts.DB, floors.ListOptions{WorkspaceID: wsID, IncludePast: true})
		if err != nil {
			return
		}
		for _, e := range past {
			if e.Name == f.Name && (e.State == floors.StateLanded || e.State == floors.StateCancelled) && e.EndedAt != nil && *e.EndedAt >= f.First {
				return
			}
		}
	}
	if !known {
		// The Maestro never told Maestria about this floor: create its record so history is whole.
		if _, err := floors.Start(s.opts.DB, wsID, f.Name, floors.StartInput{Brief: unrecordedBrief}, nowMs); err != nil {
			return
		}
	}
	// Wire does not say whether a floor was landed or deleted. Git does: a floor whose branch tip
	// is already in the default branch was landed. Otherwise fall back to what Wire last showed.
	landed := f.Done
	if !landed {
		if dir := s.workspacePath(wsID); dir != "" {
			// The branch may be gone already (landed with its branch deleted): then the commit
			// recorded while the floor was alive is what git can still place in the default branch.
			landed = BranchLanded(dir, f.Branch) || CommitLanded(dir, f.Tip)
		}
	}
	outcome := "cancelled"
	if landed {
		outcome = "landed"
		_, err = floors.Land(s.opts.DB, wsID, f.Name, "", true, nowMs)
	} else {
		_, err = floors.Cancel(s.opts.DB, wsID, f.Name, "", nowMs)
	}
	if err != nil {
		return
	}
	if s.opts.Audit != nil {
		_ = s.opts.Audit.Write("floor.history_auto", "wire", map[string]any{"workspace": wsID, "floor": f.Name, "outcome": outcome})
	}
	s.opts.OnChange("floors")
}

// LiveFloors implements floors.LiveSource from the Wire feed. It fails, rather than serving old
// data, when the workspace is not live.
func (s *Service) LiveFloors(wsID string) ([]floors.LiveInfo, error) {
	snap, err := s.Mgr.Feed(wsID)
	if err != nil {
		return nil, err
	}
	fl := snap.Floors
	if fl == nil {
		fl = snap.Workspace.Floors
	}
	terms := snap.Terminals()
	var out []floors.LiveInfo
	for _, f := range fl {
		if f.ID == "" || f.IsRemoving {
			continue
		}
		li := floors.LiveInfo{Name: f.Name, Branch: f.Branch, Attention: f.AttentionCount}
		if f.Status != nil {
			li.Status, li.StatusText, li.Progress = f.Status.State, f.Status.Text, f.Status.Progress
		}
		attn := 0
		for _, t := range terms {
			if t.FloorID != f.ID {
				continue
			}
			li.Agents = append(li.Agents, t.Name)
			if st := ClassifyAgent(t); st == StateYN || st == StateNeeds {
				attn++
			}
		}
		if attn > 0 {
			li.Attention = attn
		}
		out = append(out, li)
	}
	return out, nil
}

// ---- reads for the web layer ----

// Activity returns the live agents of a workspace, grouped by floor.
func (s *Service) Activity(wsID string) (Activity, error) {
	snap, err := s.Mgr.Feed(wsID)
	if err != nil {
		return Activity{}, err
	}
	briefs, _ := Briefs(s.opts.DB, wsID)
	return Build(snap, briefs), nil
}

// ---- writes ----

// MaestroTerminal finds the workspace's Maestro terminal in the live feed.
func (s *Service) MaestroTerminal(wsID string) (wire.FeedTerminal, error) {
	snap, err := s.Mgr.Feed(wsID)
	if err != nil {
		return wire.FeedTerminal{}, err
	}
	agent := ""
	for _, w := range s.workspaces() {
		if w.ID == wsID {
			agent = w.MaestroAgent
		}
	}
	t, ok := wire.PickMaestro(snap, agent)
	if !ok {
		return wire.FeedTerminal{}, wire.ErrUnknownTerminal
	}
	return t, nil
}

// PromptMaestro sends a chat message to the workspace's Maestro terminal.
func (s *Service) PromptMaestro(ctx context.Context, source, wsID, text string) error {
	t, err := s.MaestroTerminal(wsID)
	if err != nil {
		return err
	}
	if !t.IsRunning {
		return ErrMaestroNotRunning
	}
	return s.Mgr.Prompt(ctx, source, wsID, t.ID, text)
}

// ErrMaestroNotRunning means the Maestro's terminal is closed.
var ErrMaestroNotRunning = errors.New("the Maestro is not running")

// AnswerInboxPrompt answers the agent_prompt inbox item itemID with approve or reject: it sends
// the answer to the agent's terminal through Wire and, only when Wire accepted it, closes the item.
func (s *Service) AnswerInboxPrompt(ctx context.Context, source, wsID string, itemID int64, approve bool) (inbox.Item, error) {
	ref, err := inbox.AgentPromptRef(s.opts.DB, wsID, itemID)
	if err != nil {
		return inbox.Item{}, err
	}
	term, hash, _ := strings.Cut(ref, ":")
	if err := s.Mgr.Answer(ctx, source, wsID, term, hash, approve); err != nil {
		if errors.Is(err, wire.ErrPromptChanged) || errors.Is(err, wire.ErrUnknownTerminal) {
			if snap, ferr := s.Mgr.Feed(wsID); ferr == nil {
				s.syncPrompts(wsID, snap) // the prompt is gone or different: refresh Needs you
			}
		}
		return inbox.Item{}, err
	}
	resp := inbox.ResponseRejected
	if approve {
		resp = inbox.ResponseApproved
	}
	it, err := inbox.ResolveAgentPrompt(s.opts.DB, wsID, itemID, resp, s.opts.Now().UnixMilli())
	if errors.Is(err, inbox.ErrNotOpen) {
		return it, nil // the feed already closed it
	}
	return it, err
}

// ---- standby ----

// StandbyResult says what HandleStandby did.
type StandbyResult struct {
	Interrupted int
	Woken       []string
	Errors      []string
}

// HandleStandby carries out the Wire side of a standby transition: it interrupts the working
// agents of a provider that hit its limit (action "interrupt"), and wakes a workspace's Maestro
// with a prompt when a window resets (when wake-on-reset is on).
func (s *Service) HandleStandby(ctx context.Context, evs []events.Event, cfg *config.Config) StandbyResult {
	var res StandbyResult
	interrupted := map[string]bool{}
	for _, ev := range evs {
		var p struct {
			Provider string `json:"provider"`
			Action   string `json:"action"`
			Reason   string `json:"reason"`
		}
		if json.Unmarshal(ev.Payload, &p) != nil {
			continue
		}
		switch ev.Kind {
		case events.KindStandby:
			if p.Action == config.StandbyActionInterrupt && !interrupted[p.Provider] {
				interrupted[p.Provider] = true
				n, errs := s.interruptProvider(ctx, p.Provider)
				res.Interrupted += n
				res.Errors = append(res.Errors, errs...)
			}
		case events.KindResume:
			if p.Reason == "reset" && cfg != nil && cfg.Standby.WakeOnReset {
				msg := fmt.Sprintf("Maestria: the %s usage window has reset. You can resume work now; tell any teammates that were interrupted to continue.", p.Provider)
				if err := s.PromptMaestro(ctx, "standby", ev.WorkspaceID, msg); err != nil {
					res.Errors = append(res.Errors, ev.WorkspaceID+": "+err.Error())
				} else {
					res.Woken = append(res.Woken, ev.WorkspaceID)
				}
			}
		}
	}
	return res
}

func providerMatches(agentType, provider string) bool {
	a, p := strings.ToLower(strings.TrimSpace(agentType)), strings.ToLower(strings.TrimSpace(provider))
	return a != "" && p != "" && (a == p || strings.Contains(a, p) || strings.Contains(p, a))
}

// interruptProvider sends Esc to every working agent on the provider, in every live workspace.
// The Maestro is left alone: it is the one told to stand by.
func (s *Service) interruptProvider(ctx context.Context, provider string) (int, []string) {
	n := 0
	var errs []string
	for wsID, snap := range s.Mgr.Feeds() {
		agent := ""
		for _, w := range s.workspaces() {
			if w.ID == wsID {
				agent = w.MaestroAgent
			}
		}
		maestro, _ := wire.PickMaestro(snap, agent)
		for _, t := range snap.Terminals() {
			if t.ID == maestro.ID || t.IsManager || ClassifyAgent(t) != StateWorking || !providerMatches(t.AgentType, provider) {
				continue
			}
			if err := s.Mgr.Interrupt(ctx, "standby", wsID, t.ID); err != nil {
				errs = append(errs, t.Name+": "+err.Error())
				continue
			}
			n++
		}
	}
	return n, errs
}
