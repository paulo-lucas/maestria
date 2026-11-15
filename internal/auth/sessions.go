package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"
)

// Session is a live browser session.
type Session struct {
	ID         string // sha256 of the cookie token
	CSRFToken  string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
}

// touchInterval limits how often last_seen_at is written.
const touchInterval = 30 * time.Second

// PendingTTL is how long a password-verified login waits for its second factor.
const PendingTTL = 5 * time.Minute

// RandomToken returns 32 random bytes, base64url without padding (43 chars).
func RandomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// HashToken returns the hex sha256 of a token. Only this is stored.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// SessionStore keeps server-side sessions: an opaque random token in the cookie, its hash in SQLite.
type SessionStore struct {
	db       *sql.DB
	idle     time.Duration
	absolute time.Duration
	now      func() time.Time
}

// NewSessionStore returns a store with the given idle and absolute limits. now may be nil.
func NewSessionStore(db *sql.DB, idle, absolute time.Duration, now func() time.Time) *SessionStore {
	if now == nil {
		now = time.Now
	}
	return &SessionStore{db: db, idle: idle, absolute: absolute, now: now}
}

// Create starts a session and returns the cookie token.
func (s *SessionStore) Create(ip, userAgent string) (string, *Session, error) {
	token := RandomToken()
	t := s.now()
	sess := &Session{ID: HashToken(token), CSRFToken: RandomToken(), CreatedAt: t, LastSeenAt: t, ExpiresAt: t.Add(s.absolute)}
	_, err := s.db.Exec(`INSERT INTO sessions (id, csrf_token, created_at, last_seen_at, expires_at, ip, user_agent) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sess.ID, sess.CSRFToken, t.UnixMilli(), t.UnixMilli(), sess.ExpiresAt.UnixMilli(), clip(ip, 64), clip(userAgent, 200))
	if err != nil {
		return "", nil, err
	}
	return token, sess, nil
}

// Validate returns the live session for a cookie token, enforcing idle and absolute expiry.
// Expired sessions are deleted. A nil session with a nil error means "not logged in".
func (s *SessionStore) Validate(token string) (*Session, error) {
	if token == "" || len(token) > 128 {
		return nil, nil
	}
	id := HashToken(token)
	var csrf string
	var created, lastSeen, expires int64
	err := s.db.QueryRow("SELECT csrf_token, created_at, last_seen_at, expires_at FROM sessions WHERE id = ?", id).
		Scan(&csrf, &created, &lastSeen, &expires)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t := s.now()
	ms := t.UnixMilli()
	if ms >= expires || ms-lastSeen >= s.idle.Milliseconds() {
		_, err := s.db.Exec("DELETE FROM sessions WHERE id = ?", id)
		return nil, err
	}
	if ms-lastSeen >= touchInterval.Milliseconds() {
		if _, err := s.db.Exec("UPDATE sessions SET last_seen_at = ? WHERE id = ?", ms, id); err != nil {
			return nil, err
		}
		lastSeen = ms
	}
	return &Session{
		ID: id, CSRFToken: csrf,
		CreatedAt: time.UnixMilli(created), LastSeenAt: time.UnixMilli(lastSeen), ExpiresAt: time.UnixMilli(expires),
	}, nil
}

// Destroy ends one session.
func (s *SessionStore) Destroy(token string) error {
	if token == "" {
		return nil
	}
	_, err := s.db.Exec("DELETE FROM sessions WHERE id = ?", HashToken(token))
	return err
}

// DestroyAll ends every session and every pending login ("log out everywhere", kill switch).
// It returns how many sessions were removed.
func (s *SessionStore) DestroyAll() (int, error) {
	res, err := s.db.Exec("DELETE FROM sessions")
	if err != nil {
		return 0, err
	}
	if _, err := s.db.Exec("DELETE FROM pending_logins"); err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// PurgeExpired removes expired sessions and pending logins.
func (s *SessionStore) PurgeExpired() error {
	ms := s.now().UnixMilli()
	if _, err := s.db.Exec("DELETE FROM sessions WHERE expires_at <= ? OR last_seen_at <= ?", ms, ms-s.idle.Milliseconds()); err != nil {
		return err
	}
	_, err := s.db.Exec("DELETE FROM pending_logins WHERE expires_at <= ?", ms)
	return err
}

// Count returns the number of stored sessions.
func (s *SessionStore) Count() (int, error) {
	var n int
	err := s.db.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&n)
	return n, err
}

// Bounds of the pending-login table. A pending login belongs to the browser that holds its random
// cookie and is never replaced or evicted by another client's attempt (before, one client's first
// step wiped the pending login of every client that shared its address, which is every visitor
// behind a tunnel that does not report addresses). The table is bounded by refusing new logins
// instead: per address, and in total.
const (
	MaxPendingPerIP  = 200
	MaxPendingTotal  = 5000
	pendingRetryHint = 30 * time.Second
)

// ErrPendingFull means the table of pending logins has no room for this client now.
var ErrPendingFull = errors.New("auth: too many logins in progress")

// CreatePending records the first login step and returns its cookie token. The record keeps, on
// the server only, whether the password was right and which IP did the first step.
func (s *SessionStore) CreatePending(ip string, passwordOK bool) (string, error) {
	token := RandomToken()
	t := s.now()
	ip = clip(ip, 64)
	ok := 0
	if passwordOK {
		ok = 1
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM pending_logins WHERE expires_at <= ?", t.UnixMilli()); err != nil {
		return "", err
	}
	var perIP, total int
	if err := tx.QueryRow("SELECT COUNT(*), COALESCE(SUM(ip = ?), 0) FROM pending_logins", ip).Scan(&total, &perIP); err != nil {
		return "", err
	}
	if perIP >= MaxPendingPerIP || total >= MaxPendingTotal {
		return "", ErrPendingFull
	}
	if _, err := tx.Exec("INSERT INTO pending_logins (id, created_at, expires_at, ip, password_ok) VALUES (?, ?, ?, ?, ?)",
		HashToken(token), t.UnixMilli(), t.Add(PendingTTL).UnixMilli(), ip, ok); err != nil {
		return "", err
	}
	return token, tx.Commit()
}

// Pending is a live first-step login.
type Pending struct {
	PasswordOK bool
}

// PendingFor returns the live pending login for a cookie token, only when ip is the IP that did
// the first step (a stolen pending cookie is useless from another address). Nil means none.
func (s *SessionStore) PendingFor(token, ip string) (*Pending, error) {
	if token == "" || len(token) > 128 {
		return nil, nil
	}
	var expires int64
	var rowIP string
	var ok int
	err := s.db.QueryRow("SELECT expires_at, ip, password_ok FROM pending_logins WHERE id = ?", HashToken(token)).Scan(&expires, &rowIP, &ok)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if s.now().UnixMilli() >= expires || rowIP != clip(ip, 64) {
		return nil, nil
	}
	return &Pending{PasswordOK: ok == 1}, nil
}

// ConsumePending deletes a pending login, reporting whether this call removed it. Single use.
func (s *SessionStore) ConsumePending(token string) (bool, error) {
	if token == "" || len(token) > 128 {
		return false, nil
	}
	res, err := s.db.Exec("DELETE FROM pending_logins WHERE id = ?", HashToken(token))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
