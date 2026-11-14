// Package floors manages planned (Future), active (Present), and landed/cancelled (Past) floors.
package floors

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/paulo-lucas/maestria/internal/inbox"
)

const (
	StatePlanned   = "planned"
	StateActive    = "active"
	StateLanded    = "landed"
	StateCancelled = "cancelled"

	StatusWorking = "working"
	StatusBlocked = "blocked"
	StatusReview  = "review"
	StatusDone    = "done"

	MaxNameChars       = 100
	MaxBriefChars      = 4000
	MaxOutcomeChars    = 4000
	MaxStatusTextChars = 120
	MaxAgents          = 20
	MaxAgentNameChars  = 64
)

var (
	ErrNotFound     = errors.New("floor not found")
	ErrInvalidInput = errors.New("invalid floor input")
	ErrConflict     = errors.New("floor already exists")
	ErrInvalidState = errors.New("invalid floor state transition")
)

// Floor represents a workspace floor in Present, Future, or Past.
type Floor struct {
	ID          int64    `json:"id"`
	WorkspaceID string   `json:"workspaceId"`
	Name        string   `json:"name"`
	Brief       string   `json:"brief"`
	BriefHTML   string   `json:"briefHtml"`
	State       string   `json:"state"`
	Ongoing     bool     `json:"ongoing"`
	Progress    *float64 `json:"progress,omitempty"`
	Status      string   `json:"status"`
	StatusText  string   `json:"statusText,omitempty"`
	Agents      []string `json:"agents,omitempty"`
	// Branch and Attention come from the live runtime (Wire); they are never stored.
	Branch      string `json:"branch,omitempty"`
	Attention   int    `json:"attention,omitempty"`
	Live        bool   `json:"live,omitempty"`
	Outcome     string `json:"outcome,omitempty"`
	OutcomeHTML string `json:"outcomeHtml,omitempty"`
	SortOrder   int    `json:"sortOrder"`
	PlannedAt   *int64 `json:"plannedAt,omitempty"`
	StartedAt   *int64 `json:"startedAt,omitempty"`
	EndedAt     *int64 `json:"endedAt,omitempty"`
	UpdatedAt   int64  `json:"updatedAt"`
}

// LiveInfo holds live status, progress, and agent info from an external runtime (e.g. Maestri Wire).
type LiveInfo struct {
	Name       string
	Status     string
	StatusText string
	Progress   *float64
	Agents     []string
	Ongoing    *bool
	// Branch is the floor's git branch, Attention the number of agents that need the human.
	Branch    string
	Attention int
}

// LiveSource supplies live floor data for a workspace so Wire can enrich CLI-managed floors.
type LiveSource interface {
	LiveFloors(workspaceID string) ([]LiveInfo, error)
}

// PlanInput configures a new planned (Future) floor.
type PlanInput struct {
	Name    string
	Brief   string
	Ongoing bool
}

// StartInput configures starting a planned floor (or starting a new floor when Brief is given).
type StartInput struct {
	Brief      string
	Ongoing    bool
	OngoingSet bool
}

// StatusInput updates live status fields on an active (Present) floor.
type StatusInput struct {
	Status        string
	Progress      *float64
	ProgressSet   bool
	ClearProgress bool
	Text          string
	TextSet       bool
	Agents        []string
	AgentsSet     bool
}

// EditInput updates a planned or active floor's metadata.
type EditInput struct {
	Brief      string
	BriefSet   bool
	Rename     string
	Ongoing    bool
	OngoingSet bool
}

// ListOptions filters floors returned by List.
type ListOptions struct {
	WorkspaceID string
	IncludePast bool
	State       string
}

