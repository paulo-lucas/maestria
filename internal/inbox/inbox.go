package inbox

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/paulo-lucas/maestria/internal/events"
)

const (
	KindNotice  = "notice"
	KindApprove = "approve"
	KindInput   = "input"
	KindChoice  = "choice"

	StatusOpen      = "open"
	StatusAnswered  = "answered"
	StatusDismissed = "dismissed"

	MaxBodyChars     = 4000
	MaxResponseChars = 8000
	MaxOptionChars   = 80
	MaxAuthorChars   = 64
	MinChoiceOptions = 2
	MaxChoiceOptions = 4
)

var (
	ErrNotFound     = errors.New("inbox item not found")
	ErrNotOpen      = errors.New("inbox item is already closed")
	ErrInvalidInput = errors.New("invalid inbox input")
)

// Item represents one persisted human task or notice.
type Item struct {
	ID             int64    `json:"id"`
	WorkspaceID    string   `json:"workspaceId"`
	Kind           string   `json:"kind"`
	Title          string   `json:"title"`
	Body           string   `json:"body"`
	BodyHTML       string   `json:"bodyHtml"`
	Options        []string `json:"options,omitempty"`
	Response       *string  `json:"response"`
	Author         *string  `json:"author,omitempty"`
	Status         string   `json:"status"`
	CreatedAt      int64    `json:"createdAt"`
	AnsweredAt     *int64   `json:"answeredAt,omitempty"`
	Acknowledged   bool     `json:"acknowledged"`
	PostponedUntil *int64   `json:"postponedUntil,omitempty"`
	Postponed      bool     `json:"postponed"`
}

// PostInput defines parameters to create a human task.
type PostInput struct {
	Kind    string
	Body    string
	Options []string
	Author  string
}

// ListOptions filters inbox queries.
type ListOptions struct {
	WorkspaceID string
	Status      string // "", "open", "answered", "dismissed", "history", "all"
	Kind        string
	Postponed   string // "", "exclude", "only"
	NewestFirst bool
	Limit       int
	NowMs       int64
}

// WorkspaceCounts holds badge counts for one workspace.
type WorkspaceCounts struct {
	NeedsYou  int `json:"needsYou"`
	Postponed int `json:"postponed"`
	Updates   int `json:"updates"`
	History   int `json:"history"`
}

// Post creates a new inbox item after validating kind, size limits, and options.
func Post(db *sql.DB, workspaceID string, in PostInput, nowMs int64) (Item, error) {
	if strings.TrimSpace(workspaceID) == "" {
		return Item{}, fmt.Errorf("%w: workspaceId is required", ErrInvalidInput)
	}
	if !isValidKind(in.Kind) {
		return Item{}, fmt.Errorf("%w: unknown kind %q", ErrInvalidInput, in.Kind)
	}
	body := strings.TrimSpace(in.Body)
	if body == "" || utf8.RuneCountInString(body) > MaxBodyChars {
		return Item{}, fmt.Errorf("%w: body must be 1..%d characters", ErrInvalidInput, MaxBodyChars)
	}
	author := strings.TrimSpace(in.Author)
	if utf8.RuneCountInString(author) > MaxAuthorChars {
		return Item{}, fmt.Errorf("%w: author must be <= %d characters", ErrInvalidInput, MaxAuthorChars)
	}

	var optJSON sql.NullString
	if in.Kind == KindChoice {
		cleanOpts, err := validateChoiceOptions(in.Options)
		if err != nil {
			return Item{}, err
		}
		raw, _ := json.Marshal(cleanOpts)
		optJSON = sql.NullString{String: string(raw), Valid: true}
	} else if len(in.Options) > 0 {
		return Item{}, fmt.Errorf("%w: --option is only valid for choice items", ErrInvalidInput)
	}

	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}
	var authorArg sql.NullString
	if author != "" {
		authorArg = sql.NullString{String: author, Valid: true}
	}

	res, err := db.Exec(`
		INSERT INTO inbox (workspace_id, kind, body, options_json, author, status, created_at)
		VALUES (?, ?, ?, ?, ?, 'open', ?)
	`, workspaceID, in.Kind, body, optJSON, authorArg, nowMs)
	if err != nil {
		return Item{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Item{}, err
	}
	return Get(db, id, nowMs)
}

