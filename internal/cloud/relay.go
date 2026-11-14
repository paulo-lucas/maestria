package cloud

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/maestria-dev/maestria/internal/relay"
)

// The headers the cloud forwards to a connector: the five signature headers and three plain ones.
// Everything else (cookies above all) stays here.
var forwardHeaders = []string{
	relay.HeaderDevice, relay.HeaderTS, relay.HeaderNonce, relay.HeaderBody, relay.HeaderSig,
	"Content-Type", "Accept", "X-Requested-With",
}

// safeContentType lets a few media types through and turns anything else into a download.
func safeContentType(ct string) string {
	base := strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
	switch base {
	case "application/json", "text/html", "text/event-stream", "text/plain", "text/css",
		"image/png", "image/jpeg", "image/gif", "image/webp", "image/svg+xml", "image/x-icon", "image/avif":
		return ct
	}
	return "application/octet-stream"
}

func (s *Server) relayError(w http.ResponseWriter, code string) {
	writeJSON(w, relay.StatusFor(code), relay.ErrorBody{Error: code})
}

// relayHandler is /c/<connectorId>/<path>: it wraps the browser's signed request in an envelope,
// sends it over the connector's socket and streams the answer back. It adds nothing, signs
// nothing, and never looks at or keeps the body.
func (s *Server) relayHandler(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	id := r.PathValue("id")
	cn, err := s.cons.Owned(sess.User.ID, id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if cn == nil {
		writeJSON(w, http.StatusNotFound, relay.ErrorBody{Error: "not_found"})
		return
	}
	if !s.rate.Allow("relay:"+sess.User.ID, 1800, time.Minute) {
		writeJSON(w, http.StatusTooManyRequests, relay.ErrorBody{Error: relay.ErrRateLimited})
		return
	}

	// The path exactly as the browser sent (and signed) it, without the /c/<id> prefix.
	target := strings.TrimPrefix(r.RequestURI, "/c/"+id)
	path, query, _ := strings.Cut(target, "?")
	if relay.ValidatePath(path) != nil {
		s.relayError(w, relay.ErrBadRequest)
		return
	}

	hdr := map[string]string{}
	for _, k := range forwardHeaders {
		if v := r.Header.Get(k); v != "" {
			hdr[strings.ToLower(k)] = clip(v, 400)
		}
	}
	var body []byte
	if r.Method == http.MethodPost {
		b, err := readAll(r, relay.MaxRequestBody)
		if err != nil {
			s.relayError(w, relay.ErrTooLarge)
			return
		}
		body = b
	}

	call, err := s.hub.Dispatch(sess.User.ID, id, RelayRequest{Method: r.Method, Path: path, Query: query, Header: hdr, Body: body})
	switch {
	case errors.Is(err, ErrOffline):
		s.relayError(w, relay.ErrConnectorOffline)
		return
	case errors.Is(err, ErrBusy):
		s.relayError(w, relay.ErrConnectorBusy)
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	defer call.Close()

	ctx := r.Context()
	stream := relay.IsStream(r.Method, path)
	if stream {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	} else {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, relay.RequestTimeout)
		defer cancel()
	}

	head, errF := call.Head(ctx)
	if errF != nil {
		s.relayError(w, errF.Code)
		return
	}
	status := head.Status
	if status < 200 || status > 299 && status < 400 || status > 599 {
		if status >= 300 && status < 400 {
			s.relayError(w, relay.ErrInternal)
			return
		}
		status = http.StatusBadGateway
	}
	h := w.Header()
	h.Set("Content-Type", safeContentType(head.Header["content-type"]))
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	// A relayed body can never run as a page of this origin.
	h.Set("Content-Security-Policy", "sandbox")
	if cd := head.Header["content-disposition"]; strings.HasPrefix(strings.ToLower(cd), "attachment") {
		h.Set("Content-Disposition", clip(cd, 200))
	}
	if ra := head.Header["retry-after"]; ra != "" {
		h.Set("Retry-After", clip(ra, 20))
	}
	if stream {
		h.Set("X-Accel-Buffering", "no")
	}
	w.WriteHeader(status)
	flusher, _ := w.(http.Flusher)

	for {
		data, more, err := call.Next(ctx)
		if err != nil {
			return // the response was cut short; the browser sees a truncated body
		}
		if len(data) > 0 {
			if _, err := w.Write(data); err != nil {
				return
			}
			if stream && flusher != nil {
				flusher.Flush()
			}
			call.Ack(len(data))
		}
		if !more {
			return
		}
	}
}

func readAll(r *http.Request, max int) ([]byte, error) {
	var buf []byte
	tmp := make([]byte, 4096)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if len(buf) > max {
			return nil, errors.New("too large")
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return buf, nil
			}
			return nil, err
		}
	}
}
