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
	// OK means the step succeeded. Step 1 always reports OK (see Password).
	OK Outcome = iota
	// Invalid means the attempt failed. It never says whether the password or the code was wrong.
	Invalid
	// Locked means the limiter refuses attempts for now.
	Locked
	// Expired means the pending login is missing, expired, used, or from another IP.
	Expired
)

// Result of a login step. On OK, Token is the pending-login token (step 1) or the session token (step 2).
type Result struct {
	Outcome    Outcome
	RetryAfter time.Duration
	Token      string
	Session    *Session
}

const lastStepKey = "totp_last_step"

// Authenticator runs the two-step login.
//
// Step 1 (Password) always moves on to the second-factor page, whether the password was right or
// not: the server remembers the verdict in the pending login, bound to the IP, and never tells the
// browser. Step 2 (TOTP or Recovery) succeeds only if the remembered password verdict was good AND
// the code is good; every other combination fails identically, ends the pending login (back to
// step 1) and counts as one failed attempt. So there is no oracle for the password, and a login
// attempt costs the same whatever was wrong.
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

// Password is step 1. It always runs one argon2 verification (against a dummy hash when none is
// configured), records the verdict in a new pending login for this IP, and returns its token.
// It does not count a failure: nothing can be learned from this step alone, and the attempt is
// counted once, at step 2.
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
	good := VerifyPassword(password, hash)
	token, err := a.Sessions.CreatePending(ip, good)
	if err != nil {
		return Result{}, err
	}
	return Result{Outcome: OK, Token: token}, nil
}

// TOTP is step 2 with an authenticator code. A code is accepted once (replay protection) and
// within ±1 step.
func (a *Authenticator) TOTP(ip, userAgent, pending, code string) (Result, error) {
	return a.second(ip, userAgent, pending, "totp", code)
}

// Recovery is step 2 with a single-use recovery code.
func (a *Authenticator) Recovery(ip, userAgent, pending, code string) (Result, error) {
	return a.second(ip, userAgent, pending, "recovery", code)
}

func (a *Authenticator) second(ip, userAgent, pending, method, code string) (Result, error) {
	if r, ok := a.gate(ip); !ok {
		return r, nil
	}
	p, err := a.Sessions.PendingFor(pending, ip)
	if err != nil {
		return Result{}, err
	}
	if p == nil {
		return Result{Outcome: Expired}, nil
	}

	// Always check the second factor, without side effects, whatever the password verdict was.
	var (
		step       int64
		recoveryID int64
		factorOK   bool
	)
	if method == "totp" {
		step, factorOK = VerifyTOTP(a.Secrets.TOTPSecret, code, a.now(), 1)
	} else {
		recoveryID, err = MatchRecoveryCode(a.DB, code)
		if err != nil {
			return Result{}, err
		}
		factorOK = recoveryID != 0
	}

	// Side effects (burning the code) only when the whole attempt is good, so a wrong password
	// never costs the owner a valid code.
	good := p.PasswordOK && factorOK
	if good {
		if method == "totp" {
			good, err = a.claimStep(step)
		} else {
			good, err = UseRecoveryCode(a.DB, recoveryID, a.now())
		}
		if err != nil {
			return Result{}, err
		}
	}

	// One attempt, one pending login: it ends here, good or bad.
	consumed, err := a.Sessions.ConsumePending(pending)
	if err != nil {
		return Result{}, err
	}
	if !good || !consumed {
		locked := a.Limiter.Fail(ip)
		_ = a.Audit.Write("login_fail", ip, map[string]any{"method": method})
		if locked {
			_ = a.Audit.Write("login_lockout", ip, nil)
			return Result{Outcome: Locked, RetryAfter: a.Limiter.Check(ip).RetryAfter}, nil
		}
		return Result{Outcome: Invalid}, nil
	}

	a.Limiter.Success(ip)
	token, sess, err := a.Sessions.Create(ip, userAgent)
	if err != nil {
		return Result{}, err
	}
	detail := map[string]any{"method": method}
	if method == "recovery" {
		left, _ := RemainingRecoveryCodes(a.DB)
		_ = a.Audit.Write("recovery_code_used", ip, map[string]any{"remaining": left})
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
