package wire

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Errors for reads of mirrored data.
var (
	// ErrUnmapped means no Maestri workspace matches this Maestria workspace.
	ErrUnmapped = errors.New("wire: this workspace is not open in Maestri")
	// ErrLocked means Maestri serves nothing for this workspace (free-plan lock).
	ErrLocked = errors.New("wire: this workspace is locked in Maestri")
)

// Options configure a Manager.
type Options struct {
	CredentialsPath string
	// Workspaces lists the Maestria workspaces to mirror.
	Workspaces func() []WorkspaceRef
	// Audit records a Wire write: event, source (an IP, or "standby"), detail. Never a token.
	Audit func(event, source string, detail any)
	// PollInterval is how often the link is re-verified (default 15s).
	PollInterval time.Duration
	// MaxBackoff caps reconnect delays (default 30s).
	MaxBackoff time.Duration
	Now        func() time.Time
	Logf       func(format string, args ...any)
}

// Handlers receive what the manager learns. They run on the manager's goroutines and must be quick.
type Handlers struct {
	// Feed is called with every snapshot of a mapped workspace.
	Feed func(wsID string, snap *FeedSnapshot)
	// Status is called whenever the link status changes.
	Status func(Status)
}

// Status is the link state shown by the panel and the CLI. It holds no secrets.
type Status struct {
	State           State    `json:"state"`
	Message         string   `json:"message,omitempty"`
	Host            string   `json:"host,omitempty"`
	PinFingerprint  string   `json:"pin,omitempty"`
	HostName        string   `json:"hostName,omitempty"`
	GuestPaired     bool     `json:"guestPaired"`
	OwnerPaired     bool     `json:"ownerPaired"`
	OwnerOK         bool     `json:"ownerOk"`
	Capabilities    []string `json:"capabilities,omitempty"`
	ProtocolVersion int      `json:"protocolVersion,omitempty"`
	LastError       string   `json:"lastError,omitempty"`
	LastErrorAt     int64    `json:"lastErrorAt,omitempty"`
	Since           int64    `json:"since"`
}

// Has reports whether the host advertised capability c.
func (s Status) Has(c string) bool {
	for _, x := range s.Capabilities {
		if x == c {
			return true
		}
	}
	return false
}

// Connected reports whether the link is live.
func (s Status) Connected() bool { return s.State == StateConnected }

// ChatView is a Maestro terminal's chat as last streamed.
type ChatView struct {
	Terminal FeedTerminal
	Snapshot *ChatSnapshot // nil until the first frame arrives
	Live     bool
}

type worker struct {
	wsID     string
	wireID   string
	maestro  string
	cancel   context.CancelFunc
	feed     *FeedSnapshot
	live     bool
	locked   bool
	chatTerm string
	chatStop context.CancelFunc
	chat     *ChatSnapshot
	chatLive bool
}

// Manager keeps the Wire link alive and mirrors workspace feeds and Maestro chats in memory.
type Manager struct {
	opts Options
	h    Handlers

	mu       sync.RWMutex
	st       Status
	info     Info
	credsKey string
	creds    *Credentials
	guest    *Client
	owner    *Client
	workers  map[string]*worker
	subs     map[chan struct{}]struct{}
	runCtx   context.Context
	kick     chan struct{}
}

// NewManager builds a manager. Call Run to start it.
func NewManager(opts Options, h Handlers) *Manager {
	if opts.PollInterval <= 0 {
		opts.PollInterval = 15 * time.Second
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = 30 * time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	if opts.Workspaces == nil {
		opts.Workspaces = func() []WorkspaceRef { return nil }
	}
	return &Manager{
		opts: opts, h: h,
		st:      Status{State: StateDisabled, Since: opts.Now().UnixMilli()},
		workers: map[string]*worker{},
		subs:    map[chan struct{}]struct{}{},
		kick:    make(chan struct{}, 1),
	}
}

// Status returns the current link status.
func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st := m.st
	st.Capabilities = append([]string(nil), m.st.Capabilities...)
	return st
}

// Subscribe returns a channel that receives a value whenever mirrored data or the status changes.
func (m *Manager) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	m.mu.Lock()
	m.subs[ch] = struct{}{}
	m.mu.Unlock()
	return ch, func() {
		m.mu.Lock()
		delete(m.subs, ch)
		m.mu.Unlock()
	}
}

