package web

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

type sseEvent struct {
	Type string `json:"type"`
	TS   int64  `json:"ts"`
}

type sseBroker struct {
	mu   sync.Mutex
	subs map[chan sseEvent]struct{}
}

func newSSEBroker() *sseBroker {
	return &sseBroker{subs: map[chan sseEvent]struct{}{}}
}

func (b *sseBroker) subscribe() chan sseEvent {
	ch := make(chan sseEvent, 16)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *sseBroker) unsubscribe(ch chan sseEvent) {
	b.mu.Lock()
	if _, ok := b.subs[ch]; ok {
		delete(b.subs, ch)
		close(ch)
	}
	b.mu.Unlock()
}

func (b *sseBroker) publish(typ string, now time.Time) {
	ev := sseEvent{Type: typ, TS: now.UnixMilli()}
	b.mu.Lock()
	for ch := range b.subs {
		select {
		case ch <- ev:
		default:
			// Drop duplicate bursts if a subscriber is behind; next event or heartbeat catches up.
		}
	}
	b.mu.Unlock()
}

func (s *Server) notifyStream(typ string) {
	if s.broker != nil {
		s.broker.publish(typ, s.Now())
	}
}

func (s *Server) apiStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		deny(w, http.StatusInternalServerError, "Streaming unsupported")
		return
	}

	// Disable the server's per-request write timeout for this long-lived SSE stream.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-store")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	writeEvent := func(name string, payload any) bool {
		b, err := json.Marshal(payload)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	if !writeEvent("ready", map[string]any{"ts": s.Now().UnixMilli()}) {
		return
	}

	ch := s.broker.subscribe()
	defer s.broker.unsubscribe(ch)

	pollInterval := s.ssePollInterval
	if pollInterval <= 0 {
		pollInterval = 800 * time.Millisecond
	}
	pollTicker := time.NewTicker(pollInterval)
	defer pollTicker.Stop()

	pingTicker := time.NewTicker(20 * time.Second)
	defer pingTicker.Stop()

	lastSig := dbChangeSignature(s.DB)

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			lastSig = dbChangeSignature(s.DB)
			if !writeEvent("update", ev) {
				return
			}
		case <-pollTicker.C:
			// Detect writes made directly to SQLite by CLI processes (post, say, standby).
			sig := dbChangeSignature(s.DB)
			if sig != lastSig {
				lastSig = sig
				if !writeEvent("update", sseEvent{Type: "sync", TS: s.Now().UnixMilli()}) {
					return
				}
			}
		case <-pingTicker.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

type dbSignature struct {
	inboxCount   int64
	inboxMaxID   int64
	inboxOpen    int64
	inboxPostp   int64
	chatMaxID    int64
	eventsMaxID  int64
	floorsCount  int64
	floorsMaxID  int64
	floorsRevSum int64
	standbyRaw   string
}

func dbChangeSignature(db *sql.DB) dbSignature {
	var sig dbSignature
	_ = db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(id), 0),
		COALESCE(SUM(CASE WHEN status = 'open' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN postponed_until IS NOT NULL THEN 1 ELSE 0 END), 0)
		FROM inbox`).Scan(&sig.inboxCount, &sig.inboxMaxID, &sig.inboxOpen, &sig.inboxPostp)
	_ = db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM chat_messages`).Scan(&sig.chatMaxID)
	_ = db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM events`).Scan(&sig.eventsMaxID)
	_ = db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(id), 0), COALESCE(SUM(rev), 0) FROM floors`).Scan(&sig.floorsCount, &sig.floorsMaxID, &sig.floorsRevSum)
	_ = db.QueryRow(`SELECT COALESCE((SELECT value FROM kv WHERE key = 'standby.state_v1'), '')`).Scan(&sig.standbyRaw)
	return sig
}