// Plan creates a new Future floor in workspaceID.
func Plan(db *sql.DB, workspaceID string, in PlanInput, nowMs int64) (Floor, error) {
	if strings.TrimSpace(workspaceID) == "" {
		return Floor{}, fmt.Errorf("%w: workspaceId is required", ErrInvalidInput)
	}
	name, err := validateName(in.Name)
	if err != nil {
		return Floor{}, err
	}
	brief, err := validateMarkdownRequired("brief", in.Brief, MaxBriefChars)
	if err != nil {
		return Floor{}, err
	}
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}

	tx, err := db.Begin()
	if err != nil {
		return Floor{}, err
	}
	defer tx.Rollback()

	if existing, err := getLiveTx(tx, workspaceID, name); err == nil {
		return Floor{}, fmt.Errorf("%w: floor %q is already %s", ErrConflict, existing.Name, existing.State)
	} else if !errors.Is(err, ErrNotFound) {
		return Floor{}, err
	}

	var maxOrder int
	if err := tx.QueryRow(
		"SELECT COALESCE(MAX(sort_order), 0) FROM floors WHERE workspace_id = ? AND state = 'planned'",
		workspaceID,
	).Scan(&maxOrder); err != nil {
		return Floor{}, err
	}

	ongoingInt := 0
	if in.Ongoing {
		ongoingInt = 1
	}
	res, err := tx.Exec(`
		INSERT INTO floors (workspace_id, name, brief, state, ongoing, status, sort_order, planned_at, updated_at)
		VALUES (?, ?, ?, 'planned', ?, 'working', ?, ?, ?)
	`, workspaceID, name, brief, ongoingInt, maxOrder+1, nowMs, nowMs)
	if err != nil {
		return Floor{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Floor{}, err
	}
	if err := tx.Commit(); err != nil {
		return Floor{}, err
	}
	return GetByID(db, id)
}

// Start transitions a planned floor to active, or creates and activates one if Brief is provided.
func Start(db *sql.DB, workspaceID, rawName string, in StartInput, nowMs int64) (Floor, error) {
	if strings.TrimSpace(workspaceID) == "" {
		return Floor{}, fmt.Errorf("%w: workspaceId is required", ErrInvalidInput)
	}
	name, err := validateName(rawName)
	if err != nil {
		return Floor{}, err
	}
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}

	tx, err := db.Begin()
	if err != nil {
		return Floor{}, err
	}
	defer tx.Rollback()

	cur, err := getLiveTx(tx, workspaceID, name)
	if errors.Is(err, ErrNotFound) {
		if strings.TrimSpace(in.Brief) == "" {
			return Floor{}, fmt.Errorf("%w: floor %q not found (plan it first or pass --brief)", ErrNotFound, name)
		}
		brief, err := validateMarkdownRequired("brief", in.Brief, MaxBriefChars)
		if err != nil {
			return Floor{}, err
		}
		ongoingInt := 0
		if in.Ongoing {
			ongoingInt = 1
		}
		res, err := tx.Exec(`
			INSERT INTO floors (workspace_id, name, brief, state, ongoing, status, started_at, updated_at)
			VALUES (?, ?, ?, 'active', ?, 'working', ?, ?)
		`, workspaceID, name, brief, ongoingInt, nowMs, nowMs)
		if err != nil {
			return Floor{}, err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return Floor{}, err
		}
		if err := tx.Commit(); err != nil {
			return Floor{}, err
		}
		return GetByID(db, id)
	}
	if err != nil {
		return Floor{}, err
	}
	if cur.State == StateActive {
		return Floor{}, fmt.Errorf("%w: floor %q is already active", ErrInvalidState, cur.Name)
	}

	brief := cur.Brief
	if strings.TrimSpace(in.Brief) != "" {
		brief, err = validateMarkdownRequired("brief", in.Brief, MaxBriefChars)
		if err != nil {
			return Floor{}, err
		}
	}
	ongoing := cur.Ongoing
	if in.OngoingSet {
		ongoing = in.Ongoing
	}
	ongoingInt := 0
	if ongoing {
		ongoingInt = 1
	}
	if _, err := tx.Exec(`
		UPDATE floors
		SET state = 'active', brief = ?, ongoing = ?, status = 'working',
			progress = CASE WHEN ? = 1 THEN NULL ELSE progress END,
			sort_order = 0, started_at = ?, updated_at = ?, rev = rev + 1
		WHERE id = ?
	`, brief, ongoingInt, ongoingInt, nowMs, nowMs, cur.ID); err != nil {
		return Floor{}, err
	}
	if err := tx.Commit(); err != nil {
		return Floor{}, err
	}
	return GetByID(db, cur.ID)
}