func (m *Manager) changed() {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for ch := range m.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (m *Manager) kickNow() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

// Run supervises the link until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	m.mu.Lock()
	m.runCtx = ctx
	m.mu.Unlock()
	backoff := time.Second
	for {
		ok := m.tick(ctx)
		wait := m.opts.PollInterval
		st := m.Status().State
		switch {
		case st == StateDisabled:
			wait = min(2*time.Second, m.opts.PollInterval)
			backoff = time.Second
		case !ok:
			wait = min(backoff, m.opts.MaxBackoff)
			backoff = min(backoff*2, m.opts.MaxBackoff)
		default:
			backoff = time.Second
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			m.stopWorkers()
			return
		case <-m.kick:
			t.Stop()
		case <-t.C:
		}
	}
}

// loadCreds loads wire.json when it changed since the last look. It returns the credentials in
// use, ErrNotPaired when there are none.
func (m *Manager) loadCreds() (*Credentials, error) {
	fi, err := os.Stat(m.opts.CredentialsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			m.mu.Lock()
			m.creds, m.guest, m.owner, m.credsKey = nil, nil, nil, ""
			m.mu.Unlock()
			return nil, ErrNotPaired
		}
		return nil, err
	}
	key := fmt.Sprintf("%d:%d", fi.ModTime().UnixNano(), fi.Size())
	m.mu.RLock()
	same := m.creds != nil && key == m.credsKey
	cur := m.creds
	m.mu.RUnlock()
	if same {
		return cur, nil
	}
	c, err := LoadCredentials(m.opts.CredentialsPath)
	if err != nil {
		return nil, err
	}
	guest, owner, err := c.Clients()
	if err != nil {
		return nil, err
	}
	m.stopWorkers() // new tokens or host: start over
	m.mu.Lock()
	m.creds, m.guest, m.owner, m.credsKey = c, guest, owner, key
	m.mu.Unlock()
	return c, nil
}

func (m *Manager) setStatus(mut func(*Status)) {
	m.mu.Lock()
	prev := m.st
	mut(&m.st)
	if m.st.State != prev.State {
		m.st.Since = m.opts.Now().UnixMilli()
	}
	cur := m.st
	cur.Capabilities = append([]string(nil), m.st.Capabilities...)
	m.mu.Unlock()
	if cur.State != prev.State || cur.Message != prev.Message || cur.OwnerOK != prev.OwnerOK || cur.LastError != prev.LastError {
		m.changed()
		if m.h.Status != nil {
			m.h.Status(cur)
		}
	}
}

func (m *Manager) fail(state State, msg string, err error) {
	m.setStatus(func(s *Status) {
		s.State, s.Message = state, msg
		if err != nil {
			s.LastError = err.Error()
			s.LastErrorAt = m.opts.Now().UnixMilli()
		}
		if state != StateConnected {
			s.Capabilities = nil
		}
	})
}

// Messages shown for each failing state.
func stateMessage(state State, err error) string {
	switch state {
	case StatePinMismatch:
		return "Maestri's security key changed, so Maestria refused to connect. If you reset Wire on purpose, run maestria wire unpair and pair again."
	case StateRevoked:
		return "Maestri no longer accepts this connector's access. Pair it again with maestria wire pair."
	case StateIncompatible:
		return "This Maestri Wire speaks a protocol Maestria does not support yet. Update Maestria."
	case StateRole:
		return "The read-only access has more rights than expected. Run maestria wire unpair and pair again."
	}
	return "Maestri Wire is off or not reachable. Turn it on in Maestri: Settings → Wire. Maestria keeps trying."
}

