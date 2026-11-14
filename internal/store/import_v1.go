package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const importMapKVKey = "import_v1.imported_ids_v1"

// V1FileName is V1 Maestria's SQLite filename.
const V1FileName = "maestria.db"

// ImportReport summarizes one idempotent V1 -> V2 SQLite import run.
type ImportReport struct {
	V1Path         string `json:"v1Path"`
	InboxImported  int    `json:"inboxImported"`
	InboxSkipped   int    `json:"inboxSkipped"`
	ChatImported   int    `json:"chatImported"`
	ChatSkipped    int    `json:"chatSkipped"`
	EventsImported int    `json:"eventsImported"`
	EventsSkipped  int    `json:"eventsSkipped"`
}

type importMap struct {
	Inbox  map[int64]int64 `json:"inbox"`
	Chat   map[int64]int64 `json:"chat"`
	Events map[int64]int64 `json:"events"`
}

// ImportV1 reads V1's SQLite database read-only and imports inbox, chat_messages, and events
// into v2DB idempotently without modifying V1.
func ImportV1(v2DB *sql.DB, configDir, v1Path string) (ImportReport, error) {
	if strings.TrimSpace(v1Path) == "" {
		v1Path = filepath.Join(configDir, V1FileName)
	}
	report := ImportReport{V1Path: v1Path}
	if _, err := os.Stat(v1Path); errors.Is(err, fs.ErrNotExist) {
		return report, fmt.Errorf("V1 database not found at %s", v1Path)
	} else if err != nil {
		return report, err
	}

	dsn := "file:" + filepath.ToSlash(v1Path) + "?mode=ro"
	v1DB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return report, fmt.Errorf("open V1 database: %w", err)
	}
	defer v1DB.Close()

	// Verify V1 file is readable and non-corrupt.
	if err := v1DB.Ping(); err != nil {
		return report, fmt.Errorf("read V1 database: %w", err)
	}

	tables, err := existingTables(v1DB)
	if err != nil {
		return report, fmt.Errorf("inspect V1 schema: %w", err)
	}

	im := loadImportMap(v2DB)

	tx, err := v2DB.Begin()
	if err != nil {
		return report, err
	}
	defer tx.Rollback()

	if tables["inbox"] {
		hasPostpone, err := hasColumn(v1DB, "inbox", "postponed_until")
		if err != nil {
			return report, err
		}
		if err := importInbox(v1DB, tx, &im, hasPostpone, &report); err != nil {
			return report, err
		}
	}
	if tables["chat_messages"] {
		if err := importChat(v1DB, tx, &im, &report); err != nil {
			return report, err
		}
	}
	if tables["events"] {
		if err := importEvents(v1DB, tx, &im, &report); err != nil {
			return report, err
		}
	}

	rawMap, err := json.Marshal(im)
	if err != nil {
		return report, err
	}
	if _, err := tx.Exec(
		"INSERT INTO kv (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		importMapKVKey, string(rawMap),
	); err != nil {
		return report, err
	}

	if err := tx.Commit(); err != nil {
		return report, err
	}
	return report, nil
}

func importInbox(v1DB *sql.DB, tx *sql.Tx, im *importMap, hasPostpone bool, report *ImportReport) error {
	sel := "SELECT id, workspace_id, kind, body, options_json, response, author, status, created_at, answered_at, acknowledged"
	if hasPostpone {
		sel += ", postponed_until"
	}
	rows, err := v1DB.Query(sel + " FROM inbox ORDER BY id ASC")
	if err != nil {
		return fmt.Errorf("read V1 inbox: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id, createdAt                           int64
			wsID, kind, body, status                string
			optJSON, resp, author                   sql.NullString
			answeredAt, postponedUntil              sql.NullInt64
			acknowledged                            int
		)
		var scanErr error
		if hasPostpone {
			scanErr = rows.Scan(&id, &wsID, &kind, &body, &optJSON, &resp, &author, &status, &createdAt, &answeredAt, &acknowledged, &postponedUntil)
		} else {
			scanErr = rows.Scan(&id, &wsID, &kind, &body, &optJSON, &resp, &author, &status, &createdAt, &answeredAt, &acknowledged)
		}
		if scanErr != nil {
			return scanErr
		}
		if _, done := im.Inbox[id]; done {
			report.InboxSkipped++
			continue
		}

		// Preserve numeric V1 ID if unoccupied so Maestro references stay valid; otherwise allocate a fresh ID.
		var exists int
		_ = tx.QueryRow("SELECT COUNT(*) FROM inbox WHERE id = ?", id).Scan(&exists)
		var newID int64
		if exists == 0 {
			_, err := tx.Exec(`
				INSERT INTO inbox (id, workspace_id, kind, body, options_json, response, author, status, created_at, answered_at, acknowledged, postponed_until)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			`, id, wsID, kind, body, optJSON, resp, author, status, createdAt, answeredAt, acknowledged, postponedUntil)
			if err != nil {
				return err
			}
			newID = id
		} else {
			res, err := tx.Exec(`
				INSERT INTO inbox (workspace_id, kind, body, options_json, response, author, status, created_at, answered_at, acknowledged, postponed_until)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			`, wsID, kind, body, optJSON, resp, author, status, createdAt, answeredAt, acknowledged, postponedUntil)
			if err != nil {
				return err
			}
			newID, err = res.LastInsertId()
			if err != nil {
				return err
			}
		}
		im.Inbox[id] = newID
		report.InboxImported++
	}
	return rows.Err()
}