// Get loads a single inbox item by ID.
func Get(db *sql.DB, id, nowMs int64) (Item, error) {
	return getOne(db, "", id, nowMs)
}

// GetInWorkspace loads a single inbox item scoped to workspaceID.
func GetInWorkspace(db *sql.DB, workspaceID string, id, nowMs int64) (Item, error) {
	return getOne(db, workspaceID, id, nowMs)
}

func getOne(db *sql.DB, workspaceID string, id, nowMs int64) (Item, error) {
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}
	query := `SELECT id, workspace_id, kind, body, options_json, response, author, status,
		created_at, answered_at, acknowledged, postponed_until
		FROM inbox WHERE id = ?`
	args := []any{id}
	if workspaceID != "" {
		query += " AND workspace_id = ?"
		args = append(args, workspaceID)
	}
	row := db.QueryRow(query, args...)
	item, err := scanItem(row, nowMs)
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, ErrNotFound
	}
	return item, err
}

// List returns inbox items matching opts ordered newest-first (except open ascending optional? newest-first is uniform; CLI/answers uses ASC where appropriate).
func List(db *sql.DB, opts ListOptions) ([]Item, error) {
	nowMs := opts.NowMs
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 200
	}

	where := []string{"1=1"}
	args := []any{}
	if opts.WorkspaceID != "" {
		where = append(where, "workspace_id = ?")
		args = append(args, opts.WorkspaceID)
	}
	switch opts.Status {
	case "", "open":
		where = append(where, "status = 'open'")
	case "answered", "dismissed":
		where = append(where, "status = ?")
		args = append(args, opts.Status)
	case "history":
		where = append(where, "status != 'open'")
	case "all":
	default:
		return nil, fmt.Errorf("%w: invalid status filter %q", ErrInvalidInput, opts.Status)
	}
	if opts.Kind != "" {
		where = append(where, "kind = ?")
		args = append(args, opts.Kind)
	}
	switch opts.Postponed {
	case "":
	case "exclude":
		where = append(where, "(postponed_until IS NULL OR postponed_until <= ?)")
		args = append(args, nowMs)
	case "only":
		where = append(where, "postponed_until IS NOT NULL AND postponed_until > ?")
		args = append(args, nowMs)
	}

	order := " ORDER BY id ASC"
	if opts.NewestFirst {
		order = " ORDER BY id DESC"
	}
	if opts.Status == "history" || opts.Status == "answered" || opts.Status == "dismissed" {
		order = " ORDER BY COALESCE(answered_at, created_at) DESC, id DESC"
	}

	query := `SELECT id, workspace_id, kind, body, options_json, response, author, status,
		created_at, answered_at, acknowledged, postponed_until
		FROM inbox WHERE ` + strings.Join(where, " AND ") + order + " LIMIT ?"
	args = append(args, limit)

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Item{}
	for rows.Next() {
		it, err := scanItem(rows, nowMs)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// Answers returns answered inbox items for workspaceID not yet acknowledged.
// If ack is true, marks those returned items acknowledged atomically.
func Answers(db *sql.DB, workspaceID string, ack bool, nowMs int64) ([]Item, error) {
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`
		SELECT id, workspace_id, kind, body, options_json, response, author, status,
		created_at, answered_at, acknowledged, postponed_until
		FROM inbox
		WHERE workspace_id = ? AND status = 'answered' AND acknowledged = 0
		ORDER BY id ASC
	`, workspaceID)
	if err != nil {
		return nil, err
	}
	items := []Item{}
	for rows.Next() {
		it, err := scanItem(rows, nowMs)
		if err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, it)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if ack && len(items) > 0 {
		for _, it := range items {
			if _, err := tx.Exec("UPDATE inbox SET acknowledged = 1 WHERE id = ?", it.ID); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return items, nil
}

// Respond answers an open approve, input, or choice item and emits an inbox_response event.
func Respond(db *sql.DB, workspaceID string, id int64, rawResponse string, nowMs int64) (Item, events.Event, error) {
	return RespondWithComment(db, workspaceID, id, rawResponse, "", nowMs)
}

// RespondWithComment answers an open item and optionally attaches a comment on approve items.
func RespondWithComment(db *sql.DB, workspaceID string, id int64, rawResponse, comment string, nowMs int64) (Item, events.Event, error) {
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}
	tx, err := db.Begin()
	if err != nil {
		return Item{}, events.Event{}, err
	}
	defer tx.Rollback()

	it, err := getTxItem(tx, workspaceID, id, nowMs)
	if err != nil {
		return Item{}, events.Event{}, err
	}
	if it.Status != StatusOpen {
		return Item{}, events.Event{}, ErrNotOpen
	}

	resp, err := validateResponse(it, rawResponse)
	if err != nil {
		return Item{}, events.Event{}, err
	}
	cleanComment := strings.TrimSpace(comment)
	if cleanComment != "" {
		if it.Kind != KindApprove {
			return Item{}, events.Event{}, fmt.Errorf("%w: comment is only supported for approve items", ErrInvalidInput)
		}
		if utf8.RuneCountInString(cleanComment) > MaxResponseChars {
			return Item{}, events.Event{}, fmt.Errorf("%w: comment must be <= %d characters", ErrInvalidInput, MaxResponseChars)
		}
	}
	storedResp := resp
	if cleanComment != "" {
		storedResp = resp + " — " + cleanComment
	}

	res, err := tx.Exec(`
		UPDATE inbox SET status = 'answered', response = ?, answered_at = ?, postponed_until = NULL
		WHERE id = ? AND status = 'open'
	`, storedResp, nowMs, id)
	if err != nil {
		return Item{}, events.Event{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Item{}, events.Event{}, ErrNotOpen
	}

	payload := map[string]any{
		"id":       it.ID,
		"kind":     it.Kind,
		"response": storedResp,
		"body":     it.Body,
	}
	if it.Kind == KindApprove {
		payload["decision"] = resp
		if cleanComment != "" {
			payload["comment"] = cleanComment
		}
	}
	if it.Author != nil {
		payload["author"] = *it.Author
	}
	ev, err := events.Append(tx, it.WorkspaceID, events.KindInboxResponse, payload, nowMs)
	if err != nil {
		return Item{}, events.Event{}, err
	}
	if err := tx.Commit(); err != nil {
		return Item{}, events.Event{}, err
	}
	updated, err := Get(db, id, nowMs)
	return updated, ev, err
}

// Dismiss marks an open item dismissed and emits an inbox_dismiss event.
func Dismiss(db *sql.DB, workspaceID string, id int64, nowMs int64) (Item, events.Event, error) {
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}
	tx, err := db.Begin()
	if err != nil {
		return Item{}, events.Event{}, err
	}
	defer tx.Rollback()

	it, err := getTxItem(tx, workspaceID, id, nowMs)
	if err != nil {
		return Item{}, events.Event{}, err
	}
	if it.Status != StatusOpen {
		return Item{}, events.Event{}, ErrNotOpen
	}

	res, err := tx.Exec(`
		UPDATE inbox SET status = 'dismissed', answered_at = ?, postponed_until = NULL
		WHERE id = ? AND status = 'open'
	`, nowMs, id)
	if err != nil {
		return Item{}, events.Event{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Item{}, events.Event{}, ErrNotOpen
	}

	payload := map[string]any{
		"id":   it.ID,
		"kind": it.Kind,
		"body": it.Body,
	}
	ev, err := events.Append(tx, it.WorkspaceID, events.KindInboxDismiss, payload, nowMs)
	if err != nil {
		return Item{}, events.Event{}, err
	}
	if err := tx.Commit(); err != nil {
		return Item{}, events.Event{}, err
	}
	updated, err := Get(db, id, nowMs)
	return updated, ev, err
}

// Postpone defers an open item until untilMs (or clears postponement when untilMs <= 0).
func Postpone(db *sql.DB, workspaceID string, id int64, untilMs, nowMs int64) (Item, error) {
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}
	it, err := getOne(db, workspaceID, id, nowMs)
	if err != nil {
		return Item{}, err
	}
	if it.Status != StatusOpen {
		return Item{}, ErrNotOpen
	}
	var until sql.NullInt64
	if untilMs > 0 {
		until = sql.NullInt64{Int64: untilMs, Valid: true}
	}
	if _, err := db.Exec("UPDATE inbox SET postponed_until = ? WHERE id = ?", until, id); err != nil {
		return Item{}, err
	}
	return Get(db, id, nowMs)
}

// DismissNotices dismisses all non-postponed open notice items (optionally scoped to workspaceID).
func DismissNotices(db *sql.DB, workspaceID string, nowMs int64) (int, error) {
	items, err := List(db, ListOptions{
		WorkspaceID: workspaceID,
		Status:      StatusOpen,
		Kind:        KindNotice,
		Postponed:   "exclude",
		NowMs:       nowMs,
	})
	if err != nil {
		return 0, err
	}
	count := 0
	for _, it := range items {
		if _, _, err := Dismiss(db, it.WorkspaceID, it.ID, nowMs); err == nil {
			count++
		}
	}
	return count, nil
}

// CountsByWorkspace aggregates task counts per workspace.
func CountsByWorkspace(db *sql.DB, nowMs int64) (map[string]WorkspaceCounts, error) {
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}
	rows, err := db.Query(`
		SELECT workspace_id,
			SUM(CASE WHEN status = 'open' AND kind != 'notice' AND (postponed_until IS NULL OR postponed_until <= ?) THEN 1 ELSE 0 END) AS needs_you,
			SUM(CASE WHEN status = 'open' AND postponed_until IS NOT NULL AND postponed_until > ? THEN 1 ELSE 0 END) AS postponed,
			SUM(CASE WHEN status = 'open' AND kind = 'notice' AND (postponed_until IS NULL OR postponed_until <= ?) THEN 1 ELSE 0 END) AS updates,
			SUM(CASE WHEN status != 'open' THEN 1 ELSE 0 END) AS history
		FROM inbox
		GROUP BY workspace_id
	`, nowMs, nowMs, nowMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]WorkspaceCounts{}
	for rows.Next() {
		var wsID string
		var c WorkspaceCounts
		if err := rows.Scan(&wsID, &c.NeedsYou, &c.Postponed, &c.Updates, &c.History); err != nil {
			return nil, err
		}
		out[wsID] = c
	}
	return out, rows.Err()
}

