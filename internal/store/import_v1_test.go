package store

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestImportV1IdempotentAndReadOnly(t *testing.T) {
	dir := t.TempDir()
	v1Path := filepath.Join(dir, V1FileName)
	v1DB, err := sql.Open("sqlite", v1Path)
	if err != nil {
		t.Fatal(err)
	}
	// Create V1 schema without postponed_until (authentic V1 shape).
	_, err = v1DB.Exec(`
		CREATE TABLE inbox (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			workspace_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			body TEXT NOT NULL,
			options_json TEXT,
			response TEXT,
			author TEXT,
			status TEXT NOT NULL DEFAULT 'open',
			created_at INTEGER NOT NULL,
			answered_at INTEGER,
			acknowledged INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE chat_messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			workspace_id TEXT NOT NULL,
			sender TEXT NOT NULL,
			body TEXT NOT NULL,
			created_at INTEGER NOT NULL
		);
		CREATE TABLE events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			workspace_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			payload TEXT NOT NULL,
			created_at INTEGER NOT NULL
		);
		INSERT INTO inbox (id, workspace_id, kind, body, response, author, status, created_at, answered_at, acknowledged)
		VALUES
			(1, 'ws1', 'approve', 'Deploy v1?', 'Yes', 'Maestro', 'answered', 1000, 1100, 1),
			(2, 'ws1', 'input', 'Version?', NULL, NULL, 'open', 1200, NULL, 0);
		INSERT INTO chat_messages (id, workspace_id, sender, body, created_at)
		VALUES
			(1, 'ws1', 'human', 'hello', 1000),
			(2, 'ws1', 'maestro', 'hi there', 1050);
		INSERT INTO events (id, workspace_id, kind, payload, created_at)
		VALUES
			(1, 'ws1', 'chat', '{"body":"hello"}', 1000);
	`)
	if err != nil {
		t.Fatal(err)
	}
	if err := v1DB.Close(); err != nil {
		t.Fatal(err)
	}
	v1BytesBefore, err := os.ReadFile(v1Path)
	if err != nil {
		t.Fatal(err)
	}

	v2DB, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer v2DB.Close()

	// Pre-populate V2 with an existing inbox id=1 row to prove conflict-safe allocation.
	if _, err := v2DB.Exec(
		"INSERT INTO inbox (id, workspace_id, kind, body, created_at) VALUES (1, 'ws2', 'notice', 'Existing V2 notice', 900)",
	); err != nil {
		t.Fatal(err)
	}

	r1, err := ImportV1(v2DB, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if r1.InboxImported != 2 || r1.ChatImported != 2 || r1.EventsImported != 1 {
		t.Fatalf("unexpected report 1: %+v", r1)
	}

	// Re-run must be completely idempotent.
	r2, err := ImportV1(v2DB, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if r2.InboxImported != 0 || r2.InboxSkipped != 2 || r2.ChatImported != 0 || r2.ChatSkipped != 2 || r2.EventsImported != 0 || r2.EventsSkipped != 1 {
		t.Fatalf("expected full skip on rerun: %+v", r2)
	}

	var inboxCount, chatCount, evCount int
	_ = v2DB.QueryRow("SELECT COUNT(*) FROM inbox").Scan(&inboxCount)
	_ = v2DB.QueryRow("SELECT COUNT(*) FROM chat_messages").Scan(&chatCount)
	_ = v2DB.QueryRow("SELECT COUNT(*) FROM events").Scan(&evCount)
	if inboxCount != 3 || chatCount != 2 || evCount != 1 {
		t.Fatalf("row counts mismatch: inbox=%d chat=%d events=%d", inboxCount, chatCount, evCount)
	}

	// Verify V1 file bytes remained bit-for-bit untouched.
	v1BytesAfter, err := os.ReadFile(v1Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(v1BytesBefore, v1BytesAfter) {
		t.Fatal("V1 database was modified by ImportV1")
	}
}
