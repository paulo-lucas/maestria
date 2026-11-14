// Package events manages the per-workspace event log and persistent wait cursors.
package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	KindChat          = "chat"
	KindInboxResponse = "inbox_response"
	KindInboxDismiss  = "inbox_dismiss"
	KindStandby       = "standby"
	KindResume        = "resume"

	DefaultCursor   = "default"
	defaultLimit    = 500
	defaultPollStep = 150 * time.Millisecond
)

// Event represents one persisted event row in the events table.
type Event struct {
	ID          int64           `json:"id"`
	WorkspaceID string          `json:"workspace_id"`
	Kind        string          `json:"kind"`
	Payload     json.RawMessage `json:"payload"`
	CreatedAt   int64           `json:"created_at"`
}

// WaitOptions configures a blocking or immediate event wait.
type WaitOptions struct {
	Cursor       string
	SinceID      int64
	SinceSet     bool
	FromNow      bool
	Timeout      time.Duration
	PollInterval time.Duration
	Now          func() int64
}

// Append writes a new event for workspaceID and returns the persisted row.
func Append(db *sql.DB, workspaceID, kind string, payload any, nowMs int64) (Event, error) {
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return Event{}, fmt.Errorf("events: marshal payload: %w", err)
	}
	res, err := db.Exec(
		"INSERT INTO events (workspace_id, kind, payload, created_at) VALUES (?, ?, ?, ?)",
		workspaceID, kind, string(raw), nowMs,
	)
	if err != nil {
		return Event{}, fmt.Errorf("events: insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Event{}, err
	}
	return Event{
		ID:          id,
		WorkspaceID: workspaceID,
		Kind:        kind,
		Payload:     json.RawMessage(raw),
		CreatedAt:   nowMs,
	}, nil
}

// LatestID returns the highest event ID for workspaceID (0 when none exist).
func LatestID(db *sql.DB, workspaceID string) (int64, error) {
	var id sql.NullInt64
	err := db.QueryRow("SELECT MAX(id) FROM events WHERE workspace_id = ?", workspaceID).Scan(&id)
	if err != nil {
		return 0, err
	}
	if !id.Valid {
		return 0, nil
	}
	return id.Int64, nil
}

// LatestGlobalID returns the highest event ID across all workspaces.
func LatestGlobalID(db *sql.DB) (int64, error) {
	var id sql.NullInt64
	err := db.QueryRow("SELECT MAX(id) FROM events").Scan(&id)
	if err != nil {
		return 0, err
	}
	if !id.Valid {
		return 0, nil
	}
	return id.Int64, nil
}

// Since returns events for workspaceID with id > afterID in ascending order.
func Since(db *sql.DB, workspaceID string, afterID int64, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = defaultLimit
	}
	rows, err := db.Query(
		"SELECT id, workspace_id, kind, payload, created_at FROM events WHERE workspace_id = ? AND id > ? ORDER BY id ASC LIMIT ?",
		workspaceID, afterID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

// SinceGlobal returns events across all workspaces with id > afterID in ascending order.
func SinceGlobal(db *sql.DB, afterID int64, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = defaultLimit
	}
	rows, err := db.Query(
		"SELECT id, workspace_id, kind, payload, created_at FROM events WHERE id > ? ORDER BY id ASC LIMIT ?",
		afterID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

func scanEvents(rows *sql.Rows) ([]Event, error) {
	out := []Event{}
	for rows.Next() {
		var ev Event
		var payload string
		if err := rows.Scan(&ev.ID, &ev.WorkspaceID, &ev.Kind, &payload, &ev.CreatedAt); err != nil {
			return nil, err
		}
		ev.Payload = json.RawMessage(payload)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// GetCursor returns the saved last_event_id for (workspaceID, name), if present.
func GetCursor(db *sql.DB, workspaceID, name string) (int64, bool, error) {
	var lastID int64
	err := db.QueryRow(
		"SELECT last_event_id FROM wait_cursors WHERE workspace_id = ? AND name = ?",
		workspaceID, name,
	).Scan(&lastID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return lastID, true, nil
}

// SetCursor inserts or updates a persistent wait cursor.
func SetCursor(db *sql.DB, workspaceID, name string, lastEventID, nowMs int64) error {
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}
	_, err := db.Exec(`
		INSERT INTO wait_cursors (workspace_id, name, last_event_id, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(workspace_id, name) DO UPDATE SET
			last_event_id = excluded.last_event_id,
			updated_at    = excluded.updated_at
	`, workspaceID, name, lastEventID, nowMs)
	return err
}

// Wait blocks until one or more new events arrive for workspaceID, the timeout expires,
// or ctx is cancelled. When events are returned (or cursor initialized), the cursor is
// updated when opts.Cursor != "" (or defaults to DefaultCursor when SinceSet is false).
func Wait(ctx context.Context, db *sql.DB, workspaceID string, opts WaitOptions) ([]Event, int64, error) {
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = func() int64 { return time.Now().UnixMilli() }
	}
	poll := opts.PollInterval
	if poll <= 0 {
		poll = defaultPollStep
	}
	cursorName := opts.Cursor
	if cursorName == "" {
		cursorName = DefaultCursor
	}

	var afterID int64
	switch {
	case opts.SinceSet:
		afterID = opts.SinceID
	case opts.FromNow:
		latest, err := LatestID(db, workspaceID)
		if err != nil {
			return nil, 0, err
		}
		afterID = latest
		if err := SetCursor(db, workspaceID, cursorName, afterID, nowFn()); err != nil {
			return nil, 0, err
		}
	default:
		saved, ok, err := GetCursor(db, workspaceID, cursorName)
		if err != nil {
			return nil, 0, err
		}
		if ok {
			afterID = saved
		} else {
			afterID = 0
		}
	}

	var deadline <-chan time.Time
	var timer *time.Timer
	if opts.Timeout > 0 {
		timer = time.NewTimer(opts.Timeout)
		defer timer.Stop()
		deadline = timer.C
	}

	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	for {
		evs, err := Since(db, workspaceID, afterID, defaultLimit)
		if err != nil {
			return nil, afterID, err
		}
		if len(evs) > 0 {
			lastID := evs[len(evs)-1].ID
			if !opts.SinceSet || opts.Cursor != "" {
				if err := SetCursor(db, workspaceID, cursorName, lastID, nowFn()); err != nil {
					return nil, afterID, err
				}
			}
			return evs, lastID, nil
		}

		select {
		case <-ctx.Done():
			return nil, afterID, ctx.Err()
		case <-deadline:
			return []Event{}, afterID, nil
		case <-ticker.C:
		}
	}
}
