package cloud

import (
	"errors"
	"fmt"
	"github.com/paulo-lucas/maestria/internal/auth"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func inviteBed(t *testing.T, mut ...func(*Config)) *bed {
	return newBed(t, append([]func(*Config){func(c *Config) { c.Signup = SignupInvite }}, mut...)...)
}

func (b *bed) invite(opts InviteOptions) (*Invite, string) {
	b.t.Helper()
	inv, tok, err := b.srv.acc.CreateInvite(opts)
	if err != nil {
		b.t.Fatal(err)
	}
	return inv, tok
}

func (br *browser) signupWith(token, email string) *resp {
	br.b.t.Helper()
	br.get("/signup?invite=" + url.QueryEscape(token))
	return br.form("/signup", url.Values{"email": {email}, "password": {testPassword}, "invite": {token}})
}

func countUsers(b *bed) int {
	var n int
	_ = b.srv.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&n)
	return n
}

func TestInviteIsTheDefaultAndGatesEverySubmission(t *testing.T) {
	b := inviteBed(t)
	br := b.browser("10.0.0.1")
	if r := br.get("/signup"); r.StatusCode != 200 || !strings.Contains(r.body, "by invitation") || strings.Contains(r.body, `name="password"`) {
		t.Fatalf("no invitation, no form: %d", r.StatusCode)
	}
	// Posting without one creates nothing and sends nothing.
	r := br.form("/signup", url.Values{"email": {"eve@example.com"}, "password": {testPassword}})
	if r.StatusCode != 403 || !strings.Contains(r.body, "invalid or has expired") || countUsers(b) != 0 {
		t.Fatalf("sign-up without an invitation: %d", r.StatusCode)
	}
	b.noMail("eve@example.com")

	_, tok := b.invite(InviteOptions{Note: "for ann"})
	page := br.get("/signup?invite=" + tok)
	if page.StatusCode != 200 || !strings.Contains(page.body, `name="invite" value="`+tok+`"`) {
		t.Fatalf("a live invitation shows the form and carries the token in a hidden field: %d", page.StatusCode)
	}
	if strings.Contains(page.body, `action="/signup?`) || strings.Contains(page.body, `action="/signup`+"?invite") {
		t.Fatal("the token must not be in the form's action")
	}
	if r := br.form("/signup", url.Values{"email": {"ann@example.com"}, "password": {testPassword}, "invite": {tok}}); r.StatusCode != 200 {
		t.Fatalf("sign-up with an invitation: %d %s", r.StatusCode, r.body)
	}
	b.waitMail("ann@example.com", "Verify")
	// It works once.
	if r := b.browser("10.0.0.2").signupWith(tok, "bob@example.com"); r.StatusCode != 403 || countUsers(b) != 1 {
		t.Fatalf("a spent invitation must not work again: %d users=%d", r.StatusCode, countUsers(b))
	}
}

// Every way a token can fail looks the same: one status, one page.
func TestInviteFailuresAreIndistinguishable(t *testing.T) {
	b := inviteBed(t)
	_, used := b.invite(InviteOptions{})
	b.browser("10.0.0.1").signupWith(used, "first@example.com")
	revoked, revTok := b.invite(InviteOptions{})
	if err := b.srv.acc.RevokeInvite("", "", revoked.ID); err != nil {
		t.Fatal(err)
	}
	_, expTok := b.invite(InviteOptions{TTL: time.Hour})
	_, boundTok := b.invite(InviteOptions{Email: "right@example.com"})
	b.clock.advance(2 * time.Hour)

	var pages []string
	for i, tok := range []string{"nope", used, revTok, expTok, boundTok, strings.Repeat("A", 43)} {
		email := fmt.Sprintf("p%d@example.com", i)
		if tok == boundTok {
			email = "wrong@example.com"
		}
		r := b.browser(fmt.Sprintf("10.0.1.%d", i)).form("/signup", url.Values{"email": {email}, "password": {testPassword}, "invite": {tok}})
		if r.StatusCode != 403 {
			t.Fatalf("token %d: status %d", i, r.StatusCode)
		}
		pages = append(pages, stripCSRF(r.body))
		// The same through the GET page.
		g := b.browser(fmt.Sprintf("10.0.2.%d", i)).get("/signup?invite=" + url.QueryEscape(tok))
		if tok != boundTok && tok != "" && g.StatusCode != 403 {
			t.Fatalf("token %d: GET status %d", i, g.StatusCode)
		}
	}
	for i, p := range pages {
		if p != pages[0] {
			t.Fatalf("page %d differs from page 0:\n%s\n----\n%s", i, p, pages[0])
		}
	}
	if strings.Contains(pages[4], "right@") || strings.Contains(pages[4], "r***") {
		t.Fatal("the bound address must never be echoed")
	}
	if countUsers(b) != 1 {
		t.Fatalf("only the first sign-up should exist, got %d", countUsers(b))
	}
}

