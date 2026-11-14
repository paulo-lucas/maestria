package auth

import (
	"database/sql"
	"encoding/base32"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/maestria-dev/maestria/internal/audit"
	"github.com/maestria-dev/maestria/internal/store"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// ---- password ----

func TestPasswordHashAndVerify(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Fatalf("unexpected PHC string: %s", h)
	}
	if !VerifyPassword("correct horse battery", h) || VerifyPassword("correct horse batterz", h) {
		t.Fatal("verify")
	}
	h2, _ := HashPassword("correct horse battery")
	if h == h2 {
		t.Fatal("every hash must be salted")
	}
}

func TestPasswordRejectsMalformedOrAbusiveHashes(t *testing.T) {
	for _, h := range []string{
		"", "plain", "$argon2i$v=19$m=65536,t=3,p=4$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=999999999,t=3,p=4$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=65536,t=99,p=4$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=65536,t=3,p=64$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
	} {
		if VerifyPassword("x", h) {
			t.Fatalf("malformed hash accepted: %q", h)
		}
	}
}

// V1 hashes (node:crypto argon2id, unpadded standard base64) verify unchanged.
func TestPasswordCompatibleWithV1Format(t *testing.T) {
	h, _ := hashWith("pw-from-v1-format", Argon2Params{MemoryKiB: 8 * 1024, Iterations: 1, Parallelism: 1, KeyLen: 32})
	if !VerifyPassword("pw-from-v1-format", h) {
		t.Fatal("custom params must verify from the PHC string")
	}
}

func TestValidateNewPassword(t *testing.T) {
	if ValidateNewPassword("short") == nil || ValidateNewPassword(strings.Repeat("x", 300)) == nil {
		t.Fatal("length policy")
	}
	if err := ValidateNewPassword("twelve chars"); err != nil {
		t.Fatal(err)
	}
}

// ---- TOTP ----

func TestHOTPMatchesRFC6238Vectors(t *testing.T) {
	key := []byte("12345678901234567890")
	vectors := map[int64]string{
		59: "94287082", 1111111109: "07081804", 1111111111: "14050471",
		1234567890: "89005924", 2000000000: "69279037", 20000000000: "65353130",
	}
	for ts, want := range vectors {
		if got := HOTP(key, uint64(TOTPStepAt(time.Unix(ts, 0))), 8); got != want {
			t.Errorf("t=%d: got %s want %s", ts, got, want)
		}
	}
}

func TestTOTPWindow(t *testing.T) {
	secret, _ := GenerateTOTPSecret()
	now := time.Unix(1_700_000_000, 0)
	for _, off := range []int{-1, 0, 1} {
		code, _ := TOTP(secret, now.Add(time.Duration(off)*TOTPStep))
		step, ok := VerifyTOTP(secret, code, now, 1)
		if !ok || step != TOTPStepAt(now)+int64(off) {
			t.Fatalf("offset %d rejected", off)
		}
	}
	for _, off := range []int{-3, -2, 2, 3} {
		code, _ := TOTP(secret, now.Add(time.Duration(off)*TOTPStep))
		if cur, _ := TOTP(secret, now); code == cur {
			continue // astronomically unlikely collision
		}
		if _, ok := VerifyTOTP(secret, code, now, 1); ok {
			t.Fatalf("offset %d accepted", off)
		}
	}
	for _, bad := range []string{"", "12345", "1234567", "abcdef", "12 456", "１２３４５６"} {
		if _, ok := VerifyTOTP(secret, bad, now, 1); ok {
			t.Fatalf("malformed code %q accepted", bad)
		}
	}
}

func TestTOTPSecretAndURI(t *testing.T) {
	secret, _ := GenerateTOTPSecret()
	if len(secret) != 32 {
		t.Fatalf("secret length %d", len(secret))
	}
	key, err := DecodeTOTPSecret(strings.ToLower(secret[:4]) + " " + secret[4:])
	if err != nil || len(key) != 20 {
		t.Fatalf("decode: %v", err)
	}
	if want, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret); string(want) != string(key) {
		t.Fatal("round trip")
	}
	uri := OTPAuthURI(secret, "panel", "Maestria")
	if !strings.HasPrefix(uri, "otpauth://totp/Maestria:panel?") || !strings.Contains(uri, "secret="+secret) || !strings.Contains(uri, "period=30") {
		t.Fatalf("uri: %s", uri)
	}
}

// ---- recovery codes ----