func importChat(v1DB *sql.DB, tx *sql.Tx, im *importMap, report *ImportReport) error {
	rows, err := v1DB.Query("SELECT id, workspace_id, sender, body, created_at FROM chat_messages ORDER BY id ASC")
	if err != nil {
		return fmt.Errorf("read V1 chat_messages: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id, createdAt    int64
			wsID, sender, b  string
		)
		if err := rows.Scan(&id, &wsID, &sender, &b, &createdAt); err != nil {
			return err
		}
		if _, done := im.Chat[id]; done {
			report.ChatSkipped++
			continue
		}
		var exists int
		_ = tx.QueryRow("SELECT COUNT(*) FROM chat_messages WHERE id = ?", id).Scan(&exists)
		var newID int64
		if exists == 0 {
			if _, err := tx.Exec(
				"INSERT INTO chat_messages (id, workspace_id, sender, body, created_at) VALUES (?, ?, ?, ?, ?)",
				id, wsID, sender, b, createdAt,
			); err != nil {
				return err
			}
			newID = id
		} else {
			res, err := tx.Exec(
				"INSERT INTO chat_messages (workspace_id, sender, body, created_at) VALUES (?, ?, ?, ?)",
				wsID, sender, b, createdAt,
			)
			if err != nil {
				return err
			}
			newID, err = res.LastInsertId()
			if err != nil {
				return err
			}
		}
		im.Chat[id] = newID
		report.ChatImported++
	}
	return rows.Err()
}

func importEvents(v1DB *sql.DB, tx *sql.Tx, im *importMap, report *ImportReport) error {
	rows, err := v1DB.Query("SELECT id, workspace_id, kind, payload, created_at FROM events ORDER BY id ASC")
	if err != nil {
		return fmt.Errorf("read V1 events: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id, createdAt         int64
			wsID, kind, payload   string
		)
		if err := rows.Scan(&id, &wsID, &kind, &payload, &createdAt); err != nil {
			return err
		}
		if _, done := im.Events[id]; done {
			report.EventsSkipped++
			continue
		}
		var exists int
		_ = tx.QueryRow("SELECT COUNT(*) FROM events WHERE id = ?", id).Scan(&exists)
		var newID int64
		if exists == 0 {
			if _, err := tx.Exec(
				"INSERT INTO events (id, workspace_id, kind, payload, created_at) VALUES (?, ?, ?, ?, ?)",
				id, wsID, kind, payload, createdAt,
			); err != nil {
				return err
			}
			newID = id
		} else {
			res, err := tx.Exec(
				"INSERT INTO events (workspace_id, kind, payload, created_at) VALUES (?, ?, ?, ?)",
				wsID, kind, payload, createdAt,
			)
			if err != nil {
				return err
			}
			newID, err = res.LastInsertId()
			if err != nil {
				return err
			}
		}
		im.Events[id] = newID
		report.EventsImported++
	}
	return rows.Err()
}

func existingTables(db *sql.DB) (map[string]bool, error) {
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type = 'table'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

func hasColumn(db *sql.DB, table, col string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid      int
			name     string
			typ      string
			notnull  int
			dflt     sql.NullString
			pk       int
		)
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == col {
			return true, nil
		}
	}
	return false, rows.Err()
}

func loadImportMap(db *sql.DB) importMap {
	im := importMap{
		Inbox:  map[int64]int64{},
		Chat:   map[int64]int64{},
		Events: map[int64]int64{},
	}
	raw, ok, err := GetKV(db, importMapKVKey)
	if err != nil || !ok || raw == "" {
		return im
	}
	_ = json.Unmarshal([]byte(raw), &im)
	if im.Inbox == nil {
		im.Inbox = map[int64]int64{}
	}
	if im.Chat == nil {
		im.Chat = map[int64]int64{}
	}
	if im.Events == nil {
		im.Events = map[int64]int64{}
	}
	return im
}
