package cloud

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/paulo-lucas/maestria/internal/audit"
)

// mailCount waits for the mailer to settle and counts what it was handed.
func mailCount(b *bed) int {
	time.Sleep(60 * time.Millisecond)
	return len(b.mail.all())
}

func mustRecent(t *testing.T, b *bed) []audit.Entry {
	t.Helper()
	ents, err := b.srv.audit.Recent(500)
	if err != nil {
		t.Fatal(err)
	}
	return ents
}

func retryAfter(t *testing.T, r *resp) int {
	t.Helper()
	n, err := strconv.Atoi(r.Header.Get("Retry-After"))
	if err != nil || n < 1 {
		t.Fatalf("a refused request needs a Retry-After in seconds, got %q", r.Header.Get("Retry-After"))
	}
	return n
}

// "Resend verification email" cannot be clicked endlessly: one email a minute to an address, a
// few an hour, and the page says when to try again.
func TestResendVerificationIsLimitedPerAddress(t *testing.T) {
	b := newBed(t)
	br := b.browser("10.20.0.1")
	br.get("/signup")
	br.form("/signup", url.Values{"email": {"ann@example.com"}, "password": {testPassword}})
	if mailCount(b) != 1 {
		t.Fatal("the sign-up must send one email")
	}
	resend := func() *resp { return br.form("/signup/resend", url.Values{"email": {"ann@example.com"}}) }

	// Right after the sign-up mail: refused politely, nothing sent, with a wait of up to a minute.
	r := resend()
	if r.StatusCode != 429 || !strings.Contains(r.body, "try again in") {
		t.Fatalf("a resend within a minute: %d %.300s", r.StatusCode, r.body)
	}
	if n := retryAfter(t, r); n > 60 {
		t.Fatalf("Retry-After %d is longer than the cooldown", n)
	}
	if mailCount(b) != 1 {
		t.Fatal("a refused resend sent an email")
	}
	if !strings.Contains(r.body, "second") {
		t.Fatalf("under two minutes the page counts in seconds: %.400s", r.body)
	}

	// After the cooldown one more goes out; hammering it stays at one a minute.
	b.clock.advance(MailCooldown + time.Second)
	if r := resend(); r.StatusCode != 200 {
		t.Fatalf("resend after the cooldown: %d", r.StatusCode)
	}
	if r := resend(); r.StatusCode != 429 {
		t.Fatalf("second resend in the same minute: %d", r.StatusCode)
	}
	if mailCount(b) != 2 {
		t.Fatalf("%d emails sent", mailCount(b))
	}

	// The hourly cap: five emails to one address in an hour, then it asks for a long wait.
	for i := 0; i < 10 && mailCount(b) < MailPerAddressPerHour; i++ {
		b.clock.advance(MailCooldown + time.Second)
		resend()
	}
	if mailCount(b) != MailPerAddressPerHour {
		t.Fatalf("%d emails sent, want %d", mailCount(b), MailPerAddressPerHour)
	}
	b.clock.advance(MailCooldown + time.Second)
	r = resend()
	if r.StatusCode != 429 || retryAfter(t, r) <= 60 || !strings.Contains(r.body, "minute") {
		t.Fatalf("past the hourly cap: %d Retry-After %q %.300s", r.StatusCode, r.Header.Get("Retry-After"), r.body)
	}
	if mailCount(b) != MailPerAddressPerHour {
		t.Fatal("an email was sent past the cap")
	}
	b.clock.advance(time.Hour)
	if r := resend(); r.StatusCode != 200 {
		t.Fatalf("the cap is hourly: %d", r.StatusCode)
	}
	if !strings.Contains(auditEvents(t, b), "mail_limit") {
		t.Fatal("a limit being hit must be audited")
	}
}

// Password-reset emails are limited the same way, and the page answers the same whether or not
// the account exists, for the accepted request and for the refused one.
func TestResetEmailsAreLimitedAndDoNotRevealAccounts(t *testing.T) {
	b := newBed(t)
	b.browser("10.21.0.1").signup("ann@example.com", testPassword)
	b.clock.advance(time.Hour) // the sign-up mails are not part of this
	ask := func(ip, email string) *resp {
		x := b.browser(ip)
		x.get("/reset")
		return x.form("/reset", url.Values{"email": {email}})
	}
	norm := func(s, email string) string { return strings.ReplaceAll(s, email, "EMAIL") }

	known1, ghost1 := ask("10.21.0.2", "ann@example.com"), ask("10.21.0.3", "ghost@example.com")
	if known1.StatusCode != ghost1.StatusCode || norm(known1.body, "ann@example.com") != norm(ghost1.body, "ghost@example.com") {
		t.Fatalf("first reset request differs: %d vs %d", known1.StatusCode, ghost1.StatusCode)
	}
	known2, ghost2 := ask("10.21.0.2", "ann@example.com"), ask("10.21.0.3", "ghost@example.com")
	if known2.StatusCode != 429 || ghost2.StatusCode != 429 {
		t.Fatalf("a second request within a minute: %d / %d", known2.StatusCode, ghost2.StatusCode)
	}
	if !strings.Contains(known2.body, "try again in") || !strings.Contains(ghost2.body, "try again in") {
		t.Fatal("the page must say when to try again")
	}
	if known2.Header.Get("Retry-After") == "" || ghost2.Header.Get("Retry-After") == "" {
		t.Fatal("Retry-After missing")
	}
	// Only the real account got an email.
	b.waitMail("ann@example.com", "Reset")
	for _, m := range b.mail.all() {
		if m.To == "ghost@example.com" {
			t.Fatal("an email went to an address with no account")
		}
	}
	n := mailCount(b)
	// Hammering it with the cooldown passed each time never gets past the hourly cap of five.
	for i := 0; i < 12; i++ {
		b.clock.advance(MailCooldown + time.Second)
		ask("10.21.0.2", "ann@example.com")
	}
	if got := mailCount(b) - n + 1; got > MailPerAddressPerHour {
		t.Fatalf("%d reset emails to one address within about 12 minutes", got)
	}
	if !strings.Contains(auditEvents(t, b), "mail_limit") {
		t.Fatal("a limit being hit must be audited")
	}
}

