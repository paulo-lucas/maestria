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
	throttle map[string]time.Time
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
	t := l.now()
	l.mu.Lock()
	last, seen := l.throttle[key]
	if seen && t.Sub(last) < interval {
		l.mu.Unlock()
		return nil
	}
	if len(l.throttle) > 1000 {
		l.throttle = map[string]time.Time{}
	}
	l.throttle[key] = t
	l.mu.Unlock()
	return l.Write(event, ip, detail)
}

// Recent returns the newest entries first.
func (l *Log) Recent(limit int) ([]Entry, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := l.db.Query("SELECT id, ts, event, COALESCE(ip, ''), COALESCE(detail, '') FROM audit ORDER BY id DESC LIMIT ?", limit)
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
