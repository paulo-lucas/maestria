package auth

import (
	"database/sql"
	"strconv"
	"time"

	"github.com/maestria-dev/maestria/internal/audit"
)

// Outcome is the result of one login step.
type Outcome int

const (
	// OK means the step succeeded.
	OK Outcome = iota
	// Invalid means a wrong password or code. The message never says which factor failed.
	Invalid
	// Locked means the limiter refuses attempts for now.
	Locked
	// Expired means the pending login (password step) is missing or expired.
	Expired
)

// Result of a login step. On OK, Token is the pending-login token (password step) or the
// session token (second factor).
type Result struct {
	Outcome    Outcome
	RetryAfter time.Duration
	Token      string
	Session    *Session
	Remaining  int // attempts left for this IP before lockout (on Invalid)
}

const lastStepKey = "totp_last_step"

// Authenticator runs the two-step login: password, then TOTP or a recovery code.
type Authenticator struct {
	DB       *sql.DB
	Secrets  *Secrets
	Sessions *SessionStore
	Limiter  *LoginLimiter
	Audit    *audit.Log
	Now      func() time.Time
}

func (a *Authenticator) now() time.Time {
	if a.Now == nil {
		return time.Now()
	}
	return a.Now()
}

func (a *Authenticator) gate(ip string) (Result, bool) {
	d := a.Limiter.Check(ip)
	if d.Allowed {
		return Result{}, true
	}
	_ = a.Audit.WriteThrottled("blocked:"+d.Scope+":"+ip, time.Minute, "login_blocked", ip, map[string]any{"scope": d.Scope})
	return Result{Outcome: Locked, RetryAfter: d.RetryAfter}, false
}

func (a *Authenticator) fail(ip, step string) Result {
	locked := a.Limiter.Fail(ip)
	_ = a.Audit.Write("login_fail", ip, map[string]any{"step": step})
	if locked {
		_ = a.Audit.Write("login_lockout", ip, nil)
		d := a.Limiter.Check(ip)
		return Result{Outcome: Locked, RetryAfter: d.RetryAfter}
	}
	return Result{Outcome: Invalid, Remaining: a.Limiter.Remaining(ip)}
}

// Password verifies the password and, on success, returns a pending-login token.
func (a *Authenticator) Password(ip, password string) (Result, error) {
	if r, ok := a.gate(ip); !ok {
		return r, nil
	}
	if len(password) > MaxPasswordLength {
		password = password[:MaxPasswordLength]
	}
	hash := a.Secrets.PasswordHash
	if hash == "" {
		hash = dummyHash()
	}
	if !VerifyPassword(password, hash) {
		return a.fail(ip, "password"), nil
	}
	token, err := a.Sessions.CreatePending(ip)
	if err != nil {
		return Result{}, err
	}
	_ = a.Audit.Write("login_password_ok", ip, nil)
	return Result{Outcome: OK, Token: token}, nil
}

// TOTP verifies a code for a pending login and, on success, creates a session.
// A code is accepted once (replay protection) and within ±1 step.
func (a *Authenticator) TOTP(ip, userAgent, pending, code string) (Result, error) {
	if r, ok := a.gate(ip); !ok {
		return r, nil
	}
	live, err := a.Sessions.PendingValid(pending)
	if err != nil {
		return Result{}, err
	}
	if !live {
		return Result{Outcome: Expired}, nil
	}
	step, ok := VerifyTOTP(a.Secrets.TOTPSecret, code, a.now(), 1)
	if ok {
		fresh, err := a.claimStep(step)
		if err != nil {
			return Result{}, err
		}
		ok = fresh
	}
	if !ok {
		return a.fail(ip, "totp"), nil
	}
	return a.finish(ip, userAgent, pending, map[string]any{"method": "totp"})
}

// Recovery accepts a single-use recovery code for a pending login.
func (a *Authenticator) Recovery(ip, userAgent, pending, code string) (Result, error) {
	if r, ok := a.gate(ip); !ok {
		return r, nil
	}
	live, err := a.Sessions.PendingValid(pending)
	if err != nil {
		return Result{}, err
	}
	if !live {
		return Result{Outcome: Expired}, nil
	}
	used, err := ConsumeRecoveryCode(a.DB, code, a.now())
	if err != nil {
		return Result{}, err
	}
	if !used {
		return a.fail(ip, "recovery"), nil
	}
	left, _ := RemainingRecoveryCodes(a.DB)
	_ = a.Audit.Write("recovery_code_used", ip, map[string]any{"remaining": left})
	return a.finish(ip, userAgent, pending, map[string]any{"method": "recovery"})
}

func (a *Authenticator) finish(ip, userAgent, pending string, detail map[string]any) (Result, error) {
	ok, err := a.Sessions.ConsumePending(pending)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return Result{Outcome: Expired}, nil
	}
	a.Limiter.Success(ip)
	token, sess, err := a.Sessions.Create(ip, userAgent)
	if err != nil {
		return Result{}, err
	}
	_ = a.Audit.Write("login_ok", ip, detail)
	return Result{Outcome: OK, Token: token, Session: sess}, nil
}

// claimStep atomically records step as the last used TOTP step if it is newer than the stored one.
func (a *Authenticator) claimStep(step int64) (bool, error) {
	res, err := a.DB.Exec(`INSERT INTO kv (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
		WHERE CAST(kv.value AS INTEGER) < CAST(excluded.value AS INTEGER)`, lastStepKey, strconv.FormatInt(step, 10))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ResetTOTPReplay forgets the last used step (after enrolling a new TOTP secret).
func ResetTOTPReplay(db *sql.DB) error {
	_, err := db.Exec("DELETE FROM kv WHERE key = ?", lastStepKey)
	return err
}