func (m *Manager) tick(ctx context.Context) bool {
	creds, err := m.loadCreds()
	if errors.Is(err, ErrNotPaired) {
		m.stopWorkers()
		m.setStatus(func(s *Status) {
			*s = Status{State: StateDisabled, Since: s.Since, LastError: s.LastError, LastErrorAt: s.LastErrorAt}
		})
		return true
	}
	if err != nil {
		m.stopWorkers()
		m.fail(StateDisabled, "wire.json cannot be used: "+err.Error(), err)
		return true
	}
	m.mu.RLock()
	guest, owner := m.guest, m.owner
	m.mu.RUnlock()
	pin, _ := creds.PinValue()
	m.setStatus(func(s *Status) {
		s.Host, s.PinFingerprint = creds.Host, pin.Hex()
		s.GuestPaired, s.OwnerPaired = true, creds.Owner != nil
		if s.State == StateDisabled {
			s.State, s.Message = StateConnecting, ""
		}
	})

	ictx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	info, err := guest.Info(ictx)
	if err != nil {
		st := Classify(err)
		if st != StateUnreachable {
			m.stopWorkers()
		}
		m.fail(st, stateMessage(st, err), err)
		return false
	}
	if info.ProtocolVersion != ProtocolVersion {
		m.stopWorkers()
		m.fail(StateIncompatible, stateMessage(StateIncompatible, nil), fmt.Errorf("protocolVersion %d", info.ProtocolVersion))
		return false
	}
	if info.Role == "" {
		// /api/info answers anonymously when the token is unknown: the token was revoked or forgotten.
		m.stopWorkers()
		err := &APIError{Status: 401, Code: "unauthorized"}
		m.fail(StateRevoked, stateMessage(StateRevoked, err), err)
		return false
	}
	if info.Role != "guest" {
		m.stopWorkers()
		m.fail(StateRole, stateMessage(StateRole, nil), errors.New("read token has role "+info.Role))
		return false
	}
	ownerOK := false
	if owner != nil {
		if oi, err := owner.Info(ictx); err == nil && oi.Role == "owner" {
			ownerOK = true
		}
	}
	metas, err := guest.Workspaces(ictx)
	if err != nil {
		st := Classify(err)
		m.fail(st, stateMessage(st, err), err)
		return false
	}
	m.mu.Lock()
	m.info = info
	m.mu.Unlock()
	m.reconcile(ctx, guest, info, metas)
	m.setStatus(func(s *Status) {
		s.State, s.Message = StateConnected, ""
		s.HostName, s.Capabilities, s.ProtocolVersion = info.Name, append([]string(nil), info.Capabilities...), info.ProtocolVersion
		s.OwnerOK = ownerOK
	})
	return true
}

func (m *Manager) stopWorkers() {
	m.mu.Lock()
	ws := m.workers
	m.workers = map[string]*worker{}
	m.mu.Unlock()
	for _, w := range ws {
		w.cancel()
		if w.chatStop != nil {
			w.chatStop()
		}
	}
	if len(ws) > 0 {
		m.changed()
	}
}

func (m *Manager) reconcile(ctx context.Context, guest *Client, info Info, metas []WorkspaceMeta) {
	refs := m.opts.Workspaces()
	byRef := map[string]WorkspaceRef{}
	for _, r := range refs {
		byRef[r.ID] = r
	}
	if !info.Has(CapActions) {
		// Without workingDirectory only the name fallback can match.
		for i := range metas {
			metas[i].WorkingDirectory = ""
		}
	}
	mapped := MapWorkspaces(refs, metas)

	m.mu.Lock()
	var stop []*worker
	for id, w := range m.workers {
		if meta, ok := mapped[id]; !ok || meta.ID != w.wireID {
			stop = append(stop, w)
			delete(m.workers, id)
		}
	}
	var start []*worker
	for id, meta := range mapped {
		if w, ok := m.workers[id]; ok {
			w.maestro = byRef[id].MaestroAgent
			continue
		}
		wctx, cancel := context.WithCancel(ctx)
		w := &worker{wsID: id, wireID: meta.ID, maestro: byRef[id].MaestroAgent, cancel: cancel, locked: meta.IsLocked}
		m.workers[id] = w
		start = append(start, w)
		if info.Has(CapFeed) && !meta.IsLocked {
			go m.runFeed(wctx, w, guest, info)
		}
	}
	m.mu.Unlock()
	for _, w := range stop {
		w.cancel()
		if w.chatStop != nil {
			w.chatStop()
		}
	}
	if len(stop) > 0 || len(start) > 0 {
		m.changed()
	}
}

func (m *Manager) runFeed(ctx context.Context, w *worker, guest *Client, info Info) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := guest.FeedStream(ctx, w.wireID, func(s *FeedSnapshot) {
			backoff = time.Second
			m.onSnapshot(ctx, w, guest, info, s)
		})
		m.mu.Lock()
		w.live = false
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == 403 {
			w.locked = true
		}
		m.mu.Unlock()
		m.changed()
		if ctx.Err() != nil {
			return
		}
		m.opts.Logf("wire: feed for %s ended: %v", w.wsID, err)
		m.kickNow() // re-verify the link now so the panel shows the real reason
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		backoff = min(backoff*2, m.opts.MaxBackoff)
	}
}

func (m *Manager) onSnapshot(ctx context.Context, w *worker, guest *Client, info Info, s *FeedSnapshot) {
	m.mu.Lock()
	w.feed, w.live, w.locked = s, true, false
	var stopChat context.CancelFunc
	if info.Has(CapChat) {
		term, ok := PickMaestro(s, w.maestro)
		if !ok {
			stopChat, w.chatStop, w.chatTerm, w.chat, w.chatLive = w.chatStop, nil, "", nil, false
		} else if term.ID != w.chatTerm {
			stopChat = w.chatStop
			cctx, cancel := context.WithCancel(ctx)
			w.chatStop, w.chatTerm, w.chat, w.chatLive = cancel, term.ID, nil, false
			go m.runChat(cctx, w, guest, term.ID)
		}
	}
	m.mu.Unlock()
	if stopChat != nil {
		stopChat()
	}
	if m.h.Feed != nil {
		m.h.Feed(w.wsID, s)
	}
	m.changed()
}

