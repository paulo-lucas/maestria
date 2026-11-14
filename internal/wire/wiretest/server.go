// Package wiretest is a fake Maestri Wire host for tests and for the dev command in tools/wirefake.
// It speaks the subset of Wire that Maestria uses over HTTPS/WSS with a self-signed certificate,
// so SPKI pinning is exercised for real, and records every request so tests can prove which
// token touched which route.
package wiretest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/maestria-dev/maestria/internal/wire"
)

// AllCapabilities is what the fake advertises by default.
var AllCapabilities = []string{
	"feedSnapshots", "terminalStreaming", "terminalInputBytes", "terminalInputText", "terminalResize",
	"terminalChat", "chatThreadPreviews", "promptSegments", "workspaceActions", "floorStatus", "deviceRoles", "pairingCodes", "someFutureThing",
}

// Device is a paired device.
type Device struct {
	ID, Name, Identifier, Role, Token string
}

// Request is one request the fake served.
type Request struct {
	Method string
	Path   string
	Role   string // "" when unauthenticated
	Host   string
	Body   string
	Status int
}

// Server is the fake host.
type Server struct {
	// Host is host:port to give to the client ("127.0.0.1:NNNNN").
	Host string
	srv  *httptest.Server

	mu         sync.Mutex
	cert       tls.Certificate
	pin        wire.Pin
	caps       []string
	protocol   int
	password   string
	codes      map[string]bool
	failures   int
	throttleAt int
	pairRole   string
	devices    map[string]*Device // by token
	workspaces []wire.WorkspaceMeta
	feeds      map[string]*feedState
	chats      map[string]*chatState
	threads    map[string]wire.ChatThreadPreviews
	reqs       []Request
	inputs     []Input
	feedSubs   []string // roles of every feed subscription ever made
	sockets    map[*websocket.Conn]struct{}
	nextID     int

	// OnPrompt, OnAnswer run after a successful write, so a dev command can change the fake world.
	OnPrompt func(terminalID, text string)
	OnAnswer func(terminalID string, approve bool)

	dropReplies bool
}

// Input is a frame a client sent on a terminal stream.
type Input struct {
	Terminal string
	Role     string
	Data     []byte
}

type feedState struct {
	snap json.RawMessage
	subs map[chan json.RawMessage]struct{}
}

type chatState struct {
	snap json.RawMessage
	subs map[chan json.RawMessage]struct{}
}

func newCert() (tls.Certificate, wire.Pin, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, wire.Pin{}, err
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "Maestri Wire (fake)"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, wire.Pin{}, err
	}
	cert, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, wire.SPKIHash(cert), nil
}

// New starts a fake host on 127.0.0.1 with a random port.
func New() (*Server, error) {
	s := &Server{
		caps: append([]string(nil), AllCapabilities...), protocol: 1, codes: map[string]bool{}, pairRole: "owner",
		throttleAt: 1 << 30, devices: map[string]*Device{}, feeds: map[string]*feedState{}, chats: map[string]*chatState{}, threads: map[string]wire.ChatThreadPreviews{},
		sockets: map[*websocket.Conn]struct{}{},
	}
	var err error
	if s.cert, s.pin, err = newCert(); err != nil {
		return nil, err
	}
	s.srv = httptest.NewUnstartedServer(http.HandlerFunc(s.serve))
	s.srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	s.srv.TLS = &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			c := s.cert
			return &c, nil
		},
	}
	s.srv.StartTLS()
	s.Host = strings.TrimPrefix(s.srv.URL, "https://")
	return s, nil
}

// Close stops the server.
func (s *Server) Close() { s.DropSockets(); s.srv.CloseClientConnections(); s.srv.Close() }

// Pin is the SPKI pin of the current certificate.
func (s *Server) Pin() wire.Pin { s.mu.Lock(); defer s.mu.Unlock(); return s.pin }

// PinHex is the "Security key" Maestri's Manual tab shows (colon-separated uppercase hex).
func (s *Server) PinHex() string {
	h := strings.ToUpper(hex.EncodeToString(func() []byte { p := s.Pin(); return p[:] }()))
	var parts []string
	for i := 0; i < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":")
}

