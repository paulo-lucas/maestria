package store

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestOpenMigratesAndUsesV2FileName(t *testing.T) {
	dir := t.TempDir()
	v1 := filepath.Join(dir, "maestria.db")
	if err := os.WriteFile(v1, []byte("v1 database"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion() {
		t.Fatalf("version %d err %v", version, err)
	}
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal mode %q err %v", mode, err)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(v1); string(b) != "v1 database" {
		t.Fatal("V1 database must not be touched")
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(filepath.Join(dir, FileName))
		if st.Mode().Perm()&0o077 != 0 {
			t.Fatalf("db mode %v", st.Mode().Perm())
		}
	}
}

func TestReopenIsIdempotentAndRefusesNewerSchema(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetKV(db, "k", "v"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok, err := GetKV(db, "k"); err != nil || !ok || v != "v" {
		t.Fatalf("kv after reopen: %q %v %v", v, ok, err)
	}
	if _, err := db.Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := Open(dir); err == nil {
		t.Fatal("a newer schema must be refused")
	}
}

func TestAuditIsAppendOnly(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("INSERT INTO audit (ts, event) VALUES (1, 'x')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE audit SET event = 'y'"); err == nil {
		t.Fatal("UPDATE on audit must fail")
	}
	if _, err := db.Exec("DELETE FROM audit"); err == nil {
		t.Fatal("DELETE on audit must fail")
	}
}

func TestKV(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, ok, _ := GetKV(db, "missing"); ok {
		t.Fatal("missing key")
	}
	_ = SetKV(db, "a", "1")
	_ = SetKV(db, "a", "2")
	if v, _, _ := GetKV(db, "a"); v != "2" {
		t.Fatalf("upsert: %q", v)
	}
	_ = DeleteKV(db, "a")
	if _, ok, _ := GetKV(db, "a"); ok {
		t.Fatal("delete")
	}
}

// A database created by the first migration alone upgrades in place and keeps its rows.
func TestUpgradeFromSchemaVersion1(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	db, err := OpenPath(path)
	if err != nil {
		t.Fatal(err)
	}
	// Rewind to what a build with only migration 1 would have left behind.
	if _, err := db.Exec("ALTER TABLE pending_logins DROP COLUMN password_ok"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO pending_logins (id, created_at, expires_at, ip) VALUES ('p', 1, 2, '1.2.3.4')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO audit (ts, event) VALUES (1, 'kept')"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db, err = OpenPath(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, ok int
	_ = db.QueryRow("PRAGMA user_version").Scan(&version)
	if version != SchemaVersion() || SchemaVersion() < 2 {
		t.Fatalf("version %d", version)
	}
	if err := db.QueryRow("SELECT password_ok FROM pending_logins WHERE id = 'p'").Scan(&ok); err != nil || ok != 0 {
		t.Fatalf("existing pending login after upgrade: %d %v", ok, err)
	}
	var ev string
	if err := db.QueryRow("SELECT event FROM audit").Scan(&ev); err != nil || ev != "kept" {
		t.Fatalf("audit row after upgrade: %q %v", ev, err)
	}
}