// SetStatus updates status, progress, status text, and/or agents for an active floor.
func SetStatus(db *sql.DB, workspaceID, rawName string, in StatusInput, nowMs int64) (Floor, error) {
	if strings.TrimSpace(workspaceID) == "" {
		return Floor{}, fmt.Errorf("%w: workspaceId is required", ErrInvalidInput)
	}
	name, err := validateName(rawName)
	if err != nil {
		return Floor{}, err
	}
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}

	tx, err := db.Begin()
	if err != nil {
		return Floor{}, err
	}
	defer tx.Rollback()

	cur, err := getLiveOrPastErrorTx(tx, workspaceID, name)
	if err != nil {
		return Floor{}, err
	}
	if cur.State != StateActive {
		return Floor{}, fmt.Errorf("%w: cannot set status on %s floor %q; start it first", ErrInvalidState, cur.State, cur.Name)
	}

	status := cur.Status
	if strings.TrimSpace(in.Status) != "" {
		s := strings.ToLower(strings.TrimSpace(in.Status))
		if !IsValidStatus(s) {
			return Floor{}, fmt.Errorf("%w: invalid status %q (must be working|blocked|review|done)", ErrInvalidInput, in.Status)
		}
		status = s
	}

	progress := cur.Progress
	if in.ClearProgress {
		progress = nil
	}
	if in.ProgressSet {
		if cur.Ongoing {
			return Floor{}, fmt.Errorf("%w: ongoing floor %q cannot have a progress bar", ErrInvalidInput, cur.Name)
		}
		if in.Progress != nil {
			v := *in.Progress
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
				return Floor{}, fmt.Errorf("%w: progress must be between 0 and 1", ErrInvalidInput)
			}
			progress = &v
		} else {
			progress = nil
		}
	}

	statusText := cur.StatusText
	if in.TextSet {
		t := strings.TrimSpace(in.Text)
		if utf8.RuneCountInString(t) > MaxStatusTextChars {
			return Floor{}, fmt.Errorf("%w: status text must be <= %d characters", ErrInvalidInput, MaxStatusTextChars)
		}
		statusText = t
	}

	agents := cur.Agents
	if in.AgentsSet {
		cleaned, err := validateAgents(in.Agents)
		if err != nil {
			return Floor{}, err
		}
		agents = cleaned
	}
	agentsBytes, _ := json.Marshal(agents)
	if agents == nil {
		agentsBytes = []byte("[]")
	}

	var progArg sql.NullFloat64
	if progress != nil {
		progArg = sql.NullFloat64{Float64: *progress, Valid: true}
	}

	if _, err := tx.Exec(`
		UPDATE floors
		SET status = ?, progress = ?, status_text = ?, agents_json = ?, updated_at = ?, rev = rev + 1
		WHERE id = ?
	`, status, progArg, statusText, string(agentsBytes), nowMs, cur.ID); err != nil {
		return Floor{}, err
	}
	if err := tx.Commit(); err != nil {
		return Floor{}, err
	}
	return GetByID(db, cur.ID)
}

// Land records a floor as landed (Past). Planned floors require force=true.
func Land(db *sql.DB, workspaceID, rawName, rawOutcome string, force bool, nowMs int64) (Floor, error) {
	if strings.TrimSpace(workspaceID) == "" {
		return Floor{}, fmt.Errorf("%w: workspaceId is required", ErrInvalidInput)
	}
	name, err := validateName(rawName)
	if err != nil {
		return Floor{}, err
	}
	outcome, err := validateMarkdownOptional("outcome", rawOutcome, MaxOutcomeChars)
	if err != nil {
		return Floor{}, err
	}
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}

	tx, err := db.Begin()
	if err != nil {
		return Floor{}, err
	}
	defer tx.Rollback()

	cur, err := getLiveOrPastErrorTx(tx, workspaceID, name)
	if err != nil {
		return Floor{}, err
	}
	if cur.State == StatePlanned && !force {
		return Floor{}, fmt.Errorf("%w: cannot land planned floor %q that never started (start it first or pass --force)", ErrInvalidState, cur.Name)
	}

	if _, err := tx.Exec(`
		UPDATE floors
		SET state = 'landed', status = 'done', outcome = ?, sort_order = 0, ended_at = ?, updated_at = ?, rev = rev + 1
		WHERE id = ?
	`, outcome, nowMs, nowMs, cur.ID); err != nil {
		return Floor{}, err
	}
	if err := tx.Commit(); err != nil {
		return Floor{}, err
	}
	return GetByID(db, cur.ID)
}

