package cloud

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/paulo-lucas/maestria/internal/relay"
)

// Hub holds the connector sockets and relays requests over them. It never looks inside bodies,
// never stores them and never logs them.
type Hub struct {
	cons *Connectors
	log  *slog.Logger
	now  func() time.Time

	mu      sync.Mutex
	conns   map[string]*connSess
	subs    map[string]map[chan struct{}]struct{}
	pairs   map[string]*Pairing
	streams map[string]int
}

// NewHub returns an empty hub.
func NewHub(cons *Connectors, log *slog.Logger, now func() time.Time) *Hub {
	if now == nil {
		now = time.Now
	}
	return &Hub{cons: cons, log: log, now: now, conns: map[string]*connSess{}, subs: map[string]map[chan struct{}]struct{}{}, pairs: map[string]*Pairing{}, streams: map[string]int{}}
}

type connSess struct {
	hub    *Hub
	cn     *Connector
	ws     *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
	since  time.Time

	mu    sync.Mutex
	calls map[string]*Call
	hello relay.Frame
}

// Online reports whether a connector has a live socket, and its last hello.
func (h *Hub) Online(id string) (relay.Frame, time.Time, bool) {
	h.mu.Lock()
	cs := h.conns[id]
	h.mu.Unlock()
	if cs == nil {
		return relay.Frame{}, time.Time{}, false
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.hello, cs.since, true
}

// Disconnect closes a connector's socket (revoked, unlinked).
func (h *Hub) Disconnect(id string) {
	h.mu.Lock()
	cs := h.conns[id]
	h.mu.Unlock()
	if cs != nil {
		cs.cancel()
		cs.ws.CloseNow()
	}
}

// DisconnectUser closes every socket of a user (account disabled).
func (h *Hub) DisconnectUser(userID string) {
	h.mu.Lock()
	var list []*connSess
	for _, cs := range h.conns {
		if cs.cn.UserID == userID {
			list = append(list, cs)
		}
	}
	h.mu.Unlock()
	for _, cs := range list {
		cs.cancel()
		cs.ws.CloseNow()
	}
}

// Subscribe returns a channel that is poked whenever the user's connectors or pairings change.
func (h *Hub) Subscribe(userID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	if h.subs[userID] == nil {
		h.subs[userID] = map[chan struct{}]struct{}{}
	}
	h.subs[userID][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs[userID], ch)
		h.mu.Unlock()
	}
}

func (h *Hub) notify(userID string) {
	h.mu.Lock()
	for ch := range h.subs[userID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	h.mu.Unlock()
}

// ServeConnector is GET /connector: the connector's outbound WebSocket.
func (h *Hub) ServeConnector(w http.ResponseWriter, r *http.Request, clientIP string, audit func(event, ip string, detail map[string]any)) {
	if r.Header.Get("Origin") != "" {
		http.Error(w, "forbidden", http.StatusForbidden) // browsers must never reach this endpoint
		return
	}
	authz := r.Header.Get("Authorization")
	cred := strings.TrimPrefix(authz, "Bearer ")
	if cred == authz {
		writeJSON(w, http.StatusUnauthorized, relay.ErrorBody{Error: "unauthorized"})
		return
	}
	cn, err := h.cons.Authenticate(cred)
	if err != nil || cn == nil {
		audit("connector_auth_fail", clientIP, nil)
		writeJSON(w, http.StatusUnauthorized, relay.ErrorBody{Error: "unauthorized"})
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	ws.SetReadLimit(relay.MaxFrame)
	ctx, cancel := context.WithCancel(context.Background())
	cs := &connSess{hub: h, cn: cn, ws: ws, ctx: ctx, cancel: cancel, since: h.now(), calls: map[string]*Call{}}
	defer cancel()

	// The first frame must be hello, soon.
	hctx, hcancel := context.WithTimeout(ctx, 10*time.Second)
	_, data, err := ws.Read(hctx)
	hcancel()
	var hello relay.Frame
	if err != nil || json.Unmarshal(data, &hello) != nil || hello.T != relay.TypeHello || hello.Proto != relay.Proto {
		ws.Close(websocket.StatusPolicyViolation, "hello expected")
		return
	}
	cs.hello = hello

	h.mu.Lock()
	old := h.conns[cn.ID]
	h.conns[cn.ID] = cs
	h.mu.Unlock()
	if old != nil {
		old.cancel()
		old.ws.Close(websocket.StatusPolicyViolation, "replaced by a newer connection")
	}
	h.cons.Seen(cn.ID, hello, true)
	h.cons.SetDevices(cn.ID, hello.Devices)
	_ = cs.send(relay.Frame{T: relay.TypeWelcome, ServerTime: h.now().UnixMilli()})
	if fresh, _ := h.cons.Get(cn.ID); fresh != nil && fresh.ForgetAt != 0 {
		_ = cs.send(relay.Frame{T: relay.TypeForget, At: fresh.ForgetAt})
	}
	h.offerLive(cs)
	h.notify(cn.UserID)
	audit("connector_online", clientIP, map[string]any{"user": cn.UserID, "connector": cn.ID, "version": clip(hello.Version, 40)})

	go cs.pinger()
	cs.readLoop()

	cancel()
	h.mu.Lock()
	if h.conns[cn.ID] == cs {
		delete(h.conns, cn.ID)
	}
	h.mu.Unlock()
	cs.failAll(relay.ErrConnectorOffline)
	h.cons.Touch(cn.ID)
	h.notify(cn.UserID)
	ws.CloseNow()
}

func (cs *connSess) send(f relay.Frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cs.ctx, 30*time.Second)
	defer cancel()
	return cs.ws.Write(ctx, websocket.MessageText, b)
}

func (cs *connSess) pinger() {
	t := time.NewTicker(relay.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-cs.ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(cs.ctx, 15*time.Second)
			err := cs.ws.Ping(pctx)
			cancel()
			if err != nil {
				cs.ws.CloseNow()
				return
			}
			cs.hub.cons.Touch(cs.cn.ID)
		}
	}
}

