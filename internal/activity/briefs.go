// Package activity turns the Wire mirror into what the panel shows and does: agents grouped by
// floor with their briefs, Y/n prompts in Needs you, live floors, automatic floor history, and the
// standby interrupt and wake actions.
package activity

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// MaxBriefChars is the longest agent brief. A brief is one line.
const MaxBriefChars = 160

// MaxAgentChars bounds an agent name.
const MaxAgentChars = 64

// ErrInvalidBrief marks a brief or agent name that cannot be stored.
var ErrInvalidBrief = errors.New("invalid agent brief")

// SetBrief stores what an agent is doing, in one line.
func SetBrief(db *sql.DB, workspaceID, agent, text string, nowMs int64) error {
	agent = strings.TrimSpace(agent)
	if workspaceID == "" || agent == "" || utf8.RuneCountInString(agent) > MaxAgentChars {
		return fmt.Errorf("%w: agent name must be 1..%d characters", ErrInvalidBrief, MaxAgentChars)
	}
	brief := cleanLine(text)
	if brief == "" {
		return fmt.Errorf("%w: the brief is empty (use --clear to remove it)", ErrInvalidBrief)
	}
	if utf8.RuneCountInString(brief) > MaxBriefChars {
		return fmt.Errorf("%w: keep it to one line of at most %d characters", ErrInvalidBrief, MaxBriefChars)
	}
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}
	_, err := db.Exec(`INSERT INTO agent_briefs (workspace_id, agent, brief, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(workspace_id, agent) DO UPDATE SET brief = excluded.brief, updated_at = excluded.updated_at`,
		workspaceID, strings.ToLower(agent), brief, nowMs)
	return err
}

// ClearBrief removes an agent's brief. It reports whether there was one.
func ClearBrief(db *sql.DB, workspaceID, agent string) (bool, error) {
	res, err := db.Exec(`DELETE FROM agent_briefs WHERE workspace_id = ? AND agent = ?`, workspaceID, strings.ToLower(strings.TrimSpace(agent)))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Briefs returns the stored briefs of a workspace keyed by lowercase agent name.
func Briefs(db *sql.DB, workspaceID string) (map[string]string, error) {
	rows, err := db.Query(`SELECT agent, brief FROM agent_briefs WHERE workspace_id = ?`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			return nil, err
		}
		out[a] = b
	}
	return out, rows.Err()
}

// cleanLine collapses whitespace and drops control characters, so a brief is always one plain line.
func cleanLine(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			b.WriteRune(' ')
		case unicode.IsControl(r):
		default:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