var csrfFieldRE = regexp.MustCompile(`name="_csrf" value="[^"]*"`)

func stripCSRF(s string) string { return csrfFieldRE.ReplaceAllString(s, `name="_csrf"`) }

// A bound invitation used from another address is refused and is NOT spent: seeing a link must
// not be enough to burn it.
func TestBoundInviteChecksTheAddressAndSurvivesAMismatch(t *testing.T) {
	b := inviteBed(t)
	_, tok := b.invite(InviteOptions{Email: "Ann@Example.com"})
	if r := b.browser("10.0.0.1").signupWith(tok, "mallory@example.com"); r.StatusCode != 403 {
		t.Fatalf("mismatch: %d", r.StatusCode)
	}
	var used int
	_ = b.srv.db.QueryRow("SELECT COUNT(used_at) FROM invites").Scan(&used)
	if used != 0 {
		t.Fatal("a mismatch must not spend the invitation")
	}
	if r := b.browser("10.0.0.2").signupWith(tok, " ANN@example.com "); r.StatusCode != 200 {
		t.Fatalf("the bound address (any case) must work: %d", r.StatusCode)
	}
}

// Spending on every accepted submission means holding a token reveals nothing about an address:
// new, pending and existing addresses all leave it spent and answer alike.
func TestSpendingTheInviteLeaksNoAccountExistence(t *testing.T) {
	b := inviteBed(t)
	// An existing verified account.
	_, first := b.invite(InviteOptions{})
	br := b.browser("10.0.0.1")
	br.get("/signup?invite=" + first)
	br.form("/signup", url.Values{"email": {"ann@example.com"}, "password": {testPassword}, "invite": {first}})
	link := b.link(b.waitMail("ann@example.com", "Verify"))
	br.form(link, url.Values{"password": {testPassword}})
	// (not enrolled yet: a "pending" one) and a fresh address
	b.clock.advance(5 * time.Minute) // past the per-address mail cooldown
	var bodies []string
	for i, email := range []string{"ann@example.com", "newcomer@example.com"} {
		_, tok := b.invite(InviteOptions{})
		bb := b.browser(fmt.Sprintf("10.0.3.%d", i))
		r := bb.signupWith(tok, email)
		if r.StatusCode != 200 {
			t.Fatalf("%s: %d", email, r.StatusCode)
		}
		bodies = append(bodies, strings.ReplaceAll(stripCSRF(r.body), email, "EMAIL"))
		if again := b.browser(fmt.Sprintf("10.0.4.%d", i)).signupWith(tok, "other"+email); again.StatusCode != 403 {
			t.Fatalf("%s: the token must be spent after an accepted submission, got %d", email, again.StatusCode)
		}
	}
	if bodies[0] != bodies[1] {
		t.Fatal("an existing and a new address must get the same answer")
	}
}

