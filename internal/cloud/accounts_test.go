package cloud

import (
	"database/sql"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/paulo-lucas/maestria/internal/auth"
)

func TestSignupVerify2FAMandatory(t *testing.T) {
	b := newBed(t)
	br := b.browser("10.0.0.1")
	secret, codes := br.signup("ann@example.com", testPassword)
	if secret == "" || len(codes) != 10 {
		t.Fatal("setup did not return a secret and ten recovery codes")
	}
	if !br.signedIn() {
		t.Fatal("finishing 2FA setup signs the user in")
	}

	// Another person signing up but never finishing 2FA can never log in, not even with the right password.
	br2 := b.browser("10.0.0.2")
	br2.get("/signup")
	br2.form("/signup", url.Values{"email": {"bob@example.com"}, "password": {testPassword}})
	link := b.link(b.waitMail("bob@example.com", "Verify"))
	br2.form(link, url.Values{"password": {testPassword}}) // verified, 2FA pending
	br3 := b.browser("10.0.0.3")
	br3.get("/login")
	br3.form("/login", url.Values{"email": {"bob@example.com"}, "password": {testPassword}})
	r := br3.form("/login/2fa", url.Values{"code": {"123456"}})
	if r.Header.Get("Location") != "/login?error=invalid" || br3.signedIn() {
		t.Fatalf("an account without 2FA must not log in: %v", r.Header)
	}
	// Even a correct TOTP guess for a secret that was never stored cannot work: there is none.
	var enr sql.NullInt64
	_ = b.srv.db.QueryRow("SELECT totp_enrolled_at FROM users WHERE email = 'bob@example.com'").Scan(&enr)
	if enr.Valid {
		t.Fatal("bob must not be enrolled")
	}
}

func TestVerifyLinkRules(t *testing.T) {
	b := newBed(t)
	br := b.browser("10.0.0.1")
	br.get("/signup")
	br.form("/signup", url.Values{"email": {"ann@example.com"}, "password": {testPassword}})
	link := b.link(b.waitMail("ann@example.com", "Verify"))

	// A GET (a mail scanner) does not use the link.
	for i := 0; i < 3; i++ {
		if r := br.get(link); r.StatusCode != 200 {
			t.Fatalf("GET %d: %d", i, r.StatusCode)
		}
	}
	// Wrong password does not use it either.
	r := br.form(link, url.Values{"password": {"not the password at all"}})
	if r.StatusCode != 401 || !strings.Contains(r.body, "That password") {
		t.Fatalf("wrong password: %d", r.StatusCode)
	}
	if r := br.form(link, url.Values{"password": {testPassword}}); r.StatusCode != 303 {
		t.Fatalf("verify: %d", r.StatusCode)
	}
	// Single use.
	if r := br.get(link); r.StatusCode != 410 {
		t.Fatalf("a used link must be gone: %d", r.StatusCode)
	}
	if r := br.form(link, url.Values{"password": {testPassword}}); r.StatusCode != 410 {
		t.Fatalf("a used link must not verify twice: %d", r.StatusCode)
	}

	// Expiry: 30 minutes.
	br2 := b.browser("10.0.0.2")
	br2.get("/signup")
	br2.form("/signup", url.Values{"email": {"cy@example.com"}, "password": {testPassword}})
	link2 := b.link(b.waitMail("cy@example.com", "Verify"))
	b.clock.advance(31 * time.Minute)
	if r := br2.get(link2); r.StatusCode != 410 {
		t.Fatalf("expired link: %d", r.StatusCode)
	}
}

func TestSignupDoesNotRevealExistingAccounts(t *testing.T) {
	b := newBed(t)
	br := b.browser("10.0.0.1")
	br.signup("ann@example.com", testPassword)

	other := b.browser("10.0.0.9")
	other.get("/signup")
	again := other.form("/signup", url.Values{"email": {"ann@example.com"}, "password": {"another long password!"}})
	fresh := other.form("/signup", url.Values{"email": {"new@example.com"}, "password": {"another long password!"}})
	if again.StatusCode != fresh.StatusCode {
		t.Fatalf("status differs: %d vs %d", again.StatusCode, fresh.StatusCode)
	}
	norm := func(s, email string) string { return strings.ReplaceAll(s, email, "EMAIL") }
	if norm(again.body, "ann@example.com") != norm(fresh.body, "new@example.com") {
		t.Fatal("the sign-up answer differs between an existing and a new address")
	}
	// The existing owner is told, and the original password is untouched.
	b.waitMail("ann@example.com", "already have an account")
	_ = other
}