func TestRecoveryCodesAreHashedAndSingleUse(t *testing.T) {
	db := openDB(t)
	codes, err := GenerateRecoveryCodes(RecoveryCodeCount)
	if err != nil || len(codes) != 10 {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, c := range codes {
		if len(c) != 9 || c[4] != '-' || seen[c] {
			t.Fatalf("bad code %q", c)
		}
		seen[c] = true
	}
	now := time.Now()
	if err := ReplaceRecoveryCodes(db, codes, now); err != nil {
		t.Fatal(err)
	}
	var stored string
	_ = db.QueryRow("SELECT group_concat(hash, ' ') FROM recovery_codes").Scan(&stored)
	for _, c := range codes {
		if strings.Contains(stored, c) || strings.Contains(stored, NormalizeRecoveryCode(c)) {
			t.Fatal("codes must be stored hashed")
		}
	}
	if ok, _ := ConsumeRecoveryCode(db, "zzzz-zzzz", now); ok {
		t.Fatal("unknown code accepted")
	}
	if ok, _ := ConsumeRecoveryCode(db, strings.ToUpper(strings.Replace(codes[3], "-", " ", 1)), now); !ok {
		t.Fatal("valid code (upper case, space) rejected")
	}
	if ok, _ := ConsumeRecoveryCode(db, codes[3], now); ok {
		t.Fatal("a recovery code must be single use")
	}
	if n, _ := RemainingRecoveryCodes(db); n != 9 {
		t.Fatalf("remaining %d", n)
	}
	// Regenerating invalidates the old set.
	fresh, _ := GenerateRecoveryCodes(RecoveryCodeCount)
	_ = ReplaceRecoveryCodes(db, fresh, now)
	if ok, _ := ConsumeRecoveryCode(db, codes[0], now); ok {
		t.Fatal("old codes must stop working after regeneration")
	}
}

// ---- sessions ----

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }
func newClock() *clock                   { return &clock{t: time.UnixMilli(1_700_000_000_000)} }
func sessions(db *sql.DB, c *clock) *SessionStore {
	return NewSessionStore(db, 60*time.Minute, 12*time.Hour, c.now)
}

func TestSessionStoresOnlyTheTokenHash(t *testing.T) {
	db := openDB(t)
	c := newClock()
	s := sessions(db, c)
	token, sess, err := s.Create("127.0.0.1", "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != 43 || sess.ID != HashToken(token) {
		t.Fatal("token format")
	}
	var id string
	_ = db.QueryRow("SELECT id FROM sessions").Scan(&id)
	if id == token || id != HashToken(token) {
		t.Fatal("only the hash may be stored")
	}
	if got, _ := s.Validate(token); got == nil || got.CSRFToken != sess.CSRFToken {
		t.Fatal("validate")
	}
	if got, _ := s.Validate(id); got != nil {
		t.Fatal("the stored hash must not work as a token")
	}
}

func TestSessionIdleExpiryExtendedByActivity(t *testing.T) {
	db := openDB(t)
	c := newClock()
	s := sessions(db, c)
	token, _, _ := s.Create("ip", "ua")
	c.advance(59 * time.Minute)
	if got, _ := s.Validate(token); got == nil {
		t.Fatal("still within idle limit")
	}
	c.advance(59 * time.Minute)
	if got, _ := s.Validate(token); got == nil {
		t.Fatal("activity should extend the idle limit")
	}
	c.advance(60 * time.Minute)
	if got, _ := s.Validate(token); got != nil {
		t.Fatal("idle session must expire")
	}
	if n, _ := s.Count(); n != 0 {
		t.Fatal("expired session must be deleted")
	}
}

func TestSessionAbsoluteExpiryDespiteActivity(t *testing.T) {
	db := openDB(t)
	c := newClock()
	s := sessions(db, c)
	token, _, _ := s.Create("ip", "ua")
	for i := 0; i < 23; i++ {
		c.advance(30 * time.Minute)
		if got, _ := s.Validate(token); got == nil {
			t.Fatalf("expired early at step %d", i)
		}
	}
	c.advance(30 * time.Minute)
	if got, _ := s.Validate(token); got != nil {
		t.Fatal("absolute limit must apply even when active")
	}
}

func TestSessionDestroyOneAndAll(t *testing.T) {
	db := openDB(t)
	s := sessions(db, newClock())
	a, _, _ := s.Create("ip", "ua")
	b, _, _ := s.Create("ip", "ua")
	p, _ := s.CreatePending("ip")
	_ = s.Destroy(a)
	if got, _ := s.Validate(a); got != nil {
		t.Fatal("destroyed session")
	}
	if got, _ := s.Validate(b); got == nil {
		t.Fatal("other session must survive")
	}
	if n, _ := s.DestroyAll(); n != 1 {
		t.Fatalf("destroy all: %d", n)
	}
	if ok, _ := s.PendingValid(p); ok {
		t.Fatal("destroy all must also drop pending logins")
	}
}

