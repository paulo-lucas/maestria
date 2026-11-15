// Package insights rates, on a scale of 1 to 7, how well agent work on this machine turns usage
// into shipped floors. It reads only what the connector already keeps (floors, human tasks) plus
// usage samples it records itself, and every level comes with the measure behind it, the
// threshold for the next level and what to change. Nothing leaves the machine.
//
// The rating is machine-wide: provider usage windows are shared by every workspace on an account,
// so splitting spend per workspace would be a guess.
package insights

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/paulo-lucas/maestria/internal/usage"
)

const (
	// Days is the period every measure looks back over.
	Days = 30
	// MaxLevel is the top of the scale.
	MaxLevel = 7

	// SampleEvery is the minimum time between two usage samples of one window.
	SampleEvery = 5 * time.Minute
	// KeepSamples is how long usage samples are kept.
	KeepSamples = 90 * 24 * time.Hour
	// maxGap: usage between two samples further apart than this (the machine slept, the panel was
	// stopped) cannot be placed in time, so it is left out rather than guessed.
	maxGap = 20 * time.Minute

	minFloors  = 3   // landed (delivery) or ended (quality) floors needed for a level
	minAnswers = 3   // answered tasks or approvals needed for a level
	minSpend   = 5.0 // percentage points of usage needed for a level
)

// Area ids, in display order.
const (
	AreaDelivery = "delivery"
	AreaQuality  = "quality"
	AreaSpend    = "spend"
	AreaResponse = "response"
)

// Area is one rated area of the report.
type Area struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Level is 1..7, or 0 when there is not enough data yet (Missing says what is needed).
	Level   int    `json:"level"`
	Value   string `json:"value,omitempty"`   // the measure, in words
	Next    string `json:"next,omitempty"`    // what the next level needs
	Tip     string `json:"tip,omitempty"`     // what to change, when below the top level
	Note    string `json:"note,omitempty"`    // extra context (estimates)
	Missing string `json:"missing,omitempty"` // why there is no level yet
}

// Report is the rating for one period.
type Report struct {
	From int64 `json:"from"` // epoch ms
	To   int64 `json:"to"`
	// Level is the rounded mean of the rated areas, or 0 when fewer than two areas are rated.
	Level int    `json:"level"`
	Areas []Area `json:"areas"`
	// Focus is the id of the area to work on first (the lowest below 7), "" when none.
	Focus string `json:"focus,omitempty"`
}

// scale maps a measure to a level: meeting limits[0] gives 7, limits[1] gives 6 ... limits[5]
// gives 2, and meeting none gives 1. These are the published thresholds (docs/insights.md).
type scale struct {
	limits [MaxLevel - 1]float64
	lower  bool // smaller is better
	unit   func(float64) string
}

func (s scale) meets(v, limit float64) bool {
	if s.lower {
		return v <= limit
	}
	return v >= limit
}

func (s scale) level(v float64) int {
	for i, l := range s.limits {
		if s.meets(v, l) {
			return MaxLevel - i
		}
	}
	return 1
}

// target is the threshold of the level above lvl ("≤ 8 h"), "" at the top.
func (s scale) target(lvl int) string {
	if lvl >= MaxLevel {
		return ""
	}
	op := "≥"
	if s.lower {
		op = "≤"
	}
	return op + " " + s.unit(s.limits[MaxLevel-1-lvl])
}

// next says what the level above lvl needs, "" at the top.
func (s scale) next(lvl int) string {
	if lvl >= MaxLevel {
		return ""
	}
	return fmt.Sprintf("Level %d at %s", lvl+1, s.target(lvl))
}

var (
	leadScale     = scale{limits: [6]float64{4, 8, 24, 48, 96, 168}, lower: true, unit: hoursText}
	landedScale   = scale{limits: [6]float64{95, 90, 80, 70, 60, 50}, unit: pctText}
	rejectScale   = scale{limits: [6]float64{2, 5, 10, 15, 25, 40}, lower: true, unit: pctText}
	spendScale    = scale{limits: [6]float64{90, 80, 70, 60, 45, 30}, unit: pctText}
	responseScale = scale{limits: [6]float64{5, 15, 30, 60, 180, 480}, lower: true, unit: minutesText}
)

