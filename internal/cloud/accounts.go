package cloud

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/mail"
	"strings"
	"sync"
	"time"

	"github.com/paulo-lucas/maestria/internal/audit"
	"github.com/paulo-lucas/maestria/internal/auth"
)

// Token lifetimes.
const (
	EmailTokenTTL = 30 * time.Minute
	SetupTTL      = 30 * time.Minute
	// UnverifiedTTL is how long an address may stay unverified before the account is removed and
	// the address is free again.
	UnverifiedTTL = 24 * time.Hour
)

// Account errors. They are deliberately coarse: callers never say which check failed.
var (
	ErrInvalid  = errors.New("invalid")
	ErrExpired  = errors.New("expired")
	ErrNoMailer = errors.New("email is not configured")
	// ErrNotReady: the account has not finished sign-up (verified email and two-factor).
	ErrNotReady = errors.New("account has not finished signing up")
)

// LockedError means a limiter refuses attempts for now.
type LockedError struct {
	RetryAfter time.Duration
	// Reason is "mail" when the limit is on emails sent (the page says so); empty otherwise.
	Reason string
}

func (e *LockedError) Error() string { return "too many attempts" }

// User is an account.
type User struct {
	ID         string
	Email      string
	VerifiedAt int64
	EnrolledAt int64
	DisabledAt int64
	CreatedAt  int64
	// IsAdmin is read from the users table on every request, never copied into a session.
	IsAdmin bool
}

// CanLogin reports whether the account may sign in.
func (u *User) CanLogin() bool { return u.VerifiedAt != 0 && u.EnrolledAt != 0 && u.DisabledAt == 0 }

// Accounts is the account store and its flows.
type Accounts struct {
	db     *sql.DB
	sealer *Sealer
	audit  *audit.Log
	mail   func(Message)
	now    func() time.Time
	cfg    *Config
	url    string

	ipLim   *auth.LoginLimiter
	acctLim *auth.LoginLimiter
	rate    *rateLimiter
	// hashes bounds the argon2 work a stranger can start (sign-up, verify, sign-in, reset).
	hashes *auth.HashGuard

	idle, absolute time.Duration
	touch          sync.Mutex
}

// NewAccounts wires the account flows. mail is called with every outgoing message.
func NewAccounts(db *sql.DB, sealer *Sealer, aud *audit.Log, cfg *Config, mail func(Message), now func() time.Time) *Accounts {
	if now == nil {
		now = time.Now
	}
	lim := cfg.Login
	win, lock := time.Duration(lim.WindowSeconds)*time.Second, time.Duration(lim.LockoutSeconds)*time.Second
	return &Accounts{
		db: db, sealer: sealer, audit: aud, mail: mail, now: now, cfg: cfg, url: strings.TrimRight(cfg.PublicURL, "/"),
		ipLim:   auth.NewLoginLimiter(auth.LimiterOptions{MaxFailuresPerIP: lim.MaxFailuresPerIP, MaxFailuresGlobal: lim.MaxFailuresGlobal, Window: win, Lockout: lock}, now),
		acctLim: auth.NewLoginLimiter(auth.LimiterOptions{MaxFailuresPerIP: lim.MaxFailuresPerAccount, MaxFailuresGlobal: 1 << 30, Window: win, Lockout: lock}, now),
		rate:    newRateLimiter(now),
		hashes:  auth.NewHashGuard(auth.GuardOptions{}, now),
		idle:    time.Duration(cfg.SessionIdleMinutes) * time.Minute, absolute: time.Duration(cfg.SessionAbsoluteHours) * time.Hour,
	}
}

// guard takes a password-check slot for the client address. A refusal is a LockedError (429 with
// Retry-After) and says nothing about the credentials. The caller must call release.
func (a *Accounts) guard(ip string) (release func(), err error) {
	rel, d := a.hashes.Acquire(ip)
	if !d.Allowed {
		_ = a.audit.WriteThrottled("throttled:"+d.Reason+":"+ip, time.Minute, "login_throttled", ip, map[string]any{"scope": d.Reason})
		return nil, &LockedError{RetryAfter: d.RetryAfter}
	}
	return rel, nil
}

