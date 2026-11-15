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
	"strings"
	"time"

	"github.com/paulo-lucas/maestria/internal/privfile"

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
	// 3: connector core tables — human tasks (inbox with postpone), workspace chat, per-workspace events, and wait cursors.
	`
	CREATE TABLE IF NOT EXISTS inbox (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		workspace_id    TEXT NOT NULL,
		kind            TEXT NOT NULL CHECK (kind IN ('notice','approve','input','choice')),
		body            TEXT NOT NULL,
		options_json    TEXT,
		response        TEXT,
		author          TEXT,
		status          TEXT NOT NULL CHECK (status IN ('open','answered','dismissed')) DEFAULT 'open',
		created_at      INTEGER NOT NULL,
		answered_at     INTEGER,
		acknowledged    INTEGER NOT NULL DEFAULT 0,
		postponed_until INTEGER
	);
	CREATE INDEX IF NOT EXISTS idx_inbox_ws_status ON inbox (workspace_id, status, id);

	CREATE TABLE IF NOT EXISTS chat_messages (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		workspace_id TEXT NOT NULL,
		sender       TEXT NOT NULL CHECK (sender IN ('human','maestro','system')),
		body         TEXT NOT NULL,
		created_at   INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_chat_ws_id ON chat_messages (workspace_id, id);

	CREATE TABLE IF NOT EXISTS events (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		workspace_id TEXT NOT NULL,
		kind         TEXT NOT NULL,
		payload      TEXT NOT NULL,
		created_at   INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_events_ws_id ON events (workspace_id, id);

	CREATE TABLE IF NOT EXISTS wait_cursors (
		workspace_id  TEXT NOT NULL,
		name          TEXT NOT NULL,
		last_event_id INTEGER NOT NULL,
		updated_at    INTEGER NOT NULL,
		PRIMARY KEY (workspace_id, name)
	);
	`,
	// 4: floors module — planned (future), active (present), and landed/cancelled (past) floors.
	`
	CREATE TABLE IF NOT EXISTS floors (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		workspace_id TEXT NOT NULL,
		name         TEXT NOT NULL,
		brief        TEXT NOT NULL DEFAULT '',
		state        TEXT NOT NULL CHECK (state IN ('planned','active','landed','cancelled')),
		ongoing      INTEGER NOT NULL DEFAULT 0,
		progress     REAL,
		status       TEXT NOT NULL DEFAULT 'working' CHECK (status IN ('working','blocked','review','done')),
		status_text  TEXT NOT NULL DEFAULT '',
		agents_json  TEXT NOT NULL DEFAULT '[]',
		outcome      TEXT NOT NULL DEFAULT '',
		sort_order   INTEGER NOT NULL DEFAULT 0,
		planned_at   INTEGER,
		started_at   INTEGER,
		ended_at     INTEGER,
		updated_at   INTEGER NOT NULL,
		rev          INTEGER NOT NULL DEFAULT 1
	);
	CREATE INDEX IF NOT EXISTS idx_floors_ws_state ON floors (workspace_id, state, sort_order, id);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_floors_live_name ON floors (workspace_id, name) WHERE state IN ('planned','active');
	`,
	// 5: Maestri Wire. Inbox gains the agent_prompt kind (a Y/n an agent waits on, mirrored from
	// the Wire feed) and a ref that keeps it unique while open; agents get a short brief the Maestro
	// writes through the CLI.
	`
	CREATE TABLE inbox_v5 (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		workspace_id    TEXT NOT NULL,
		kind            TEXT NOT NULL CHECK (kind IN ('notice','approve','input','choice','agent_prompt')),
		body            TEXT NOT NULL,
		options_json    TEXT,
		response        TEXT,
		author          TEXT,
		status          TEXT NOT NULL CHECK (status IN ('open','answered','dismissed')) DEFAULT 'open',
		created_at      INTEGER NOT NULL,
		answered_at     INTEGER,
		acknowledged    INTEGER NOT NULL DEFAULT 0,
		postponed_until INTEGER,
		ref             TEXT
	);
	INSERT INTO inbox_v5 (id, workspace_id, kind, body, options_json, response, author, status, created_at, answered_at, acknowledged, postponed_until)
		SELECT id, workspace_id, kind, body, options_json, response, author, status, created_at, answered_at, acknowledged, postponed_until FROM inbox;
	DROP TABLE inbox;
	ALTER TABLE inbox_v5 RENAME TO inbox;
	CREATE INDEX IF NOT EXISTS idx_inbox_ws_status ON inbox (workspace_id, status, id);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_inbox_open_ref ON inbox (workspace_id, ref) WHERE ref IS NOT NULL AND status = 'open';

	CREATE TABLE IF NOT EXISTS agent_briefs (
		workspace_id TEXT NOT NULL,
		agent        TEXT NOT NULL,
		brief        TEXT NOT NULL,
		updated_at   INTEGER NOT NULL,
		PRIMARY KEY (workspace_id, agent)
	);
	`,
	// 6: hosted mode. Devices (browsers) approved on this machine, and pairings waiting for the
	// local approval. The connector is the authority: the cloud only mirrors the public keys.
	`
	CREATE TABLE IF NOT EXISTS devices (
		id           TEXT PRIMARY KEY,   -- hex(sha256(spki))[:32]
		spki         TEXT NOT NULL,      -- SPKI DER, base64url
		label        TEXT NOT NULL,
		state        TEXT NOT NULL CHECK (state IN ('active','revoked','expired')),
		approved_at  INTEGER NOT NULL,   -- epoch ms
		last_used_at INTEGER NOT NULL,
		revoked_at   INTEGER
	);
	CREATE TABLE IF NOT EXISTS device_pairings (
		pair_id        TEXT PRIMARY KEY,
		commit_hash    TEXT NOT NULL,    -- base64url sha256 sent before the key
		label          TEXT NOT NULL,
		ua             TEXT NOT NULL,
		r              TEXT NOT NULL,    -- connector random, base64url
		spki           TEXT,
		nonce          TEXT,
		code           TEXT,             -- 6 digits, set once revealed and verified
		state          TEXT NOT NULL CHECK (state IN ('pending','revealed','approved','rejected','expired','mismatch')),
		reported_state TEXT NOT NULL DEFAULT '',
		created_at     INTEGER NOT NULL,
		expires_at     INTEGER NOT NULL
	);
	`,
	// 7: the pairing attempt budget. One row per pairing offered, failed reveal and wrong code
	// typed on the machine, so the limits survive a restart and cannot be reset by the cloud.
	`
	CREATE TABLE IF NOT EXISTS pair_events (
		id   INTEGER PRIMARY KEY AUTOINCREMENT,
		at   INTEGER NOT NULL,           -- epoch ms
		kind TEXT NOT NULL CHECK (kind IN ('offer','mismatch','bad_code'))
	);
	CREATE INDEX IF NOT EXISTS idx_pair_events_kind_at ON pair_events (kind, at);
	`,
	// 8: pairings are looked up by state and expiry on every offer and every list, and finished
	// ones are pruned (see devices.sweep), so the table stays small however many offers arrive.
	`
	CREATE INDEX IF NOT EXISTS idx_device_pairings_state_expires ON device_pairings (state, expires_at);
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
func OpenPath(path string) (*sql.DB, error) { return OpenWith(path, migrations) }

// OpenWith opens a database at an explicit path and applies the given ordered, append-only
// migrations. The hosted server uses it with its own list.
func OpenWith(path string, migs []string) (*sql.DB, error) {
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
	if err := pingWhileBusy(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	if err := MigrateWith(db, migs); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// pingWhileBusy makes the first connection. Switching a brand-new database to WAL needs the file
// to itself and, unlike other statements, does not wait for the busy timeout: when another
// process is creating the same database at that moment, try again until it is done.
func pingWhileBusy(db *sql.DB) error {
	deadline := time.Now().Add(10 * time.Second)
	for wait := 5 * time.Millisecond; ; wait = min(wait*2, 200*time.Millisecond) {
		err := db.Ping()
		if err == nil || !isBusy(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(wait)
	}
}

func isBusy(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_BUSY") || strings.Contains(msg, "database is locked")
}

// Migrate applies pending migrations in one exclusive transaction.
func Migrate(db *sql.DB) error { return MigrateWith(db, migrations) }

// MigrateWith applies the given migrations (PRAGMA user_version records how many ran).
//
// Two processes can open a fresh database at the same time (`maestria serve` and an agent's
// `maestria post`). The write lock is therefore taken first (BEGIN IMMEDIATE waits for it, up to
// the busy timeout) and the schema version is read only after that, so the one that waited sees
// what the other applied and runs only what is still missing. All pending migrations share the
// transaction: a failure leaves the database as it was.
func MigrateWith(db *sql.DB, migrations []string) (err error) {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("store: migrate: lock the database: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	var version int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("store: database schema version %d is newer than this build (%d); upgrade maestria", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		if _, err := conn.ExecContext(ctx, migrations[i]); err != nil {
			return fmt.Errorf("store: migration %d: %w", i+1, err)
		}
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			return fmt.Errorf("store: migration %d: %w", i+1, err)
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("store: migrate: commit: %w", err)
	}
	committed = true
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