// One client address cannot make the server mail many different people.
func TestEmailsAreLimitedPerClientAddress(t *testing.T) {
	b := newBed(t)
	bomber := b.browser("10.22.0.1")
	bomber.get("/reset")
	refused := 0
	for i := 0; i < MailPerIPPerHour+4; i++ {
		r := bomber.form("/reset", url.Values{"email": {fmt.Sprintf("victim%d@example.com", i)}})
		if r.StatusCode == 429 {
			refused++
			retryAfter(t, r)
		}
	}
	if refused != 4 {
		t.Fatalf("%d requests refused, want 4 past the cap of %d for one client address", refused, MailPerIPPerHour)
	}
	// Somebody else is not affected.
	other := b.browser("10.22.0.2")
	other.get("/reset")
	if r := other.form("/reset", url.Values{"email": {"someone@example.com"}}); r.StatusCode != 200 {
		t.Fatalf("another client: %d", r.StatusCode)
	}
}

// The server as a whole sends at most MailDailyLimit emails in 24 hours (configurable), and
// refuses politely, with a Retry-After, once that is reached. The count is read from the audit
// log, so it survives a restart.
func TestDailyEmailCapForTheWholeServer(t *testing.T) {
	b := newBed(t, func(c *Config) { c.MailDailyLimit = 4 })
	send := func(i int) *resp {
		x := b.browser(fmt.Sprintf("10.23.%d.1", i))
		x.get("/signup")
		return x.form("/signup", url.Values{"email": {fmt.Sprintf("user%d@example.com", i)}, "password": {testPassword}})
	}
	for i := 0; i < 4; i++ {
		if r := send(i); r.StatusCode != 200 {
			t.Fatalf("email %d: %d", i, r.StatusCode)
		}
	}
	if mailCount(b) != 4 {
		t.Fatalf("%d sent", mailCount(b))
	}
	b.clock.advance(3 * time.Hour) // far from the day's end, the cap still holds
	r := send(9)
	if r.StatusCode != 429 || retryAfter(t, r) < 60 || !strings.Contains(r.body, "hour") {
		t.Fatalf("past the daily cap: %d Retry-After %q %.300s", r.StatusCode, r.Header.Get("Retry-After"), r.body)
	}
	// Reset is refused the same way, for known and unknown accounts alike.
	x := b.browser("10.23.50.1")
	x.get("/reset")
	if r := x.form("/reset", url.Values{"email": {"user0@example.com"}}); r.StatusCode != 429 {
		t.Fatalf("reset past the daily cap: %d", r.StatusCode)
	}
	if r := x.form("/reset", url.Values{"email": {"ghost@example.com"}}); r.StatusCode != 429 {
		t.Fatalf("reset for an unknown address past the daily cap: %d", r.StatusCode)
	}
	if mailCount(b) != 4 {
		t.Fatal("an email was sent past the daily cap")
	}
	if !strings.Contains(auditEvents(t, b), "mail_limit") {
		t.Fatal("the daily cap being hit must be audited")
	}
	// The audit entries hold no address.
	for _, e := range mustRecent(t, b) {
		if strings.Contains(e.Detail, "@") {
			t.Fatalf("an email address reached the audit log: %s %s", e.Event, e.Detail)
		}
	}
	// A day after the first emails the cap frees up again.
	b.clock.advance(MailDay)
	if r := send(10); r.StatusCode != 200 {
		t.Fatalf("a day later: %d", r.StatusCode)
	}
}

func TestMailDailyLimitConfig(t *testing.T) {
	get := func(v string) func(string) string {
		return func(k string) string {
			if k == "MAESTRIA_CLOUD_MAIL_DAILY_LIMIT" {
				return v
			}
			return ""
		}
	}
	if c := Defaults(); c.MailDailyLimit != 200 {
		t.Fatalf("default %d, want 200", c.MailDailyLimit)
	}
	cfg, err := LoadConfig(t.TempDir(), get("50"))
	if err != nil || cfg.MailDailyLimit != 50 {
		t.Fatalf("env override: %v %+v", err, cfg)
	}
	for _, bad := range []string{"abc", "0", "-3", "99999999"} {
		if _, err := LoadConfig(t.TempDir(), get(bad)); err == nil {
			t.Errorf("MAESTRIA_CLOUD_MAIL_DAILY_LIMIT=%q must be refused", bad)
		}
	}
}

func TestRetryInSaysWhenToTryAgain(t *testing.T) {
	for in, want := range map[time.Duration]string{
		time.Second: "1 second", 45 * time.Second: "45 seconds", 100 * time.Second: "100 seconds",
		3 * time.Minute: "3 minutes", 59*time.Minute + time.Second: "60 minutes",
		3 * time.Hour: "3 hours", 23*time.Hour + 10*time.Minute: "24 hours",
	} {
		if got := retryIn(in); got != want {
			t.Errorf("retryIn(%v) = %q, want %q", in, got, want)
		}
	}
}
