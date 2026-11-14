package wire

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const (
	requestTimeout = 20 * time.Second // Wire's own request timeout
	maxResponse    = 16 << 20
	maxFrame       = 32 << 20
	feedPingEvery  = 20 * time.Second
	chatPingEvery  = 25 * time.Second
	streamIdle     = 75 * time.Second
	hostHeader     = "localhost"
)

// Client talks to one Wire host with one token. Every request is checked against the allowlist
// (allow.go) for the client's scope before it is built.
type Client struct {
	host     string
	pin      Pin
	scope    Scope
	token    string
	deviceID string
	rest     *http.Client
	ws       *http.Client
}

// NewClient builds a client for host ("localhost:7434") pinned to pin. token may be empty for
// the unauthenticated calls of the pairing flow.
func NewClient(host string, pin Pin, scope Scope, token, deviceID string) *Client {
	mk := func() *http.Transport {
		return &http.Transport{
			Proxy:               nil, // Wire is local: never go through a proxy
			DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSClientConfig:     TLSConfig(pin),
			TLSHandshakeTimeout: 10 * time.Second,
			// Wire answers HTTP/1.1 only and closes every connection.
			DisableKeepAlives: true,
			ForceAttemptHTTP2: false,
			TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},
		}
	}
	return &Client{
		host: host, pin: pin, scope: scope, token: token, deviceID: deviceID,
		rest: &http.Client{Transport: mk(), Timeout: requestTimeout},
		ws:   &http.Client{Transport: mk()}, // no overall timeout: sockets live long
	}
}

// Host returns the host:port this client dials.
func (c *Client) Host() string { return c.host }

func (c *Client) scrub(err error) error { return scrub(err, c.token) }

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	if err := Check(c.scope, method, path, c.deviceID); err != nil {
		return err
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://"+c.host+path, body)
	if err != nil {
		return c.scrub(err)
	}
	req.Host = hostHeader
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "maestria")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.rest.Do(req)
	if err != nil {
		return c.scrub(unwrapURL(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return c.scrub(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return parseAPIError(resp, data)
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("wire: unexpected response from %s", path)
		}
	}
	return nil
}

// unwrapURL drops the *url.Error wrapper, whose text repeats the request URL.
func unwrapURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}

func parseAPIError(resp *http.Response, data []byte) error {
	ae := &APIError{Status: resp.StatusCode}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &env) == nil {
		ae.Code, ae.Message = env.Error.Code, env.Error.Message
	}
	if s := resp.Header.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
			ae.RetryAfter = time.Duration(n) * time.Second
		}
	}
	return ae
}

// Info is GET /api/info. With a valid token it includes the caller's role.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var in Info
	err := c.do(ctx, http.MethodGet, "/api/info", nil, &in)
	return in, err
}

// Workspaces is GET /api/workspaces.
func (c *Client) Workspaces(ctx context.Context) ([]WorkspaceMeta, error) {
	var out struct {
		Workspaces []WorkspaceMeta `json:"workspaces"`
	}
	err := c.do(ctx, http.MethodGet, "/api/workspaces", nil, &out)
	return out.Workspaces, err
}

// ChatThreads is GET /api/terminals/{id}/chat/threads: the newest entries of the chat's other
// threads. It selects nothing and marks nothing read.
func (c *Client) ChatThreads(ctx context.Context, terminalID string) (ChatThreadPreviews, error) {
	var out ChatThreadPreviews
	err := c.do(ctx, http.MethodGet, "/api/terminals/"+terminalID+"/chat/threads", nil, &out)
	return out, err
}

// Prompt sends text to a terminal (owner only). A transport failure after the request may have
// been sent is reported as ErrAmbiguous and is never retried.
func (c *Client) Prompt(ctx context.Context, terminalID, text string) error {
	err := c.do(ctx, http.MethodPost, "/api/terminals/"+terminalID+"/prompt", map[string]string{"text": text}, nil)
	return ambiguous(err)
}

// Approve answers a pending Y/n with yes.
func (c *Client) Approve(ctx context.Context, terminalID string) error {
	return ambiguous(c.do(ctx, http.MethodPost, "/api/terminals/"+terminalID+"/approve", nil, nil))
}

// Reject answers a pending Y/n with no.
func (c *Client) Reject(ctx context.Context, terminalID string) error {
	return ambiguous(c.do(ctx, http.MethodPost, "/api/terminals/"+terminalID+"/reject", nil, nil))
}

// ambiguous marks a failure that is not an HTTP answer: the host may have acted on the request.
func ambiguous(err error) error {
	if err == nil {
		return nil
	}
	var ae *APIError
	var pe *PinError
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Op == "dial" {
		return err // never connected: certainly not delivered
	}
	if errors.As(err, &ae) || errors.As(err, &pe) || errors.Is(err, ErrNotAllowed) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrAmbiguous, err)
}

