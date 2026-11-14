// Package devices is the connector's side of device pairing and request verification: which
// browsers this machine approved, the session policy (absolute and idle limits), the nonce cache
// and the per-device rate limit. The connector is the authority: nothing here trusts the cloud.
// See docs/hosted-protocol.md, sections 6 and 7.
package devices

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"regexp"
	"sync"
	"time"

	"github.com/maestria-dev/maestria/internal/relay"
)

// Policy is the connector's session policy for relayed requests.
type Policy struct {
	Enabled       bool
	Absolute      time.Duration
	Idle          time.Duration
	RatePerMinute int
}

// DefaultPolicy is 30 days absolute, 7 days idle, 120 requests per minute.
func DefaultPolicy() Policy {
	return Policy{Enabled: true, Absolute: 30 * 24 * time.Hour, Idle: 7 * 24 * time.Hour, RatePerMinute: 120}
}

// PairingTTL is how long a pairing waits for local approval.
const PairingTTL = 5 * time.Minute

// MaxPending is how many pairings may wait at once.
const MaxPending = 5

const touchEvery = 30 * time.Second

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

// Info is the display summary reported to the cloud.
func (d Device) Info() relay.DeviceInfo {
	return relay.DeviceInfo{ID: d.ID, Label: d.Label, State: d.State, ApprovedAt: d.ApprovedAt, LastUsedAt: d.LastUsedAt, PublicKey: d.SPKI}
}

// Pairing is a browser waiting for approval.
type Pairing struct {
	ID        string
	Label     string
	UA        string
	State     string
	Code      string // empty until the browser revealed its key
	CreatedAt int64
	ExpiresAt int64
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

var pairIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (m *Manager) sweep() error {
	_, err := m.db.Exec(`UPDATE device_pairings SET state = 'expired' WHERE state IN ('pending','revealed') AND expires_at <= ?`, m.now().UnixMilli())
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
		pair, commit, clip(label, 80), clip(ua, 200), r, now.UnixMilli(), now.Add(PairingTTL).UnixMilli())
	if err != nil {
		return "", err
	}
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
	rows, err := m.db.Query(`SELECT pair_id, label, ua, state, COALESCE(code, ''), created_at, expires_at FROM device_pairings
		WHERE state IN ('pending','revealed') ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pairing
	for rows.Next() {
		var p Pairing
		if err := rows.Scan(&p.ID, &p.Label, &p.UA, &p.State, &p.Code, &p.CreatedAt, &p.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (m *Manager) byCode(code string) (string, error) {
	code = relay.NormalizePairCode(code)
	if len(code) != 6 {
		return "", ErrNoPairing
	}
	if err := m.sweep(); err != nil {
		return "", err
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
		return "", ErrNoPairing
	case 1:
		return ids[0], nil
	}
	return "", ErrAmbiguous
}

// Approve is `maestria devices approve <code>`: it approves the revealed pairing with that code
// and stores the device. A key that was revoked is refused.
func (m *Manager) Approve(code string) (*Device, error) {
	id, err := m.byCode(code)
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
	return m.Get(devID)
}

// Reject is `maestria devices reject <code>`. It also accepts a pairing that has not revealed its
// key yet when given its id.
func (m *Manager) Reject(codeOrID string) error {
	id, err := m.byCode(codeOrID)
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
		if _, err := m.db.Exec("UPDATE device_pairings SET reported_state = state WHERE pair_id = ?", c.Pair); err != nil {
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
		if d.State == "active" && (now-d.ApprovedAt >= p.Absolute.Milliseconds() || now-d.LastUsedAt >= p.Idle.Milliseconds()) {
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