// PinBase64 is the QR code form.
func (s *Server) PinBase64() string {
	p := s.Pin()
	return base64.StdEncoding.EncodeToString(p[:])
}

// RotateCert gives the host a new certificate: pinned clients must refuse it.
func (s *Server) RotateCert() {
	c, p, _ := newCert()
	s.mu.Lock()
	s.cert, s.pin = c, p
	s.mu.Unlock()
	s.srv.CloseClientConnections()
}

// SetCapabilities replaces the advertised capabilities.
func (s *Server) SetCapabilities(c ...string) { s.mu.Lock(); s.caps = c; s.mu.Unlock() }

// SetProtocol changes the advertised protocolVersion.
func (s *Server) SetProtocol(v int) { s.mu.Lock(); s.protocol = v; s.mu.Unlock() }

// DropWriteReplies makes the host act on writes but close the connection without answering, so a
// client cannot tell whether the write was delivered.
func (s *Server) DropWriteReplies(v bool) { s.mu.Lock(); s.dropReplies = v; s.mu.Unlock() }

// SetPassword enables password pairing ("" disables it: 503).
func (s *Server) SetPassword(p string) { s.mu.Lock(); s.password = p; s.mu.Unlock() }

// SetThrottle makes pairing answer 429 once n failures have happened.
func (s *Server) SetThrottle(n int) { s.mu.Lock(); s.throttleAt = n; s.mu.Unlock() }

// NewCode issues a single-use six-digit pairing code.
func (s *Server) NewCode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, _ := rand.Int(rand.Reader, big.NewInt(900000))
	c := strconv.Itoa(int(n.Int64()) + 100000)
	s.codes[c] = true
	return c
}

// SetWorkspaces replaces the workspace list.
func (s *Server) SetWorkspaces(w ...wire.WorkspaceMeta) { s.mu.Lock(); s.workspaces = w; s.mu.Unlock() }

// SetFeed stores and broadcasts a feed snapshot for a Wire workspace id.
func (s *Server) SetFeed(wsID string, snap wire.FeedSnapshot) {
	raw, _ := json.Marshal(map[string]any{"type": "feed", "snapshot": snap})
	s.mu.Lock()
	f := s.feed(wsID)
	f.snap = raw
	for ch := range f.subs {
		select {
		case ch <- raw:
		default:
		}
	}
	s.mu.Unlock()
}

// SetThreads sets what GET /api/terminals/{id}/chat/threads answers for a terminal: the other
// threads (not the selected one) that hold messages.
func (s *Server) SetThreads(terminalID string, p wire.ChatThreadPreviews) {
	s.mu.Lock()
	s.threads[terminalID] = p
	s.mu.Unlock()
}

// SetChat stores and broadcasts a chat snapshot for a terminal id.
func (s *Server) SetChat(terminalID string, snap wire.ChatSnapshot) {
	raw, _ := json.Marshal(map[string]any{"type": "chat", "snapshot": snap})
	s.mu.Lock()
	c, ok := s.chats[terminalID]
	if !ok {
		c = &chatState{subs: map[chan json.RawMessage]struct{}{}}
		s.chats[terminalID] = c
	}
	c.snap = raw
	for ch := range c.subs {
		select {
		case ch <- raw:
		default:
		}
	}
	s.mu.Unlock()
}

func (s *Server) feed(wsID string) *feedState {
	f, ok := s.feeds[wsID]
	if !ok {
		f = &feedState{subs: map[chan json.RawMessage]struct{}{}}
		s.feeds[wsID] = f
	}
	return f
}

// DropSockets closes every open WebSocket (a network blip): clients should reconnect.
func (s *Server) DropSockets() {
	s.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(s.sockets))
	for c := range s.sockets {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.Close(websocket.StatusGoingAway, "")
	}
}

// RevokeAll forgets every device: all tokens now get 401.
func (s *Server) RevokeAll() {
	s.mu.Lock()
	s.devices = map[string]*Device{}
	s.mu.Unlock()
	s.DropSockets()
}