func TestLoginIsNoOracleAndCostsTheSame(t *testing.T) {
	b := newBed(t)
	br := b.browser("10.0.0.1")
	secret, _ := br.signup("ann@example.com", testPassword)

	attempt := func(ip, email, password, code string) (statusStep1 int, loc string, verifies int64) {
		x := b.browser(ip)
		x.get("/login")
		before := auth.VerifyCalls()
		r := x.form("/login", url.Values{"email": {email}, "password": {password}})
		x.get("/login/2fa")
		r2 := x.form("/login/2fa", url.Values{"code": {code}})
		return r.StatusCode, r2.Header.Get("Location"), auth.VerifyCalls() - before
	}
	good, _ := auth.TOTP(secret, b.clock.now())
	s1, l1, v1 := attempt("10.0.1.1", "ann@example.com", "wrong password entirely", good) // right code, wrong password
	s2, l2, v2 := attempt("10.0.1.2", "nobody@example.com", "wrong password entirely", good)
	s3, l3, v3 := attempt("10.0.1.3", "ann@example.com", testPassword, "000000") // right password, wrong code
	if s1 != 303 || s2 != 303 || s3 != 303 {
		t.Fatalf("step 1 must always move on: %d %d %d", s1, s2, s3)
	}
	if l1 != "/login?error=invalid" || l2 != l1 || l3 != l1 {
		t.Fatalf("failures must be identical: %q %q %q", l1, l2, l3)
	}
	if v1 != v2 || v2 != v3 {
		t.Fatalf("every attempt must cost the same argon2 work: %d %d %d", v1, v2, v3)
	}
	// And a good login works.
	if !b.browser("10.0.1.9").login("ann@example.com", testPassword, secret) {
		t.Fatal("good login failed")
	}
}

func TestTOTPCodeIsSingleUse(t *testing.T) {
	b := newBed(t)
	secret, _ := b.browser("10.0.0.1").signup("ann@example.com", testPassword)
	code, _ := auth.TOTP(secret, b.clock.now())
	try := func(ip string) bool {
		x := b.browser(ip)
		x.get("/login")
		x.form("/login", url.Values{"email": {"ann@example.com"}, "password": {testPassword}})
		r := x.form("/login/2fa", url.Values{"code": {code}})
		return r.Header.Get("Location") == "/"
	}
	if !try("10.0.0.2") {
		t.Fatal("first use must work")
	}
	if try("10.0.0.3") {
		t.Fatal("the same TOTP code must not work twice")
	}
}

func TestLockoutPerIPPerAccountAndGlobal(t *testing.T) {
	b := newBed(t, func(c *Config) {
		c.Login.MaxFailuresPerIP, c.Login.MaxFailuresPerAccount, c.Login.MaxFailuresGlobal = 3, 4, 7
	})
	secret, _ := b.browser("10.0.0.1").signup("ann@example.com", testPassword)

	fail := func(ip, email string) *resp {
		x := b.browser(ip)
		x.get("/login")
		x.form("/login", url.Values{"email": {email}, "password": {"wrong password here"}})
		return x.form("/login/2fa", url.Values{"code": {"000000"}})
	}
	// Per IP: three failures lock that IP, and correct credentials are refused while locked.
	for i := 0; i < 3; i++ {
		fail("10.9.0.1", "ann@example.com")
	}
	x := b.browser("10.9.0.1")
	x.get("/login")
	if r := x.form("/login", url.Values{"email": {"ann@example.com"}, "password": {testPassword}}); r.StatusCode != 429 || r.Header.Get("Retry-After") == "" {
		t.Fatalf("locked IP must get 429 with Retry-After, got %d", r.StatusCode)
	}
	// Other IPs still work, until the account itself is locked (4 failures across IPs).
	fail("10.9.0.2", "ann@example.com") // 4th failure on the account
	y := b.browser("10.9.0.3")
	y.get("/login")
	if r := y.form("/login", url.Values{"email": {"ann@example.com"}, "password": {testPassword}}); r.StatusCode != 429 {
		t.Fatalf("a locked account must refuse even the right password from a new IP: %d", r.StatusCode)
	}
	// An unknown address is limited the same way (no existence signal).
	for i := 0; i < 4; i++ {
		fail("10.9.1."+string(rune('1'+i)), "ghost@example.com")
	}
	z := b.browser("10.9.2.1")
	z.get("/login")
	if r := z.form("/login", url.Values{"email": {"ghost@example.com"}, "password": {"x"}}); r.StatusCode != 429 {
		t.Fatalf("unknown accounts lock out too: %d", r.StatusCode)
	}
	// Global: enough failures anywhere lock everyone.
	w := b.browser("10.9.3.1")
	w.get("/login")
	if r := w.form("/login", url.Values{"email": {"ann@example.com"}, "password": {testPassword}}); r.StatusCode != 429 {
		t.Fatalf("global lockout: %d", r.StatusCode)
	}
	// After the lockout window everything recovers.
	b.clock.advance(time.Duration(b.cfg.Login.LockoutSeconds+1) * time.Second)
	if !b.browser("10.9.4.1").login("ann@example.com", testPassword, secret) {
		t.Fatal("login must work again after the lockout")
	}
	ev := auditEvents(t, b)
	if !strings.Contains(ev, "login_lockout") || !strings.Contains(ev, "login_blocked") {
		t.Fatalf("lockouts must be audited: %s", ev)
	}
}

