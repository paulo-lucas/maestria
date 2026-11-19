// Package devices is the connector's side of device pairing and request verification: which
// browsers this machine approved, the session policy (absolute and idle limits), the nonce cache
// and the per-device rate limit. The connector is the authority: nothing here trusts the cloud.
// See docs/hosted-protocol.md, sections 6 and 7.
package devices

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/paulo-lucas/maestria/internal/audit"
	"github.com/paulo-lucas/maestria/internal/relay"
	"github.com/paulo-lucas/maestria/internal/termsafe"
)

// Policy is the connector's session policy for relayed requests. A zero Absolute or Idle is no
// limit.
type Policy struct {
	Enabled       bool
	Absolute      time.Duration
	Idle          time.Duration
	RatePerMinute int
}

// DefaultPolicy keeps an approved browser until it is revoked (no absolute or idle limit), at 120
// requests per minute. Revocation, the kill switch and the audit log still apply; a machine that
// wants approvals to end sets limits with "maestria devices limits".
func DefaultPolicy() Policy {
	return Policy{Enabled: true, Absolute: DefaultAbsoluteDays * 24 * time.Hour, Idle: DefaultIdleDays * 24 * time.Hour, RatePerMinute: 120}
}

// Default session lifetimes of an approved device, in days; 0 is no limit (the config defaults
// use the same).
const (
	DefaultAbsoluteDays = 0
	DefaultIdleDays     = 0
)

// MaxLimitDays is the longest limit that can be set, in days.
const MaxLimitDays = 3650

// expiry tells whether a device approved at approvedAt and last used at lastUsed (epoch ms) is
// past this policy at now: relay.ErrApprovalExpired, relay.ErrIdleExpired, or "".
func (p Policy) expiry(approvedAt, lastUsed, now int64) string {
	switch {
	case p.Absolute > 0 && now-approvedAt >= p.Absolute.Milliseconds():
		return relay.ErrApprovalExpired
	case p.Idle > 0 && now-lastUsed >= p.Idle.Milliseconds():
		return relay.ErrIdleExpired
	}
	return ""
}

// PairingTTL is how long a pairing waits for local approval.
const PairingTTL = 5 * time.Minute

// MaxPending is how many pairings may wait at once.
const MaxPending = 5

// The attempt budget of this connector. A pairing is a request from the cloud to put a key on
// this machine's approved list, so the number of tries is limited here, whatever the cloud does:
// a few offers per window and per day, a long pause after a failed reveal, and a few wrong codes
// typed at `devices approve`.
const (
	PairWindow         = 10 * time.Minute
	MaxOffersPerWindow = 5
	MaxOffersPerDay    = 20
	PairLockout        = time.Hour // no new offer after a failed reveal
	MaxBadCodes        = 5         // wrong codes per PairWindow
)

const touchEvery = 30 * time.Second

// ThrottledError says a pairing call was refused by the attempt budget and when to retry.
type ThrottledError struct {
	Reason string
	Retry  time.Duration
}

func (e *ThrottledError) Error() string {
	mins := int((e.Retry + time.Minute - 1) / time.Minute)
	if mins < 1 {
		mins = 1
	}
	return fmt.Sprintf("pairing is paused (%s); try again in about %d min", e.Reason, mins)
}

// Errors returned by the pairing and approval calls.
var (
	ErrTooManyPending = errors.New("too many pairings are waiting; approve or reject one first")
	ErrNoPairing      = errors.New("no pairing is waiting with that code")
	ErrAmbiguous      = errors.New("more than one pairing matches that code; reject the extra ones first")
	ErrBadPairing     = errors.New("invalid pairing")
	ErrUnknownPair    = errors.New("unknown pairing")
	ErrPairState      = errors.New("the pairing is not waiting for a key")
	ErrKeyRevoked     = errors.New("this browser key was revoked; the browser must create a new one")
	ErrNoDevice       = errors.New("no such device")
)

// Device is a browser approved on this machine.
type Device struct {
	ID         string
	SPKI       string
	Label      string
	State      string // active | revoked | expired
	ApprovedAt int64
	LastUsedAt int64
	RevokedAt  int64
}

// Fingerprint is the short form of the device id shown next to a device ("a1b2 c3d4 e5f6 0718").
func (d Device) Fingerprint() string { return relay.Fingerprint(d.ID) }

