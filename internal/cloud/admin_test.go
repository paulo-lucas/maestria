package cloud

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/paulo-lucas/maestria/internal/store"
)

// cliEnv runs the maestria-cloud command line against a data dir.
func cliRun(t *testing.T, dir string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	env := Env{Stdout: &out, Stderr: &errb, Home: t.TempDir(), Now: time.Now, Version: "test",
		Getenv: func(k string) string {
			if k == "MAESTRIA_CLOUD_DIR" {
				return dir
			}
			return ""
		}}
	code = Run(args, env)
	return out.String(), errb.String(), code
}

func TestSignupModeConfig(t *testing.T) {
	if Defaults().Signup != SignupInvite {
		t.Fatalf("the default sign-up mode must be invite, got %q", Defaults().Signup)
	}
	dir := t.TempDir()
	cfg, err := LoadConfig(dir, func(k string) string {
		if k == "MAESTRIA_CLOUD_SIGNUP" {
			return " Closed "
		}
		return ""
	})
	if err != nil || cfg.Signup != SignupClosed {
		t.Fatalf("env override: %v %q", err, cfg.Signup)
	}
	if _, err := LoadConfig(dir, func(k string) string {
		if k == "MAESTRIA_CLOUD_SIGNUP" {
			return "everyone"
		}
		return ""
	}); err == nil {
		t.Fatal("an unknown sign-up mode must be refused")
	}
}

func TestPromoteDemoteOnlyFromTheCLI(t *testing.T) {
	b := newBed(t)
	br := b.browser("10.0.0.1")
	br.signup("ann@example.com", testPassword)

	if _, _, code := cliRun(t, b.cfg.DataDir, "user", "promote", "nobody@example.com"); code == 0 {
		t.Fatal("promoting an unknown account must fail")
	}
	// An account that never finished sign-up cannot be promoted.
	br2 := b.browser("10.0.0.2")
	br2.get("/signup")
	br2.form("/signup", map[string][]string{"email": {"bob@example.com"}, "password": {testPassword}})
	if _, _, code := cliRun(t, b.cfg.DataDir, "user", "promote", "bob@example.com"); code == 0 {
		t.Fatal("promoting an account without finished sign-up must fail")
	}

	if out, _, code := cliRun(t, b.cfg.DataDir, "user", "promote", "ANN@example.com"); code != 0 || !strings.Contains(out, "administrator") {
		t.Fatalf("promote: %d %q", code, out)
	}
	u, _ := b.srv.acc.UserByEmail("ann@example.com")
	if u == nil || !u.IsAdmin {
		t.Fatal("ann should be an admin")
	}
	if out, _, _ := cliRun(t, b.cfg.DataDir, "user", "list"); !strings.Contains(out, "admin") || !strings.Contains(out, "ROLE") {
		t.Fatalf("user list shows the role: %q", out)
	}
	if _, _, code := cliRun(t, b.cfg.DataDir, "user", "demote", "ann@example.com"); code != 0 {
		t.Fatal("demote failed")
	}
	if u, _ := b.srv.acc.UserByEmail("ann@example.com"); u.IsAdmin {
		t.Fatal("ann should no longer be an admin")
	}
	// Both are audited, with the account id and no email.
	var n int
	_ = b.srv.db.QueryRow("SELECT COUNT(*) FROM audit WHERE event IN ('user_promoted','user_demoted')").Scan(&n)
	if n != 2 {
		t.Fatalf("promote and demote are audited, got %d", n)
	}
	var leaked int
	_ = b.srv.db.QueryRow("SELECT COUNT(*) FROM audit WHERE detail LIKE '%@%'").Scan(&leaked)
	if leaked != 0 {
		t.Fatal("the audit log must not hold email addresses")
	}
}

// A database made by the previous release keeps working: the owner's account is untouched, is not
// an admin, and can be promoted from the command line.
func TestMigrationKeepsExistingAccounts(t *testing.T) {
	dir := t.TempDir()
	old, err := store.OpenWith(dir+"/"+DBFileName, migrations[:1])
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if _, err := old.Exec(`INSERT INTO users (id, email, password_hash, email_verified_at, totp_secret_enc, totp_enrolled_at, created_at)
		VALUES ('u1', 'owner@example.com', 'x', ?, 'sealed', ?, ?)`, now, now, now); err != nil {
		t.Fatal(err)
	}
	old.Close()

	db, err := OpenDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var admin int
	var email string
	if err := db.QueryRow("SELECT email, is_admin FROM users WHERE id = 'u1'").Scan(&email, &admin); err != nil || email != "owner@example.com" || admin != 0 {
		t.Fatalf("owner after migration: %v %q %d", err, email, admin)
	}
	if _, err := db.Exec("SELECT id, token_hash, created_by, note, email, created_at, expires_at, used_at, used_by, revoked_at FROM invites"); err != nil {
		t.Fatalf("invites table: %v", err)
	}
	db.Close()

	if _, _, code := cliRun(t, dir, "user", "promote", "owner@example.com"); code != 0 {
		t.Fatal("the owner can be promoted after the update")
	}
	if out, _, _ := cliRun(t, dir, "user", "list"); !strings.Contains(out, "owner@example.com") || !strings.Contains(out, "admin") {
		t.Fatalf("user list: %q", out)
	}
}