// DeleteDevice revokes this client's own device.
func (c *Client) DeleteDevice(ctx context.Context) error {
	return c.do(ctx, http.MethodDelete, "/api/devices/"+c.deviceID, nil, nil)
}

// SetGuestRole demotes a device to guest (setup scope only, used right after pairing it).
func (c *Client) SetGuestRole(ctx context.Context, deviceID string) error {
	return c.do(ctx, http.MethodPut, "/api/devices/"+deviceID+"/role", map[string]string{"role": "guest"}, nil)
}

func (c *Client) dialWS(ctx context.Context, path string, q url.Values) (*websocket.Conn, error) {
	if err := Check(c.scope, http.MethodGet, path, c.deviceID); err != nil {
		return nil, err
	}
	if q == nil {
		q = url.Values{}
	}
	q.Set("token", c.token) // sockets authenticate with ?token= only
	u := url.URL{Scheme: "wss", Host: c.host, Path: path, RawQuery: q.Encode()}
	conn, resp, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		HTTPClient: c.ws,
		Host:       hostHeader,
	})
	if err != nil {
		if resp != nil && resp.StatusCode >= 400 {
			return nil, &APIError{Status: resp.StatusCode}
		}
		return nil, c.scrub(unwrapURL(err))
	}
	conn.SetReadLimit(maxFrame)
	return conn, nil
}

// readFrame reads one text message, failing when the host is silent for streamIdle.
func readFrame(ctx context.Context, conn *websocket.Conn) ([]byte, error) {
	rctx, cancel := context.WithTimeout(ctx, streamIdle)
	defer cancel()
	typ, data, err := conn.Read(rctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageText {
		return nil, nil
	}
	return data, nil
}

// FeedStream subscribes to a workspace feed with this (guest) client and calls fn with every
// snapshot, until the socket fails or ctx ends. The returned error is never nil.
func (c *Client) FeedStream(ctx context.Context, wireWorkspaceID string, fn func(*FeedSnapshot)) error {
	conn, err := c.dialWS(ctx, "/api/feed/stream", url.Values{"ws": {wireWorkspaceID}})
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		t := time.NewTicker(feedPingEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if conn.Write(ctx, websocket.MessageText, []byte(`{"type":"ping"}`)) != nil {
					return
				}
			}
		}
	}()
	for {
		data, err := readFrame(ctx, conn)
		if err != nil {
			return c.scrub(err)
		}
		if data == nil {
			continue
		}
		var env struct {
			Type     string          `json:"type"`
			Snapshot json.RawMessage `json:"snapshot"`
		}
		if json.Unmarshal(data, &env) != nil || env.Type != "feed" {
			continue // pong, mutation, and anything newer
		}
		var snap FeedSnapshot
		if json.Unmarshal(env.Snapshot, &snap) != nil {
			continue
		}
		fn(&snap)
	}
}

// ChatStream follows a terminal's chat. Each frame is a full replacement of the tail. The chat
// socket never wakes or resizes the terminal.
func (c *Client) ChatStream(ctx context.Context, terminalID string, fn func(*ChatSnapshot)) error {
	conn, err := c.dialWS(ctx, "/api/terminals/"+terminalID+"/chat/stream", nil)
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		t := time.NewTicker(chatPingEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, pc := context.WithTimeout(ctx, 10*time.Second)
				err := conn.Ping(pctx)
				pc()
				if err != nil {
					return
				}
			}
		}
	}()
	for {
		data, err := readFrame(ctx, conn)
		if err != nil {
			return c.scrub(err)
		}
		if data == nil {
			continue
		}
		var env struct {
			Type     string          `json:"type"`
			Snapshot json.RawMessage `json:"snapshot"`
		}
		if json.Unmarshal(data, &env) != nil || env.Type != "chat" {
			continue
		}
		var snap ChatSnapshot
		if json.Unmarshal(env.Snapshot, &snap) != nil {
			continue
		}
		fn(&snap)
	}
}

// Interrupt sends Escape into a terminal through its stream (owner only), which interrupts the
// agent's current turn. It needs terminalStreaming and terminalInputBytes.
func (c *Client) Interrupt(ctx context.Context, terminalID string) error {
	if err := CheckInput([]byte{esc}); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	conn, err := c.dialWS(ctx, "/api/terminals/"+terminalID+"/stream", nil)
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	msg, _ := json.Marshal(map[string]string{"type": "inputBytes", "data": base64.StdEncoding.EncodeToString([]byte{esc})})
	if err := conn.Write(ctx, websocket.MessageText, msg); err != nil {
		return ambiguous(c.scrub(err))
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
	return nil
}