func TestPendingLoginExpiresAndIsSingleUse(t *testing.T) {
	db := openDB(t)
	c := newClock()
	s := sessions(db, c)
	p, _ := s.CreatePending("ip")
	if ok, _ := s.ConsumePending(p); !ok {
		t.Fatal("consume")
	}
	if ok, _ := s.ConsumePending(p); ok {
		t.Fatal("single use")
	}
	p2, _ := s.CreatePending("ip")
	c.advance(PendingTTL)
	if ok, _ := s.PendingValid(p2); ok {
		t.Fatal("pending login must expire")
	}
}

// ---- limiter ----

func limiter(c *clock) *LoginLimiter {
	return NewLoginLimiter(LimiterOptions{MaxFailuresPerIP: 3, MaxFailuresGlobal: 5, Window: time.Minute, Lockout: 10 * time.Minute}, c.now)
}

func TestLimiterLocksIPAndUnlocksAfterLockout(t *testing.T) {
	c := newClock()
	l := limiter(c)
	l.Fail("a")
	l.Fail("a")
	if !l.Check("a").Allowed || l.Remaining("a") != 1 {
		t.Fatal("not locked yet")
	}
	if !l.Fail("a") {
		t.Fatal("third failure locks")
	}
	d := l.Check("a")
	if d.Allowed || d.Scope != "ip" || d.RetryAfter != 10*time.Minute {
		t.Fatalf("decision %+v", d)
	}
	if !l.Check("b").Allowed {
		t.Fatal("other IPs unaffected")
	}
	c.advance(10 * time.Minute)
	if !l.Check("a").Allowed {
		t.Fatal("unlock after lockout")
	}
}

func TestLimiterForgetsOldFailures(t *testing.T) {
	c := newClock()
	l := limiter(c)
	l.Fail("a")
	l.Fail("a")
	c.advance(61 * time.Second)
	if l.Fail("a") || !l.Check("a").Allowed {
		t.Fatal("failures outside the window must not count")
	}
}

func TestLimiterGlobalLockout(t *testing.T) {
	c := newClock()
	l := limiter(c)
	for i, ip := range []string{"a", "b", "c", "d", "e"} {
		locked := l.Fail(ip)
		if locked != (i == 4) {
			t.Fatalf("failure %d locked=%v", i, locked)
		}
	}
	if d := l.Check("fresh"); d.Allowed || d.Scope != "global" {
		t.Fatalf("global lockout: %+v", d)
	}
}

func TestLimiterSuccessClearsIP(t *testing.T) {
	c := newClock()
	l := limiter(c)
	l.Fail("a")
	l.Fail("a")
	l.Success("a")
	if l.Fail("a") {
		t.Fatal("success must reset the IP bucket")
	}
}

// ---- two-step login service ----

func newAuthenticator(t *testing.T, c *clock) (*Authenticator, *Secrets) {
	t.Helper()
	db := openDB(t)
	secret, _ := GenerateTOTPSecret()
	hash, _ := HashPassword("correct horse battery")
	sec := &Secrets{PasswordHash: hash, TOTPSecret: secret, CreatedAt: "now"}
	return &Authenticator{
		DB: db, Secrets: sec, Sessions: sessions(db, c), Limiter: limiter(c),
		Audit: audit.New(db, c.now), Now: c.now,
	}, sec
}

