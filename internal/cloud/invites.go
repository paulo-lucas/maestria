package cloud

import (
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/paulo-lucas/maestria/internal/auth"
	"github.com/paulo-lucas/maestria/internal/termsafe"
)

// Invitation limits.
const (
	InviteDefaultTTL = 7 * 24 * time.Hour
	InviteMinTTL     = time.Hour
	InviteMaxTTL     = 30 * 24 * time.Hour
	InviteNoteMax    = 200
	// invitesKept is how long a spent, withdrawn or expired invitation stays in the list.
	invitesKept = 90 * 24 * time.Hour

	// Per administrator: how many invitations they may create and how many they may email an hour.
	inviteCreatePerHour = 20
	inviteMailPerHour   = 10
	// Per recipient address: invitation emails in 24 hours, from any administrator.
	inviteMailPerAddressPerDay = 3
)

// mailInvite is the mail kind of an invitation email (cooldown and audit).
const mailInvite = "invite"

// Invitation errors. ErrInviteInvalid is the only answer the sign-up gives about a token: unknown,
// expired, used, withdrawn and "bound to another address" all look the same.
var (
	ErrInviteInvalid = errors.New("invalid or expired invitation")
	ErrSignupClosed  = errors.New("sign-up is closed")
	ErrBadExpiry     = errors.New("the expiry must be between 1 hour and 30 days")
	ErrBadNote       = errors.New("the note is too long (200 characters at most)")
	ErrBadEmail      = errors.New("enter a valid email address")
)

// Invite is one invitation as an administrator sees it. It never carries the token: that exists
// only in the link shown once, at creation.
type Invite struct {
	ID        string
	Note      string
	Email     string // the address it is bound to, or ""
	CreatedBy string // account id; "" when made from the server's command line
	CreatedAt int64
	ExpiresAt int64
	UsedAt    int64
	UsedBy    string
	RevokedAt int64
}

// Status is "used", "revoked", "expired" or "active".
func (i *Invite) Status(now time.Time) string {
	switch {
	case i.UsedAt != 0:
		return "used"
	case i.RevokedAt != 0:
		return "revoked"
	case now.UnixMilli() >= i.ExpiresAt:
		return "expired"
	}
	return "active"
}

// ParseExpiry reads "7d", "12h" or "90m" (and any Go duration). Anything else, and anything
// outside 1 hour to 30 days, is refused: a bad value is never turned into a default.
func ParseExpiry(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	var d time.Duration
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil || days < 0 || days > 3650 {
			return 0, ErrBadExpiry
		}
		d = time.Duration(days) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil {
			return 0, ErrBadExpiry
		}
	}
	if d < InviteMinTTL || d > InviteMaxTTL {
		return 0, ErrBadExpiry
	}
	return d, nil
}

// InviteOptions is what an administrator asks for.
type InviteOptions struct {
	// By is the creating account, "" for the server's command line (which is not rate limited).
	By string
	IP string
	// TTL is how long it works; 0 means the default (7 days). Anything else must be 1h to 30d.
	TTL   time.Duration
	Note  string
	Email string // bound address (optional)
	// SendEmail mails the link to Email through the mail gate.
	SendEmail bool
}

// InviteLink is the sign-up address of an invitation. It is built from the configured public URL,
// never from a request.
func (a *Accounts) InviteLink(token string) string { return a.url + "/signup?invite=" + token }