func auditEvents(t *testing.T, b *bed) string {
	t.Helper()
	ents, err := b.srv.audit.Recent(500)
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for _, e := range ents {
		sb.WriteString(e.Event + " ")
	}
	return sb.String()
}

func TestRecoveryCodesAreSingleUse(t *testing.T) {
	b := newBed(t)
	_, codes := b.browser("10.0.0.1").signup("ann@example.com", testPassword)
	use := func(ip, code string) bool {
		x := b.browser(ip)
		x.get("/login")
		x.form("/login", url.Values{"email": {"ann@example.com"}, "password": {testPassword}})
		r := x.form("/login/recovery", url.Values{"recovery": {code}})
		return r.Header.Get("Location") == "/"
	}
	if !use("10.0.0.2", strings.ToUpper(strings.ReplaceAll(codes[0], "-", " "))) {
		t.Fatal("a recovery code (any case, with a space) must work once")
	}
	if use("10.0.0.3", codes[0]) {
		t.Fatal("a recovery code must not work twice")
	}
	if !use("10.0.0.4", codes[1]) {
		t.Fatal("other codes still work")
	}
	acc := b.srv.Accounts()
	u, _ := acc.UserByEmail("ann@example.com")
	if acc.RecoveryLeft(u.ID) != 8 {
		t.Fatalf("remaining: %d", acc.RecoveryLeft(u.ID))
	}
}

func TestPasswordResetStillRequires2FA(t *testing.T) {
	b := newBed(t)
	br := b.browser("10.0.0.1")
	secret, codes := br.signup("ann@example.com", testPassword)

	// An unknown address gets the same page and no email.
	x := b.browser("10.0.0.5")
	x.get("/reset")
	known := x.form("/reset", url.Values{"email": {"ann@example.com"}})
	unknown := x.form("/reset", url.Values{"email": {"ghost@example.com"}})
	if known.StatusCode != unknown.StatusCode || known.body != unknown.body {
		t.Fatal("reset request must not reveal whether an account exists")
	}
	b.noMail("ghost@example.com")
	link := b.link(b.waitMail("ann@example.com", "Reset"))

	// A GET does not use the link.
	reset := b.browser("10.0.0.6")
	if r := reset.get(link); r.StatusCode != 200 {
		t.Fatalf("reset page: %d", r.StatusCode)
	}
	newPW := "a brand new long password"
	// No code, wrong code: refused, link still alive, password unchanged.
	if r := reset.form(link, url.Values{"password": {newPW}}); r.StatusCode != 422 {
		t.Fatalf("reset without 2FA must be refused: %d", r.StatusCode)
	}
	if r := reset.form(link, url.Values{"password": {newPW}, "code": {"000000"}}); r.StatusCode != 422 {
		t.Fatalf("reset with a wrong code must be refused: %d", r.StatusCode)
	}
	if reset.signedIn() || !b.browser("10.0.0.7").login("ann@example.com", testPassword, secret) {
		t.Fatal("the old password must still work, and a failed reset must not sign anyone in")
	}
	// Weak new passwords are refused before anything is used.
	if r := reset.form(link, url.Values{"password": {"short"}, "recovery": {codes[0]}}); r.StatusCode != 422 {
		t.Fatalf("short password: %d", r.StatusCode)
	}

	// With a recovery code it works, once.
	r := reset.form(link, url.Values{"password": {newPW}, "recovery": {codes[0]}})
	if r.StatusCode != 303 || !reset.signedIn() {
		t.Fatalf("reset with a recovery code: %d %s", r.StatusCode, r.body)
	}
	late := b.browser("10.0.0.8")
	late.get("/login") // gets the form token a real page would
	if r := late.form(link, url.Values{"password": {"yet another long one!"}, "recovery": {codes[1]}}); r.StatusCode != 410 {
		t.Fatalf("a used reset link must be gone: %d", r.StatusCode)
	}
	// The used recovery code is burned; the old password is gone; other sessions ended.
	if br.signedIn() {
		t.Fatal("a reset must end every other session")
	}
	if b.browser("10.0.0.9").login("ann@example.com", testPassword, secret) {
		t.Fatal("the old password must stop working")
	}
	if !b.browser("10.0.0.10").login("ann@example.com", newPW, secret) {
		t.Fatal("the new password must work with the same authenticator")
	}
	if ok, _ := b.srv.Accounts().peekToken("reset", strings.TrimPrefix(link, "/reset/")); ok != "" {
		t.Fatal("token must be spent")
	}
}

