// Package store opens the v2 SQLite database in the config directory and applies migrations.
//
// The file is maestria-v2.db, so V1's maestria.db in the same directory is never touched.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"

	"github.com/maestria-dev/maestria/internal/privfile"

	_ "modernc.org/sqlite" // registers the "sqlite" driver (pure Go, no CGO)
)

// FileName is the v2 database file name inside the config directory.
const FileName = "maestria-v2.db"

// migrations are ordered and append-only. PRAGMA user_version records how many were applied.
// Never edit a migration that has shipped: add a new one.
var migrations = []string{
	// 1: auth, sessions, key/value state, append-only audit.
	`
	CREATE TABLE sessions (
		id           TEXT PRIMARY KEY,    -- sha256 of the cookie token (the token is never stored)
		csrf_token   TEXT NOT NULL,
		created_at   INTEGER NOT NULL,    -- epoch ms
		last_seen_at INTEGER NOT NULL,
		expires_at   INTEGER NOT NULL,    -- absolute expiry, epoch ms
		ip           TEXT NOT NULL,
		user_agent   TEXT NOT NULL
	);
	CREATE TABLE pending_logins (
		id         TEXT PRIMARY KEY,      -- sha256 of the cookie token: password checked, second factor pending
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL,
		ip         TEXT NOT NULL
	);
	CREATE TABLE recovery_codes (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		hash       TEXT NOT NULL,         -- argon2id PHC string; the code itself is never stored
		created_at INTEGER NOT NULL,
		used_at    INTEGER
	);
	CREATE TABLE kv (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);
	CREATE TABLE audit (
		id     INTEGER PRIMARY KEY AUTOINCREMENT,
		ts     INTEGER NOT NULL,          -- epoch ms
		event  TEXT NOT NULL,
		ip     TEXT,
		detail TEXT
	);
	CREATE INDEX audit_ts ON audit (ts);
	CREATE TRIGGER audit_no_update BEFORE UPDATE ON audit BEGIN SELECT RAISE(ABORT, 'audit log is append-only'); END;
	CREATE TRIGGER audit_no_delete BEFORE DELETE ON audit BEGIN SELECT RAISE(ABORT, 'audit log is append-only'); END;
	`,
	// 2: a pending login remembers, server-side, whether the password was right. The browser never
	// learns it: step 2 fails the same way for a wrong password and a wrong code.
	`
	ALTER TABLE pending_logins ADD COLUMN password_ok INTEGER NOT NULL DEFAULT 0;
	`,
}

// Open opens (creating if needed) the v2 database in configDir and migrates it.
func Open(configDir string) (*sql.DB, error) {
	if err := privfile.EnsureDir(configDir); err != nil {
		return nil, fmt.Errorf("store: config dir: %w", err)
	}
	path := filepath.Join(configDir, FileName)
	db, err := OpenPath(path)
	if err != nil {
		return nil, err
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		privfile.RestrictBestEffort(path + suffix)
	}
	return db, nil
}

// OpenPath opens a database at an explicit path and migrates it.
func OpenPath(path string) (*sql.DB, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Set("_txlock", "immediate")
	p := filepath.ToSlash(path)
	if runtime.GOOS == "windows" {
		p = "/" + p // file:///C:/... form
	}
	dsn := "file:" + (&url.URL{Path: p}).EscapedPath() + "?" + q.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	if err := Migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Migrate applies pending migrations, each in its own transaction.
func Migrate(db *sql.DB) error {
	ctx := context.Background()
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("store: database schema version %d is newer than this build (%d); upgrade maestria", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migration %d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: migration %d: %w", i+1, err)
		}
	}
	return nil
}

// SchemaVersion returns the number of migrations this build knows.
func SchemaVersion() int { return len(migrations) }

// GetKV reads a key; ok is false when the key is absent.
func GetKV(db *sql.DB, key string) (value string, ok bool, err error) {
	err = db.QueryRow("SELECT value FROM kv WHERE key = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return value, err == nil, err
}

// SetKV upserts a key.
func SetKV(db *sql.DB, key, value string) error {
	_, err := db.Exec("INSERT INTO kv (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", key, value)
	return err
}

// DeleteKV removes a key.
func DeleteKV(db *sql.DB, key string) error {
	_, err := db.Exec("DELETE FROM kv WHERE key = ?", key)
	return err
}