func getTxItem(tx *sql.Tx, workspaceID string, id, nowMs int64) (Item, error) {
	query := `SELECT id, workspace_id, kind, body, options_json, response, author, status,
		created_at, answered_at, acknowledged, postponed_until
		FROM inbox WHERE id = ?`
	args := []any{id}
	if workspaceID != "" {
		query += " AND workspace_id = ?"
		args = append(args, workspaceID)
	}
	row := tx.QueryRow(query, args...)
	item, err := scanItem(row, nowMs)
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, ErrNotFound
	}
	return item, err
}

type scanner interface {
	Scan(dest ...any) error
}

func scanItem(s scanner, nowMs int64) (Item, error) {
	var (
		it          Item
		optJSON     sql.NullString
		resp        sql.NullString
		author      sql.NullString
		answeredAt  sql.NullInt64
		ackInt      int
		postponedAt sql.NullInt64
	)
	err := s.Scan(
		&it.ID, &it.WorkspaceID, &it.Kind, &it.Body, &optJSON, &resp, &author,
		&it.Status, &it.CreatedAt, &answeredAt, &ackInt, &postponedAt,
	)
	if err != nil {
		return Item{}, err
	}
	if optJSON.Valid && optJSON.String != "" {
		_ = json.Unmarshal([]byte(optJSON.String), &it.Options)
	}
	if resp.Valid {
		v := resp.String
		it.Response = &v
	}
	if author.Valid {
		v := author.String
		it.Author = &v
	}
	if answeredAt.Valid {
		v := answeredAt.Int64
		it.AnsweredAt = &v
	}
	it.Acknowledged = ackInt != 0
	if postponedAt.Valid {
		v := postponedAt.Int64
		it.PostponedUntil = &v
		it.Postponed = it.Status == StatusOpen && v > nowMs
	}
	it.Title = SummaryTitle(it.Body)
	it.BodyHTML = RenderMarkdown(it.Body)
	return it, nil
}