// CreateInvite makes an invitation and returns it with its token (shown once; only a hash is kept).
// Limits come first, so a refusal leaves nothing behind.
func (a *Accounts) CreateInvite(o InviteOptions) (*Invite, string, error) {
	ttl := o.TTL
	if ttl == 0 {
		ttl = InviteDefaultTTL
	}
	if ttl < InviteMinTTL || ttl > InviteMaxTTL {
		return nil, "", ErrBadExpiry
	}
	note := termsafe.Clean(o.Note, 0)
	if utf8.RuneCountInString(note) > InviteNoteMax {
		return nil, "", ErrBadNote
	}
	email := ""
	if strings.TrimSpace(o.Email) != "" {
		var ok bool
		if email, ok = NormalizeEmail(o.Email); !ok {
			return nil, "", ErrBadEmail
		}
	}
	if o.SendEmail && email == "" {
		return nil, "", ErrBadEmail
	}
	if o.SendEmail && !a.cfg.mailConfigured() {
		return nil, "", ErrNoMailer
	}
	if o.By != "" {
		if ok, retry := a.rate.Take("invite:create:"+o.By, inviteCreatePerHour, time.Hour); !ok {
			return nil, "", &LockedError{RetryAfter: retry}
		}
	}
	if o.SendEmail {
		if err := a.inviteMailLimits(o.By, o.IP, email); err != nil {
			return nil, "", err
		}
	}
	now := a.now()
	inv := &Invite{ID: "inv_" + newID()[:12], Note: note, Email: email, CreatedBy: o.By, CreatedAt: now.UnixMilli(), ExpiresAt: now.Add(ttl).UnixMilli()}
	token := auth.RandomToken()
	var by, bound any
	if o.By != "" {
		by = o.By
	}
	if email != "" {
		bound = email
	}
	if _, err := a.db.Exec("INSERT INTO invites (id, token_hash, created_by, note, email, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
		inv.ID, auth.HashToken(token), by, note, bound, inv.CreatedAt, inv.ExpiresAt); err != nil {
		return nil, "", err
	}
	a.event(o.By, "invite_created", o.IP, map[string]any{"invite": inv.ID, "bound": email != "", "emailed": o.SendEmail, "expiresInSeconds": int(ttl.Seconds())})
	if o.SendEmail {
		a.sendMail(mailInvite, a.inviteMessage(email, token, note, time.UnixMilli(inv.ExpiresAt)))
	}
	return inv, token, nil
}

// inviteMailLimits is what an invitation email adds to the mail gate: the same daily cap, address
// and client limits as every other email, plus a limit per administrator and one per recipient.
func (a *Accounts) inviteMailLimits(by, ip, email string) error {
	if err := a.mailGate(mailInvite, ip, email); err != nil {
		return err
	}
	if by != "" {
		if ok, retry := a.rate.Take("invite:mail:"+by, inviteMailPerHour, time.Hour); !ok {
			return a.mailRefused("admin", mailInvite, ip, email, retry)
		}
	}
	if ok, retry := a.rate.Take("invite:to:"+email, inviteMailPerAddressPerDay, MailDay); !ok {
		return a.mailRefused("recipient", mailInvite, ip, email, retry)
	}
	return nil
}

// inviteMessage is fixed text around the link. The note, written by the inviter, goes in the body
// only, labelled as theirs: never in a header or the subject.
func (a *Accounts) inviteMessage(to, token, note string, expires time.Time) Message {
	body := "You have been invited to create an account on Maestria (" + a.url + ").\n\n" +
		"Open this link to sign up. It works once and expires on " + expires.UTC().Format("2006-01-02 15:04 UTC") + ":\n\n" + a.InviteLink(token) + "\n\n"
	if note != "" {
		body += "Note written by the person who invited you (not by Maestria):\n" + note + "\n\n"
	}
	body += "If you were not expecting this, ignore this email: nothing happens until you use the link."
	return Message{To: to, Subject: "You are invited to Maestria", Body: body}
}

// InviteInfo is what the sign-up page learns from a live token.
type InviteInfo struct {
	// Bound is true when only one address can use it (the address itself is never shown).
	Bound bool
}

// invalidAttemptsPerHour bounds how many wrong invitation tokens one client address may try.
const invalidAttemptsPerHour = 20