func (m *Manager) runChat(ctx context.Context, w *worker, guest *Client, terminalID string) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := guest.ChatStream(ctx, terminalID, func(c *ChatSnapshot) {
			backoff = time.Second
			m.mu.Lock()
			if w.chatTerm == terminalID {
				w.chat, w.chatLive = c, true
			}
			m.mu.Unlock()
			m.changed()
		})
		m.mu.Lock()
		if w.chatTerm == terminalID {
			w.chatLive = false
		}
		m.mu.Unlock()
		m.changed()
		if ctx.Err() != nil {
			return
		}
		m.opts.Logf("wire: chat for %s ended: %v", w.wsID, err)
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		backoff = min(backoff*2, m.opts.MaxBackoff)
	}
}

// Feed returns the live feed of a Maestria workspace. It returns an error, never stale data,
// when the link is down, the host lacks the capability, or the workspace is not open in Maestri.
func (m *Manager) Feed(wsID string) (*FeedSnapshot, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.st.State != StateConnected {
		return nil, ErrNotConnected
	}
	if !m.st.Has(CapFeed) {
		return nil, ErrNoCapability
	}
	w, ok := m.workers[wsID]
	if !ok {
		return nil, ErrUnmapped
	}
	if w.locked {
		return nil, ErrLocked
	}
	if !w.live || w.feed == nil {
		return nil, ErrNotConnected
	}
	return w.feed, nil
}

// Feeds returns every live feed by Maestria workspace id.
func (m *Manager) Feeds() map[string]*FeedSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string]*FeedSnapshot{}
	if m.st.State != StateConnected || !m.st.Has(CapFeed) {
		return out
	}
	for id, w := range m.workers {
		if w.live && w.feed != nil && !w.locked {
			out[id] = w.feed
		}
	}
	return out
}