// SummaryTitle extracts a clean plain-text single-line title from Markdown body.
func SummaryTitle(body string) string {
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = strings.TrimLeft(line, "#>*+- ")
		line = strings.ReplaceAll(line, "`", "")
		line = strings.ReplaceAll(line, "**", "")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		const maxTitle = 96
		if utf8.RuneCountInString(line) > maxTitle {
			r := []rune(line)
			return strings.TrimSpace(string(r[:maxTitle-1])) + "…"
		}
		return line
	}
	return ""
}

func isValidKind(k string) bool {
	return k == KindNotice || k == KindApprove || k == KindInput || k == KindChoice
}

func validateChoiceOptions(opts []string) ([]string, error) {
	if len(opts) < MinChoiceOptions || len(opts) > MaxChoiceOptions {
		return nil, fmt.Errorf("%w: choice requires %d..%d options", ErrInvalidInput, MinChoiceOptions, MaxChoiceOptions)
	}
	clean := make([]string, 0, len(opts))
	seen := map[string]bool{}
	for _, o := range opts {
		s := strings.TrimSpace(o)
		if s == "" || utf8.RuneCountInString(s) > MaxOptionChars {
			return nil, fmt.Errorf("%w: each option must be 1..%d characters", ErrInvalidInput, MaxOptionChars)
		}
		if seen[s] {
			return nil, fmt.Errorf("%w: duplicate option %q", ErrInvalidInput, s)
		}
		seen[s] = true
		clean = append(clean, s)
	}
	return clean, nil
}

func validateResponse(it Item, raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	switch it.Kind {
	case KindNotice:
		return "", fmt.Errorf("%w: notice items cannot be answered; dismiss instead", ErrInvalidInput)
	case KindApprove:
		switch strings.ToLower(trimmed) {
		case "yes", "y", "approve", "approved", "true":
			return "Yes", nil
		case "no", "n", "reject", "rejected", "deny", "denied", "false":
			return "No", nil
		default:
			return "", fmt.Errorf("%w: approve response must be Yes or No", ErrInvalidInput)
		}
	case KindChoice:
		for _, opt := range it.Options {
			if trimmed == opt {
				return opt, nil
			}
		}
		return "", fmt.Errorf("%w: response must match one of the available options", ErrInvalidInput)
	case KindInput:
		if trimmed == "" || utf8.RuneCountInString(trimmed) > MaxResponseChars {
			return "", fmt.Errorf("%w: response must be 1..%d characters", ErrInvalidInput, MaxResponseChars)
		}
		return trimmed, nil
	default:
		return "", fmt.Errorf("%w: unsupported kind %q", ErrInvalidInput, it.Kind)
	}
}