func (a *Accounts) event(userID, event, ip string, detail map[string]any) {
	if detail == nil {
		detail = map[string]any{}
	}
	if userID != "" {
		detail["user"] = userID
	}
	_ = a.audit.Write(event, ip, detail)
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func emailKey(email string) string {
	sum := sha256.Sum256([]byte("acct:" + email))
	return hex.EncodeToString(sum[:])
}

// NormalizeEmail lower-cases and validates an address. It accepts plain addr-spec only.
func NormalizeEmail(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || len(s) > 254 || strings.ContainsAny(s, " \t\r\n<>,;\"") {
		return "", false
	}
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Address != s || !strings.Contains(s[strings.LastIndex(s, "@"):], ".") {
		return "", false
	}
	return s, true
}

// MaskEmail returns j***@example.com.
func MaskEmail(e string) string {
	at := strings.LastIndex(e, "@")
	if at < 1 {
		return "***"
	}
	return e[:1] + "***" + e[at:]
}

// execer is what *sql.DB and *sql.Tx share, so a step can run on either.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

func (a *Accounts) userBy(query string, args ...any) (*User, error) {
	return a.userByQ(a.db, query, args...)
}

func (a *Accounts) userByQ(q execer, query string, args ...any) (*User, error) {
	var u User
	var ver, enr, dis sql.NullInt64
	err := q.QueryRow("SELECT id, email, email_verified_at, totp_enrolled_at, disabled_at, created_at, is_admin FROM users WHERE "+query, args...).
		Scan(&u.ID, &u.Email, &ver, &enr, &dis, &u.CreatedAt, &u.IsAdmin)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.VerifiedAt, u.EnrolledAt, u.DisabledAt = ver.Int64, enr.Int64, dis.Int64
	return &u, nil
}

// UserByID returns a user or nil.
func (a *Accounts) UserByID(id string) (*User, error) { return a.userBy("id = ?", id) }

// UserByEmail returns a user or nil.
func (a *Accounts) UserByEmail(email string) (*User, error) { return a.userBy("email = ?", email) }

func (a *Accounts) newEmailToken(userID, kind string) (string, error) {
	return a.newEmailTokenQ(a.db, userID, kind)
}

func (a *Accounts) newEmailTokenQ(q execer, userID, kind string) (string, error) {
	tok := auth.RandomToken()
	now := a.now()
	_, err := q.Exec("INSERT INTO email_tokens (hash, user_id, kind, created_at, expires_at) VALUES (?, ?, ?, ?, ?)",
		auth.HashToken(tok), userID, kind, now.UnixMilli(), now.Add(EmailTokenTTL).UnixMilli())
	return tok, err
}

func (a *Accounts) peekToken(kind, token string) (string, error) {
	if token == "" || len(token) > 128 {
		return "", ErrInvalid
	}
	var uid string
	var exp int64
	var used sql.NullInt64
	err := a.db.QueryRow("SELECT user_id, expires_at, used_at FROM email_tokens WHERE hash = ? AND kind = ?", auth.HashToken(token), kind).Scan(&uid, &exp, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrInvalid
	}
	if err != nil {
		return "", err
	}
	if used.Valid || a.now().UnixMilli() >= exp {
		return "", ErrExpired
	}
	return uid, nil
}

func (a *Accounts) burnToken(token string) (bool, error) {
	res, err := a.db.Exec("UPDATE email_tokens SET used_at = ? WHERE hash = ? AND used_at IS NULL", a.now().UnixMilli(), auth.HashToken(token))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ---- sign-up ----

// PasswordError is a policy failure the person can fix.
type PasswordError struct{ Msg string }

func (e *PasswordError) Error() string { return e.Msg }

func (a *Accounts) sendVerify(userID, email string) error {
	tok, err := a.newEmailToken(userID, "verify")
	if err != nil {
		return err
	}
	a.sendMail(mailVerify, Message{To: email, Subject: "Verify your email for Maestria",
		Body: "Confirm your email to finish creating your Maestria account:\n\n" + a.url + "/verify/" + tok + "\n\nThe link expires in 30 minutes. If you did not sign up, ignore this email."})
	return nil
}

// PeekVerify reports whether a verification link is live, without using it.
func (a *Accounts) PeekVerify(token string) (*User, error) {
	uid, err := a.peekToken("verify", token)
	if err != nil {
		return nil, err
	}
	return a.UserByID(uid)
}

// gate refuses the attempt while the IP or the account key is locked out.
func (a *Accounts) gate(ip, key string) error {
	if d := a.ipLim.Check(ip); !d.Allowed {
		_ = a.audit.WriteThrottled("blocked:ip:"+ip, time.Minute, "login_blocked", ip, map[string]any{"scope": d.Scope})
		return &LockedError{RetryAfter: d.RetryAfter}
	}
	if d := a.acctLim.Check(key); !d.Allowed {
		_ = a.audit.WriteThrottled("blocked:acct:"+key, time.Minute, "login_blocked", ip, map[string]any{"scope": "account"})
		return &LockedError{RetryAfter: d.RetryAfter}
	}
	return nil
}

func (a *Accounts) fail(ip, key string) (locked bool) {
	l1 := a.ipLim.Fail(ip)
	l2 := a.acctLim.Fail(key)
	return l1 || l2
}

func (a *Accounts) passwordOf(userID string) (string, error) {
	var h string
	err := a.db.QueryRow("SELECT password_hash FROM users WHERE id = ?", userID).Scan(&h)
	return h, err
}

// ConfirmVerify consumes the verification token after the password is checked, marks the email
// verified and starts the 2FA setup. It returns the setup cookie token.
func (a *Accounts) ConfirmVerify(ip, token, password string) (string, error) {
	uid, err := a.peekToken("verify", token)
	if err != nil {
		return "", err
	}
	u, err := a.UserByID(uid)
	if err != nil || u == nil {
		return "", ErrInvalid
	}
	key := emailKey(u.Email)
	if err := a.gate(ip, key); err != nil {
		return "", err
	}
	hash, err := a.passwordOf(uid)
	if err != nil {
		return "", err
	}
	if len(password) > auth.MaxPasswordLength {
		password = password[:auth.MaxPasswordLength]
	}
	release, err := a.guard(ip)
	if err != nil {
		return "", err
	}
	verified := auth.VerifyPassword(password, hash)
	release()
	if !verified || u.DisabledAt != 0 {
		if a.fail(ip, key) {
			a.event(uid, "login_lockout", ip, nil)
		}
		a.event(uid, "verify_fail", ip, nil)
		return "", ErrInvalid
	}
	if ok, err := a.burnToken(token); err != nil || !ok {
		return "", ErrExpired
	}
	if _, err := a.db.Exec("UPDATE users SET email_verified_at = COALESCE(email_verified_at, ?) WHERE id = ?", a.now().UnixMilli(), uid); err != nil {
		return "", err
	}
	a.event(uid, "email_verified", ip, nil)
	return a.newSetup(uid)
}

// ResumeSetup gives a verified account with unfinished 2FA a new setup session.
func (a *Accounts) ResumeSetup(ip, rawEmail, password string) (string, error) {
	email, ok := NormalizeEmail(rawEmail)
	if !ok {
		email = strings.ToLower(strings.TrimSpace(rawEmail))
	}
	key := emailKey(email)
	if err := a.gate(ip, key); err != nil {
		return "", err
	}
	u, _ := a.UserByEmail(email)
	hash := auth.DummyHash()
	if u != nil {
		hash, _ = a.passwordOf(u.ID)
	}
	if len(password) > auth.MaxPasswordLength {
		password = password[:auth.MaxPasswordLength]
	}
	release, err := a.guard(ip)
	if err != nil {
		return "", err
	}
	good := auth.VerifyPassword(password, hash)
	release()
	if !good || u == nil || u.VerifiedAt == 0 || u.EnrolledAt != 0 || u.DisabledAt != 0 {
		if a.fail(ip, key) {
			a.event("", "login_lockout", ip, nil)
		}
		return "", ErrInvalid
	}
	return a.newSetup(u.ID)
}

func (a *Accounts) newSetup(userID string) (string, error) {
	tok := auth.RandomToken()
	now := a.now()
	_, err := a.db.Exec("DELETE FROM setup_sessions WHERE user_id = ?", userID)
	if err != nil {
		return "", err
	}
	_, err = a.db.Exec("INSERT INTO setup_sessions (id, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)", auth.HashToken(tok), userID, now.UnixMilli(), now.Add(SetupTTL).UnixMilli())
	return tok, err
}

// Setup is a live 2FA setup session.
type Setup struct {
	ID     string
	User   *User
	secret string
}

// SetupFor returns the live setup session for a cookie token, or nil.
func (a *Accounts) SetupFor(token string) (*Setup, error) {
	if token == "" || len(token) > 128 {
		return nil, nil
	}
	id := auth.HashToken(token)
	var uid string
	var exp int64
	var pend sql.NullString
	err := a.db.QueryRow("SELECT user_id, expires_at, totp_pending_enc FROM setup_sessions WHERE id = ?", id).Scan(&uid, &exp, &pend)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if a.now().UnixMilli() >= exp {
		_, _ = a.db.Exec("DELETE FROM setup_sessions WHERE id = ?", id)
		return nil, nil
	}
	u, err := a.UserByID(uid)
	if err != nil || u == nil || u.VerifiedAt == 0 || u.EnrolledAt != 0 || u.DisabledAt != 0 {
		return nil, err
	}
	s := &Setup{ID: id, User: u}
	if pend.Valid {
		if sec, err := a.sealer.Open(pend.String); err == nil {
			s.secret = sec
		}
	}
	return s, nil
}

// BeginTOTP returns the TOTP secret and otpauth URI for a setup session, creating the secret on
// first use.
func (a *Accounts) BeginTOTP(s *Setup) (secret, uri string, err error) {
	if s.secret == "" {
		sec, err := auth.GenerateTOTPSecret()
		if err != nil {
			return "", "", err
		}
		sealed, err := a.sealer.Seal(sec)
		if err != nil {
			return "", "", err
		}
		if _, err := a.db.Exec("UPDATE setup_sessions SET totp_pending_enc = ? WHERE id = ?", sealed, s.ID); err != nil {
			return "", "", err
		}
		s.secret = sec
	}
	return s.secret, auth.OTPAuthURI(s.secret, s.User.Email, "Maestria"), nil
}

// FinishTOTP checks the first code, stores the secret, issues recovery codes and signs the user
// in. The recovery codes are returned once and never stored in clear text.
func (a *Accounts) FinishTOTP(ip, ua string, s *Setup, code string) (recovery []string, sessionToken string, err error) {
	key := emailKey(s.User.Email)
	if err := a.gate(ip, key); err != nil {
		return nil, "", err
	}
	if s.secret == "" {
		return nil, "", ErrInvalid
	}
	step, ok := auth.VerifyTOTP(s.secret, strings.TrimSpace(code), a.now(), 1)
	if !ok {
		if a.fail(ip, key) {
			a.event(s.User.ID, "login_lockout", ip, nil)
		}
		return nil, "", ErrInvalid
	}
	sealed, err := a.sealer.Seal(s.secret)
	if err != nil {
		return nil, "", err
	}
	codes, err := auth.GenerateRecoveryCodes(auth.RecoveryCodeCount)
	if err != nil {
		return nil, "", err
	}
	hashes := make([]string, len(codes))
	for i, c := range codes {
		if hashes[i], err = auth.HashRecoveryCode(c); err != nil {
			return nil, "", err
		}
	}
	tx, err := a.db.Begin()
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()
	now := a.now().UnixMilli()
	res, err := tx.Exec("UPDATE users SET totp_secret_enc = ?, totp_enrolled_at = ?, totp_last_step = ? WHERE id = ? AND totp_enrolled_at IS NULL", sealed, now, step, s.User.ID)
	if err != nil {
		return nil, "", err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, "", ErrInvalid
	}
	if _, err := tx.Exec("DELETE FROM recovery_codes WHERE user_id = ?", s.User.ID); err != nil {
		return nil, "", err
	}
	for _, h := range hashes {
		if _, err := tx.Exec("INSERT INTO recovery_codes (user_id, hash, created_at) VALUES (?, ?, ?)", s.User.ID, h, now); err != nil {
			return nil, "", err
		}
	}
	if _, err := tx.Exec("DELETE FROM setup_sessions WHERE user_id = ?", s.User.ID); err != nil {
		return nil, "", err
	}
	if err := tx.Commit(); err != nil {
		return nil, "", err
	}
	a.ipLim.Success(ip)
	a.event(s.User.ID, "2fa_enrolled", ip, nil)
	tok, _, err := a.CreateSession(s.User.ID, ip, ua)
	return codes, tok, err
}

// ---- login ----

// LoginPassword is step 1. It always costs one argon2id verification and always yields a pending
// login, whatever the email and password were; the verdict stays on the server.
func (a *Accounts) LoginPassword(ip, rawEmail, password string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(rawEmail))
	if len(email) > 254 {
		email = email[:254]
	}
	key := emailKey(email)
	if err := a.gate(ip, key); err != nil {
		return "", err
	}
	if len(password) > auth.MaxPasswordLength {
		password = password[:auth.MaxPasswordLength]
	}
	u, _ := a.UserByEmail(email)
	hash := auth.DummyHash()
	if u != nil {
		if h, err := a.passwordOf(u.ID); err == nil {
			hash = h
		}
	}
	release, err := a.guard(ip)
	if err != nil {
		return "", err
	}
	good := auth.VerifyPassword(password, hash) && u != nil && u.CanLogin()
	release()
	tok := auth.RandomToken()
	now := a.now()
	uid := sql.NullString{}
	if u != nil {
		uid = sql.NullString{String: u.ID, Valid: true}
	}
	ok := 0
	if good {
		ok = 1
	}
	tx, err := a.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	// A pending login belongs to the browser holding its cookie: another client's first step
	// never replaces it. The table is bounded by refusing new logins, per address and in total.
	if _, err := tx.Exec("DELETE FROM pending_logins WHERE expires_at <= ?", now.UnixMilli()); err != nil {
		return "", err
	}
	var perIP, total int
	if err := tx.QueryRow("SELECT COUNT(*), COALESCE(SUM(ip = ?), 0) FROM pending_logins", clip(ip, 64)).Scan(&total, &perIP); err != nil {
		return "", err
	}
	if perIP >= auth.MaxPendingPerIP || total >= auth.MaxPendingTotal {
		return "", &LockedError{RetryAfter: 30 * time.Second}
	}
	if _, err := tx.Exec("INSERT INTO pending_logins (id, ip, user_id, email_key, password_ok, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
		auth.HashToken(tok), clip(ip, 64), uid, key, ok, now.UnixMilli(), now.Add(auth.PendingTTL).UnixMilli()); err != nil {
		return "", err
	}
	return tok, tx.Commit()
}

type pending struct {
	userID  sql.NullString
	key     string
	passOK  bool
	expires int64
	ip      string
}

func (a *Accounts) pendingFor(token, ip string) (*pending, error) {
	if token == "" || len(token) > 128 {
		return nil, nil
	}
	var p pending
	var ok int
	err := a.db.QueryRow("SELECT user_id, email_key, password_ok, expires_at, ip FROM pending_logins WHERE id = ?", auth.HashToken(token)).
		Scan(&p.userID, &p.key, &ok, &p.expires, &p.ip)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if a.now().UnixMilli() >= p.expires || p.ip != clip(ip, 64) {
		return nil, nil
	}
	p.passOK = ok == 1
	return &p, nil
}

// HasPending reports whether the cookie token is a live pending login for this IP.
func (a *Accounts) HasPending(token, ip string) bool {
	p, _ := a.pendingFor(token, ip)
	return p != nil
}

// LoginSecond is step 2 with a TOTP code (method "totp") or a recovery code ("recovery"). It
// succeeds only if the remembered password verdict and the code are both good, and every other
// outcome is identical and counts as one failed attempt.
func (a *Accounts) LoginSecond(ip, ua, pendingToken, method, code string) (string, error) {
	p, err := a.pendingFor(pendingToken, ip)
	if err != nil {
		return "", err
	}
	if p == nil {
		return "", ErrExpired
	}
	if err := a.gate(ip, p.key); err != nil {
		return "", err
	}

	var (
		factorOK bool
		step     int64
		recID    int64
	)
	// Recovery codes are argon2 hashes, every unused one checked: bound them like passwords.
	release := func() {}
	if method == "recovery" {
		if release, err = a.guard(ip); err != nil {
			return "", err
		}
	}
	if p.userID.Valid {
		factorOK, step, recID, err = a.checkFactor(p.userID.String, method, code)
		if err != nil {
			release()
			return "", err
		}
	} else {
		a.dummyFactor(method, code)
	}
	release()

	good := p.passOK && factorOK && p.userID.Valid
	if good {
		if method == "totp" {
			good, err = a.claimStep(p.userID.String, step)
		} else {
			res, e := a.db.Exec("UPDATE recovery_codes SET used_at = ? WHERE id = ? AND used_at IS NULL", a.now().UnixMilli(), recID)
			if e != nil {
				return "", e
			}
			n, _ := res.RowsAffected()
			good = n == 1
		}
		if err != nil {
			return "", err
		}
	}
	consumed, err := a.consumePending(pendingToken)
	if err != nil {
		return "", err
	}
	uid := p.userID.String
	if !good || !consumed {
		locked := a.fail(ip, p.key)
		a.event(uid, "login_fail", ip, map[string]any{"method": method})
		if locked {
			a.event(uid, "login_lockout", ip, nil)
			return "", &LockedError{RetryAfter: a.ipLim.Check(ip).RetryAfter}
		}
		return "", ErrInvalid
	}
	a.ipLim.Success(ip)
	if method == "recovery" {
		a.event(uid, "recovery_code_used", ip, map[string]any{"remaining": a.recoveryLeft(uid)})
	}
	a.event(uid, "login_ok", ip, map[string]any{"method": method})
	tok, _, err := a.CreateSession(uid, ip, ua)
	return tok, err
}

func (a *Accounts) consumePending(token string) (bool, error) {
	res, err := a.db.Exec("DELETE FROM pending_logins WHERE id = ?", auth.HashToken(token))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// checkFactor verifies a code without side effects.
func (a *Accounts) checkFactor(userID, method, code string) (ok bool, step, recoveryID int64, err error) {
	if method == "totp" {
		var sealed sql.NullString
		if err := a.db.QueryRow("SELECT totp_secret_enc FROM users WHERE id = ?", userID).Scan(&sealed); err != nil {
			return false, 0, 0, err
		}
		if !sealed.Valid {
			return false, 0, 0, nil
		}
		secret, err := a.sealer.Open(sealed.String)
		if err != nil {
			return false, 0, 0, err
		}
		step, ok = auth.VerifyTOTP(secret, strings.TrimSpace(code), a.now(), 1)
		return ok, step, 0, nil
	}
	rows, err := a.db.Query("SELECT id, hash FROM recovery_codes WHERE user_id = ? AND used_at IS NULL", userID)
	if err != nil {
		return false, 0, 0, err
	}
	type cand struct {
		id   int64
		hash string
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.hash); err != nil {
			rows.Close()
			return false, 0, 0, err
		}
		cands = append(cands, c)
	}
	rows.Close()
	// Always verify against every unused code, and pad to the full count, so timing does not
	// depend on which one matched or on how many are left.
	var match int64
	for _, c := range cands {
		if auth.VerifyRecoveryCode(code, c.hash) {
			match = c.id
		}
	}
	for i := len(cands); i < auth.RecoveryCodeCount; i++ {
		auth.VerifyRecoveryCode(code, auth.DummyHash())
	}
	return match != 0, 0, match, nil
}

// dummyFactor costs about what checkFactor costs, for logins whose email does not exist.
func (a *Accounts) dummyFactor(method, code string) {
	if method == "totp" {
		auth.VerifyTOTP("JBSWY3DPEHPK3PXP", strings.TrimSpace(code), a.now(), 1)
		return
	}
	for i := 0; i < auth.RecoveryCodeCount; i++ {
		auth.VerifyRecoveryCode(code, auth.DummyHash())
	}
}

func (a *Accounts) claimStep(userID string, step int64) (bool, error) {
	res, err := a.db.Exec("UPDATE users SET totp_last_step = ? WHERE id = ? AND totp_last_step < ?", step, userID, step)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (a *Accounts) recoveryLeft(userID string) int {
	var n int
	_ = a.db.QueryRow("SELECT COUNT(*) FROM recovery_codes WHERE user_id = ? AND used_at IS NULL", userID).Scan(&n)
	return n
}

// RecoveryLeft reports how many unused recovery codes a user has.
func (a *Accounts) RecoveryLeft(userID string) int { return a.recoveryLeft(userID) }

// ---- password reset ----

// RequestReset emails a reset link if the account can use one. The caller shows the same page
// whether or not the account exists; the only other answer is a *LockedError when the email
// limits are reached, which depends on how often the address was asked for, not on the account.
func (a *Accounts) RequestReset(ip, rawEmail string) error {
	email, ok := NormalizeEmail(rawEmail)
	if !ok {
		return nil
	}
	if err := a.mailGate(mailReset, ip, email); err != nil {
		return err
	}
	u, err := a.UserByEmail(email)
	if err != nil || u == nil || !u.CanLogin() {
		a.event("", "reset_requested_unknown", ip, nil)
		return nil
	}
	tok, err := a.newEmailToken(u.ID, "reset")
	if err != nil {
		return nil
	}
	a.event(u.ID, "reset_requested", ip, nil)
	a.sendMail(mailReset, Message{To: email, Subject: "Reset your Maestria password",
		Body: "Someone asked to reset the password of your Maestria account.\n\n" + a.url + "/reset/" + tok +
			"\n\nYou will still need your authenticator app or a recovery code. The link expires in 30 minutes. If it was not you, ignore this email: nothing changes."})
	return nil
}

// PeekReset reports whether a reset link is live and for whom, without using it.
func (a *Accounts) PeekReset(token string) (*User, error) {
	uid, err := a.peekToken("reset", token)
	if err != nil {
		return nil, err
	}
	u, err := a.UserByID(uid)
	if err != nil || u == nil || !u.CanLogin() {
		return nil, ErrInvalid
	}
	return u, nil
}

// CompleteReset sets a new password. It needs the reset link AND a TOTP or recovery code: a
// reset never bypasses 2FA. Every session of the user ends and a new one is returned.
func (a *Accounts) CompleteReset(ip, ua, token, newPassword, method, code string) (string, *User, error) {
	u, err := a.PeekReset(token)
	if err != nil {
		return "", nil, err
	}
	if err := auth.ValidateNewPassword(newPassword); err != nil {
		return "", nil, &PasswordError{err.Error()}
	}
	key := emailKey(u.Email)
	if err := a.gate(ip, key); err != nil {
		return "", nil, err
	}
	// One slot for the whole reset: a recovery code check and the new password's hash are both
	// argon2 work, and a refusal must come before the code or the link is spent.
	release, err := a.guard(ip)
	if err != nil {
		return "", nil, err
	}
	defer release()
	ok, step, recID, err := a.checkFactor(u.ID, method, code)
	if err != nil {
		return "", nil, err
	}
	if ok {
		if method == "totp" {
			ok, err = a.claimStep(u.ID, step)
		} else {
			var res sql.Result
			res, err = a.db.Exec("UPDATE recovery_codes SET used_at = ? WHERE id = ? AND used_at IS NULL", a.now().UnixMilli(), recID)
			if err == nil {
				n, _ := res.RowsAffected()
				ok = n == 1
			}
		}
		if err != nil {
			return "", nil, err
		}
	}
	if !ok {
		if a.fail(ip, key) {
			a.event(u.ID, "login_lockout", ip, nil)
		}
		a.event(u.ID, "reset_fail", ip, map[string]any{"method": method})
		return "", nil, ErrInvalid
	}
	if used, err := a.burnToken(token); err != nil || !used {
		return "", nil, ErrExpired
	}
	hash, err := auth.HashPassword(newPassword)
	if err != nil {
		return "", nil, err
	}
	if _, err := a.db.Exec("UPDATE users SET password_hash = ? WHERE id = ?", hash, u.ID); err != nil {
		return "", nil, err
	}
	if _, err := a.db.Exec("DELETE FROM sessions WHERE user_id = ?", u.ID); err != nil {
		return "", nil, err
	}
	if _, err := a.db.Exec("UPDATE email_tokens SET used_at = ? WHERE user_id = ? AND kind = 'reset' AND used_at IS NULL", a.now().UnixMilli(), u.ID); err != nil {
		return "", nil, err
	}
	a.ipLim.Success(ip)
	a.event(u.ID, "password_reset", ip, map[string]any{"method": method})
	tok, _, err := a.CreateSession(u.ID, ip, ua)
	return tok, u, err
}

// ---- sessions ----

// Session is a live browser session with its user.
type Session struct {
	ID        string
	User      *User
	CSRFToken string
	CreatedAt time.Time
	ExpiresAt time.Time
}

const touchInterval = 30 * time.Second

// CreateSession starts a session and returns the cookie token.
func (a *Accounts) CreateSession(userID, ip, ua string) (string, *Session, error) {
	tok := auth.RandomToken()
	now := a.now()
	s := &Session{ID: auth.HashToken(tok), CSRFToken: auth.RandomToken(), CreatedAt: now, ExpiresAt: now.Add(a.absolute)}
	_, err := a.db.Exec("INSERT INTO sessions (id, user_id, csrf_token, created_at, last_seen_at, expires_at, ip, user_agent) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		s.ID, userID, s.CSRFToken, now.UnixMilli(), now.UnixMilli(), s.ExpiresAt.UnixMilli(), clip(ip, 64), clip(ua, 200))
	return tok, s, err
}

// Validate returns the live session for a cookie token, or nil. Disabled users have no session.
func (a *Accounts) Validate(token string) (*Session, error) {
	if token == "" || len(token) > 128 {
		return nil, nil
	}
	id := auth.HashToken(token)
	var uid, csrf string
	var created, last, exp int64
	err := a.db.QueryRow("SELECT user_id, csrf_token, created_at, last_seen_at, expires_at FROM sessions WHERE id = ?", id).Scan(&uid, &csrf, &created, &last, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ms := a.now().UnixMilli()
	if ms >= exp || ms-last >= a.idle.Milliseconds() {
		_, err := a.db.Exec("DELETE FROM sessions WHERE id = ?", id)
		return nil, err
	}
	u, err := a.UserByID(uid)
	if err != nil || u == nil || !u.CanLogin() {
		return nil, err
	}
	if ms-last >= touchInterval.Milliseconds() {
		if _, err := a.db.Exec("UPDATE sessions SET last_seen_at = ? WHERE id = ?", ms, id); err != nil {
			return nil, err
		}
	}
	return &Session{ID: id, User: u, CSRFToken: csrf, CreatedAt: time.UnixMilli(created), ExpiresAt: time.UnixMilli(exp)}, nil
}

// Destroy ends one session by cookie token.
func (a *Accounts) Destroy(token string) error {
	_, err := a.db.Exec("DELETE FROM sessions WHERE id = ?", auth.HashToken(token))
	return err
}

// DestroyUser ends every session of a user.
func (a *Accounts) DestroyUser(userID string) error {
	_, err := a.db.Exec("DELETE FROM sessions WHERE user_id = ?", userID)
	return err
}

// Purge removes expired sessions, pending logins, setup sessions and used or expired tokens.
func (a *Accounts) Purge() {
	ms := a.now().UnixMilli()
	_, _ = a.db.Exec("DELETE FROM sessions WHERE expires_at <= ? OR last_seen_at <= ?", ms, ms-a.idle.Milliseconds())
	_, _ = a.db.Exec("DELETE FROM pending_logins WHERE expires_at <= ?", ms)
	_, _ = a.db.Exec("DELETE FROM setup_sessions WHERE expires_at <= ?", ms)
	_, _ = a.db.Exec("DELETE FROM email_tokens WHERE expires_at <= ? OR used_at IS NOT NULL AND used_at <= ?", ms, ms-int64(24*time.Hour/time.Millisecond))
	_, _ = a.db.Exec("DELETE FROM link_codes WHERE expires_at <= ? AND used_at IS NULL", ms-int64(time.Hour/time.Millisecond))
	// An address nobody verified within a day is released (its tokens and setup go with it).
	_, _ = a.db.Exec("DELETE FROM users WHERE email_verified_at IS NULL AND created_at <= ?", ms-UnverifiedTTL.Milliseconds())
	a.rate.Prune()
}

// ---- admin ----

// ListUsers returns every account, newest first.
func (a *Accounts) ListUsers() ([]User, error) {
	rows, err := a.db.Query("SELECT id, email, email_verified_at, totp_enrolled_at, disabled_at, created_at, is_admin FROM users ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		var ver, enr, dis sql.NullInt64
		if err := rows.Scan(&u.ID, &u.Email, &ver, &enr, &dis, &u.CreatedAt, &u.IsAdmin); err != nil {
			return nil, err
		}
		u.VerifiedAt, u.EnrolledAt, u.DisabledAt = ver.Int64, enr.Int64, dis.Int64
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetDisabled disables or re-enables an account by email. Disabling ends its sessions.
func (a *Accounts) SetDisabled(rawEmail string, disabled bool) (*User, error) {
	email, ok := NormalizeEmail(rawEmail)
	if !ok {
		return nil, ErrInvalid
	}
	u, err := a.UserByEmail(email)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, ErrInvalid
	}
	if disabled {
		if _, err := a.db.Exec("UPDATE users SET disabled_at = COALESCE(disabled_at, ?) WHERE id = ?", a.now().UnixMilli(), u.ID); err != nil {
			return nil, err
		}
		if err := a.DestroyUser(u.ID); err != nil {
			return nil, err
		}
		a.event(u.ID, "user_disabled", "", map[string]any{"via": "admin"})
	} else {
		if _, err := a.db.Exec("UPDATE users SET disabled_at = NULL WHERE id = ?", u.ID); err != nil {
			return nil, err
		}
		a.event(u.ID, "user_enabled", "", map[string]any{"via": "admin"})
	}
	return a.UserByID(u.ID)
}

// SetAdmin makes an account an administrator, or takes that away. It is only called from the
// server's command line: no web route reaches it. Only an account that finished sign-up (email
// verified, two-factor on, not disabled) can be promoted. Demoting takes effect on the person's
// next request, because the flag is read from the users table every time.
func (a *Accounts) SetAdmin(rawEmail string, admin bool) (*User, error) {
	email, ok := NormalizeEmail(rawEmail)
	if !ok {
		return nil, ErrInvalid
	}
	u, err := a.UserByEmail(email)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, ErrInvalid
	}
	if admin && !u.CanLogin() {
		return nil, ErrNotReady
	}
	v := 0
	event := "user_demoted"
	if admin {
		v, event = 1, "user_promoted"
	}
	if _, err := a.db.Exec("UPDATE users SET is_admin = ? WHERE id = ?", v, u.ID); err != nil {
		return nil, err
	}
	a.event(u.ID, event, "", map[string]any{"via": "cli"})
	return a.UserByID(u.ID)
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ResendVerify emails a fresh verification link to an unverified account. The caller shows the
// same page whether or not the address exists. The only difference a person can see is a
// *LockedError when the email limits are reached, and that depends on how often the address was
// asked for, never on whether it has an account.
func (a *Accounts) ResendVerify(ip, rawEmail string) error {
	email, ok := NormalizeEmail(rawEmail)
	if !ok {
		return nil
	}
	if err := a.mailGate(mailVerify, ip, email); err != nil {
		return err
	}
	u, err := a.UserByEmail(email)
	if err != nil || u == nil || u.DisabledAt != 0 || u.EnrolledAt != 0 {
		return nil
	}
	_ = a.sendVerify(u.ID, email)
	return nil
}