func (cs *connSess) readLoop() {
	for {
		_, data, err := cs.ws.Read(cs.ctx)
		if err != nil {
			return
		}
		var f relay.Frame
		if json.Unmarshal(data, &f) != nil {
			continue
		}
		switch f.T {
		case relay.TypeHello:
			cs.mu.Lock()
			cs.hello = f
			cs.mu.Unlock()
			cs.hub.cons.Seen(cs.cn.ID, f, false)
			cs.hub.cons.SetDevices(cs.cn.ID, f.Devices)
			if fresh, _ := cs.hub.cons.Get(cs.cn.ID); fresh != nil && fresh.ForgetAt != 0 {
				active := false
				for _, d := range f.Devices {
					if d.State == "active" {
						active = true
					}
				}
				if !active {
					cs.hub.cons.ClearForget(cs.cn.ID) // the connector confirmed: no device is left
				}
			}
			cs.hub.notify(cs.cn.UserID)
		case relay.TypeRes, relay.TypeChunk, relay.TypeErr:
			cs.mu.Lock()
			call := cs.calls[f.ID]
			cs.mu.Unlock()
			if call != nil {
				call.deliver(f)
			}
		case relay.TypePairChallenge, relay.TypePairState:
			cs.hub.pairFrame(cs, f)
		}
	}
}

// failAll ends every pending call with an error code (the socket is gone).
func (cs *connSess) failAll(code string) {
	cs.mu.Lock()
	calls := make([]*Call, 0, len(cs.calls))
	for _, c := range cs.calls {
		calls = append(calls, c)
	}
	cs.mu.Unlock()
	for _, c := range calls {
		c.deliver(relay.Frame{T: relay.TypeErr, ID: c.id, Code: code, Status: relay.StatusFor(code)})
	}
}

// ---- calls ----

// ErrOffline is returned when the connector has no live socket.
var ErrOffline = errors.New(relay.ErrConnectorOffline)

// ErrBusy is returned when too many requests are in flight.
var ErrBusy = errors.New(relay.ErrConnectorBusy)

type chunk struct {
	data []byte
	more bool
}

// Call is one relayed request in flight. The response is read with Head and Next.
type Call struct {
	id     string
	cs     *connSess
	user   string
	stream bool

	mu     sync.Mutex
	head   *relay.Frame
	errF   *relay.Frame
	chunks []chunk
	queued int
	total  int64
	sig    chan struct{}
	closed bool
}

// RelayRequest is what the cloud forwards: nothing but the signed fields and a few headers.
type RelayRequest struct {
	Method string
	Path   string
	Query  string
	Header map[string]string
	Body   []byte
}