// Without an invitation nobody can re-sign-up a pending address (that replaces its password).
func TestRepeatSignupNeedsAnInvitationToo(t *testing.T) {
	b := inviteBed(t)
	_, tok := b.invite(InviteOptions{})
	b.browser("10.0.0.1").signupWith(tok, "ann@example.com")
	b.waitMail("ann@example.com", "Verify")
	var before string
	_ = b.srv.db.QueryRow("SELECT password_hash FROM users WHERE email = 'ann@example.com'").Scan(&before)

	r := b.browser("10.0.0.9").form("/signup", url.Values{"email": {"ann@example.com"}, "password": {"another long passphrase"}})
	if r.StatusCode != 403 {
		t.Fatalf("repeat sign-up without an invitation: %d", r.StatusCode)
	}
	var after string
	_ = b.srv.db.QueryRow("SELECT password_hash FROM users WHERE email = 'ann@example.com'").Scan(&after)
	if before != after {
		t.Fatal("the pending account's password must not change")
	}
	// Resend and continue stay open: they create nothing and change no password.
	b.clock.advance(5 * time.Minute)
	br := b.browser("10.0.0.9")
	br.get("/signup/continue")
	if r := br.form("/signup/resend", url.Values{"email": {"ann@example.com"}}); r.StatusCode != 200 {
		t.Fatalf("resend: %d", r.StatusCode)
	}
	if r := br.get("/signup/continue"); r.StatusCode != 200 {
		t.Fatalf("continue: %d", r.StatusCode)
	}
}

func TestClosedModeRefusesEvenWithAnInvitation(t *testing.T) {
	b := inviteBed(t)
	_, tok := b.invite(InviteOptions{})
	b.srv.cfg.Signup = SignupClosed
	br := b.browser("10.0.0.1")
	if r := br.get("/signup?invite=" + tok); r.StatusCode != 403 || !strings.Contains(r.body, "closed") {
		t.Fatalf("GET: %d", r.StatusCode)
	}
	if r := br.form("/signup", url.Values{"email": {"ann@example.com"}, "password": {testPassword}, "invite": {tok}}); r.StatusCode != 403 || countUsers(b) != 0 {
		t.Fatalf("POST: %d", r.StatusCode)
	}
	var used int
	_ = b.srv.db.QueryRow("SELECT COUNT(used_at) FROM invites").Scan(&used)
	if used != 0 {
		t.Fatal("closed must not spend invitations")
	}
}

func TestOpenModeNeedsNoInvitation(t *testing.T) {
	b := newBed(t) // open
	br := b.browser("10.0.0.1")
	br.signup("ann@example.com", testPassword)
	if !br.signedIn() {
		t.Fatal("open mode lets anyone sign up")
	}
}

