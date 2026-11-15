package connect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/paulo-lucas/maestria/internal/devices"
	"github.com/paulo-lucas/maestria/internal/relay"
	"github.com/paulo-lucas/maestria/internal/termsafe"
)

// State is the connection state shown by `maestria connect status`.
type State string

// Connection states.
const (
	StateConnecting   State = "connecting"
	StateOnline       State = "online"
	StateOffline      State = "offline"
	StateRevoked      State = "revoked"
	StateShuttingDown State = "stopped"
)

// ErrRevoked means the cloud no longer accepts this connector's credential.
var ErrRevoked = errors.New("the cloud rejected this connector's credential: it was unlinked or revoked")

// Client keeps the outbound WebSocket to the cloud and serves relayed requests.
type Client struct {
	Link    *Link
	Version string
	Name    string
	// Handler runs the allowlisted API routes (the same handlers as local mode).
	Handler http.Handler
	Gate    *Gate
	Devices *devices.Manager
	Killed  func() bool
	Log     *slog.Logger
	// OnState is called on every connection state change. May be nil.
	OnState func(State, error)
	// OnPairing is called when the cloud offers a pairing that was accepted, so the machine can
	// tell the person that a browser asked to be approved. May be nil.
	OnPairing func(label string)
	// HTTP overrides the dial client (tests). May be nil.
	HTTP *http.Client
	// Backoff bounds the reconnect delay. Zero values use 1s and 60s.
	MinBackoff, MaxBackoff time.Duration
	// Tick is how often pairing and device changes are checked. Zero means 1s.
	Tick time.Duration
}

func (c *Client) log() *slog.Logger {
	if c.Log == nil {
		return slog.New(slog.NewTextHandler(nopWriter{}, nil))
	}
	return c.Log
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

func (c *Client) state(s State, err error) {
	if c.OnState != nil {
		c.OnState(s, err)
	}
}

// Run connects and reconnects until ctx ends or the cloud revokes the credential.
func (c *Client) Run(ctx context.Context) error {
	minB, maxB := c.MinBackoff, c.MaxBackoff
	if minB <= 0 {
		minB = time.Second
	}
	if maxB <= 0 {
		maxB = 60 * time.Second
	}
	backoff := minB
	for {
		c.state(StateConnecting, nil)
		start := time.Now()
		err := c.session(ctx)
		if ctx.Err() != nil {
			c.state(StateShuttingDown, nil)
			return nil
		}
		if errors.Is(err, ErrRevoked) {
			c.state(StateRevoked, err)
			return err
		}
		c.state(StateOffline, err)
		c.log().Warn("connection to the cloud ended", "error", errText(err))
		if time.Since(start) > 30*time.Second {
			backoff = minB
		}
		jitter := time.Duration(float64(backoff) * (0.8 + 0.4*rand.Float64()))
		select {
		case <-ctx.Done():
			c.state(StateShuttingDown, nil)
			return nil
		case <-time.After(jitter):
		}
		backoff = min(backoff*2, maxB)
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type inflight struct {
	id     string
	req    Request
	body   bytes.Buffer
	ctx    context.Context
	cancel context.CancelFunc
	rw     *respWriter
}

type session struct {
	c        *Client
	ws       *websocket.Conn
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	inflight map[string]*inflight
	wg       sync.WaitGroup
	lastSent string
}

func (c *Client) session(parent context.Context) error {
	sock, err := relay.SocketURL(c.Link.Cloud)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	dctx, dcancel := context.WithTimeout(ctx, 20*time.Second)
	ws, resp, err := websocket.Dial(dctx, sock, &websocket.DialOptions{
		HTTPClient: c.HTTP,
		HTTPHeader: http.Header{"Authorization": {"Bearer " + c.Link.Credential}, "User-Agent": {"maestria-connector/" + c.Version}},
	})
	dcancel()
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			return ErrRevoked
		}
		return err
	}
	ws.SetReadLimit(relay.MaxFrame)
	s := &session{c: c, ws: ws, ctx: ctx, cancel: cancel, inflight: map[string]*inflight{}}
	defer func() {
		cancel()
		s.wg.Wait()
	}()

	if err := s.sendHello(); err != nil {
		ws.CloseNow()
		return err
	}
	_ = c.Devices.MarkUnreported()
	c.state(StateOnline, nil)

	s.wg.Add(3)
	go s.pinger()
	go s.ticker()
	go func() {
		// Shutdown: send a normal close and let the reader finish, instead of cancelling a read
		// (which would tear the socket down without the close handshake).
		defer s.wg.Done()
		<-ctx.Done()
		closeGracefully(ws)
	}()

	err = s.readLoop()
	if ctx.Err() != nil || parent.Err() != nil {
		return nil
	}
	cancel()
	ws.CloseNow()
	return err
}

// closeGracefully sends a normal close and waits briefly for the peer's answer, then gives up.
func closeGracefully(ws *websocket.Conn) {
	done := make(chan struct{})
	go func() {
		_ = ws.Close(websocket.StatusNormalClosure, "bye")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		ws.CloseNow()
	}
}

func (s *session) send(f relay.Frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	return s.ws.Write(ctx, websocket.MessageText, b)
}

func (s *session) hello() relay.Frame {
	infos, _ := s.c.Devices.Infos()
	for i := range infos {
		infos[i].LastUsedAt = 0 // would make every use look like a change
	}
	return relay.Frame{
		T: relay.TypeHello, Proto: relay.Proto, Version: s.c.Version, OS: runtime.GOOS, Arch: runtime.GOARCH,
		Name: s.c.Name, KillSwitch: s.c.Killed != nil && s.c.Killed(), Devices: infos,
	}
}

func (s *session) sendHello() error {
	f := s.hello()
	b, _ := json.Marshal(f)
	s.mu.Lock()
	s.lastSent = string(b)
	s.mu.Unlock()
	return s.send(f)
}

func (s *session) pinger() {
	defer s.wg.Done()
	t := time.NewTicker(relay.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(s.ctx, 15*time.Second)
			err := s.ws.Ping(pctx)
			cancel()
			if err != nil {
				s.ws.CloseNow()
				return
			}
		}
	}
}

