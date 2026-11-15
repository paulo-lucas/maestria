// Package audit is the append-only audit trail: logins, failed logins, logouts, the kill switch
// and every write. The database refuses UPDATE and DELETE on the table (triggers), and this
// package never issues them.
//
// Never pass secrets (passwords, codes, tokens) as detail.
package audit

import (
	"database/sql"
	"encoding/json"
	"sync"
	"time"
	"unicode/utf8"
)

const maxDetail = 1000

// Entry is one audit row.
type Entry struct {
	ID     int64  `json:"id"`
	TS     int64  `json:"ts"`
	Event  string `json:"event"`
	IP     string `json:"ip,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Log writes audit entries.
type Log struct {
	db  *sql.DB
	now func() time.Time

	mu       sync.Mutex
	throttle map[string]time.Time // key -> when it may act again
}

// New returns an audit log on db. now may be nil (time.Now).
func New(db *sql.DB, now func() time.Time) *Log {
	if now == nil {
		now = time.Now
	}
	return &Log{db: db, now: now, throttle: map[string]time.Time{}}
}

// Write appends an entry. detail may be nil, a string, or any JSON-encodable value.
func (l *Log) Write(event, ip string, detail any) error {
	var d sql.NullString
	switch v := detail.(type) {
	case nil:
	case string:
		d = sql.NullString{String: v, Valid: true}
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		d = sql.NullString{String: string(b), Valid: true}
	}
	if len(d.String) > maxDetail {
		cut := maxDetail
		for cut > 0 && !utf8.RuneStart(d.String[cut]) {
			cut--
		}
		d.String = d.String[:cut] + "…"
	}
	var ipv sql.NullString
	if ip != "" {
		if len(ip) > 64 {
			ip = ip[:64]
		}
		ipv = sql.NullString{String: ip, Valid: true}
	}
	_, err := l.db.Exec("INSERT INTO audit (ts, event, ip, detail) VALUES (?, ?, ?, ?)", l.now().UnixMilli(), event, ipv, d)
	return err
}

// WriteThrottled is like Write but at most once per interval per key, so floods of rejected
// requests cannot fill the table.
func (l *Log) WriteThrottled(key string, interval time.Duration, event, ip string, detail any) error {
	if !l.Allow(key, interval) {
		return nil
	}
	return l.Write(event, ip, detail)
}

// maxThrottleKeys bounds the throttle map. When it is full, entries whose interval has passed are
// dropped first, then the ones that would expire soonest; a flood of new keys (they can come from
// an attacker-chosen address) cannot make the map forget a key that is still being throttled
// while an older one is kept.
const maxThrottleKeys = 1000

// Allow reports whether key may act now, and if so starts its interval. It is the bookkeeping
// behind WriteThrottled, exposed so a caller can combine several keys (per address and global).
func (l *Log) Allow(key string, interval time.Duration) bool {
	t := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if until, seen := l.throttle[key]; seen && t.Before(until) {
		return false
	}
	if len(l.throttle) >= maxThrottleKeys {
		for k, until := range l.throttle {
			if !t.Before(until) {
				delete(l.throttle, k)
			}
		}
		for len(l.throttle) >= maxThrottleKeys {
			var oldest string
			var oldestUntil time.Time
			for k, until := range l.throttle {
				if oldest == "" || until.Before(oldestUntil) {
					oldest, oldestUntil = k, until
				}
			}
			delete(l.throttle, oldest)
		}
	}
	l.throttle[key] = t.Add(interval)
	return true
}

// Recent returns the newest entries first.
func (l *Log) Recent(limit int) ([]Entry, error) { return l.recent(limit, false) }

// RecentSignificant is Recent without the noise of refused requests (rejected tokens and origins,
// blocked and throttled sign-ins, refused relay requests and pairings). Anyone can cause those
// with a few requests, so a page that listed them with the rest could be flooded until a sign-in,
// an approval or a kill switch scrolled out of view. The refusals stay in the log and in Recent.
func (l *Log) RecentSignificant(limit int) ([]Entry, error) { return l.recent(limit, true) }

// noisyEvents are the refusal events RecentSignificant leaves out (besides every "*_reject").
const noisyEvents = `'login_blocked', 'login_blocked_killed', 'login_throttled', 'relay_refused', 'pair_refused', 'pair_bad_code', 'cli_confirm_refused'`

func (l *Log) recent(limit int, significant bool) ([]Entry, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	where := ""
	if significant {
		where = ` WHERE event NOT LIKE '%\_reject' ESCAPE '\' AND event NOT IN (` + noisyEvents + `)`
	}
	rows, err := l.db.Query("SELECT id, ts, event, COALESCE(ip, ''), COALESCE(detail, '') FROM audit"+where+" ORDER BY id DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.TS, &e.Event, &e.IP, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