// Info is the display summary reported to the cloud.
func (d Device) Info() relay.DeviceInfo {
	return relay.DeviceInfo{ID: d.ID, Label: d.Label, State: d.State, ApprovedAt: d.ApprovedAt, LastUsedAt: d.LastUsedAt, PublicKey: d.SPKI}
}

// Pairing is a browser waiting for approval.
type Pairing struct {
	ID          string
	Label       string
	UA          string
	State       string
	Code        string // empty until the browser revealed its key
	DeviceID    string // empty until the browser revealed its key
	Fingerprint string // short form of DeviceID, the same one the browser shows
	CreatedAt   int64
	ExpiresAt   int64
}

// Change is a pairing whose state moved since it was last reported to the cloud.
type Change struct {
	Pair  string
	State string
	Code  string
}

// Manager keeps devices and pairings in the connector database.
type Manager struct {
	db     *sql.DB
	policy func() Policy
	now    func() time.Time
	audit  *audit.Log

	mu     sync.Mutex
	nonces map[string]int64 // deviceID|nonce -> expiry (epoch ms)
	rate   map[string]*window
	touch  map[string]int64
}

type window struct {
	start int64
	count int
}

// New returns a manager. policy and now may be nil.
func New(db *sql.DB, policy func() Policy, now func() time.Time) *Manager {
	if policy == nil {
		policy = DefaultPolicy
	}
	if now == nil {
		now = time.Now
	}
	return &Manager{db: db, policy: policy, now: now, nonces: map[string]int64{}, rate: map[string]*window{}, touch: map[string]int64{}}
}

// Policy returns the current policy.
func (m *Manager) Policy() Policy { return m.policy() }

// SetAudit makes the manager record every pairing offer, refusal, failure and approval in the
// local audit log.
func (m *Manager) SetAudit(l *audit.Log) { m.audit = l }

func (m *Manager) auditWrite(event string, detail any) {
	if m.audit != nil {
		_ = m.audit.Write(event, "", detail)
	}
}

// record adds one event to the attempt budget (and forgets events older than two days).
func (m *Manager) record(kind string) {
	now := m.now().UnixMilli()
	_, _ = m.db.Exec("INSERT INTO pair_events (at, kind) VALUES (?, ?)", now, kind)
	_, _ = m.db.Exec("DELETE FROM pair_events WHERE at < ?", now-int64(48*time.Hour/time.Millisecond))
}

// since counts the events of a kind newer than d, and returns the time of the oldest of them.
func (m *Manager) since(kind string, d time.Duration) (n int, oldest int64) {
	from := m.now().Add(-d).UnixMilli()
	var o sql.NullInt64
	_ = m.db.QueryRow("SELECT COUNT(*), MIN(at) FROM pair_events WHERE kind = ? AND at > ?", kind, from).Scan(&n, &o)
	return n, o.Int64
}

// offerThrottle says whether a new pairing may be offered now.
func (m *Manager) offerThrottle() *ThrottledError {
	now := m.now().UnixMilli()
	if n, _ := m.since("mismatch", PairLockout); n > 0 {
		var last sql.NullInt64
		_ = m.db.QueryRow("SELECT MAX(at) FROM pair_events WHERE kind = 'mismatch'").Scan(&last)
		return &ThrottledError{Reason: "a browser key did not match its commitment", Retry: time.Duration(last.Int64+PairLockout.Milliseconds()-now) * time.Millisecond}
	}
	if n, oldest := m.since("offer", PairWindow); n >= MaxOffersPerWindow {
		return &ThrottledError{Reason: "too many pairings were offered", Retry: time.Duration(oldest+PairWindow.Milliseconds()-now) * time.Millisecond}
	}
	if n, oldest := m.since("offer", 24*time.Hour); n >= MaxOffersPerDay {
		return &ThrottledError{Reason: "too many pairings were offered today", Retry: time.Duration(oldest+(24*time.Hour).Milliseconds()-now) * time.Millisecond}
	}
	return nil
}

// Budget is how much of the pairing attempt budget is used, for `devices pending`.
type Budget struct {
	Offers    int             // offers accepted in the last PairWindow
	MaxOffers int             // MaxOffersPerWindow
	Paused    *ThrottledError // non-nil while new offers are refused
}

// Budget reports the budget now.
func (m *Manager) Budget() Budget {
	n, _ := m.since("offer", PairWindow)
	return Budget{Offers: n, MaxOffers: MaxOffersPerWindow, Paused: m.offerThrottle()}
}