// Dispatch sends a request to the connector and returns the call to read the response from.
func (h *Hub) Dispatch(userID, connectorID string, rq RelayRequest) (*Call, error) {
	h.mu.Lock()
	cs := h.conns[connectorID]
	h.mu.Unlock()
	if cs == nil || cs.cn.UserID != userID {
		return nil, ErrOffline
	}
	stream := relay.IsStream(rq.Method, rq.Path)
	if stream {
		h.mu.Lock()
		if h.streams[userID] >= relay.MaxStreamsPerUser {
			h.mu.Unlock()
			return nil, ErrBusy
		}
		h.streams[userID]++
		h.mu.Unlock()
	}
	idb := make([]byte, 8)
	_, _ = rand.Read(idb)
	call := &Call{id: "r_" + hex.EncodeToString(idb), cs: cs, user: userID, stream: stream, sig: make(chan struct{}, 1)}
	cs.mu.Lock()
	if len(cs.calls) >= relay.MaxInFlight {
		cs.mu.Unlock()
		call.release()
		return nil, ErrBusy
	}
	cs.calls[call.id] = call
	cs.mu.Unlock()

	first := rq.Body
	if len(first) > relay.BodyChunk {
		first = first[:relay.BodyChunk]
	}
	rest := rq.Body[len(first):]
	f := relay.Frame{T: relay.TypeReq, ID: call.id, Method: rq.Method, Path: rq.Path, Query: rq.Query, Header: rq.Header, Body: relay.Enc(first), More: len(rest) > 0}
	if err := cs.send(f); err != nil {
		call.Close()
		return nil, ErrOffline
	}
	for len(rest) > 0 {
		n := min(len(rest), relay.BodyChunk)
		if err := cs.send(relay.Frame{T: relay.TypeBody, ID: call.id, Body: relay.Enc(rest[:n]), More: n < len(rest)}); err != nil {
			call.Close()
			return nil, ErrOffline
		}
		rest = rest[n:]
	}
	return call, nil
}

func (c *Call) deliver(f relay.Frame) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	switch f.T {
	case relay.TypeErr:
		c.errF = &f
	case relay.TypeRes:
		if c.head != nil {
			c.mu.Unlock()
			return
		}
		c.head = &f
		c.addChunk(f)
	case relay.TypeChunk:
		if c.head == nil {
			c.mu.Unlock()
			return
		}
		c.addChunk(f)
	}
	over := c.queued > 2*relay.FlowWindow+relay.BodyChunk
	if !c.stream && c.total > relay.MaxResponseBody {
		over = true
	}
	if over {
		// A connector that ignores flow control or sends too much is cut off from this call.
		c.errF = &relay.Frame{T: relay.TypeErr, ID: c.id, Code: relay.ErrTooLarge, Status: relay.StatusFor(relay.ErrTooLarge)}
		c.chunks, c.queued = nil, 0
	}
	c.mu.Unlock()
	select {
	case c.sig <- struct{}{}:
	default:
	}
	if over {
		c.cancelRemote()
	}
}

func (c *Call) addChunk(f relay.Frame) {
	b, err := relay.Dec(f.Body)
	if err != nil {
		c.errF = &relay.Frame{T: relay.TypeErr, ID: c.id, Code: relay.ErrInternal, Status: 500}
		return
	}
	c.total += int64(len(b))
	c.queued += len(b)
	c.chunks = append(c.chunks, chunk{data: b, more: f.More})
}

func (c *Call) cancelRemote() {
	_ = c.cs.send(relay.Frame{T: relay.TypeCancel, ID: c.id})
}

// Head waits for the response head. It returns the status and headers, or the error frame the
// connector (or the cloud) produced. A timeout is reported as gateway_timeout.
func (c *Call) Head(ctx context.Context) (relay.Frame, *relay.Frame) {
	timer := time.NewTimer(relay.HeadTimeout)
	defer timer.Stop()
	for {
		c.mu.Lock()
		if c.errF != nil {
			e := *c.errF
			c.mu.Unlock()
			return relay.Frame{}, &e
		}
		if c.head != nil {
			h := *c.head
			c.mu.Unlock()
			return h, nil
		}
		c.mu.Unlock()
		select {
		case <-c.sig:
		case <-timer.C:
			return relay.Frame{}, &relay.Frame{T: relay.TypeErr, Code: relay.ErrGatewayTimeout, Status: relay.StatusFor(relay.ErrGatewayTimeout)}
		case <-ctx.Done():
			return relay.Frame{}, &relay.Frame{T: relay.TypeErr, Code: relay.ErrGatewayTimeout, Status: relay.StatusFor(relay.ErrGatewayTimeout)}
		}
	}
}

