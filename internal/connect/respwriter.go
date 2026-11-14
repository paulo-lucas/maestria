package connect

import (
	"bytes"
	"context"
	"net/http"
	"sync"

	"github.com/maestria-dev/maestria/internal/relay"
)

// respWriter turns a handler's response into frames: a res head with the first bytes, then
// chunks. Writes block while the cloud has not acknowledged enough of what was already sent
// (flow control), so a slow browser pauses the handler instead of growing a buffer.
type respWriter struct {
	ctx    context.Context
	cancel context.CancelFunc
	id     string
	send   func(relay.Frame) error
	stream bool

	mu        sync.Mutex
	hdr       http.Header
	status    int
	headSent  bool
	buf       bytes.Buffer
	total     int64
	unacked   int64
	creditSig chan struct{}
	failed    bool
	tooLarge  bool
}

func newRespWriter(ctx context.Context, cancel context.CancelFunc, id string, stream bool, send func(relay.Frame) error) *respWriter {
	return &respWriter{ctx: ctx, cancel: cancel, id: id, send: send, stream: stream, hdr: http.Header{}, creditSig: make(chan struct{}, 1)}
}

func (w *respWriter) Header() http.Header { return w.hdr }

func (w *respWriter) WriteHeader(code int) {
	w.mu.Lock()
	if w.status == 0 {
		w.status = code
	}
	w.mu.Unlock()
}

// Ack returns flow control credit for n delivered bytes.
func (w *respWriter) Ack(n int64) {
	w.mu.Lock()
	w.unacked -= n
	if w.unacked < 0 {
		w.unacked = 0
	}
	w.mu.Unlock()
	select {
	case w.creditSig <- struct{}{}:
	default:
	}
}

func (w *respWriter) Write(p []byte) (int, error) {
	if w.ctx.Err() != nil {
		return 0, w.ctx.Err()
	}
	w.mu.Lock()
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.failed {
		w.mu.Unlock()
		return 0, context.Canceled
	}
	if !w.stream && w.total+int64(w.buf.Len())+int64(len(p)) > relay.MaxResponseBody {
		w.tooLarge, w.failed = true, true
		w.mu.Unlock()
		w.cancel()
		return 0, context.Canceled
	}
	w.buf.Write(p)
	full := w.buf.Len() >= relay.BodyChunk
	w.mu.Unlock()
	if full {
		if err := w.flush(true); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Flush sends what is buffered (and the head, if it was not sent yet).
func (w *respWriter) Flush() { _ = w.flush(true) }

func (w *respWriter) waitCredit() error {
	for {
		w.mu.Lock()
		ok := w.unacked < relay.FlowWindow
		w.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-w.creditSig:
		case <-w.ctx.Done():
			return w.ctx.Err()
		}
	}
}

// flush sends buffered bytes in chunk-size frames. more is false only for the last frame.
func (w *respWriter) flush(more bool) error {
	for {
		w.mu.Lock()
		if w.failed && more {
			w.mu.Unlock()
			return context.Canceled
		}
		n := w.buf.Len()
		if n > relay.BodyChunk {
			n = relay.BodyChunk
		}
		data := append([]byte(nil), w.buf.Next(n)...)
		remaining := w.buf.Len()
		first := !w.headSent
		if first && w.status == 0 {
			w.status = http.StatusOK
		}
		if !first && n == 0 && more {
			w.mu.Unlock()
			return nil
		}
		w.headSent = true
		status := w.status
		hdr := pickHeaders(w.hdr)
		w.total += int64(n)
		w.unacked += int64(n)
		w.mu.Unlock()

		if err := w.waitCredit(); err != nil {
			return err
		}
		last := !more && remaining == 0
		var f relay.Frame
		if first {
			f = relay.Frame{T: relay.TypeRes, ID: w.id, Status: status, Header: hdr, Body: relay.Enc(data), More: !last}
		} else {
			f = relay.Frame{T: relay.TypeChunk, ID: w.id, Body: relay.Enc(data), More: !last}
		}
		if err := w.send(f); err != nil {
			w.cancel()
			return err
		}
		if remaining == 0 {
			return nil
		}
	}
}

// finish ends the response: the last frame carries more=false. If nothing was written the
// response is an empty 200.
func (w *respWriter) finish() error {
	w.mu.Lock()
	if w.failed {
		w.mu.Unlock()
		return nil
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.mu.Unlock()
	return w.flush(false)
}

// pickHeaders passes the few response headers the cloud may forward.
func pickHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for _, k := range []string{"Content-Type", "Content-Disposition", "Retry-After"} {
		if v := h.Get(k); v != "" {
			out[lower(k)] = v
		}
	}
	return out
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}