// codeThrottle says whether `devices approve` may look at a code now (after too many wrong ones
// it does not even answer, so a wrong guess and a right one look the same).
func (m *Manager) codeThrottle() *ThrottledError {
	if n, oldest := m.since("bad_code", PairWindow); n >= MaxBadCodes {
		return &ThrottledError{Reason: "too many wrong codes", Retry: time.Duration(oldest+PairWindow.Milliseconds()-m.now().UnixMilli()) * time.Millisecond}
	}
	return nil
}

var pairIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// PairingKeep is how long a finished pairing (approved, rejected, expired, mismatched) stays in
// the table after its expiry. Past that it has been reported to the cloud long ago and only slows
// every offer down (a flood of offers once grew the table to 133,000 rows).
const PairingKeep = time.Hour

func (m *Manager) sweep() error {
	now := m.now().UnixMilli()
	if _, err := m.db.Exec(`UPDATE device_pairings SET state = 'expired' WHERE state IN ('pending','revealed') AND expires_at <= ?`, now); err != nil {
		return err
	}
	_, err := m.db.Exec(`DELETE FROM device_pairings WHERE state NOT IN ('pending','revealed') AND expires_at < ?`, now-PairingKeep.Milliseconds())
	return err
}

// Offer records a pairing offered by the cloud and returns the connector's random value r
// (b64u). Offering the same pairing again returns the same r.
func (m *Manager) Offer(pair, commit, label, ua string) (string, error) {
	if !pairIDRE.MatchString(pair) {
		return "", ErrBadPairing
	}
	if c, err := relay.Dec(commit); err != nil || len(c) != 32 {
		return "", ErrBadPairing
	}
	if err := m.sweep(); err != nil {
		return "", err
	}
	var existing string
	if err := m.db.QueryRow("SELECT r FROM device_pairings WHERE pair_id = ?", pair).Scan(&existing); err == nil {
		return existing, nil
	}
	label = termsafe.Clean(label, 80)
	if label == "" {
		label = "Unnamed browser"
	}
	ua = termsafe.Clean(ua, 200)
	if te := m.offerThrottle(); te != nil {
		if m.audit != nil {
			// At most one entry per minute: a hostile cloud can offer as fast as it likes.
			_ = m.audit.WriteThrottled("pair_refused", time.Minute, "pair_refused", "", map[string]any{"reason": te.Reason, "label": label})
		}
		return "", te
	}
	var live int
	if err := m.db.QueryRow("SELECT COUNT(*) FROM device_pairings WHERE state IN ('pending','revealed')").Scan(&live); err != nil {
		return "", err
	}
	if live >= MaxPending {
		return "", ErrTooManyPending
	}
	rb := make([]byte, 16)
	if _, err := rand.Read(rb); err != nil {
		return "", err
	}
	r := relay.Enc(rb)
	now := m.now()
	_, err := m.db.Exec(`INSERT INTO device_pairings (pair_id, commit_hash, label, ua, r, state, created_at, expires_at) VALUES (?, ?, ?, ?, ?, 'pending', ?, ?)`,
		pair, commit, label, ua, r, now.UnixMilli(), now.Add(PairingTTL).UnixMilli())
	if err != nil {
		return "", err
	}
	m.record("offer")
	m.auditWrite("pair_offered", map[string]any{"label": label, "ua": ua})
	return r, nil
}

// Reveal takes the browser's key and nonce, checks them against the commitment and the device id
// rules, and computes the code the user will compare. A failed check ends the pairing
// (state mismatch) and returns ErrBadPairing.
func (m *Manager) Reveal(pair, spkiB64, nonceB64 string) (string, error) {
	if err := m.sweep(); err != nil {
		return "", err
	}
	var commit, r, state string
	err := m.db.QueryRow("SELECT commit_hash, r, state FROM device_pairings WHERE pair_id = ?", pair).Scan(&commit, &r, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrUnknownPair
	}
	if err != nil {
		return "", err
	}
	if state != relay.PairPending {
		return "", ErrPairState
	}
	fail := func() (string, error) {
		_, _ = m.db.Exec("UPDATE device_pairings SET state = 'mismatch' WHERE pair_id = ?", pair)
		// A failed reveal is the signature of a guessing attempt: it counts against the budget
		// and pauses new offers for PairLockout.
		m.record("mismatch")
		m.auditWrite("pair_mismatch", map[string]any{"pair": pair})
		return "", ErrBadPairing
	}
	spki, err1 := relay.Dec(spkiB64)
	nonce, err2 := relay.Dec(nonceB64)
	if err1 != nil || err2 != nil || len(nonce) < 8 || len(nonce) > 64 {
		return fail()
	}
	if _, err := relay.ParseSPKI(spki); err != nil {
		return fail()
	}
	if relay.PairCommit(spki, nonce) != commit {
		return fail()
	}
	rb, err := relay.Dec(r)
	if err != nil {
		return fail()
	}
	code := relay.PairCode(spki, nonce, rb)
	_, err = m.db.Exec(`UPDATE device_pairings SET spki = ?, nonce = ?, code = ?, state = 'revealed' WHERE pair_id = ?`, spkiB64, nonceB64, code, pair)
	return code, err
}