// Next returns the next piece of the body. more is false after the last one. An error means the
// response was cut short.
func (c *Call) Next(ctx context.Context) (data []byte, more bool, err error) {
	for {
		c.mu.Lock()
		if len(c.chunks) > 0 {
			ch := c.chunks[0]
			c.chunks = c.chunks[1:]
			c.queued -= len(ch.data)
			c.mu.Unlock()
			return ch.data, ch.more, nil
		}
		if c.errF != nil {
			e := *c.errF
			c.mu.Unlock()
			return nil, false, errors.New(e.Code)
		}
		c.mu.Unlock()
		select {
		case <-c.sig:
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}
}

// Ack tells the connector n response bytes reached the browser (flow control credit).
func (c *Call) Ack(n int) {
	if n > 0 {
		_ = c.cs.send(relay.Frame{T: relay.TypeAck, ID: c.id, N: int64(n)})
	}
}

// Close forgets the call; if the response was not finished it tells the connector to stop.
func (c *Call) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	done := len(c.chunks) == 0 && c.head != nil && c.errF == nil
	c.mu.Unlock()
	c.cs.mu.Lock()
	delete(c.cs.calls, c.id)
	c.cs.mu.Unlock()
	if !done {
		c.cancelRemote()
	}
	c.release()
}

func (c *Call) release() {
	if c.stream {
		c.cs.hub.mu.Lock()
		if c.cs.hub.streams[c.user] > 0 {
			c.cs.hub.streams[c.user]--
		}
		c.cs.hub.mu.Unlock()
	}
}

// ---- pairing (in memory, 5 minutes) ----

// PairMachine is one connector's side of a pairing, as the browser sees it.
type PairMachine struct {
	ConnectorID string `json:"connectorId"`
	Name        string `json:"name"`
	Online      bool   `json:"online"`
	State       string `json:"state"` // waiting | pending | revealed | approved | rejected | expired | mismatch
	Reason      string `json:"reason,omitempty"`
	R           string `json:"r,omitempty"`
}

// Pairing is a browser waiting to be approved on the user's machines.
type Pairing struct {
	ID       string
	UserID   string
	Commit   string
	Label    string
	UA       string
	Expires  time.Time
	machines map[string]*PairMachine
	order    []string
}

// ErrNoPairing is returned for an unknown or expired pairing.
var ErrNoPairing = errors.New("no such pairing")

func (h *Hub) purgePairs() {
	t := h.now()
	for id, p := range h.pairs {
		if t.After(p.Expires) {
			delete(h.pairs, id)
		}
	}
}

// StartPairing offers a new browser key commitment to every online connector of the user.
func (h *Hub) StartPairing(userID, commit, label, ua string, cons []*Connector) (*Pairing, error) {
	if c, err := relay.Dec(commit); err != nil || len(c) != 32 {
		return nil, ErrInvalid
	}
	idb := make([]byte, 12)
	_, _ = rand.Read(idb)
	p := &Pairing{ID: "p_" + hex.EncodeToString(idb), UserID: userID, Commit: commit, Label: clip(label, 80), UA: clip(ua, 200), Expires: h.now().Add(5 * time.Minute), machines: map[string]*PairMachine{}}
	h.mu.Lock()
	h.purgePairs()
	// One live pairing per user: a new one replaces the old.
	for id, old := range h.pairs {
		if old.UserID == userID {
			delete(h.pairs, id)
		}
	}
	for _, cn := range cons {
		p.machines[cn.ID] = &PairMachine{ConnectorID: cn.ID, Name: cn.Name, State: "waiting"}
		p.order = append(p.order, cn.ID)
	}
	h.pairs[p.ID] = p
	var targets []*connSess
	for _, cn := range cons {
		if cs := h.conns[cn.ID]; cs != nil {
			targets = append(targets, cs)
		}
	}
	h.mu.Unlock()
	for _, cs := range targets {
		h.offer(p, cs)
	}
	h.notify(userID)
	return p, nil
}