// Devices returns the paired devices.
func (s *Server) Devices() []Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Device
	for _, d := range s.devices {
		out = append(out, *d)
	}
	return out
}

// Requests returns every request served so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.reqs...)
}

// Inputs returns every frame sent on terminal streams.
func (s *Server) Inputs() []Input {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Input(nil), s.inputs...)
}

// FeedSubscriberRoles lists the role of every feed subscription ever opened.
func (s *Server) FeedSubscriberRoles() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.feedSubs...)
}

// ClearRequests forgets the recorded requests and inputs.
func (s *Server) ClearRequests() { s.mu.Lock(); s.reqs, s.inputs = nil, nil; s.mu.Unlock() }

type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(c int)           { r.status = c; r.ResponseWriter.WriteHeader(c) }
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) deviceFor(r *http.Request) *Device {
	tok := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		tok = strings.TrimPrefix(h, "Bearer ")
	} else if r.Method == http.MethodGet {
		tok = r.URL.Query().Get("token")
	}
	if tok == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.devices[tok]
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Connection", "close")
	rec := &recorder{ResponseWriter: w, status: 200}
	dev := s.deviceFor(r)
	role := ""
	if dev != nil {
		role = dev.Role
	}
	var body string
	if r.Body != nil && r.Method != http.MethodGet {
		b := make([]byte, 4096)
		n, _ := r.Body.Read(b)
		body = string(b[:n])
		r.Body = nopCloser{strings.NewReader(body)}
	}
	defer func() {
		s.mu.Lock()
		s.reqs = append(s.reqs, Request{Method: r.Method, Path: r.URL.Path, Role: role, Host: r.Host, Body: redactBody(body), Status: rec.status})
		s.mu.Unlock()
	}()
	if h := strings.Split(r.Host, ":")[0]; h != "localhost" && net.ParseIP(h) == nil && !strings.HasSuffix(h, ".local") && !strings.HasSuffix(h, ".ts.net") {
		writeErr(rec, 403, "forbidden", "bad host")
		return
	}
	s.route(rec, r, dev)
}

type nopCloser struct{ *strings.Reader }

func (nopCloser) Close() error { return nil }

func redactBody(b string) string {
	if strings.Contains(b, `"password"`) || strings.Contains(b, `"code"`) {
		return "[pairing body]"
	}
	return b
}

func (s *Server) route(w *recorder, r *http.Request, dev *Device) {
	p := r.URL.Path
	switch {
	case r.Method == http.MethodGet && p == "/api/info":
		s.mu.Lock()
		out := map[string]any{"name": "Fake Mac", "protocolVersion": s.protocol, "capabilities": s.caps, "requiresPassword": s.password != "", "hosts": []string{"localhost"}}
		if dev != nil {
			out["role"] = dev.Role
		}
		s.mu.Unlock()
		writeJSON(w, out)
	case r.Method == http.MethodPost && p == "/pair":
		s.pair(w, r)
	default:
		if dev == nil {
			writeErr(w, 401, "unauthorized", "invalid token")
			return
		}
		s.authed(w, r, dev)
	}
}

