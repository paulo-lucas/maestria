package cloud

import (
	"strings"
	"time"
)

// Email send limits. Every page that can make the server send an email (sign-up, "resend the
// verification email", "forgot password") goes through mailGate, and every email that is sent
// goes through sendMail. Without them one person clicking a button, or a script, could burn the
// SMTP provider's daily quota and fill somebody's inbox.
//
// What is counted is the request, not only the email that goes out: a request for an address that
// has no account is counted exactly like one that has, so a refusal never tells whether an
// account exists, and the pages answer the same either way.
const (
	// MailCooldown is the shortest time between two emails of the same kind to one address.
	MailCooldown = 60 * time.Second
	// MailPerAddressPerHour caps all emails to one address (an account has exactly one).
	MailPerAddressPerHour = 5
	// MailPerIPPerHour caps the emails one client address can ask for.
	MailPerIPPerHour = 10
	// MailDay is the window of the global cap.
	MailDay = 24 * time.Hour
)

// Mail kinds, used for the cooldown and the audit log.
const (
	mailVerify = "verify"
	mailReset  = "reset"
	mailNotice = "notice" // "you already have an account"
)

func (a *Accounts) dailyLimit() int {
	if a.cfg != nil && a.cfg.MailDailyLimit > 0 {
		return a.cfg.MailDailyLimit
	}
	return 200
}

// mailsSent counts the emails sent in the last 24 hours and returns when the oldest of them stops
// counting. They are read from the audit log, so the cap survives a restart.
func (a *Accounts) mailsSent() (n int, oldest time.Time) {
	from := a.now().Add(-MailDay).UnixMilli()
	var min *int64
	_ = a.db.QueryRow("SELECT COUNT(*), MIN(ts) FROM audit WHERE event = 'mail_sent' AND ts > ?", from).Scan(&n, &min)
	if min != nil {
		oldest = time.UnixMilli(*min)
	}
	return n, oldest
}

// mailRefused records that a limit was hit (at most once a minute per limit and address) and
// returns the polite refusal: a LockedError, answered as 429 with Retry-After.
func (a *Accounts) mailRefused(scope, kind, ip, email string, retry time.Duration) error {
	_ = a.audit.WriteThrottled("maillimit:"+scope+":"+kind+":"+emailKey(email), time.Minute, "mail_limit", ip,
		map[string]any{"scope": scope, "kind": kind, "retryAfterSeconds": int(retry.Seconds())})
	return &LockedError{RetryAfter: retry, Reason: "mail"}
}

// mailGate is asked before a page does anything that may send an email to address email. It
// counts the request against the limits and refuses politely when one is reached:
//
//   - the whole server may send MailDailyLimit emails in 24 hours (checked here, counted when the
//     email is really sent);
//   - one address gets one email of a kind per MailCooldown and MailPerAddressPerHour in total;
//   - one client address may ask for MailPerIPPerHour.
func (a *Accounts) mailGate(kind, ip, email string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	if n, oldest := a.mailsSent(); n >= a.dailyLimit() {
		retry := oldest.Add(MailDay).Sub(a.now())
		if retry < time.Minute {
			retry = time.Minute
		}
		return a.mailRefused("daily", kind, ip, email, retry)
	}
	if ok, retry := a.rate.Take("mail:cool:"+kind+":"+email, 1, MailCooldown); !ok {
		return a.mailRefused("cooldown", kind, ip, email, retry)
	}
	if ok, retry := a.rate.Take("mail:addr:"+email, MailPerAddressPerHour, time.Hour); !ok {
		return a.mailRefused("address", kind, ip, email, retry)
	}
	if ok, retry := a.rate.Take("mail:ip:"+ip, MailPerIPPerHour, time.Hour); !ok {
		return a.mailRefused("ip", kind, ip, email, retry)
	}
	return nil
}

// sendMail hands one email to the mailer and records it (kind only, never the address or the
// content) so the daily cap can count it.
func (a *Accounts) sendMail(kind string, m Message) {
	_ = a.audit.Write("mail_sent", "", map[string]any{"kind": kind})
	a.mail(m)
}