// Compute builds the report for the Days before now.
func Compute(db *sql.DB, now time.Time) (Report, error) {
	to := now.UnixMilli()
	from := now.Add(-Days * 24 * time.Hour).UnixMilli()
	rep := Report{From: from, To: to}

	ivs, err := floorIntervals(db, from)
	if err != nil {
		return rep, err
	}
	delivery, err := deliveryArea(ivs, from)
	if err != nil {
		return rep, err
	}
	quality, err := qualityArea(db, ivs, from)
	if err != nil {
		return rep, err
	}
	spend, err := spendArea(db, ivs, from)
	if err != nil {
		return rep, err
	}
	response, err := responseArea(db, from)
	if err != nil {
		return rep, err
	}
	rep.Areas = []Area{delivery, quality, spend, response}

	sum, n, low := 0, 0, MaxLevel
	for _, a := range rep.Areas {
		if a.Level == 0 {
			continue
		}
		sum += a.Level
		n++
		if a.Level < low {
			low, rep.Focus = a.Level, a.ID
		}
	}
	if n >= 2 {
		rep.Level = int(math.Round(float64(sum) / float64(n)))
	}
	return rep, nil
}

// interval is the life of one floor: [start, end], end = 0 while it is still open.
type interval struct {
	start, end int64
	state      string // planned, active, landed, cancelled
	ongoing    bool
}

