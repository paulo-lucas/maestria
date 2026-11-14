package inbox

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// KindAgentPrompt is a Y/n an agent is waiting on, mirrored from the Maestri Wire feed. It is
// created and resolved by the connector, never by `maestria post`, and answering it does not
// emit a Maestro event: the agent receives the answer in its own terminal.
const KindAgentPrompt = "agent_prompt"

// Resolutions of an agent prompt.
const (
	ResponseApproved = "Approved"
	ResponseRejected = "Rejected"
)

// AgentPrompt is a Y/n currently pending in an agent terminal.
type AgentPrompt struct {
	// Ref identifies the prompt (terminal id plus a hash of its text) and is unique while open.
	Ref    string
	Agent  string
	Floor  string
	Prompt string
}

// OpenAgentPrompt is an open agent_prompt item with its ref.
type OpenAgentPrompt struct {
	ID  int64
	Ref string
}

// SyncAgentPrompts makes the open agent_prompt items of workspaceID match present: missing ones
// are created, and open ones whose ref is not in present are dismissed (the prompt was answered in
// Maestri, or the agent went away). It returns whether anything changed.
func SyncAgentPrompts(db *sql.DB, workspaceID string, present []AgentPrompt, nowMs int64) (bool, error) {
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}
	tx, err := db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	open := map[string]int64{}
	rows, err := tx.Query(`SELECT id, ref FROM inbox WHERE workspace_id = ? AND kind = 'agent_prompt' AND status = 'open' AND ref IS NOT NULL`, workspaceID)
	if err != nil {
		return false, err
	}
	for rows.Next() {
		var id int64
		var ref string
		if err := rows.Scan(&id, &ref); err != nil {
			rows.Close()
			return false, err
		}
		open[ref] = id
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	rows.Close()

	changed := false
	want := map[string]bool{}
	for _, p := range present {
		if p.Ref == "" {
			continue
		}
		want[p.Ref] = true
		if _, ok := open[p.Ref]; ok {
			continue
		}
		body := strings.TrimSpace(p.Prompt)
		if body == "" {
			body = "An agent is waiting for a Y/n answer"
		}
		if utf8.RuneCountInString(body) > MaxBodyChars {
			body = string([]rune(body)[:MaxBodyChars])
		}
		var author sql.NullString
		if a := strings.TrimSpace(p.Agent); a != "" {
			if utf8.RuneCountInString(a) > MaxAuthorChars {
				a = string([]rune(a)[:MaxAuthorChars])
			}
			author = sql.NullString{String: a, Valid: true}
		}
		if _, err := tx.Exec(`INSERT INTO inbox (workspace_id, kind, body, author, status, created_at, ref)
			VALUES (?, 'agent_prompt', ?, ?, 'open', ?, ?)`, workspaceID, body, author, nowMs, p.Ref); err != nil {
			return false, err
		}
		changed = true
	}
	for ref, id := range open {
		if want[ref] {
			continue
		}
		if _, err := tx.Exec(`UPDATE inbox SET status = 'dismissed', answered_at = ?, postponed_until = NULL WHERE id = ? AND status = 'open'`, nowMs, id); err != nil {
			return false, err
		}
		changed = true
	}
	if !changed {
		return false, nil
	}
	return true, tx.Commit()
}

// DismissAllAgentPrompts closes every open agent_prompt (all workspaces), used when the Wire link
// is lost so Needs you never shows a prompt that cannot be answered.
func DismissAllAgentPrompts(db *sql.DB, nowMs int64) (int64, error) {
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}
	res, err := db.Exec(`UPDATE inbox SET status = 'dismissed', answered_at = ?, postponed_until = NULL WHERE kind = 'agent_prompt' AND status = 'open'`, nowMs)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// AgentPromptRef returns the ref of an open agent_prompt item.
func AgentPromptRef(db *sql.DB, workspaceID string, id int64) (string, error) {
	var ref sql.NullString
	var kind, status string
	err := db.QueryRow(`SELECT kind, status, ref FROM inbox WHERE id = ? AND workspace_id = ?`, id, workspaceID).Scan(&kind, &status, &ref)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if kind != KindAgentPrompt {
		return "", fmt.Errorf("%w: item %d is not an agent prompt", ErrInvalidInput, id)
	}
	if status != StatusOpen {
		return "", ErrNotOpen
	}
	return ref.String, nil
}

// ResolveAgentPrompt records the human's answer (ResponseApproved or ResponseRejected) on an
// agent_prompt. It also accepts one the feed sync just closed, because the agent moving on is the
// usual result of the answer itself. No Maestro event is emitted.
func ResolveAgentPrompt(db *sql.DB, workspaceID string, id int64, response string, nowMs int64) (Item, error) {
	if response != ResponseApproved && response != ResponseRejected {
		return Item{}, fmt.Errorf("%w: agent prompt response must be approve or reject", ErrInvalidInput)
	}
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}
	res, err := db.Exec(`UPDATE inbox SET status = 'answered', response = ?, answered_at = ?, acknowledged = 1, postponed_until = NULL
		WHERE id = ? AND workspace_id = ? AND kind = 'agent_prompt' AND status IN ('open', 'dismissed')`, response, nowMs, id, workspaceID)
	if err != nil {
		return Item{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		it, err := getOne(db, workspaceID, id, nowMs)
		if err != nil {
			return Item{}, err
		}
		return it, ErrNotOpen
	}
	return getOne(db, workspaceID, id, nowMs)
}

// ParseAgentDecision maps the answers the UI sends to Approved or Rejected.
func ParseAgentDecision(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "approve", "approved", "yes", "y":
		return ResponseApproved, true
	case "reject", "rejected", "no", "n", "deny":
		return ResponseRejected, true
	}
	return "", false
}
