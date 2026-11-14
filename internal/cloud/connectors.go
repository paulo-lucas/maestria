package cloud

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/maestria-dev/maestria/internal/auth"
	"github.com/maestria-dev/maestria/internal/relay"
)

// LinkTTL is how long a link code lives.
const LinkTTL = 10 * time.Minute

// Link code alphabet: no look-alikes (shared with recovery codes).
const linkAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

// Connector is a linked machine as stored.
type Connector struct {
	ID          string
	UserID      string
	Name        string
	Host        string
	OS          string
	Arch        string
	Version     string
	KillSwitch  bool
	CreatedAt   int64
	LastSeenAt  int64
	ConnectedAt int64
	RevokedAt   int64
	ForgetAt    int64
}

// Connectors keeps linked machines, link codes and the mirror of paired-device public keys.
type Connectors struct {
	a *Accounts
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// NormalizeLinkCode lowercases and strips dashes and spaces.
func NormalizeLinkCode(s string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(s) {
		if r == '-' || r == ' ' || r == '\t' {
			continue
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

// FormatLinkCode renders a normalized code as XXXX-XXXX.
func FormatLinkCode(n string) string {
	n = strings.ToUpper(n)
	if len(n) != 8 {
		return n
	}
	return n[:4] + "-" + n[4:]
}

func newLinkCode() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, 8)
	for i, v := range b {
		out[i] = linkAlphabet[int(v)%len(linkAlphabet)]
	}
	return string(out), nil
}

func (c *Connectors) db() *sql.DB { return c.a.db }

// CreateWebCode makes a code the signed-in user types at the machine. It returns the code and
// the id used to poll its state.
func (c *Connectors) CreateWebCode(userID string) (code, id string, expires time.Time, err error) {
	return c.createCode("web", userID)
}

func (c *Connectors) createCode(kind, userID string) (code, id string, expires time.Time, err error) {
	now := c.a.now()
	expires = now.Add(LinkTTL)
	for i := 0; i < 5; i++ {
		code, err = newLinkCode()
		if err != nil {
			return "", "", expires, err
		}
		id = newID()
		var uid sql.NullString
		if userID != "" {
			uid = sql.NullString{String: userID, Valid: true}
		}
		_, err = c.db().Exec(`INSERT INTO link_codes (id, code_hash, kind, user_id, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
			id, sha256Hex(code), kind, uid, now.UnixMilli(), expires.UnixMilli())
		if err == nil {
			return code, id, expires, nil
		}
	}
	return "", "", expires, err
}

// CodeStatus is the state of a link code for the web page that shows it.
type CodeStatus struct {
	State       string // waiting | linked | expired
	ConnectorID string
	Name        string
	Expires     time.Time
}

// WebCodeStatus reports whether the user's code was used.
func (c *Connectors) WebCodeStatus(userID, id string) (*CodeStatus, error) {
	var exp int64
	var used sql.NullInt64
	var cid sql.NullString
	err := c.db().QueryRow("SELECT expires_at, used_at, connector_id FROM link_codes WHERE id = ? AND user_id = ? AND kind = 'web'", id, userID).Scan(&exp, &used, &cid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	st := &CodeStatus{State: "waiting", Expires: time.UnixMilli(exp)}
	switch {
	case used.Valid:
		st.State = "linked"
		st.ConnectorID = cid.String
		if cn, _ := c.Get(cid.String); cn != nil {
			st.Name = cn.Name
		}
	case c.a.now().UnixMilli() >= exp:
		st.State = "expired"
	}
	return st, nil
}

func (c *Connectors) throttle(ip string) error {
	if !c.a.rate.Allow("link:ip:"+ip, 20, 10*time.Minute) || !c.a.rate.Allow("link:global", 300, 10*time.Minute) {
		return &LockedError{RetryAfter: 10 * time.Minute}
	}
	return nil
}

type linkRow struct {
	id        string
	userID    sql.NullString
	kind      string
	expires   int64
	claimedAt sql.NullInt64
	usedAt    sql.NullInt64
	confirm   sql.NullString
	poll      sql.NullString
	machine   string
}

func (c *Connectors) linkByCode(code string) (*linkRow, error) {
	var r linkRow
	err := c.db().QueryRow(`SELECT id, user_id, kind, expires_at, claimed_at, used_at, confirm_hash, poll_hash, machine_json FROM link_codes WHERE code_hash = ?`, sha256Hex(NormalizeLinkCode(code))).
		Scan(&r.id, &r.userID, &r.kind, &r.expires, &r.claimedAt, &r.usedAt, &r.confirm, &r.poll, &r.machine)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &r, err
}

func (c *Connectors) live(r *linkRow) bool {
	return r != nil && !r.usedAt.Valid && c.a.now().UnixMilli() < r.expires
}

// Redeem validates a web code from the connector and returns the account hint and a one-time
// confirmation token. The code is not consumed: that happens at Confirm.
func (c *Connectors) Redeem(ip, code string, m relay.Machine) (*relay.RedeemResponse, error) {
	if err := c.throttle(ip); err != nil {
		return nil, err
	}
	r, err := c.linkByCode(code)
	if err != nil {
		return nil, err
	}
	if !c.live(r) || r.kind != "web" || !r.userID.Valid {
		c.a.event("", "link_code_invalid", ip, nil)
		return nil, ErrInvalid
	}
	u, err := c.a.UserByID(r.userID.String)
	if err != nil || u == nil || u.DisabledAt != 0 {
		return nil, ErrInvalid
	}
	tok := auth.RandomToken()
	mj, _ := json.Marshal(m)
	if _, err := c.db().Exec("UPDATE link_codes SET confirm_hash = ?, machine_json = ? WHERE id = ? AND used_at IS NULL", sha256Hex(tok), string(mj), r.id); err != nil {
		return nil, err
	}
	return &relay.RedeemResponse{AccountHint: MaskEmail(u.Email), DefaultName: defaultName(m), Token: tok}, nil
}

func defaultName(m relay.Machine) string {
	n := strings.TrimSpace(m.Host)
	if n == "" {
		n = "My machine"
	}
	return cleanName(n)
}

func cleanName(s string) string {
	var sb strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if r < 0x20 || r == 0x7f {
			continue
		}
		sb.WriteRune(r)
		if sb.Len() >= 60 {
			break
		}
	}
	if sb.Len() == 0 {
		return "My machine"
	}
	return sb.String()
}

// Confirm consumes a web code (atomically) and creates the connector. The credential is returned
// once; only its hash is stored.
func (c *Connectors) Confirm(ip, code, token, name string) (*relay.Linked, error) {
	if err := c.throttle(ip); err != nil {
		return nil, err
	}
	r, err := c.linkByCode(code)
	if err != nil {
		return nil, err
	}
	if !c.live(r) || r.kind != "web" || !r.userID.Valid || !r.confirm.Valid ||
		subtle.ConstantTimeCompare([]byte(r.confirm.String), []byte(sha256Hex(token))) != 1 {
		c.a.event("", "link_code_invalid", ip, nil)
		return nil, ErrInvalid
	}
	var m relay.Machine
	_ = json.Unmarshal([]byte(r.machine), &m)
	return c.consume(ip, r, r.userID.String, name, m)
}

// consume atomically uses the code and creates the connector.
func (c *Connectors) consume(ip string, r *linkRow, userID, name string, m relay.Machine) (*relay.Linked, error) {
	if strings.TrimSpace(name) == "" {
		name = defaultName(m)
	}
	name = cleanName(name)
	now := c.a.now().UnixMilli()
	secretB := make([]byte, 32)
	if _, err := rand.Read(secretB); err != nil {
		return nil, err
	}
	secret := relay.Enc(secretB)
	idB := make([]byte, 16)
	if _, err := rand.Read(idB); err != nil {
		return nil, err
	}
	id := "cn_" + hex.EncodeToString(idB)

	tx, err := c.db().Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.Exec("UPDATE link_codes SET used_at = ?, connector_id = ? WHERE id = ? AND used_at IS NULL AND expires_at > ?", now, id, r.id, now)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, ErrInvalid
	}
	if _, err := tx.Exec(`INSERT INTO connectors (id, user_id, name, credential_hash, host, os, arch, version, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, userID, name, sha256Hex(secret), clip(m.Host, 100), clip(m.OS, 20), clip(m.Arch, 20), clip(m.Version, 40), now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	c.a.event(userID, "connector_linked", ip, map[string]any{"connector": id})
	return &relay.Linked{ConnectorID: id, Credential: relay.FormatCredential(id, secret), Name: name}, nil
}

// StartCLI creates a code for a connector that wants the person to type it in the web.
func (c *Connectors) StartCLI(ip string, m relay.Machine, baseURL string) (*relay.StartResponse, error) {
	if err := c.throttle(ip); err != nil {
		return nil, err
	}
	if !c.a.rate.Allow("link:start:ip:"+ip, 10, 10*time.Minute) {
		return nil, &LockedError{RetryAfter: 10 * time.Minute}
	}
	code, id, exp, err := c.createCode("cli", "")
	if err != nil {
		return nil, err
	}
	poll := auth.RandomToken()
	mj, _ := json.Marshal(m)
	if _, err := c.db().Exec("UPDATE link_codes SET poll_hash = ?, machine_json = ? WHERE id = ?", sha256Hex(poll), string(mj), id); err != nil {
		return nil, err
	}
	return &relay.StartResponse{Code: FormatLinkCode(code), PollToken: poll, ExpiresAt: exp.UnixMilli(), URL: baseURL + "/connectors/link"}, nil
}

// Claim is the signed-in user typing a CLI code in the web.
func (c *Connectors) Claim(ip, userID, code string) error {
	if err := c.throttle(ip); err != nil {
		return err
	}
	r, err := c.linkByCode(code)
	if err != nil {
		return err
	}
	if !c.live(r) || r.kind != "cli" || r.claimedAt.Valid {
		c.a.event(userID, "link_code_invalid", ip, nil)
		return ErrInvalid
	}
	res, err := c.db().Exec("UPDATE link_codes SET user_id = ?, claimed_at = ? WHERE id = ? AND claimed_at IS NULL AND used_at IS NULL AND expires_at > ?",
		userID, c.a.now().UnixMilli(), r.id, c.a.now().UnixMilli())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrInvalid
	}
	c.a.event(userID, "link_code_claimed", ip, nil)
	return nil
}

// Poll answers the connector waiting on a CLI code, and finishes the link on confirm.
func (c *Connectors) Poll(ip string, req relay.PollRequest) (*relay.PollResponse, error) {
	if !c.a.rate.Allow("link:poll:ip:"+ip, 600, 10*time.Minute) {
		return nil, &LockedError{RetryAfter: time.Minute}
	}
	if req.PollToken == "" || len(req.PollToken) > 128 {
		return nil, ErrInvalid
	}
	var r linkRow
	err := c.db().QueryRow(`SELECT id, user_id, kind, expires_at, claimed_at, used_at, machine_json FROM link_codes WHERE poll_hash = ?`, sha256Hex(req.PollToken)).
		Scan(&r.id, &r.userID, &r.kind, &r.expires, &r.claimedAt, &r.usedAt, &r.machine)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvalid
	}
	if err != nil {
		return nil, err
	}
	var m relay.Machine
	_ = json.Unmarshal([]byte(r.machine), &m)
	switch {
	case r.usedAt.Valid:
		return nil, ErrInvalid // the credential is shown once
	case c.a.now().UnixMilli() >= r.expires:
		return &relay.PollResponse{Status: "expired"}, nil
	case !r.claimedAt.Valid || !r.userID.Valid:
		return &relay.PollResponse{Status: "pending"}, nil
	}
	u, err := c.a.UserByID(r.userID.String)
	if err != nil || u == nil || u.DisabledAt != 0 {
		return nil, ErrInvalid
	}
	if !req.Confirm {
		return &relay.PollResponse{Status: "claimed", AccountHint: MaskEmail(u.Email), DefaultName: defaultName(m)}, nil
	}
	linked, err := c.consume(ip, &r, u.ID, req.Name, m)
	if err != nil {
		return nil, err
	}
	return &relay.PollResponse{Status: "linked", Linked: linked}, nil
}

// Authenticate checks a connector credential (hash lookup, constant-time compare). It returns the
// connector only when it is not revoked and its owner can still sign in.
func (c *Connectors) Authenticate(credential string) (*Connector, error) {
	id, secret, ok := relay.ParseCredential(credential)
	if !ok {
		return nil, ErrInvalid
	}
	cn, err := c.Get(id)
	if err != nil {
		return nil, err
	}
	var stored string
	if cn == nil {
		return nil, ErrInvalid
	}
	if err := c.db().QueryRow("SELECT credential_hash FROM connectors WHERE id = ?", id).Scan(&stored); err != nil {
		return nil, ErrInvalid
	}
	if subtle.ConstantTimeCompare([]byte(stored), []byte(sha256Hex(secret))) != 1 || cn.RevokedAt != 0 {
		return nil, ErrInvalid
	}
	u, err := c.a.UserByID(cn.UserID)
	if err != nil || u == nil || u.DisabledAt != 0 {
		return nil, ErrInvalid
	}
	return cn, nil
}

const connCols = `id, user_id, name, host, os, arch, version, kill_switch, created_at, last_seen_at, connected_at, revoked_at, forget_devices_at`

func scanConnector(sc interface{ Scan(...any) error }) (*Connector, error) {
	var cn Connector
	var kill int
	var last, conn, rev, forget sql.NullInt64
	if err := sc.Scan(&cn.ID, &cn.UserID, &cn.Name, &cn.Host, &cn.OS, &cn.Arch, &cn.Version, &kill, &cn.CreatedAt, &last, &conn, &rev, &forget); err != nil {
		return nil, err
	}
	cn.KillSwitch = kill == 1
	cn.LastSeenAt, cn.ConnectedAt, cn.RevokedAt, cn.ForgetAt = last.Int64, conn.Int64, rev.Int64, forget.Int64
	return &cn, nil
}

// Get returns a connector or nil.
func (c *Connectors) Get(id string) (*Connector, error) {
	cn, err := scanConnector(c.db().QueryRow("SELECT "+connCols+" FROM connectors WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return cn, err
}

// ForUser lists a user's connectors that are not revoked, oldest first.
func (c *Connectors) ForUser(userID string) ([]*Connector, error) {
	rows, err := c.db().Query("SELECT "+connCols+" FROM connectors WHERE user_id = ? AND revoked_at IS NULL ORDER BY created_at", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Connector
	for rows.Next() {
		cn, err := scanConnector(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, cn)
	}
	return out, rows.Err()
}

// Owned returns the connector if it belongs to the user and is not revoked.
func (c *Connectors) Owned(userID, id string) (*Connector, error) {
	cn, err := c.Get(id)
	if err != nil || cn == nil || cn.UserID != userID || cn.RevokedAt != 0 {
		return nil, err
	}
	return cn, nil
}

// Rename changes a connector's name.
func (c *Connectors) Rename(userID, id, name string) error {
	name = cleanName(name)
	res, err := c.db().Exec("UPDATE connectors SET name = ? WHERE id = ? AND user_id = ? AND revoked_at IS NULL", name, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrInvalid
	}
	c.a.event(userID, "connector_renamed", "", map[string]any{"connector": id})
	return nil
}

// Revoke ends a connector's credential for good. The caller closes the live socket.
func (c *Connectors) Revoke(userID, id, ip, via string) error {
	res, err := c.db().Exec("UPDATE connectors SET revoked_at = ? WHERE id = ? AND user_id = ? AND revoked_at IS NULL", c.a.now().UnixMilli(), id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrInvalid
	}
	_, _ = c.db().Exec("DELETE FROM paired_devices WHERE connector_id = ?", id)
	c.a.event(userID, "connector_revoked", ip, map[string]any{"connector": id, "via": via})
	return nil
}

// Seen records a connection: metadata from hello.
func (c *Connectors) Seen(id string, f relay.Frame, connected bool) {
	now := c.a.now().UnixMilli()
	if connected {
		_, _ = c.db().Exec(`UPDATE connectors SET last_seen_at = ?, connected_at = ?, version = ?, os = ?, arch = ?, kill_switch = ? WHERE id = ?`,
			now, now, clip(f.Version, 40), clip(f.OS, 20), clip(f.Arch, 20), b2i(f.KillSwitch), id)
		return
	}
	_, _ = c.db().Exec("UPDATE connectors SET last_seen_at = ?, kill_switch = ? WHERE id = ?", now, b2i(f.KillSwitch), id)
}

// Touch records that a connector is still there.
func (c *Connectors) Touch(id string) {
	_, _ = c.db().Exec("UPDATE connectors SET last_seen_at = ? WHERE id = ?", c.a.now().UnixMilli(), id)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// SetDevices replaces the display mirror of a connector's paired devices.
func (c *Connectors) SetDevices(id string, devs []relay.DeviceInfo) {
	now := c.a.now().UnixMilli()
	tx, err := c.db().Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	_, _ = tx.Exec("DELETE FROM paired_devices WHERE connector_id = ?", id)
	for _, d := range devs {
		if len(d.ID) > 64 || len(d.PublicKey) > 400 {
			continue
		}
		_, _ = tx.Exec(`INSERT OR REPLACE INTO paired_devices (connector_id, device_id, label, public_key, state, approved_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, d.ID, clip(d.Label, 80), d.PublicKey, clip(d.State, 16), d.ApprovedAt, now)
	}
	_ = tx.Commit()
}

// Devices returns the mirror of a connector's paired devices.
func (c *Connectors) Devices(id string) ([]relay.DeviceInfo, error) {
	rows, err := c.db().Query("SELECT device_id, label, state, COALESCE(approved_at, 0) FROM paired_devices WHERE connector_id = ? ORDER BY approved_at DESC", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []relay.DeviceInfo
	for rows.Next() {
		var d relay.DeviceInfo
		if err := rows.Scan(&d.ID, &d.Label, &d.State, &d.ApprovedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// MarkForget asks every connector of the user to forget its paired devices (after a password
// reset). Online ones get the frame now; the rest at their next hello.
func (c *Connectors) MarkForget(userID string) error {
	_, err := c.db().Exec("UPDATE connectors SET forget_devices_at = ? WHERE user_id = ? AND revoked_at IS NULL", c.a.now().UnixMilli(), userID)
	return err
}

// ClearForget clears the flag once the connector reported (hello after forget).
func (c *Connectors) ClearForget(id string) {
	_, _ = c.db().Exec("UPDATE connectors SET forget_devices_at = NULL WHERE id = ?", id)
}