// Chat returns the Maestro terminal's chat for a workspace. When the feed is live but the
// workspace has no Maestro terminal, the zero Terminal is returned with a nil error.
func (m *Manager) Chat(wsID string) (ChatView, error) {
	feed, err := m.Feed(wsID)
	if err != nil {
		return ChatView{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.st.Has(CapChat) {
		return ChatView{}, ErrNoCapability
	}
	w := m.workers[wsID]
	term, ok := PickMaestro(feed, w.maestro)
	if !ok {
		return ChatView{}, nil
	}
	v := ChatView{Terminal: term}
	if w.chatTerm == term.ID && w.chatLive {
		v.Snapshot, v.Live = w.chat, true
	}
	return v, nil
}

// ChatThreads returns the newest entries of the Maestro chat's threads that are not selected in
// Maestri, with the guest token. It changes nothing in the app: no thread switch, nothing marked
// read. It fails with ErrNoCapability on a host without chatThreadPreviews.
func (m *Manager) ChatThreads(ctx context.Context, wsID string) (ChatThreadPreviews, error) {
	cv, err := m.Chat(wsID)
	if err != nil {
		return ChatThreadPreviews{}, err
	}
	m.mu.RLock()
	guest, has := m.guest, m.st.Has(CapThreads)
	m.mu.RUnlock()
	if !has || guest == nil {
		return ChatThreadPreviews{}, ErrNoCapability
	}
	if cv.Terminal.ID == "" {
		return ChatThreadPreviews{}, ErrUnknownTerminal
	}
	return guest.ChatThreads(ctx, cv.Terminal.ID)
}

// PromptHash identifies a Y/n prompt text, so an answer is only applied to the prompt the human saw.
func PromptHash(prompt string) string {
	sum := sha256.Sum256([]byte(strings.Join(strings.Fields(prompt), " ")))
	return hex.EncodeToString(sum[:6])
}

// writer checks everything a write needs and returns the owner client and the target terminal.
func (m *Manager) writer(wsID, terminalID string, caps ...string) (*Client, FeedTerminal, error) {
	feed, err := m.Feed(wsID)
	if err != nil {
		return nil, FeedTerminal{}, err
	}
	m.mu.RLock()
	owner, ok, st := m.owner, m.st.OwnerOK, m.st
	m.mu.RUnlock()
	if owner == nil || !ok {
		return nil, FeedTerminal{}, ErrNoOwner
	}
	for _, c := range caps {
		if !st.Has(c) {
			return nil, FeedTerminal{}, ErrNoCapability
		}
	}
	t, found := feed.Terminal(terminalID)
	if !found {
		return nil, FeedTerminal{}, ErrUnknownTerminal
	}
	return owner, t, nil
}

func (m *Manager) audit(event, source string, detail map[string]any, err error) {
	if m.opts.Audit == nil {
		return
	}
	if err != nil {
		detail["result"] = "error: " + errorSummary(err)
	} else {
		detail["result"] = "ok"
	}
	m.opts.Audit(event, source, detail)
}

func errorSummary(err error) string {
	var ae *APIError
	switch {
	case errors.As(err, &ae):
		return fmt.Sprintf("HTTP %d %s", ae.Status, ae.Code)
	case errors.Is(err, ErrAmbiguous):
		return "delivery unconfirmed"
	}
	return err.Error()
}

// Prompt sends text to a terminal of workspace wsID. source says who asked (an IP address, or
// "standby"). The text is never logged, only its length.
func (m *Manager) Prompt(ctx context.Context, source, wsID, terminalID, text string) error {
	detail := map[string]any{"route": "POST /api/terminals/{id}/prompt", "workspace": wsID, "terminal": terminalID, "chars": len([]rune(text))}
	if strings.TrimSpace(text) == "" {
		return errors.New("wire: empty message")
	}
	owner, t, err := m.writer(wsID, terminalID, CapChat)
	if err == nil {
		detail["name"] = t.Name
		err = owner.Prompt(ctx, terminalID, text)
	}
	m.audit("wire.prompt", source, detail, err)
	return err
}

// Answer approves or rejects the Y/n a terminal waits on. promptHash, when set, must match the
// pending prompt, so a click never answers a prompt the human did not see.
func (m *Manager) Answer(ctx context.Context, source, wsID, terminalID, promptHash string, approve bool) error {
	verb := "reject"
	if approve {
		verb = "approve"
	}
	detail := map[string]any{"route": "POST /api/terminals/{id}/" + verb, "workspace": wsID, "terminal": terminalID}
	owner, t, err := m.writer(wsID, terminalID, CapFeed)
	if err == nil {
		detail["name"] = t.Name
		switch {
		case !t.Pending:
			err = ErrPromptChanged
		case promptHash != "" && promptHash != PromptHash(t.Prompt):
			err = ErrPromptChanged
		case approve:
			// The cached feed can be up to a snapshot old, and Wire's approve answers whatever is
			// pending at that moment. Read the prompt afresh right before approving, and only
			// approve if it is still the one the human saw.
			want := promptHash
			if want == "" {
				want = PromptHash(t.Prompt)
			}
			if err = m.recheckPrompt(ctx, wsID, terminalID, want); err == nil {
				err = owner.Approve(ctx, terminalID)
			}
		default:
			err = owner.Reject(ctx, terminalID)
		}
	}
	m.audit("wire."+verb, source, detail, err)
	return err
}

// recheckPrompt reads the feed of the workspace once more (guest) and confirms the terminal still
// waits on the prompt with hash want. It fails closed: when the feed cannot be read, nothing is
// approved.
func (m *Manager) recheckPrompt(ctx context.Context, wsID, terminalID, want string) error {
	m.mu.RLock()
	guest := m.guest
	var wireID string
	if w, ok := m.workers[wsID]; ok {
		wireID = w.wireID
	}
	m.mu.RUnlock()
	if guest == nil || wireID == "" {
		return ErrPromptUnverified
	}
	rctx, cancel := context.WithTimeout(ctx, recheckTimeout)
	defer cancel()
	snap, err := guest.Feed(rctx, wireID)
	if err != nil {
		m.opts.Logf("wire: re-checking a prompt before approving it failed: %v", err)
		return fmt.Errorf("%w", ErrPromptUnverified)
	}
	t, found := snap.Terminal(terminalID)
	switch {
	case !found:
		return ErrPromptChanged
	case !t.Pending || PromptHash(t.Prompt) != want:
		return ErrPromptChanged
	}
	return nil
}

// recheckTimeout bounds the extra read before an approval.
const recheckTimeout = 5 * time.Second

// Interrupt sends Escape to a terminal.
func (m *Manager) Interrupt(ctx context.Context, source, wsID, terminalID string) error {
	detail := map[string]any{"route": "WS /api/terminals/{id}/stream (Esc)", "workspace": wsID, "terminal": terminalID}
	owner, t, err := m.writer(wsID, terminalID, CapStream, CapInputBytes)
	if err == nil {
		detail["name"] = t.Name
		err = owner.Interrupt(ctx, terminalID)
	}
	m.audit("wire.interrupt", source, detail, err)
	return err
}