// Pending lists the pairings still waiting for a decision, oldest first.
func (m *Manager) Pending() ([]Pairing, error) {
	if err := m.sweep(); err != nil {
		return nil, err
	}
	rows, err := m.db.Query(`SELECT pair_id, label, ua, state, COALESCE(code, ''), COALESCE(spki, ''), created_at, expires_at FROM device_pairings
		WHERE state IN ('pending','revealed') ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pairing
	for rows.Next() {
		var p Pairing
		var spki string
		if err := rows.Scan(&p.ID, &p.Label, &p.UA, &p.State, &p.Code, &spki, &p.CreatedAt, &p.ExpiresAt); err != nil {
			return nil, err
		}
		if raw, err := relay.Dec(spki); err == nil && len(raw) > 0 {
			p.DeviceID = relay.DeviceID(raw)
			p.Fingerprint = relay.Fingerprint(p.DeviceID)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Lookup is the pairing a typed code points to, for the confirmation shown before Approve. A
// wrong code counts against the budget; after MaxBadCodes the lookup is refused outright.
func (m *Manager) Lookup(code string) (*Pairing, error) {
	id, err := m.byCode(code, true)
	if err != nil {
		return nil, err
	}
	list, err := m.Pending()
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].ID == id {
			return &list[i], nil
		}
	}
	return nil, ErrNoPairing
}

// byCode finds the one revealed pairing with that code. With count set, a code that matches
// nothing is a failed attempt: it is recorded, audited and, past the budget, refused before it is
// even looked up.
func (m *Manager) byCode(code string, count bool) (string, error) {
	if count {
		if te := m.codeThrottle(); te != nil {
			return "", te
		}
	}
	code = relay.NormalizePairCode(code)
	if err := m.sweep(); err != nil {
		return "", err
	}
	if len(code) != relay.PairCodeDigits {
		if count {
			m.badCode()
		}
		return "", ErrNoPairing
	}
	rows, err := m.db.Query("SELECT pair_id FROM device_pairings WHERE state = 'revealed' AND code = ?", code)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	switch len(ids) {
	case 0:
		if count {
			m.badCode()
		}
		return "", ErrNoPairing
	case 1:
		return ids[0], nil
	}
	return "", ErrAmbiguous
}

func (m *Manager) badCode() {
	m.record("bad_code")
	m.auditWrite("pair_bad_code", nil)
}

// Approve is `maestria devices approve <code>`: it approves the revealed pairing with that code
// and stores the device. A key that was revoked is refused.
func (m *Manager) Approve(code string) (*Device, error) {
	id, err := m.byCode(code, true)
	if err != nil {
		return nil, err
	}
	var spki, label string
	if err := m.db.QueryRow("SELECT spki, label FROM device_pairings WHERE pair_id = ?", id).Scan(&spki, &label); err != nil {
		return nil, err
	}
	raw, err := relay.Dec(spki)
	if err != nil {
		return nil, ErrBadPairing
	}
	devID := relay.DeviceID(raw)
	now := m.now().UnixMilli()
	tx, err := m.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var state string
	switch err := tx.QueryRow("SELECT state FROM devices WHERE id = ?", devID).Scan(&state); {
	case err == nil && state == "revoked":
		return nil, ErrKeyRevoked
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}
	if _, err := tx.Exec(`INSERT INTO devices (id, spki, label, state, approved_at, last_used_at) VALUES (?, ?, ?, 'active', ?, ?)
		ON CONFLICT(id) DO UPDATE SET label = excluded.label, state = 'active', approved_at = excluded.approved_at, last_used_at = excluded.last_used_at`,
		devID, spki, label, now, now); err != nil {
		return nil, err
	}
	if _, err := tx.Exec("UPDATE device_pairings SET state = 'approved' WHERE pair_id = ?", id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	m.auditWrite("device_approved", map[string]any{"device": devID, "fingerprint": relay.Fingerprint(devID), "label": label})
	return m.Get(devID)
}

// Reject is `maestria devices reject <code>`. It also accepts a pairing that has not revealed its
// key yet when given its id.
func (m *Manager) Reject(codeOrID string) error {
	id, err := m.byCode(codeOrID, false)
	if err != nil {
		if !pairIDRE.MatchString(codeOrID) {
			return err
		}
		id = codeOrID
	}
	res, err := m.db.Exec("UPDATE device_pairings SET state = 'rejected' WHERE pair_id = ? AND state IN ('pending','revealed')", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoPairing
	}
	return nil
}

// Changes returns pairings whose state differs from what was last reported, and marks them
// reported. One consumer only (the connect loop).
func (m *Manager) Changes() ([]Change, error) {
	if err := m.sweep(); err != nil {
		return nil, err
	}
	rows, err := m.db.Query(`SELECT pair_id, state, COALESCE(code, '') FROM device_pairings WHERE state != reported_state`)
	if err != nil {
		return nil, err
	}
	var out []Change
	for rows.Next() {
		var c Change
		if err := rows.Scan(&c.Pair, &c.State, &c.Code); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, c := range out {
		// Mark exactly the state that is being reported: if the local CLI approved the pairing
		// since the read above, the new state stays unreported and goes out on the next call.
		if _, err := m.db.Exec("UPDATE device_pairings SET reported_state = ? WHERE pair_id = ? AND state = ?", c.State, c.Pair, c.State); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// MarkUnreported makes every non-terminal pairing report again (after a reconnect).
func (m *Manager) MarkUnreported() error {
	_, err := m.db.Exec("UPDATE device_pairings SET reported_state = '' WHERE state IN ('pending','revealed','approved')")
	return err
}

// Get returns one device.
func (m *Manager) Get(id string) (*Device, error) {
	var d Device
	var revoked sql.NullInt64
	err := m.db.QueryRow(`SELECT id, spki, label, state, approved_at, last_used_at, revoked_at FROM devices WHERE id = ?`, id).
		Scan(&d.ID, &d.SPKI, &d.Label, &d.State, &d.ApprovedAt, &d.LastUsedAt, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoDevice
	}
	if err != nil {
		return nil, err
	}
	d.RevokedAt = revoked.Int64
	return &d, nil
}

// List returns every device, newest approval first. Expiry is evaluated against the policy so the
// list never shows an expired device as active.
func (m *Manager) List() ([]Device, error) {
	rows, err := m.db.Query(`SELECT id, spki, label, state, approved_at, last_used_at, revoked_at FROM devices ORDER BY approved_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	p := m.policy()
	now := m.now().UnixMilli()
	for rows.Next() {
		var d Device
		var revoked sql.NullInt64
		if err := rows.Scan(&d.ID, &d.SPKI, &d.Label, &d.State, &d.ApprovedAt, &d.LastUsedAt, &revoked); err != nil {
			return nil, err
		}
		d.RevokedAt = revoked.Int64
		if d.State == "active" && p.expiry(d.ApprovedAt, d.LastUsedAt, now) != "" {
			d.State = "expired"
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Infos is List as display summaries.
func (m *Manager) Infos() ([]relay.DeviceInfo, error) {
	list, err := m.List()
	if err != nil {
		return nil, err
	}
	out := make([]relay.DeviceInfo, len(list))
	for i, d := range list {
		out[i] = d.Info()
	}
	return out, nil
}

// Revoke ends a device for good: its key is refused from now on.
func (m *Manager) Revoke(id string) error {
	res, err := m.db.Exec("UPDATE devices SET state = 'revoked', revoked_at = ? WHERE id = ? AND state != 'revoked'", m.now().UnixMilli(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, gerr := m.Get(id); gerr != nil {
			return ErrNoDevice
		}
	}
	return nil
}

// RevokeAll revokes every device (after a password reset in the cloud) and returns how many.
func (m *Manager) RevokeAll() (int, error) {
	res, err := m.db.Exec("UPDATE devices SET state = 'revoked', revoked_at = ? WHERE state != 'revoked'", m.now().UnixMilli())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