// inviteRow reads an invitation by token and says whether it is usable for email ("" skips the
// address check, for the page that only looks). Every way of not being usable is ErrInviteInvalid.
func (a *Accounts) inviteRow(q execer, token, email string, checkEmail bool) (id string, bound bool, err error) {
	if token == "" || len(token) > 128 {
		return "", false, ErrInviteInvalid
	}
	var bnd sql.NullString
	var exp int64
	var used, rev sql.NullInt64
	err = q.QueryRow("SELECT id, email, expires_at, used_at, revoked_at FROM invites WHERE token_hash = ?", auth.HashToken(token)).Scan(&id, &bnd, &exp, &used, &rev)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, ErrInviteInvalid
	}
	if err != nil {
		return "", false, err
	}
	if used.Valid || rev.Valid || a.now().UnixMilli() >= exp {
		return "", false, ErrInviteInvalid
	}
	if checkEmail && bnd.Valid && bnd.String != email {
		return "", false, ErrInviteInvalid
	}
	return id, bnd.Valid, nil
}

// failedInvite counts one wrong token against the client address.
func (a *Accounts) failedInvite(ip string) error {
	if ok, retry := a.rate.Take("invite:bad:"+ip, invalidAttemptsPerHour, time.Hour); !ok {
		return &LockedError{RetryAfter: retry}
	}
	return ErrInviteInvalid
}

// PeekInvite is what the sign-up page asks before showing the form. It uses nothing up.
func (a *Accounts) PeekInvite(ip, token string) (*InviteInfo, error) {
	_, bound, err := a.inviteRow(a.db, token, "", false)
	if errors.Is(err, ErrInviteInvalid) {
		return nil, a.failedInvite(ip)
	}
	if err != nil {
		return nil, err
	}
	return &InviteInfo{Bound: bound}, nil
}

// useInvite spends an invitation inside the transaction of the sign-up that uses it. The UPDATE is
// the whole check (unused, not withdrawn, not expired, address matches), so two sign-ups with one
// token cannot both succeed. Zero rows is ErrInviteInvalid and the caller rolls back.
func (a *Accounts) useInvite(tx execer, token, email, userID string) (string, error) {
	var by any // NULL when this sign-up left an existing or pending account alone
	if userID != "" {
		by = userID
	}
	res, err := tx.Exec("UPDATE invites SET used_at = ?, used_by = ? WHERE token_hash = ? AND used_at IS NULL AND revoked_at IS NULL AND expires_at > ? AND (email IS NULL OR email = ?)",
		a.now().UnixMilli(), by, auth.HashToken(token), a.now().UnixMilli(), email)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return "", ErrInviteInvalid
	}
	var id string
	if err := tx.QueryRow("SELECT id FROM invites WHERE token_hash = ?", auth.HashToken(token)).Scan(&id); err != nil {
		return "", err
	}
	return id, nil
}

// ListInvites returns the newest invitations first.
func (a *Accounts) ListInvites() ([]Invite, error) {
	rows, err := a.db.Query("SELECT id, note, email, created_by, created_at, expires_at, used_at, used_by, revoked_at FROM invites ORDER BY created_at DESC, id LIMIT 500")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		var i Invite
		var email, by, usedBy sql.NullString
		var used, rev sql.NullInt64
		if err := rows.Scan(&i.ID, &i.Note, &email, &by, &i.CreatedAt, &i.ExpiresAt, &used, &usedBy, &rev); err != nil {
			return nil, err
		}
		i.Email, i.CreatedBy, i.UsedBy, i.UsedAt, i.RevokedAt = email.String, by.String, usedBy.String, used.Int64, rev.Int64
		out = append(out, i)
	}
	return out, rows.Err()
}

// RevokeInvite withdraws an invitation. It is idempotent: withdrawing one twice, or one already
// used, is not an error. An unknown id is ErrInvalid.
func (a *Accounts) RevokeInvite(by, ip, id string) error {
	id = strings.TrimSpace(id)
	var exists int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM invites WHERE id = ?", id).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return ErrInvalid
	}
	res, err := a.db.Exec("UPDATE invites SET revoked_at = ? WHERE id = ? AND used_at IS NULL AND revoked_at IS NULL", a.now().UnixMilli(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		a.event(by, "invite_revoked", ip, map[string]any{"invite": id})
	}
	return nil
}

// signupMode is the configured mode; an unset one means the default, invite.
func (a *Accounts) signupMode() string {
	if a.cfg == nil || a.cfg.Signup == "" {
		return SignupInvite
	}
	return a.cfg.Signup
}