func (s *Server) pair(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceName       string `json:"deviceName"`
		DeviceIdentifier string `json:"deviceIdentifier"`
		Code             string `json:"code"`
		Password         string `json:"password"`
	}
	if r.Header.Get("Content-Type") != "application/json" {
		writeErr(w, 415, "invalidRequest", "content type")
		return
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	s.mu.Lock()
	if s.failures >= s.throttleAt {
		s.mu.Unlock()
		w.Header().Set("Retry-After", "30")
		writeErr(w, 429, "rateLimited", "slow down")
		return
	}
	ok := false
	switch {
	case req.Code != "":
		ok = s.codes[req.Code]
		delete(s.codes, req.Code) // single use
	case req.Password != "":
		if s.password == "" {
			s.mu.Unlock()
			writeErr(w, 503, "unavailable", "password pairing is off")
			return
		}
		ok = req.Password == s.password
	}
	if !ok {
		s.failures++
		s.mu.Unlock()
		writeErr(w, 401, "unauthorized", "wrong credential")
		return
	}
	var d *Device
	if req.DeviceIdentifier != "" {
		for tok, dv := range s.devices {
			if dv.Identifier == req.DeviceIdentifier {
				delete(s.devices, tok) // re-pair replaces the record: same id and role, new token
				d = dv
			}
		}
	}
	if d == nil {
		s.nextID++
		d = &Device{ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", s.nextID), Role: s.pairRole}
	}
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	d.Token, d.Name, d.Identifier = hex.EncodeToString(b), req.DeviceName, req.DeviceIdentifier
	s.devices[d.Token] = d
	out := map[string]any{"token": d.Token, "deviceId": d.ID, "deviceName": d.Name, "protocolVersion": s.protocol, "role": d.Role}
	s.mu.Unlock()
	writeJSON(w, out)
}

func (s *Server) authed(w *recorder, r *http.Request, dev *Device) {
	p := r.URL.Path
	parts := strings.Split(strings.Trim(p, "/"), "/")
	isWrite := r.Method != http.MethodGet
	if isWrite && !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		writeErr(w, 401, "unauthorized", "writes need the Authorization header")
		return
	}
	selfRevoke := r.Method == http.MethodDelete && len(parts) == 3 && parts[1] == "devices" && parts[2] == dev.ID
	if isWrite && dev.Role != "owner" && !selfRevoke {
		writeErr(w, 403, "forbidden", "read-only device")
		return
	}
	switch {
	case r.Method == http.MethodGet && p == "/api/workspaces":
		s.mu.Lock()
		ws := s.workspaces
		s.mu.Unlock()
		writeJSON(w, map[string]any{"workspaces": ws})
	case r.Method == http.MethodGet && p == "/api/feed/stream":
		s.feedStream(w, r, dev)
	case len(parts) == 5 && parts[1] == "terminals" && parts[3] == "chat" && parts[4] == "threads" && r.Method == http.MethodGet:
		s.mu.Lock()
		p := s.threads[parts[2]]
		s.mu.Unlock()
		if p.Threads == nil {
			p.Threads = []wire.ChatThreadPreview{}
		}
		writeJSON(w, p)
	case len(parts) == 5 && parts[1] == "terminals" && parts[3] == "chat" && parts[4] == "stream":
		s.chatStream(w, r, parts[2])
	case len(parts) == 4 && parts[1] == "terminals" && parts[3] == "stream":
		s.terminalStream(w, r, dev, parts[2])
	case len(parts) == 4 && parts[1] == "terminals" && parts[3] == "prompt" && r.Method == http.MethodPost:
		var req struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if s.OnPrompt != nil {
			s.OnPrompt(parts[2], req.Text)
		}
		s.mu.Lock()
		drop := s.dropReplies
		s.mu.Unlock()
		if drop {
			if c, _, err := http.NewResponseController(w).Hijack(); err == nil {
				_ = c.Close()
			}
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	case len(parts) == 4 && parts[1] == "terminals" && (parts[3] == "approve" || parts[3] == "reject") && r.Method == http.MethodPost:
		if !s.hasPending(parts[2]) {
			writeErr(w, 409, "conflict", "no pending prompt")
			return
		}
		if s.OnAnswer != nil {
			s.OnAnswer(parts[2], parts[3] == "approve")
		}
		writeJSON(w, map[string]bool{"ok": true})
	case r.Method == http.MethodPut && len(parts) == 4 && parts[1] == "devices" && parts[3] == "role":
		var req struct {
			Role string `json:"role"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, d := range s.devices {
			if d.ID == parts[2] {
				if d.ID == dev.ID && req.Role == "guest" {
					writeErr(w, 409, "conflict", "cannot demote yourself")
					return
				}
				d.Role = req.Role
				writeJSON(w, map[string]bool{"ok": true})
				return
			}
		}
		writeErr(w, 404, "notFound", "no such device")
	case r.Method == http.MethodDelete && len(parts) == 3 && parts[1] == "devices":
		s.mu.Lock()
		for tok, d := range s.devices {
			if d.ID == parts[2] {
				delete(s.devices, tok)
			}
		}
		s.mu.Unlock()
		writeJSON(w, map[string]bool{"ok": true})
	default:
		writeErr(w, 404, "notFound", "no such route")
	}
}

func (s *Server) hasPending(terminalID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.feeds {
		var env struct {
			Snapshot wire.FeedSnapshot `json:"snapshot"`
		}
		if json.Unmarshal(f.snap, &env) != nil {
			continue
		}
		for _, it := range env.Snapshot.Items {
			if it.Kind == wire.ItemPendingPrompt && it.Terminal != nil && it.Terminal.ID == terminalID {
				return true
			}
		}
	}
	return false
}

func (s *Server) accept(w http.ResponseWriter, r *http.Request) *websocket.Conn {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return nil
	}
	c.SetReadLimit(1 << 20)
	s.mu.Lock()
	s.sockets[c] = struct{}{}
	s.mu.Unlock()
	return c
}

func (s *Server) forget(c *websocket.Conn) {
	s.mu.Lock()
	delete(s.sockets, c)
	s.mu.Unlock()
}

func (s *Server) feedStream(w http.ResponseWriter, r *http.Request, dev *Device) {
	q, _ := url.ParseQuery(r.URL.RawQuery)
	ws := q.Get("ws")
	s.mu.Lock()
	f := s.feed(ws)
	ch := make(chan json.RawMessage, 8)
	f.subs[ch] = struct{}{}
	first := f.snap
	s.feedSubs = append(s.feedSubs, dev.Role)
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(f.subs, ch); s.mu.Unlock() }()
	c := s.accept(w, r)
	if c == nil {
		return
	}
	defer s.forget(c)
	defer c.CloseNow()
	ctx := r.Context()
	if first != nil {
		_ = c.Write(ctx, websocket.MessageText, first)
	}
	go func() { // answer pings
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				c.CloseNow()
				return
			}
			if strings.Contains(string(data), `"ping"`) {
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"pong"}`))
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-ch:
			if c.Write(ctx, websocket.MessageText, m) != nil {
				return
			}
		}
	}
}