// Cancel records a planned or active floor as cancelled (Past).
func Cancel(db *sql.DB, workspaceID, rawName, rawOutcome string, nowMs int64) (Floor, error) {
	if strings.TrimSpace(workspaceID) == "" {
		return Floor{}, fmt.Errorf("%w: workspaceId is required", ErrInvalidInput)
	}
	name, err := validateName(rawName)
	if err != nil {
		return Floor{}, err
	}
	outcome, err := validateMarkdownOptional("outcome", rawOutcome, MaxOutcomeChars)
	if err != nil {
		return Floor{}, err
	}
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}

	tx, err := db.Begin()
	if err != nil {
		return Floor{}, err
	}
	defer tx.Rollback()

	cur, err := getLiveOrPastErrorTx(tx, workspaceID, name)
	if err != nil {
		return Floor{}, err
	}

	if _, err := tx.Exec(`
		UPDATE floors
		SET state = 'cancelled', outcome = ?, sort_order = 0, ended_at = ?, updated_at = ?, rev = rev + 1
		WHERE id = ?
	`, outcome, nowMs, nowMs, cur.ID); err != nil {
		return Floor{}, err
	}
	if err := tx.Commit(); err != nil {
		return Floor{}, err
	}
	return GetByID(db, cur.ID)
}

// Edit updates brief, name, and/or ongoing on a planned or active floor.
func Edit(db *sql.DB, workspaceID, rawName string, in EditInput, nowMs int64) (Floor, error) {
	if strings.TrimSpace(workspaceID) == "" {
		return Floor{}, fmt.Errorf("%w: workspaceId is required", ErrInvalidInput)
	}
	name, err := validateName(rawName)
	if err != nil {
		return Floor{}, err
	}
	if !in.BriefSet && strings.TrimSpace(in.Rename) == "" && !in.OngoingSet {
		return Floor{}, fmt.Errorf("%w: provide --brief, --rename, or --ongoing", ErrInvalidInput)
	}
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}

	tx, err := db.Begin()
	if err != nil {
		return Floor{}, err
	}
	defer tx.Rollback()

	cur, err := getLiveOrPastErrorTx(tx, workspaceID, name)
	if err != nil {
		return Floor{}, err
	}

	newName := cur.Name
	if strings.TrimSpace(in.Rename) != "" {
		newName, err = validateName(in.Rename)
		if err != nil {
			return Floor{}, err
		}
		if newName != cur.Name {
			if existing, err := getLiveTx(tx, workspaceID, newName); err == nil {
				return Floor{}, fmt.Errorf("%w: floor %q is already %s", ErrConflict, existing.Name, existing.State)
			} else if !errors.Is(err, ErrNotFound) {
				return Floor{}, err
			}
		}
	}

	brief := cur.Brief
	if in.BriefSet {
		brief, err = validateMarkdownRequired("brief", in.Brief, MaxBriefChars)
		if err != nil {
			return Floor{}, err
		}
	}

	ongoing := cur.Ongoing
	if in.OngoingSet {
		ongoing = in.Ongoing
	}
	ongoingInt := 0
	if ongoing {
		ongoingInt = 1
	}

	if _, err := tx.Exec(`
		UPDATE floors
		SET name = ?, brief = ?, ongoing = ?,
			progress = CASE WHEN ? = 1 THEN NULL ELSE progress END,
			updated_at = ?, rev = rev + 1
		WHERE id = ?
	`, newName, brief, ongoingInt, ongoingInt, nowMs, cur.ID); err != nil {
		return Floor{}, err
	}
	if err := tx.Commit(); err != nil {
		return Floor{}, err
	}
	return GetByID(db, cur.ID)
}