// ticker watches the local database for changes the cloud must hear about: pairing states, the
// device list and the kill switch.
func (s *session) ticker() {
	defer s.wg.Done()
	tick := s.c.Tick
	if tick <= 0 {
		tick = time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		changes, err := s.c.Devices.Changes()
		if err == nil {
			for _, ch := range changes {
				_ = s.send(relay.Frame{T: relay.TypePairState, Pair: ch.Pair, State: ch.State})
			}
		}
		f := s.hello()
		b, _ := json.Marshal(f)
		s.mu.Lock()
		changed := s.lastSent != string(b)
		if changed {
			s.lastSent = string(b)
		}
		s.mu.Unlock()
		if changed {
			_ = s.send(f)
		}
	}
}

func (s *session) readLoop() error {
	for {
		_, data, err := s.ws.Read(context.Background())
		if err != nil {
			return err
		}
		var f relay.Frame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		switch f.T {
		case relay.TypeWelcome:
		case relay.TypeReq:
			s.onReq(f)
		case relay.TypeBody:
			s.onBody(f)
		case relay.TypeAck:
			s.mu.Lock()
			in := s.inflight[f.ID]
			s.mu.Unlock()
			if in != nil {
				in.rw.Ack(f.N)
			}
		case relay.TypeCancel:
			s.mu.Lock()
			in := s.inflight[f.ID]
			s.mu.Unlock()
			if in != nil {
				in.cancel()
			}
		case relay.TypePairOffer:
			s.onPairOffer(f)
		case relay.TypePairReveal:
			s.onPairReveal(f)
		case relay.TypeForget:
			if n, err := s.c.Devices.RevokeAll(); err == nil {
				s.c.log().Info("the cloud asked to forget paired devices; all were revoked", "count", n)
			}
		}
	}
}

func headerBytes(h map[string]string) int {
	n := 0
	for k, v := range h {
		n += len(k) + len(v)
	}
	return n
}