func (h *Hub) offer(p *Pairing, cs *connSess) {
	if err := cs.send(relay.Frame{T: relay.TypePairOffer, Pair: p.ID, Commit: p.Commit, Label: p.Label, UA: p.UA}); err != nil {
		return
	}
}

// offerLive offers a user's live pairing to a connector that just came online.
func (h *Hub) offerLive(cs *connSess) {
	h.mu.Lock()
	h.purgePairs()
	var live *Pairing
	for _, p := range h.pairs {
		if p.UserID == cs.cn.UserID {
			live = p
			if _, ok := p.machines[cs.cn.ID]; !ok {
				p.machines[cs.cn.ID] = &PairMachine{ConnectorID: cs.cn.ID, Name: cs.cn.Name, State: "waiting"}
				p.order = append(p.order, cs.cn.ID)
			}
		}
	}
	h.mu.Unlock()
	if live != nil {
		h.offer(live, cs)
	}
}

func (h *Hub) pairFrame(cs *connSess, f relay.Frame) {
	h.mu.Lock()
	p := h.pairs[f.Pair]
	if p == nil || p.UserID != cs.cn.UserID {
		h.mu.Unlock()
		return
	}
	m := p.machines[cs.cn.ID]
	if m == nil {
		h.mu.Unlock()
		return
	}
	switch f.T {
	case relay.TypePairChallenge:
		m.R = f.R
		if m.State == "waiting" {
			m.State = "pending"
		}
	case relay.TypePairState:
		switch f.State {
		case relay.PairPending:
			if m.State == "waiting" {
				m.State = "pending"
			}
		case relay.PairRevealed, relay.PairApproved, relay.PairRejected, relay.PairExpired, relay.PairMismatch:
			m.State = f.State
			m.Reason = clip(f.Code, 40)
		}
	}
	h.mu.Unlock()
	h.notify(cs.cn.UserID)
}

// PairingView returns the browser's view of a pairing.
func (h *Hub) PairingView(userID, id string) (map[string]any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.purgePairs()
	p := h.pairs[id]
	if p == nil || p.UserID != userID {
		return nil, ErrNoPairing
	}
	ms := make([]PairMachine, 0, len(p.order))
	for _, cid := range p.order {
		m := *p.machines[cid]
		_, m.Online = h.conns[cid]
		if cs := h.conns[cid]; cs != nil {
			m.Name = cs.cn.Name
		}
		ms = append(ms, m)
	}
	return map[string]any{"id": p.ID, "expiresAt": p.Expires.UnixMilli(), "machines": ms}, nil
}

// Reveal forwards the browser's key and nonce to one connector.
func (h *Hub) Reveal(userID, id, connectorID, spki, nonce string) error {
	h.mu.Lock()
	h.purgePairs()
	p := h.pairs[id]
	var cs *connSess
	if p != nil && p.UserID == userID {
		if m := p.machines[connectorID]; m != nil && m.R != "" && m.State == "pending" {
			cs = h.conns[connectorID]
		}
	}
	h.mu.Unlock()
	if p == nil || cs == nil {
		return ErrNoPairing
	}
	return cs.send(relay.Frame{T: relay.TypePairReveal, Pair: id, SPKI: spki, Nonce: nonce})
}

// Forget tells a connector to revoke all its devices (after a password reset).
func (h *Hub) Forget(connectorID string, at int64) {
	h.mu.Lock()
	cs := h.conns[connectorID]
	h.mu.Unlock()
	if cs != nil {
		_ = cs.send(relay.Frame{T: relay.TypeForget, At: at})
	}
}

// Counts for tests and status.
func (h *Hub) OnlineCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.conns)
}

// Sweep drops sockets whose connector was revoked or whose owner was disabled (for instance by
// the admin CLI, which runs in another process).
func (h *Hub) Sweep() {
	h.mu.Lock()
	list := make([]*connSess, 0, len(h.conns))
	for _, cs := range h.conns {
		list = append(list, cs)
	}
	h.mu.Unlock()
	for _, cs := range list {
		cn, err := h.cons.Get(cs.cn.ID)
		if err != nil {
			continue
		}
		drop := cn == nil || cn.RevokedAt != 0
		if !drop {
			u, err := h.cons.a.UserByID(cn.UserID)
			drop = err == nil && (u == nil || u.DisabledAt != 0)
		}
		if drop {
			cs.cancel()
			cs.ws.CloseNow()
		}
	}
}