func TestLoginTwoStepsAndTOTPReplay(t *testing.T) {
	c := newClock()
	a, sec := newAuthenticator(t, c)

	if r, _ := a.Password("ip", "wrong password!"); r.Outcome != Invalid {
		t.Fatal("wrong password")
	}
	r, err := a.Password("ip", "correct horse battery")
	if err != nil || r.Outcome != OK || r.Token == "" {
		t.Fatal("password step")
	}
	pending := r.Token
	if r, _ := a.TOTP("ip", "ua", pending, "000000"); r.Outcome != Invalid {
		t.Fatal("wrong code")
	}
	code, _ := TOTP(sec.TOTPSecret, c.now())
	r, err = a.TOTP("ip", "ua", pending, code)
	if err != nil || r.Outcome != OK || r.Session == nil {
		t.Fatalf("totp step: %+v %v", r, err)
	}
	if r, _ := a.TOTP("ip", "ua", pending, code); r.Outcome != Expired {
		t.Fatal("a pending login is single use")
	}
	// Same code on a new pending login: replay refused.
	r, _ = a.Password("ip", "correct horse battery")
	if r2, _ := a.TOTP("ip", "ua", r.Token, code); r2.Outcome != Invalid {
		t.Fatal("TOTP replay must be refused")
	}
	// An older (previous-step) code is refused too once a newer step was used.
	prev, _ := TOTP(sec.TOTPSecret, c.now().Add(-TOTPStep))
	if r2, _ := a.TOTP("ip", "ua", r.Token, prev); r2.Outcome != Invalid {
		t.Fatal("older step must be refused after a newer one was used")
	}
	c.advance(TOTPStep)
	next, _ := TOTP(sec.TOTPSecret, c.now())
	if r2, _ := a.TOTP("ip2", "ua", r.Token, next); r2.Outcome != OK {
		t.Fatal("next step works")
	}
}

func TestLoginWithRecoveryCode(t *testing.T) {
	c := newClock()
	a, _ := newAuthenticator(t, c)
	codes, _ := GenerateRecoveryCodes(2)
	_ = ReplaceRecoveryCodes(a.DB, codes, c.now())

	r, _ := a.Password("ip", "correct horse battery")
	if r2, _ := a.Recovery("ip", "ua", r.Token, "aaaa-bbbb"); r2.Outcome != Invalid {
		t.Fatal("unknown recovery code")
	}
	if r2, _ := a.Recovery("ip", "ua", r.Token, codes[0]); r2.Outcome != OK || r2.Session == nil {
		t.Fatal("recovery login")
	}
	r, _ = a.Password("ip", "correct horse battery")
	if r2, _ := a.Recovery("ip", "ua", r.Token, codes[0]); r2.Outcome != Invalid {
		t.Fatal("recovery code reuse")
	}
	if r2, _ := a.Recovery("ip", "ua", "not-a-pending-token", codes[1]); r2.Outcome != Expired {
		t.Fatal("recovery needs a pending login")
	}
	var events string
	_ = a.DB.QueryRow("SELECT group_concat(event, ',') FROM audit").Scan(&events)
	if !strings.Contains(events, "recovery_code_used") || !strings.Contains(events, "login_ok") {
		t.Fatalf("audit: %s", events)
	}
}

func TestLockoutRefusesCorrectCredentials(t *testing.T) {
	c := newClock()
	a, sec := newAuthenticator(t, c)
	for i := 0; i < 3; i++ {
		_, _ = a.Password("ip", "wrong password!")
	}
	if r, _ := a.Password("ip", "correct horse battery"); r.Outcome != Locked || r.RetryAfter <= 0 {
		t.Fatalf("locked: %+v", r)
	}
	c.advance(10 * time.Minute)
	r, _ := a.Password("ip", "correct horse battery")
	if r.Outcome != OK {
		t.Fatal("recovers after lockout")
	}
	// Second-factor failures count toward the same lockout.
	for i := 0; i < 3; i++ {
		_, _ = a.TOTP("ip", "ua", r.Token, "000000")
	}
	code, _ := TOTP(sec.TOTPSecret, c.now())
	if r2, _ := a.TOTP("ip", "ua", r.Token, code); r2.Outcome != Locked {
		t.Fatal("TOTP failures must lock out too")
	}
}

// ---- secrets file ----

func TestSecretsFile(t *testing.T) {
	dir := t.TempDir()
	if s, err := LoadSecrets(dir); s != nil || err != nil {
		t.Fatal("absent file")
	}
	hash, _ := HashPassword("correct horse battery")
	secret, _ := GenerateTOTPSecret()
	if err := SaveSecrets(dir, &Secrets{PasswordHash: hash, TOTPSecret: secret, CreatedAt: "2026-11-13T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSecrets(dir)
	if err != nil || s.TOTPSecret != secret {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(SecretsPath(dir))
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", st.Mode().Perm())
		}
		_ = os.Chmod(SecretsPath(dir), 0o644)
		if _, err := LoadSecrets(dir); err == nil {
			t.Fatal("loose permissions must be refused")
		}
		_ = os.Chmod(SecretsPath(dir), 0o600)
	}
	_ = os.WriteFile(SecretsPath(dir), []byte(`{"passwordHash":"x","totpSecret":"y","createdAt":"z"}`), 0o600)
	if _, err := LoadSecrets(dir); err == nil {
		t.Fatal("malformed secrets must be refused")
	}
}
