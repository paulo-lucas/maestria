package cloud

import (
	"database/sql"
	"path/filepath"

	"github.com/paulo-lucas/maestria/internal/privfile"
	"github.com/paulo-lucas/maestria/internal/store"
)

// DBFileName is the cloud database in the data dir.
const DBFileName = "cloud.db"

// migrations are ordered and append-only (PRAGMA user_version). The cloud stores accounts, 2FA,
// linked connectors, paired-device public keys, link codes and an account audit log, and nothing
// else: no workspace data, no relayed bodies.
var migrations = []string{
	`
	CREATE TABLE users (
		id                TEXT PRIMARY KEY,
		email             TEXT NOT NULL UNIQUE,         -- lower-case
		password_hash     TEXT NOT NULL,                -- argon2id
		email_verified_at INTEGER,
		totp_secret_enc   TEXT,                         -- AES-256-GCM under the server key; null until 2FA is set up
		totp_last_step    INTEGER NOT NULL DEFAULT 0,
		totp_enrolled_at  INTEGER,
		disabled_at       INTEGER,
		created_at        INTEGER NOT NULL
	);
	CREATE TABLE recovery_codes (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		hash       TEXT NOT NULL,                       -- argon2id; the code itself is never stored
		created_at INTEGER NOT NULL,
		used_at    INTEGER
	);
	CREATE INDEX recovery_user ON recovery_codes (user_id);
	CREATE TABLE sessions (
		id           TEXT PRIMARY KEY,                  -- sha256 of the cookie token
		user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		csrf_token   TEXT NOT NULL,
		created_at   INTEGER NOT NULL,
		last_seen_at INTEGER NOT NULL,
		expires_at   INTEGER NOT NULL,
		ip           TEXT NOT NULL,
		user_agent   TEXT NOT NULL
	);
	CREATE INDEX sessions_user ON sessions (user_id);
	CREATE TABLE pending_logins (
		id          TEXT PRIMARY KEY,                   -- sha256 of the cookie token
		ip          TEXT NOT NULL,
		user_id     TEXT,                               -- null when the email is unknown
		email_key   TEXT NOT NULL,                      -- hash of the claimed email, for the per-account limit
		password_ok INTEGER NOT NULL,
		created_at  INTEGER NOT NULL,
		expires_at  INTEGER NOT NULL
	);
	CREATE INDEX pending_ip ON pending_logins (ip);
	CREATE TABLE email_tokens (
		hash       TEXT PRIMARY KEY,                    -- sha256 of the emailed token
		user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		kind       TEXT NOT NULL CHECK (kind IN ('verify','reset')),
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL,
		used_at    INTEGER
	);
	CREATE TABLE setup_sessions (
		id               TEXT PRIMARY KEY,              -- sha256 of the setup cookie token
		user_id          TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		totp_pending_enc TEXT,
		created_at       INTEGER NOT NULL,
		expires_at       INTEGER NOT NULL
	);
	CREATE TABLE connectors (
		id                TEXT PRIMARY KEY,             -- cn_ + 32 hex
		user_id           TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		name              TEXT NOT NULL,
		credential_hash   TEXT NOT NULL UNIQUE,         -- sha256 hex of the credential secret
		host              TEXT NOT NULL DEFAULT '',
		os                TEXT NOT NULL DEFAULT '',
		arch              TEXT NOT NULL DEFAULT '',
		version           TEXT NOT NULL DEFAULT '',
		kill_switch       INTEGER NOT NULL DEFAULT 0,
		created_at        INTEGER NOT NULL,
		last_seen_at      INTEGER,
		connected_at      INTEGER,
		revoked_at        INTEGER,
		forget_devices_at INTEGER
	);
	CREATE INDEX connectors_user ON connectors (user_id);
	CREATE TABLE paired_devices (
		connector_id TEXT NOT NULL REFERENCES connectors(id) ON DELETE CASCADE,
		device_id    TEXT NOT NULL,
		label        TEXT NOT NULL,
		public_key   TEXT NOT NULL,                     -- SPKI, display only: the connector decides
		state        TEXT NOT NULL,
		approved_at  INTEGER,
		updated_at   INTEGER NOT NULL,
		PRIMARY KEY (connector_id, device_id)
	);
	CREATE TABLE link_codes (
		id           TEXT PRIMARY KEY,
		code_hash    TEXT NOT NULL UNIQUE,              -- sha256 of the normalized code
		kind         TEXT NOT NULL CHECK (kind IN ('web','cli')),
		user_id      TEXT REFERENCES users(id) ON DELETE CASCADE, -- set at creation (web) or when claimed (cli)
		poll_hash    TEXT,                              -- cli: sha256 of the poll token
		confirm_hash TEXT,                              -- web: sha256 of the confirmation token from redeem
		machine_json TEXT NOT NULL DEFAULT '{}',
		created_at   INTEGER NOT NULL,
		expires_at   INTEGER NOT NULL,
		claimed_at   INTEGER,
		used_at      INTEGER,
		connector_id TEXT
	);
	CREATE TABLE audit (
		id     INTEGER PRIMARY KEY AUTOINCREMENT,
		ts     INTEGER NOT NULL,
		event  TEXT NOT NULL,
		ip     TEXT,
		detail TEXT                                     -- JSON; account events carry "user". Never bodies or secrets.
	);
	CREATE INDEX audit_ts ON audit (ts);
	CREATE TRIGGER audit_no_update BEFORE UPDATE ON audit BEGIN SELECT RAISE(ABORT, 'audit log is append-only'); END;
	CREATE TRIGGER audit_no_delete BEFORE DELETE ON audit BEGIN SELECT RAISE(ABORT, 'audit log is append-only'); END;
	`,
	// Administrators and sign-up invitations. is_admin is only ever set from the server's command
	// line. An invitation is a random token stored as a hash; it is spent by the sign-up that uses
	// it (used_at is set in the same transaction as the account change).
	`
	ALTER TABLE users ADD COLUMN is_admin INTEGER NOT NULL DEFAULT 0;
	CREATE TABLE invites (
		id         TEXT PRIMARY KEY,                    -- shown to admins, used in the audit log; never the token
		token_hash TEXT NOT NULL UNIQUE,                -- sha256 of the token in the link
		created_by TEXT REFERENCES users(id) ON DELETE SET NULL, -- null when made from the server's command line
		note       TEXT NOT NULL DEFAULT '',
		email      TEXT,                                -- normalized; when set only this address can use it
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL,
		used_at    INTEGER,
		used_by    TEXT REFERENCES users(id) ON DELETE SET NULL,
		revoked_at INTEGER
	);
	CREATE INDEX invites_created ON invites (created_at);
	`,
}

// OpenDB opens (creating if needed) the cloud database in dir.
func OpenDB(dir string) (*sql.DB, error) {
	if err := privfile.EnsureDir(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, DBFileName)
	db, err := store.OpenWith(path, migrations)
	if err != nil {
		return nil, err
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		privfile.RestrictBestEffort(path + suffix)
	}
	return db, nil
}
