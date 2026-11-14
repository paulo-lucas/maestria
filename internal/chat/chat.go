// Package chat stores per-workspace chat messages locally until Wire connects live agent chats.
package chat

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/maestria-dev/maestria/internal/events"
	"github.com/maestria-dev/maestria/internal/inbox"
)

const (
	SenderHuman   = "human"
	SenderMaestro = "maestro"
	SenderSystem  = "system"

	MaxBodyChars = 8000
	defaultLimit = 200
)

var ErrInvalidInput = errors.New("invalid chat message")

// Message represents one row in chat_messages.
type Message struct {
	ID          int64  `json:"id"`
	WorkspaceID string `json:"workspaceId"`
	Sender      string `json:"sender"`
	Body        string `json:"body"`
	BodyHTML    string `json:"bodyHtml"`
	CreatedAt   int64  `json:"createdAt"`
}

// Send appends a chat message to workspaceID and emits a chat event when sender is "human".
func Send(db *sql.DB, workspaceID, sender, rawBody string, nowMs int64) (Message, *events.Event, error) {
	if strings.TrimSpace(workspaceID) == "" {
		return Message{}, nil, fmt.Errorf("%w: workspaceId is required", ErrInvalidInput)
	}
	if sender != SenderHuman && sender != SenderMaestro && sender != SenderSystem {
		return Message{}, nil, fmt.Errorf("%w: invalid sender %q", ErrInvalidInput, sender)
	}
	body := strings.TrimSpace(rawBody)
	if body == "" || utf8.RuneCountInString(body) > MaxBodyChars {
		return Message{}, nil, fmt.Errorf("%w: body must be 1..%d characters", ErrInvalidInput, MaxBodyChars)
	}
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}

	tx, err := db.Begin()
	if err != nil {
		return Message{}, nil, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(
		"INSERT INTO chat_messages (workspace_id, sender, body, created_at) VALUES (?, ?, ?, ?)",
		workspaceID, sender, body, nowMs,
	)
	if err != nil {
		return Message{}, nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Message{}, nil, err
	}

	var emitted *events.Event
	if sender == SenderHuman {
		ev, err := events.Append(tx, workspaceID, events.KindChat, map[string]any{
			"id":     id,
			"sender": sender,
			"body":   body,
		}, nowMs)
		if err != nil {
			return Message{}, nil, err
		}
		emitted = &ev
	}

	if err := tx.Commit(); err != nil {
		return Message{}, nil, err
	}

	return Message{
		ID:          id,
		WorkspaceID: workspaceID,
		Sender:      sender,
		Body:        body,
		BodyHTML:    inbox.RenderMarkdown(body),
		CreatedAt:   nowMs,
	}, emitted, nil
}

// List returns messages for workspaceID with id > afterID ordered chronologically.
func List(db *sql.DB, workspaceID string, afterID int64, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = defaultLimit
	}
	rows, err := db.Query(`
		SELECT id, workspace_id, sender, body, created_at
		FROM chat_messages
		WHERE workspace_id = ? AND id > ?
		ORDER BY id ASC
		LIMIT ?
	`, workspaceID, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Message{}
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.WorkspaceID, &m.Sender, &m.Body, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.BodyHTML = inbox.RenderMarkdown(m.Body)
		out = append(out, m)
	}
	return out, rows.Err()
}