func floorIntervals(db *sql.DB, from int64) ([]interval, error) {
	rows, err := db.Query(`
		SELECT started_at, COALESCE(ended_at, 0), state, ongoing FROM floors
		WHERE started_at IS NOT NULL AND (ended_at IS NULL OR ended_at >= ?)
	`, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []interval
	for rows.Next() {
		var iv interval
		var ongoing int
		if err := rows.Scan(&iv.start, &iv.end, &iv.state, &ongoing); err != nil {
			return nil, err
		}
		iv.ongoing = ongoing != 0
		out = append(out, iv)
	}
	return out, rows.Err()
}

// deliveryArea: median time from start to landed. Small floors that land fast are the habit
// this rewards (lead time in the DORA sense).
func deliveryArea(ivs []interval, from int64) (Area, error) {
	a := Area{ID: AreaDelivery, Title: "Delivery speed"}
	var leads []float64
	for _, iv := range ivs {
		// Floors recorded when they vanished from Maestri before the start time was kept have no
		// real duration: leave them out (a minute is far below any real floor).
		if iv.state != "landed" || iv.ongoing || iv.end < from || iv.end-iv.start < int64(time.Minute/time.Millisecond) {
			continue
		}
		leads = append(leads, float64(iv.end-iv.start)/float64(time.Hour/time.Millisecond))
	}
	if len(leads) < minFloors {
		a.Missing = fmt.Sprintf("Needs %d landed floors in the last %d days (has %d).", minFloors, Days, len(leads))
		return a, nil
	}
	m := median(leads)
	a.Level = leadScale.level(m)
	a.Value = fmt.Sprintf("Median %s from start to landed, over %s", hoursText(m), floorsText(len(leads)))
	a.Next = leadScale.next(a.Level)
	if a.Level < MaxLevel {
		a.Tip = "Split work into smaller floors, each with one outcome, so every floor lands within a day."
	}
	return a, nil
}

// qualityArea: floors that landed rather than being cancelled, and the Maestro's approvals that
// were accepted. Each part is rated on its own scale and the area is their rounded mean.
func qualityArea(db *sql.DB, ivs []interval, from int64) (Area, error) {
	a := Area{ID: AreaQuality, Title: "Work that holds"}
	landed, ended := 0, 0
	for _, iv := range ivs {
		if iv.ongoing || iv.end < from || (iv.state != "landed" && iv.state != "cancelled") {
			continue
		}
		ended++
		if iv.state == "landed" {
			landed++
		}
	}
	var yes, no int
	if err := db.QueryRow(`
		SELECT COALESCE(SUM(response = 'Yes'), 0), COALESCE(SUM(response = 'No'), 0) FROM inbox
		WHERE kind = 'approve' AND status = 'answered' AND answered_at >= ?
	`, from).Scan(&yes, &no); err != nil {
		return a, err
	}

	var parts []string
	var levels []int
	var nexts []string
	tip := ""
	low := MaxLevel + 1
	if ended >= minFloors {
		share := 100 * float64(landed) / float64(ended)
		l := landedScale.level(share)
		levels = append(levels, l)
		parts = append(parts, fmt.Sprintf("%d of %d floors landed", landed, ended))
		if n := landedScale.target(l); n != "" {
			nexts = append(nexts, n+" landed")
		}
		if l < low {
			low = l
			tip = "Cancelled floors are usage that shipped nothing. Agree on the brief and when it is done before a floor starts."
		}
	}
	if yes+no >= minAnswers {
		share := 100 * float64(no) / float64(yes+no)
		l := rejectScale.level(share)
		levels = append(levels, l)
		parts = append(parts, fmt.Sprintf("%d of %d approvals rejected", no, yes+no))
		if n := rejectScale.target(l); n != "" {
			nexts = append(nexts, n+" rejected")
		}
		if l < low {
			low = l
			tip = "A rejected approval is work the Maestro proposed that you did not want. Put the constraints in the floor brief up front."
		}
	}
	if len(levels) == 0 {
		a.Missing = fmt.Sprintf("Needs %d ended floors or %d answered approvals in the last %d days (has %d and %d).", minFloors, minAnswers, Days, ended, yes+no)
		return a, nil
	}
	sum := 0
	for _, l := range levels {
		sum += l
	}
	a.Level = int(math.Round(float64(sum) / float64(len(levels))))
	a.Value = strings.Join(parts, " · ")
	// Each part has its own scale, so name the targets rather than a level number.
	if len(nexts) > 0 {
		a.Next = "Higher at " + strings.Join(nexts, " · ")
	}
	if a.Level < MaxLevel {
		a.Tip = tip
	}
	return a, nil
}

// spendArea: the share of usage spent while a floor that later landed was being worked on.
// Usage while only unfinished or ongoing floors run is still undecided and left out; usage on
// cancelled floors or with no floor at all is overhead.
func spendArea(db *sql.DB, ivs []interval, from int64) (Area, error) {
	a := Area{ID: AreaSpend, Title: "Usage that ships"}
	series, err := loadSeries(db, from)
	if err != nil {
		return a, err
	}
	byWindow := map[string]float64{} // delivered usage per provider + window, for the estimate
	delivered, overhead := 0.0, 0.0
	firstSample := int64(0)
	for _, s := range series {
		key := s.provider + "\x00" + s.window
		for i := 1; i < len(s.points); i++ {
			prev, cur := s.points[i-1], s.points[i]
			if firstSample == 0 || prev.at < firstSample {
				firstSample = prev.at
			}
			d := consumed(prev, cur)
			if d <= 0 {
				continue
			}
			switch classify(ivs, (prev.at+cur.at)/2) {
			case spentDelivered:
				delivered += d
				byWindow[key] += d
			case spentOverhead:
				overhead += d
			}
		}
	}
	if delivered+overhead < minSpend {
		a.Missing = fmt.Sprintf("Collecting usage. Samples are taken while the panel runs; a level needs %.0f points of usage over the last %d days.", minSpend, Days)
		return a, nil
	}
	share := 100 * delivered / (delivered + overhead)
	a.Level = spendScale.level(share)
	a.Value = fmt.Sprintf("%s of usage went to floors that landed", pctText(share))
	a.Next = spendScale.next(a.Level)
	if a.Level < MaxLevel {
		a.Tip = "Usage on cancelled floors, or with no floor running, ships nothing. Start work as a floor, and stop agents that sit idle."
	}

	// Estimate: usage per landed floor, for the window that carried the most delivered usage.
	// Parallel floors share one window, so this is an average, not a per-floor bill.
	bestKey, best := "", 0.0
	for k, d := range byWindow {
		if d > best || (d == best && k < bestKey) {
			bestKey, best = k, d
		}
	}
	landed := 0
	for _, iv := range ivs {
		if iv.state == "landed" && !iv.ongoing && iv.end >= from && iv.end >= firstSample {
			landed++
		}
	}
	if bestKey != "" && landed > 0 {
		provider, window, _ := strings.Cut(bestKey, "\x00")
		a.Note = fmt.Sprintf("About %s of a %s %s window per landed floor (%s). Floors that run at the same time share a window, so this is an average.",
			pctText(best/float64(landed)), titleCase(provider), window, floorsText(landed))
	}
	return a, nil
}

// responseArea: median time to answer what needs you. Agents wait meanwhile.
func responseArea(db *sql.DB, from int64) (Area, error) {
	a := Area{ID: AreaResponse, Title: "Your response time"}
	rows, err := db.Query(`
		SELECT answered_at - created_at FROM inbox
		WHERE kind IN ('approve', 'input', 'choice', 'agent_prompt') AND status = 'answered'
			AND answered_at >= ? AND answered_at >= created_at
	`, from)
	if err != nil {
		return a, err
	}
	defer rows.Close()
	var mins []float64
	for rows.Next() {
		var ms int64
		if err := rows.Scan(&ms); err != nil {
			return a, err
		}
		mins = append(mins, float64(ms)/float64(time.Minute/time.Millisecond))
	}
	if err := rows.Err(); err != nil {
		return a, err
	}
	if len(mins) < minAnswers {
		a.Missing = fmt.Sprintf("Needs %d answered tasks in the last %d days (has %d).", minAnswers, Days, len(mins))
		return a, nil
	}
	m := median(mins)
	a.Level = responseScale.level(m)
	a.Value = fmt.Sprintf("Median %s to answer, over %d tasks", minutesText(m), len(mins))
	a.Next = responseScale.next(a.Level)
	if a.Level < MaxLevel {
		a.Tip = "Agents wait while a task sits in Needs you. Answer from your phone, or use Later to say when you will."
	}
	return a, nil
}

const (
	spentUndecided = iota
	spentDelivered
	spentOverhead
)

// classify says what usage at time t went to.
func classify(ivs []interval, t int64) int {
	open := false
	for _, iv := range ivs {
		if t < iv.start || (iv.end != 0 && t > iv.end) {
			continue
		}
		if iv.state == "landed" && !iv.ongoing {
			return spentDelivered
		}
		if iv.state == "active" || iv.ongoing {
			open = true
		}
	}
	if open {
		return spentUndecided
	}
	return spentOverhead
}

type point struct {
	used    float64
	resetAt int64 // 0 when unknown
	at      int64
}

type series struct {
	provider, window string
	points           []point
}

// loadSeries returns one window per provider account: the session window when there is one
// (it moves in finer steps), otherwise the first by name.
func loadSeries(db *sql.DB, from int64) ([]series, error) {
	rows, err := db.Query(`
		SELECT provider, account, window_id, used_pct, COALESCE(reset_at, 0), at FROM usage_samples
		WHERE at >= ? ORDER BY provider, account, window_id, at
	`, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type key struct{ provider, account, window string }
	all := map[key][]point{}
	for rows.Next() {
		var k key
		var p point
		if err := rows.Scan(&k.provider, &k.account, &k.window, &p.used, &p.resetAt, &p.at); err != nil {
			return nil, err
		}
		all[k] = append(all[k], p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	chosen := map[[2]string]key{}
	for k := range all {
		acct := [2]string{k.provider, k.account}
		c, ok := chosen[acct]
		if !ok || (k.window == "session" && c.window != "session") || (c.window != "session" && k.window < c.window) {
			chosen[acct] = k
		}
	}
	out := make([]series, 0, len(chosen))
	for _, k := range chosen {
		out = append(out, series{provider: k.provider, window: k.window, points: all[k]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].provider+out[i].window < out[j].provider+out[j].window })
	return out, nil
}

// consumed is the usage between two samples of one window, in percentage points.
func consumed(prev, cur point) float64 {
	if cur.at-prev.at > maxGap.Milliseconds() || cur.at <= prev.at {
		return 0
	}
	// The window reset in between: what it shows now was all used since the reset.
	if (prev.resetAt != 0 && cur.resetAt > prev.resetAt+time.Minute.Milliseconds()) || cur.used < prev.used-1 {
		return cur.used
	}
	return max(0, cur.used-prev.used)
}

// RecordUsage stores one sample per usage window, at most every SampleEvery, and drops samples
// older than KeepSamples. Stale or failing accounts are skipped: they would repeat old numbers.
func RecordUsage(db *sql.DB, snap usage.Snapshot, now time.Time) error {
	if !snap.Available {
		return nil
	}
	nowMs := now.UnixMilli()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, a := range snap.Accounts {
		if a.Stale || a.Error != "" {
			continue
		}
		for _, w := range a.Windows {
			var reset any
			if w.ResetAt != nil {
				reset = *w.ResetAt
			}
			if _, err := tx.Exec(`
				INSERT INTO usage_samples (provider, account, window_id, used_pct, reset_at, at)
				SELECT ?, ?, ?, ?, ?, ?
				WHERE NOT EXISTS (
					SELECT 1 FROM usage_samples WHERE provider = ? AND account = ? AND window_id = ? AND at > ?
				)
			`, a.Provider, a.Key, w.ID, w.UsedPercent, reset, nowMs,
				a.Provider, a.Key, w.ID, nowMs-SampleEvery.Milliseconds()); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`DELETE FROM usage_samples WHERE at < ?`, nowMs-KeepSamples.Milliseconds()); err != nil {
		return err
	}
	return tx.Commit()
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

func hoursText(h float64) string {
	switch {
	case h < 1:
		return minutesText(h * 60)
	case h < 10:
		return trimZero(fmt.Sprintf("%.1f h", h))
	case h < 48:
		return fmt.Sprintf("%.0f h", h)
	default:
		return trimZero(fmt.Sprintf("%.1f days", h/24))
	}
}

func minutesText(m float64) string {
	if m >= 60 {
		return hoursText(m / 60)
	}
	return fmt.Sprintf("%.0f min", math.Max(m, 0))
}

func pctText(p float64) string { return fmt.Sprintf("%.0f%%", p) }

func floorsText(n int) string {
	if n == 1 {
		return "1 floor"
	}
	return fmt.Sprintf("%d floors", n)
}

func trimZero(s string) string { return strings.Replace(s, ".0 ", " ", 1) }

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