// Reorder moves planned floor rawName immediately before or after rawTarget in Future order.
func Reorder(db *sql.DB, workspaceID, rawName, rawTarget string, before bool, nowMs int64) ([]Floor, error) {
	if strings.TrimSpace(workspaceID) == "" {
		return nil, fmt.Errorf("%w: workspaceId is required", ErrInvalidInput)
	}
	name, err := validateName(rawName)
	if err != nil {
		return nil, err
	}
	target, err := validateName(rawTarget)
	if err != nil {
		return nil, err
	}
	if name == target {
		return nil, fmt.Errorf("%w: cannot reorder floor relative to itself", ErrInvalidInput)
	}
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}

	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	src, err := getLiveOrPastErrorTx(tx, workspaceID, name)
	if err != nil {
		return nil, err
	}
	if src.State != StatePlanned {
		return nil, fmt.Errorf("%w: only planned floors can be reordered (%q is %s)", ErrInvalidState, src.Name, src.State)
	}
	dst, err := getLiveOrPastErrorTx(tx, workspaceID, target)
	if err != nil {
		return nil, err
	}
	if dst.State != StatePlanned {
		return nil, fmt.Errorf("%w: target floor %q must be planned (is %s)", ErrInvalidState, dst.Name, dst.State)
	}

	rows, err := tx.Query(`
		SELECT id, workspace_id, name, brief, state, ongoing, progress, status, status_text,
			agents_json, outcome, sort_order, planned_at, started_at, ended_at, updated_at
		FROM floors
		WHERE workspace_id = ? AND state = 'planned'
		ORDER BY sort_order ASC, id ASC
	`, workspaceID)
	if err != nil {
		return nil, err
	}
	planned := []Floor{}
	for rows.Next() {
		f, err := scanFloor(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		planned = append(planned, f)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	reordered := make([]Floor, 0, len(planned))
	for _, f := range planned {
		if f.ID == src.ID {
			continue
		}
		if f.ID == dst.ID {
			if before {
				reordered = append(reordered, src, f)
			} else {
				reordered = append(reordered, f, src)
			}
			continue
		}
		reordered = append(reordered, f)
	}

	for i, f := range reordered {
		order := i + 1
		if _, err := tx.Exec(
			"UPDATE floors SET sort_order = ?, updated_at = ?, rev = rev + 1 WHERE id = ?",
			order, nowMs, f.ID,
		); err != nil {
			return nil, err
		}
		reordered[i].SortOrder = order
		reordered[i].UpdatedAt = nowMs
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return reordered, nil
}

// GetByID loads one floor by primary key.
func GetByID(db *sql.DB, id int64) (Floor, error) {
	row := db.QueryRow(`
		SELECT id, workspace_id, name, brief, state, ongoing, progress, status, status_text,
			agents_json, outcome, sort_order, planned_at, started_at, ended_at, updated_at
		FROM floors WHERE id = ?
	`, id)
	f, err := scanFloor(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Floor{}, ErrNotFound
	}
	return f, err
}

// List returns floors matching opts.
func List(db *sql.DB, opts ListOptions) ([]Floor, error) {
	where := []string{"1=1"}
	args := []any{}
	if opts.WorkspaceID != "" {
		where = append(where, "workspace_id = ?")
		args = append(args, opts.WorkspaceID)
	}
	if opts.State != "" {
		where = append(where, "state = ?")
		args = append(args, opts.State)
	} else if !opts.IncludePast {
		where = append(where, "state IN ('active', 'planned')")
	}

	query := `
		SELECT id, workspace_id, name, brief, state, ongoing, progress, status, status_text,
			agents_json, outcome, sort_order, planned_at, started_at, ended_at, updated_at
		FROM floors
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY
			CASE state
				WHEN 'active' THEN 0
				WHEN 'planned' THEN 1
				ELSE 2
			END ASC,
			CASE WHEN state = 'planned' THEN sort_order ELSE 0 END ASC,
			CASE WHEN state = 'active' THEN COALESCE(started_at, updated_at) ELSE 0 END ASC,
			CASE WHEN state IN ('landed', 'cancelled') THEN COALESCE(ended_at, updated_at) ELSE 0 END DESC,
			id ASC
	`
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Floor{}
	for rows.Next() {
		f, err := scanFloor(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ListWithLive returns workspace floors merged with optional live runtime state.
func ListWithLive(db *sql.DB, workspaceID string, includePast bool, live LiveSource) ([]Floor, error) {
	stored, err := List(db, ListOptions{WorkspaceID: workspaceID, IncludePast: includePast})
	if err != nil {
		return nil, err
	}
	if live == nil {
		return stored, nil
	}
	liveItems, err := live.LiveFloors(workspaceID)
	if err != nil {
		// Fall back to stored CLI data if live source is unavailable.
		return stored, nil
	}
	return MergeLive(workspaceID, stored, liveItems), nil
}

// MergeLive overlays live floor info onto active stored floors and appends any active live floors not yet in SQLite.
func MergeLive(workspaceID string, stored []Floor, live []LiveInfo) []Floor {
	if len(live) == 0 {
		return stored
	}
	out := make([]Floor, len(stored))
	copy(out, stored)

	byName := make(map[string]int, len(out))
	for i := range out {
		if out[i].State == StateActive {
			byName[out[i].Name] = i
		}
	}

	var extra []Floor
	for _, li := range live {
		name := strings.TrimSpace(li.Name)
		if name == "" {
			continue
		}
		if idx, ok := byName[name]; ok {
			applyLive(&out[idx], li)
			continue
		}
		f := Floor{
			WorkspaceID: workspaceID,
			Name:        name,
			State:       StateActive,
			Status:      StatusWorking,
		}
		applyLive(&f, li)
		extra = append(extra, f)
	}
	if len(extra) == 0 {
		return out
	}

	// Place synthesized active floors right after existing active floors and before planned/past floors.
	insertAt := 0
	for insertAt < len(out) && out[insertAt].State == StateActive {
		insertAt++
	}
	merged := make([]Floor, 0, len(out)+len(extra))
	merged = append(merged, out[:insertAt]...)
	merged = append(merged, extra...)
	merged = append(merged, out[insertAt:]...)
	return merged
}

func applyLive(f *Floor, li LiveInfo) {
	f.Live = true
	f.Branch = strings.TrimSpace(li.Branch)
	if li.Attention > 0 {
		f.Attention = li.Attention
	}
	if li.Ongoing != nil {
		f.Ongoing = *li.Ongoing
		if f.Ongoing {
			f.Progress = nil
		}
	}
	if s := strings.ToLower(strings.TrimSpace(li.Status)); IsValidStatus(s) {
		f.Status = s
	}
	if txt := strings.TrimSpace(li.StatusText); txt != "" {
		f.StatusText = clipRunes(txt, MaxStatusTextChars)
	}
	if !f.Ongoing && li.Progress != nil {
		v := *li.Progress
		if !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 1 {
			cp := v
			f.Progress = &cp
		}
	}
	if li.Agents != nil {
		if cleaned, err := validateAgents(li.Agents); err == nil {
			f.Agents = cleaned
		}
	}
}

// ParseProgress parses "0.4", "40%", or "2/5" into a 0..1 float64.
func ParseProgress(raw string) (float64, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, fmt.Errorf("%w: progress cannot be empty", ErrInvalidInput)
	}
	if strings.HasSuffix(s, "%") {
		numStr := strings.TrimSpace(strings.TrimSuffix(s, "%"))
		v, err := strconv.ParseFloat(numStr, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 100 {
			return 0, fmt.Errorf("%w: invalid percentage progress %q", ErrInvalidInput, raw)
		}
		return v / 100.0, nil
	}
	if numStr, denStr, ok := strings.Cut(s, "/"); ok {
		num, err1 := strconv.ParseFloat(strings.TrimSpace(numStr), 64)
		den, err2 := strconv.ParseFloat(strings.TrimSpace(denStr), 64)
		if err1 != nil || err2 != nil || math.IsNaN(num) || math.IsNaN(den) || math.IsInf(num, 0) || math.IsInf(den, 0) || den <= 0 || num < 0 || num > den {
			return 0, fmt.Errorf("%w: invalid ratio progress %q", ErrInvalidInput, raw)
		}
		return num / den, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
		return 0, fmt.Errorf("%w: progress %q must be 0..1, a/b, or N%%", ErrInvalidInput, raw)
	}
	return v, nil
}

// IsValidStatus reports whether s is one of working, blocked, review, done.
func IsValidStatus(s string) bool {
	return s == StatusWorking || s == StatusBlocked || s == StatusReview || s == StatusDone
}

func getLiveTx(tx *sql.Tx, workspaceID, name string) (Floor, error) {
	row := tx.QueryRow(`
		SELECT id, workspace_id, name, brief, state, ongoing, progress, status, status_text,
			agents_json, outcome, sort_order, planned_at, started_at, ended_at, updated_at
		FROM floors
		WHERE workspace_id = ? AND name = ? AND state IN ('planned', 'active')
	`, workspaceID, name)
	f, err := scanFloor(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Floor{}, ErrNotFound
	}
	return f, err
}

func getLiveOrPastErrorTx(tx *sql.Tx, workspaceID, name string) (Floor, error) {
	f, err := getLiveTx(tx, workspaceID, name)
	if err == nil {
		return f, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Floor{}, err
	}
	var pastState string
	err2 := tx.QueryRow(`
		SELECT state FROM floors
		WHERE workspace_id = ? AND name = ? AND state IN ('landed', 'cancelled')
		ORDER BY id DESC LIMIT 1
	`, workspaceID, name).Scan(&pastState)
	if err2 == nil {
		return Floor{}, fmt.Errorf("%w: floor %q is already %s", ErrInvalidState, name, pastState)
	}
	return Floor{}, fmt.Errorf("%w: floor %q not found", ErrNotFound, name)
}

type scanner interface {
	Scan(dest ...any) error
}

func scanFloor(s scanner) (Floor, error) {
	var (
		f          Floor
		ongoingInt int
		progress   sql.NullFloat64
		agentsJSON string
		plannedAt  sql.NullInt64
		startedAt  sql.NullInt64
		endedAt    sql.NullInt64
	)
	err := s.Scan(
		&f.ID, &f.WorkspaceID, &f.Name, &f.Brief, &f.State, &ongoingInt, &progress,
		&f.Status, &f.StatusText, &agentsJSON, &f.Outcome, &f.SortOrder,
		&plannedAt, &startedAt, &endedAt, &f.UpdatedAt,
	)
	if err != nil {
		return Floor{}, err
	}
	f.Ongoing = ongoingInt != 0
	if !f.Ongoing && progress.Valid {
		v := progress.Float64
		f.Progress = &v
	}
	if agentsJSON != "" && agentsJSON != "[]" {
		_ = json.Unmarshal([]byte(agentsJSON), &f.Agents)
	}
	if plannedAt.Valid {
		v := plannedAt.Int64
		f.PlannedAt = &v
	}
	if startedAt.Valid {
		v := startedAt.Int64
		f.StartedAt = &v
	}
	if endedAt.Valid {
		v := endedAt.Int64
		f.EndedAt = &v
	}
	if f.Brief != "" {
		f.BriefHTML = inbox.RenderMarkdown(f.Brief)
	}
	if f.Outcome != "" {
		f.OutcomeHTML = inbox.RenderMarkdown(f.Outcome)
	}
	return f, nil
}

func validateName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("%w: floor name is required", ErrInvalidInput)
	}
	if strings.HasPrefix(name, "-") {
		return "", fmt.Errorf("%w: floor name cannot start with '-'", ErrInvalidInput)
	}
	if utf8.RuneCountInString(name) > MaxNameChars {
		return "", fmt.Errorf("%w: floor name must be <= %d characters", ErrInvalidInput, MaxNameChars)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("%w: floor name cannot contain control characters", ErrInvalidInput)
		}
	}
	return name, nil
}

func validateMarkdownRequired(field, raw string, maxChars int) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" || utf8.RuneCountInString(s) > maxChars {
		return "", fmt.Errorf("%w: %s must be 1..%d characters", ErrInvalidInput, field, maxChars)
	}
	return s, nil
}

func validateMarkdownOptional(field, raw string, maxChars int) (string, error) {
	s := strings.TrimSpace(raw)
	if utf8.RuneCountInString(s) > maxChars {
		return "", fmt.Errorf("%w: %s must be <= %d characters", ErrInvalidInput, field, maxChars)
	}
	return s, nil
}

func validateAgents(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		for part := range strings.SplitSeq(item, ",") {
			name := strings.TrimSpace(part)
			if name == "" {
				continue
			}
			if utf8.RuneCountInString(name) > MaxAgentNameChars {
				return nil, fmt.Errorf("%w: agent name %q must be <= %d characters", ErrInvalidInput, name, MaxAgentNameChars)
			}
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
				if len(out) > MaxAgents {
					return nil, fmt.Errorf("%w: too many agents (max %d)", ErrInvalidInput, MaxAgents)
				}
			}
		}
	}
	return out, nil
}

func clipRunes(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	r := []rune(s)
	return string(r[:maxRunes])
}