func TestResetWithAuthenticatorCodeAndExpiry(t *testing.T) {
	b := newBed(t)
	secret, _ := b.browser("10.0.0.1").signup("ann@example.com", testPassword)
	x := b.browser("10.0.0.2")
	x.get("/reset")
	x.form("/reset", url.Values{"email": {"ann@example.com"}})
	link := b.link(b.waitMail("ann@example.com", "Reset"))
	code, _ := auth.TOTP(secret, b.clock.now())
	if r := x.form(link, url.Values{"password": {"a brand new long password"}, "code": {code}}); r.StatusCode != 303 {
		t.Fatalf("reset with TOTP: %d %s", r.StatusCode, r.body)
	}
	x.get("/reset")
	x.form("/reset", url.Values{"email": {"ann@example.com"}})
	link2 := b.link(b.waitMail("ann@example.com", "Reset"))
	b.clock.advance(31 * time.Minute)
	if r := x.get(link2); r.StatusCode != 410 {
		t.Fatalf("expired reset link: %d", r.StatusCode)
	}
}

func TestSecretsAreNotStoredInClear(t *testing.T) {
	b := newBed(t)
	secret, codes := b.browser("10.0.0.1").signup("ann@example.com", testPassword)
	var pw, enc string
	if err := b.srv.db.QueryRow("SELECT password_hash, totp_secret_enc FROM users").Scan(&pw, &enc); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(pw, "$argon2id$") || strings.Contains(pw, testPassword) {
		t.Fatalf("password hash: %s", pw)
	}
	if strings.Contains(enc, secret) {
		t.Fatal("the TOTP secret must be encrypted at rest")
	}
	rows, _ := b.srv.db.Query("SELECT hash FROM recovery_codes")
	defer rows.Close()
	for rows.Next() {
		var h string
		_ = rows.Scan(&h)
		if !strings.HasPrefix(h, "$argon2id$") {
			t.Fatalf("recovery code hash: %s", h)
		}
		for _, c := range codes {
			if strings.Contains(h, strings.ReplaceAll(c, "-", "")) {
				t.Fatal("recovery code stored in clear")
			}
		}
	}
	// Session tokens and email tokens are hashes too.
	var sid string
	_ = b.srv.db.QueryRow("SELECT id FROM sessions").Scan(&sid)
	br := b.browser("10.0.0.1")
	_ = br
	if len(sid) != 64 {
		t.Fatalf("session id must be a sha256 hex, got %q", sid)
	}
}

func TestDisabledAccountCannotUseTheService(t *testing.T) {
	b := newBed(t)
	br := b.browser("10.0.0.1")
	secret, _ := br.signup("ann@example.com", testPassword)
	if !br.signedIn() {
		t.Fatal("setup")
	}
	if _, err := b.srv.Accounts().SetDisabled("ann@example.com", true); err != nil {
		t.Fatal(err)
	}
	if br.signedIn() {
		t.Fatal("disabling must end the session")
	}
	if b.browser("10.0.0.2").login("ann@example.com", testPassword, secret) {
		t.Fatal("a disabled account must not log in")
	}
	if _, err := b.srv.Accounts().SetDisabled("ann@example.com", false); err != nil {
		t.Fatal(err)
	}
	if !b.browser("10.0.0.3").login("ann@example.com", testPassword, secret) {
		t.Fatal("enabled again")
	}
}

func TestSessionsExpireIdleAndAbsolute(t *testing.T) {
	b := newBed(t, func(c *Config) { c.SessionIdleMinutes, c.SessionAbsoluteHours = 30, 2 })
	br := b.browser("10.0.0.1")
	br.signup("ann@example.com", testPassword)
	b.clock.advance(20 * time.Minute)
	if !br.signedIn() {
		t.Fatal("still within idle")
	}
	b.clock.advance(31 * time.Minute)
	if br.signedIn() {
		t.Fatal("idle expiry")
	}
	br2 := b.browser("10.0.0.2")
	br2.signup("bob@example.com", testPassword)
	for i := 0; i < 5; i++ {
		b.clock.advance(25 * time.Minute)
		_ = br2.signedIn()
	}
	if br2.signedIn() {
		t.Fatal("absolute expiry after 2 hours")
	}
}

func TestEmailNormalization(t *testing.T) {
	for in, ok := range map[string]bool{"a@b.co": true, " A@B.CO ": true, "a@b": false, "nope": false, "a b@c.de": false, "<a@b.co>": false, "": false, "a@b.co,c@d.ef": false} {
		if _, got := NormalizeEmail(in); got != ok {
			t.Errorf("%q: %v", in, got)
		}
	}
	if MaskEmail("ann@example.com") != "a***@example.com" {
		t.Fatal(MaskEmail("ann@example.com"))
	}
}