// One token, many sign-ups at once: exactly one account and one spent invitation.
func TestInviteIsSingleUseUnderRace(t *testing.T) {
	b := inviteBed(t)
	_, tok := b.invite(InviteOptions{})
	var ok, refused int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var err error
			for try := 0; try < 400; try++ {
				// Only two password hashes run at once, so the server may say "slow down". That is
				// not a verdict on the token: ask again.
				if err = b.srv.acc.Signup(fmt.Sprintf("10.9.0.%d", i), fmt.Sprintf("racer%d@example.com", i), testPassword, tok); !errors.As(err, new(*LockedError)) {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			switch err {
			case nil:
				atomic.AddInt32(&ok, 1)
			case ErrInviteInvalid:
				atomic.AddInt32(&refused, 1)
			default:
				t.Errorf("racer %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if ok != 1 || refused != 19 || countUsers(b) != 1 {
		t.Fatalf("accepted=%d refused=%d users=%d, want 1, 19, 1", ok, refused, countUsers(b))
	}
	var used int
	_ = b.srv.db.QueryRow("SELECT COUNT(*) FROM invites WHERE used_at IS NOT NULL AND used_by IS NOT NULL").Scan(&used)
	if used != 1 {
		t.Fatalf("spent invitations: %d", used)
	}
}

func TestInviteExpiryBounds(t *testing.T) {
	b := inviteBed(t)
	for _, ttl := range []time.Duration{-time.Hour, time.Minute, 59 * time.Minute, 31 * 24 * time.Hour, 1000 * 24 * time.Hour} {
		if _, _, err := b.srv.acc.CreateInvite(InviteOptions{TTL: ttl}); err != ErrBadExpiry {
			t.Errorf("ttl %v: want ErrBadExpiry, got %v", ttl, err)
		}
	}
	for _, ttl := range []time.Duration{0, time.Hour, 30 * 24 * time.Hour} {
		if _, _, err := b.srv.acc.CreateInvite(InviteOptions{TTL: ttl}); err != nil {
			t.Errorf("ttl %v: %v", ttl, err)
		}
	}
	for in, ok := range map[string]bool{"7d": true, "30d": true, "1h": true, "90m": true, "36h": true, "0": false, "": false, "-1d": false, "31d": false, "30m": false, "forever": false, "99999999999d": false, "1.5d": false} {
		if _, err := ParseExpiry(in); (err == nil) != ok {
			t.Errorf("ParseExpiry(%q): err=%v, want ok=%v", in, err, ok)
		}
	}
	if _, _, err := b.srv.acc.CreateInvite(InviteOptions{Note: strings.Repeat("x", 201)}); err != ErrBadNote {
		t.Errorf("long note: %v", err)
	}
	if _, _, err := b.srv.acc.CreateInvite(InviteOptions{Email: "not an email"}); err != ErrBadEmail {
		t.Errorf("bad email: %v", err)
	}
}

func TestInviteTokenIsOnlyStoredHashedAndNeverAudited(t *testing.T) {
	b := inviteBed(t)
	inv, tok := b.invite(InviteOptions{Note: "n", Email: "a@example.com"})
	b.browser("10.0.0.1").signupWith(tok, "a@example.com")
	var found int
	_ = b.srv.db.QueryRow("SELECT COUNT(*) FROM invites WHERE token_hash = ? OR id = ? OR note = ?", tok, tok, tok).Scan(&found)
	if found != 0 {
		t.Fatal("the token itself must not be stored")
	}
	rows, _ := b.srv.db.Query("SELECT detail FROM audit")
	defer rows.Close()
	sawCreate, sawUse := false, false
	for rows.Next() {
		var d string
		_ = rows.Scan(&d)
		if strings.Contains(d, tok) {
			t.Fatal("the token is in the audit log")
		}
		sawCreate = sawCreate || strings.Contains(d, inv.ID) && strings.Contains(d, "expiresInSeconds")
		sawUse = sawUse || strings.Contains(d, inv.ID) && !strings.Contains(d, "expiresInSeconds")
	}
	if !sawCreate || !sawUse {
		t.Fatalf("create and use are audited by invite id: %v %v", sawCreate, sawUse)
	}
}

func TestRevokeIsIdempotent(t *testing.T) {
	b := inviteBed(t)
	inv, tok := b.invite(InviteOptions{})
	for i := 0; i < 3; i++ {
		if err := b.srv.acc.RevokeInvite("", "", inv.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.srv.acc.RevokeInvite("", "", "inv_nothing"); err == nil {
		t.Fatal("an unknown id is an error")
	}
	var n int
	_ = b.srv.db.QueryRow("SELECT COUNT(*) FROM audit WHERE event = 'invite_revoked'").Scan(&n)
	if n != 1 {
		t.Fatalf("one audit entry for one real revocation, got %d", n)
	}
	if r := b.browser("10.0.0.1").signupWith(tok, "a@example.com"); r.StatusCode != 403 {
		t.Fatalf("a revoked invitation must not work: %d", r.StatusCode)
	}
}

func hashOf(tok string) string { return auth.HashToken(tok) }

func pwHash(b *bed, email string) string {
	var h string
	_ = b.srv.db.QueryRow("SELECT password_hash FROM users WHERE email = ?", email).Scan(&h)
	return h
}

func usedBy(b *bed, token string) (used bool, by *string) {
	var u *int64
	_ = b.srv.db.QueryRow("SELECT used_at, used_by FROM invites WHERE token_hash = ?", hashOf(token)).Scan(&u, &by)
	return u != nil, by
}

// One invitee cannot lock another out of a pending sign-up: an unbound invitation leaves the
// pending password alone (and is spent, with the same answer).
func TestUnboundInviteCannotReplaceAPendingPassword(t *testing.T) {
	b := inviteBed(t)
	_, annTok := b.invite(InviteOptions{Email: "ann@example.com"})
	annBr := b.browser("10.0.0.1")
	annBr.signupWith(annTok, "ann@example.com")
	annLink := b.link(b.waitMail("ann@example.com", "Verify"))
	before := pwHash(b, "ann@example.com")

	b.clock.advance(5 * time.Minute)
	_, malTok := b.invite(InviteOptions{})
	r := b.browser("10.0.0.9").signupAs(malTok, "ann@example.com", "mallory chose this passphrase")
	if r.StatusCode != 200 || !strings.Contains(r.body, "Check your email") {
		t.Fatalf("same answer as any accepted sign-up: %d", r.StatusCode)
	}
	if pwHash(b, "ann@example.com") != before {
		t.Fatal("an unbound invitation must not replace a pending password")
	}
	if used, by := usedBy(b, malTok); !used || by != nil {
		t.Fatalf("the invitation is spent and records no account: used=%v by=%v", used, by)
	}
	// Ann's own link still works with her own password.
	if r := annBr.form(annLink, url.Values{"password": {testPassword}}); r.StatusCode != 303 {
		t.Fatalf("ann's verification link must still work: %d %s", r.StatusCode, r.body)
	}
}

// The recovery path: an invitation bound to the real owner of an address does replace a squatter's
// pending sign-up.
func TestBoundInviteReplacesASquattersPendingSignup(t *testing.T) {
	b := inviteBed(t)
	_, squatTok := b.invite(InviteOptions{})
	b.browser("10.0.0.9").signupAs(squatTok, "ann@example.com", "squatter passphrase here")
	squatLink := b.link(b.waitMail("ann@example.com", "Verify"))
	before := pwHash(b, "ann@example.com")

	b.clock.advance(5 * time.Minute)
	_, annTok := b.invite(InviteOptions{Email: "ann@example.com"})
	if r := b.browser("10.0.0.1").signupWith(annTok, "ann@example.com"); r.StatusCode != 200 {
		t.Fatalf("ann: %d", r.StatusCode)
	}
	if pwHash(b, "ann@example.com") == before {
		t.Fatal("the bound invitation replaces the squatter's password")
	}
	if used, by := usedBy(b, annTok); !used || by == nil {
		t.Fatal("the bound invitation records the account")
	}
	if r := b.browser("10.0.0.9").form(squatLink, url.Values{"password": {"squatter passphrase here"}}); r.StatusCode == 303 {
		t.Fatal("the squatter's old link must be dead")
	}
}

func TestExistingAccountIsNotCreditedWithTheInvitation(t *testing.T) {
	b := inviteBed(t)
	_, first := b.invite(InviteOptions{})
	br := b.browser("10.0.0.1")
	br.signupWith(first, "ann@example.com")
	b.clock.advance(5 * time.Minute)
	_, tok := b.invite(InviteOptions{Email: "ann@example.com"})
	b.browser("10.0.0.2").signupWith(tok, "ann@example.com") // pending, owned by a bound one
	// Now make her complete: use the CLI-free way: mark verified+enrolled directly.
	_, _ = b.srv.db.Exec("UPDATE users SET email_verified_at = 1, totp_enrolled_at = 1 WHERE email = 'ann@example.com'")
	b.clock.advance(5 * time.Minute)
	_, again := b.invite(InviteOptions{})
	b.browser("10.0.0.3").signupWith(again, "ann@example.com")
	if used, by := usedBy(b, again); !used || by != nil {
		t.Fatalf("an invitation used against an existing account is spent and credits nobody: used=%v by=%v", used, by)
	}
}

func (br *browser) signupAs(token, email, password string) *resp {
	br.b.t.Helper()
	br.get("/signup?invite=" + url.QueryEscape(token))
	return br.form("/signup", url.Values{"email": {email}, "password": {password}, "invite": {token}})
}