func (s *Server) chatStream(w http.ResponseWriter, r *http.Request, terminalID string) {
	s.mu.Lock()
	cs, ok := s.chats[terminalID]
	if !ok {
		cs = &chatState{subs: map[chan json.RawMessage]struct{}{}}
		s.chats[terminalID] = cs
	}
	ch := make(chan json.RawMessage, 8)
	cs.subs[ch] = struct{}{}
	first := cs.snap
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(cs.subs, ch); s.mu.Unlock() }()
	c := s.accept(w, r)
	if c == nil {
		return
	}
	defer s.forget(c)
	defer c.CloseNow()
	ctx := c.CloseRead(r.Context())
	if first != nil {
		_ = c.Write(ctx, websocket.MessageText, first)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-ch:
			if c.Write(ctx, websocket.MessageText, m) != nil {
				return
			}
		}
	}
}

func (s *Server) terminalStream(w http.ResponseWriter, r *http.Request, dev *Device, terminalID string) {
	c := s.accept(w, r)
	if c == nil {
		return
	}
	defer s.forget(c)
	defer c.CloseNow()
	ctx := r.Context()
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"geometry","cols":100,"rows":30}`))
	_ = c.Write(ctx, websocket.MessageBinary, []byte("\x1b[2J screen"))
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageBinary {
			s.recordInput(terminalID, dev, data)
			continue
		}
		var m struct {
			Type string `json:"type"`
			Data string `json:"data"`
		}
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.Type {
		case "inputBytes":
			if b, err := base64.StdEncoding.DecodeString(m.Data); err == nil {
				s.recordInput(terminalID, dev, b)
			}
		case "input":
			s.recordInput(terminalID, dev, []byte(m.Data))
		case "ping":
			_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"pong"}`))
		}
	}
}

func (s *Server) recordInput(terminalID string, dev *Device, b []byte) {
	if dev.Role != "owner" {
		return // guests' input is dropped
	}
	s.mu.Lock()
	s.inputs = append(s.inputs, Input{Terminal: terminalID, Role: dev.Role, Data: b})
	s.mu.Unlock()
}