func (s *session) onReq(f relay.Frame) {
	s.mu.Lock()
	if len(s.inflight) >= relay.MaxInFlight || s.inflight[f.ID] != nil || f.ID == "" {
		s.mu.Unlock()
		_ = s.send(relay.Frame{T: relay.TypeErr, ID: f.ID, Code: relay.ErrBadRequest, Status: relay.StatusFor(relay.ErrBadRequest)})
		return
	}
	hdr := make(map[string]string, len(f.Header))
	for k, v := range f.Header {
		hdr[strings.ToLower(k)] = v
	}
	if headerBytes(hdr) > relay.MaxHeaderBytes {
		s.mu.Unlock()
		_ = s.send(relay.Frame{T: relay.TypeErr, ID: f.ID, Code: relay.ErrBadRequest, Status: 400})
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	in := &inflight{id: f.ID, ctx: ctx, cancel: cancel, req: Request{Method: f.Method, Path: f.Path, Query: f.Query, Header: hdr}}
	stream := relay.IsStream(f.Method, f.Path)
	in.rw = newRespWriter(ctx, cancel, f.ID, stream, s.send)
	s.inflight[f.ID] = in
	s.mu.Unlock()
	s.addBody(in, f)
}

func (s *session) onBody(f relay.Frame) {
	s.mu.Lock()
	in := s.inflight[f.ID]
	s.mu.Unlock()
	if in == nil {
		return
	}
	s.addBody(in, f)
}

func (s *session) addBody(in *inflight, f relay.Frame) {
	if f.Body != "" {
		b, err := relay.Dec(f.Body)
		if err != nil || in.body.Len()+len(b) > relay.MaxRequestBody {
			code := relay.ErrBadRequest
			if err == nil {
				code = relay.ErrTooLarge
			}
			s.finishErr(in, code, nil)
			return
		}
		in.body.Write(b)
	}
	if f.More {
		return
	}
	in.req.Body = in.body.Bytes()
	s.wg.Add(1)
	go s.serve(in)
}

func (s *session) finishErr(in *inflight, code string, dev *devices.Device) {
	id := ""
	if dev != nil {
		id = dev.ID
	}
	s.c.Gate.Refused(code, id, in.req.Method, in.req.Path)
	_ = s.send(relay.Frame{T: relay.TypeErr, ID: in.id, Code: code, Status: relay.StatusFor(code)})
	in.cancel()
	s.mu.Lock()
	delete(s.inflight, in.id)
	s.mu.Unlock()
}

func (s *session) serve(in *inflight) {
	defer s.wg.Done()
	defer func() {
		in.cancel()
		s.mu.Lock()
		delete(s.inflight, in.id)
		s.mu.Unlock()
	}()
	dev, code := s.c.Gate.Check(in.req)
	if code != "" {
		s.finishErr(in, code, dev)
		return
	}

	target := in.req.Path
	if in.req.Query != "" {
		target += "?" + in.req.Query
	}
	var body *bytes.Reader
	if len(in.req.Body) > 0 {
		body = bytes.NewReader(in.req.Body)
	} else {
		body = bytes.NewReader(nil)
	}
	ctx := relay.WithDevice(in.ctx, dev.ID)
	hr, err := http.NewRequestWithContext(ctx, in.req.Method, "http://relay.invalid"+target, body)
	if err != nil {
		s.finishErr(in, relay.ErrBadRequest, dev)
		return
	}
	hr.RemoteAddr = "relay"
	for _, k := range []string{"content-type", "accept", "x-requested-with"} {
		if v := in.req.Header[k]; v != "" {
			hr.Header.Set(k, v)
		}
	}

	if relay.IsStream(in.req.Method, in.req.Path) {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			t := time.NewTicker(relay.StreamRecheck)
			defer t.Stop()
			for {
				select {
				case <-in.ctx.Done():
					return
				case <-t.C:
					if (s.c.Killed != nil && s.c.Killed()) || s.c.Devices.Revalidate(dev.ID) != "" || !s.c.Devices.Policy().Enabled {
						in.cancel()
						return
					}
				}
			}
		}()
	}

	func() {
		defer func() {
			if rec := recover(); rec != nil {
				s.c.log().Error("relayed handler panicked", "path", in.req.Path, "panic", fmt.Sprint(rec))
				in.rw.mu.Lock()
				in.rw.failed = true
				in.rw.mu.Unlock()
				if !in.rw.headSent {
					_ = s.send(relay.Frame{T: relay.TypeErr, ID: in.id, Code: relay.ErrInternal, Status: 500})
				}
			}
		}()
		s.c.Handler.ServeHTTP(in.rw, hr)
	}()

	in.rw.mu.Lock()
	tooLarge := in.rw.tooLarge
	status := in.rw.status
	in.rw.mu.Unlock()
	// Audit before the response completes (and whatever its outcome): the write has already
	// run, so a client must never be able to see it finish while it is still unaudited, and a
	// response that turns out too large must not leave an executed write unrecorded.
	s.c.Gate.Wrote(dev.ID, in.req.Method, in.req.Path, status)
	if tooLarge {
		_ = s.send(relay.Frame{T: relay.TypeErr, ID: in.id, Code: relay.ErrTooLarge, Status: 413})
		return
	}
	if in.ctx.Err() == nil {
		_ = in.rw.finish()
	}
}

func (s *session) onPairOffer(f relay.Frame) {
	state := func(st, code string) {
		_ = s.send(relay.Frame{T: relay.TypePairState, Pair: f.Pair, State: st, Code: code})
	}
	if s.c.Killed != nil && s.c.Killed() {
		state(relay.PairRejected, relay.ErrKillSwitch)
		return
	}
	if !s.c.Devices.Policy().Enabled {
		state(relay.PairRejected, relay.ErrRelayDisabled)
		return
	}
	r, err := s.c.Devices.Offer(f.Pair, f.Commit, f.Label, f.UA)
	var paused *devices.ThrottledError
	switch {
	case errors.Is(err, devices.ErrTooManyPending):
		state(relay.PairRejected, "too_many_pending")
	case errors.As(err, &paused):
		state(relay.PairRejected, "pairing_paused")
	case err != nil:
		state(relay.PairRejected, relay.ErrBadRequest)
	default:
		_ = s.send(relay.Frame{T: relay.TypePairChallenge, Pair: f.Pair, R: r})
		if s.c.OnPairing != nil {
			s.c.OnPairing(termsafe.Clean(f.Label, 80))
		}
	}
}

func (s *session) onPairReveal(f relay.Frame) {
	if s.c.Killed != nil && s.c.Killed() {
		_ = s.send(relay.Frame{T: relay.TypePairState, Pair: f.Pair, State: relay.PairRejected, Code: relay.ErrKillSwitch})
		return
	}
	// The code is not sent back: the person reads it on the browser and types it here.
	if _, err := s.c.Devices.Reveal(f.Pair, f.SPKI, f.Nonce); err != nil {
		if errors.Is(err, devices.ErrBadPairing) {
			_ = s.send(relay.Frame{T: relay.TypePairState, Pair: f.Pair, State: relay.PairMismatch})
		}
		return
	}
	// Changes() reports the new "revealed" state on the next tick.
}
