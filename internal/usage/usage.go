// Package usage reads Maestri's provider usage status files strictly read-only.
package usage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/paulo-lucas/maestria/internal/config"
	"github.com/paulo-lucas/maestria/internal/store"
)

const (
	lastGoodKVKey    = "usage.last_good_v1"
	recentSuccessMax = 24 * time.Hour
)

var nonKeyRE = regexp.MustCompile(`[^a-z0-9._:-]+`)

// Window is one quota meter (for example session 5h or weekly 7d).
type Window struct {
	ID           string  `json:"id"`
	Label        string  `json:"label"`
	UsedPercent  float64 `json:"usedPercent"`
	RoundedPct   int     `json:"roundedPercent"`
	RemainingPct float64 `json:"remainingPercent"`
	ResetAt      *int64  `json:"resetAt,omitempty"`
	ResetLabel   string  `json:"resetLabel,omitempty"`
	Threshold    int     `json:"threshold,omitempty"`
	Action       string  `json:"action,omitempty"`
	Tripped      bool    `json:"tripped,omitempty"`
	MarkStep     int     `json:"markStep,omitempty"`
}

// AccountGroup groups provider meters by (provider + account/plan identity).
type AccountGroup struct {
	Key           string   `json:"key"`
	Provider      string   `json:"provider"`
	ProviderLabel string   `json:"providerLabel"`
	AccountID     string   `json:"accountId"`
	AccountLabel  string   `json:"accountLabel"`
	Plan          string   `json:"plan,omitempty"`
	State         string   `json:"state"`
	Error         string   `json:"error,omitempty"`
	Stale         bool     `json:"stale"`
	UpdatedAt     int64    `json:"updatedAt,omitempty"`
	Windows       []Window `json:"windows"`
}

// SetupIssue explains why usage data is missing or unparseable.
type SetupIssue struct {
	Code    string `json:"code"`
	Title   string `json:"title"`
	Message string `json:"message"`
	Path    string `json:"path,omitempty"`
}

// HiddenProvider describes a provider hidden from the overview due to errors without recent data.
type HiddenProvider struct {
	Key           string `json:"key"`
	Provider      string `json:"provider"`
	ProviderLabel string `json:"providerLabel"`
	State         string `json:"state"`
	Error         string `json:"error,omitempty"`
}

// Snapshot holds the read-only usage view produced from .status.json.
type Snapshot struct {
	Available   bool             `json:"available"`
	Issue       *SetupIssue      `json:"issue,omitempty"`
	UpdatedAt   int64            `json:"updatedAt,omitempty"`
	StatusPath  string           `json:"statusPath"`
	Accounts    []AccountGroup   `json:"accounts"`
	Hidden      []HiddenProvider `json:"hidden"`
	HiddenCount int              `json:"hiddenCount"`
}

type cachedProvider struct {
	LastSuccessAt int64          `json:"lastSuccessAt"`
	Groups        []AccountGroup `json:"groups"`
}

// ReadSnapshot parses <maestriDir>/usage/providers/.status.json strictly read-only.
// db may be nil in tests that do not test 24h fallback caching in SQLite.
func ReadSnapshot(db *sql.DB, cfg *config.Config, now time.Time) Snapshot {
	if now.IsZero() {
		now = time.Now()
	}
	statusPath := filepath.Join(cfg.MaestriDir, "usage", "providers", ".status.json")
	snap := Snapshot{
		StatusPath: statusPath,
		Accounts:   []AccountGroup{},
		Hidden:     []HiddenProvider{},
	}

	inspect := config.InspectMaestriDir(cfg.MaestriDir)
	switch {
	case !inspect.HasDir:
		snap.Issue = &SetupIssue{
			Code:    "missing_maestri_dir",
			Title:   "Maestri data directory not found",
			Message: fmt.Sprintf("Expected Maestri data at %s. Start Maestri or set maestriDir / MAESTRI_HOME.", cfg.MaestriDir),
			Path:    cfg.MaestriDir,
		}
		return snap
	case !inspect.HasProvidersDir:
		snap.Issue = &SetupIssue{
			Code:    "missing_providers_dir",
			Title:   "Usage providers folder missing",
			Message: fmt.Sprintf("No usage/providers folder at %s. Usage data appears once Maestri tracks providers.", filepath.Join(cfg.MaestriDir, "usage", "providers")),
			Path:    filepath.Join(cfg.MaestriDir, "usage", "providers"),
		}
		return snap
	case !inspect.HasStatusFile:
		snap.Issue = &SetupIssue{
			Code:    "missing_status_file",
			Title:   "Waiting for usage telemetry",
			Message: fmt.Sprintf("Waiting for %s to be written by Maestri.", statusPath),
			Path:    statusPath,
		}
		return snap
	}

	raw, err := os.ReadFile(statusPath)
	if err != nil {
		code := "unreadable_status_file"
		if errors.Is(err, fs.ErrNotExist) {
			code = "missing_status_file"
		}
		snap.Issue = &SetupIssue{
			Code:    code,
			Title:   "Unable to read usage file",
			Message: err.Error(),
			Path:    statusPath,
		}
		return snap
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		snap.Issue = &SetupIssue{
			Code:    "invalid_status_json",
			Title:   "Usage file contains invalid JSON",
			Message: err.Error(),
			Path:    statusPath,
		}
		return snap
	}

	fileUpdatedAt := parseTimestamp(top["updatedAt"])
	if fileUpdatedAt == 0 {
		if st, err := os.Stat(statusPath); err == nil {
			fileUpdatedAt = st.ModTime().UnixMilli()
		}
	}
	snap.UpdatedAt = fileUpdatedAt
	snap.Available = true

	type namedProviderRaw struct {
		key string
		raw json.RawMessage
	}
	var entries []namedProviderRaw

	rawProviders := top["providers"]
	if len(rawProviders) == 0 {
		// Support root map without top-level "providers" wrapper.
		rawProviders = raw
	}
	var providerArr []json.RawMessage
	if len(rawProviders) > 0 && strings.TrimSpace(string(rawProviders))[0] == '[' {
		if err := json.Unmarshal(rawProviders, &providerArr); err != nil {
			snap.Available = false
			snap.Issue = &SetupIssue{
				Code:    "invalid_status_json",
				Title:   "Invalid providers array in usage file",
				Message: err.Error(),
				Path:    statusPath,
			}
			return snap
		}
		for i, item := range providerArr {
			var probe map[string]json.RawMessage
			if json.Unmarshal(item, &probe) == nil && probe != nil {
				k := firstNonEmptyStr(strField(probe["id"]), strField(probe["provider"]), fmt.Sprintf("provider-%d", i+1))
				entries = append(entries, namedProviderRaw{key: k, raw: item})
			}
		}
	} else {
		var providerMap map[string]json.RawMessage
		if err := json.Unmarshal(rawProviders, &providerMap); err != nil {
			snap.Available = false
			snap.Issue = &SetupIssue{
				Code:    "invalid_status_json",
				Title:   "Invalid providers object in usage file",
				Message: err.Error(),
				Path:    statusPath,
			}
			return snap
		}
		keys := make([]string, 0, len(providerMap))
		for k := range providerMap {
			if k == "updatedAt" || k == "version" {
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			entries = append(entries, namedProviderRaw{key: k, raw: providerMap[k]})
		}
	}

	cache := loadCache(db)
	nowMs := now.UnixMilli()
	staleAfter := time.Duration(cfg.Usage.StaleMinutes) * time.Minute
	if staleAfter <= 0 {
		staleAfter = 15 * time.Minute
	}

	cacheDirty := false
	for _, ent := range entries {
		parsed, ok := parseProviderEntry(ent.key, ent.raw, fileUpdatedAt, now)
		if !ok {
			continue
		}
		if parsed.isError {
			// Retain failing providers only if we have a recent <24h good snapshot.
			cached, hasCached := cache[parsed.cacheKey]
			lastSuccess := parsed.lastSuccessAt
			if hasCached && cached.LastSuccessAt > lastSuccess {
				lastSuccess = cached.LastSuccessAt
			}
			if lastSuccess > 0 && time.Duration(nowMs-lastSuccess)*time.Millisecond <= recentSuccessMax && len(cached.Groups) > 0 {
				for _, g := range cached.Groups {
					g.Stale = true
					g.State = parsed.state
					g.Error = parsed.errMsg
					snap.Accounts = append(snap.Accounts, g)
				}
			} else {
				snap.HiddenCount++
				snap.Hidden = append(snap.Hidden, HiddenProvider{
					Key:           parsed.cacheKey,
					Provider:      parsed.provider,
					ProviderLabel: parsed.providerLabel,
					State:         parsed.state,
					Error:         parsed.errMsg,
				})
			}
			continue
		}

		// Healthy or stale-ready entry: check age threshold against StaleMinutes.
		for i := range parsed.groups {
			updated := parsed.groups[i].UpdatedAt
			if updated == 0 {
				updated = fileUpdatedAt
			}
			if updated > 0 && time.Duration(nowMs-updated)*time.Millisecond > staleAfter {
				parsed.groups[i].Stale = true
			}
		}
		cache[parsed.cacheKey] = cachedProvider{
			LastSuccessAt: nowMs,
			Groups:        cloneGroups(parsed.groups),
		}
		cacheDirty = true
		snap.Accounts = append(snap.Accounts, parsed.groups...)
	}

	if cacheDirty && db != nil {
		saveCache(db, cache)
	}

	// Sort account groups stably: non-stale first, then by provider & account label.
	sort.SliceStable(snap.Accounts, func(i, j int) bool {
		if snap.Accounts[i].Stale != snap.Accounts[j].Stale {
			return !snap.Accounts[i].Stale
		}
		if snap.Accounts[i].Provider != snap.Accounts[j].Provider {
			return snap.Accounts[i].Provider < snap.Accounts[j].Provider
		}
		return snap.Accounts[i].AccountLabel < snap.Accounts[j].AccountLabel
	})

	// Annotate thresholds/actions from cfg.Standby onto every window.
	AnnotateStandbyConfig(snap.Accounts, cfg, now)
	return snap
}

// AnnotateStandbyConfig enriches windows with configured threshold, action, mark step, and reset label.
func AnnotateStandbyConfig(accounts []AccountGroup, cfg *config.Config, now time.Time) {
	for i := range accounts {
		g := &accounts[i]
		for j := range g.Windows {
			w := &g.Windows[j]
			th, act := ResolveWindowRule(cfg.Standby, g.Provider, g.Key, w.ID)
			w.Threshold = th
			w.Action = act
			if th > 0 {
				w.MarkStep = nearestMarkStep(th)
				w.Tripped = w.UsedPercent >= float64(th)
			}
			if w.ResetAt != nil && *w.ResetAt > 0 {
				w.ResetLabel = FormatResetETA(time.UnixMilli(*w.ResetAt), now)
			}
		}
	}
}

// ResolveWindowRule looks up threshold and action matching specific account key first, then provider ID.
func ResolveWindowRule(sb config.Standby, provider, accountKey, windowID string) (int, string) {
	threshold := 0
	for _, candidate := range []string{accountKey, provider} {
		if candidate == "" {
			continue
		}
		if wm, ok := sb.Thresholds[candidate]; ok {
			if v, ok := wm[windowID]; ok && v > 0 {
				threshold = v
				break
			}
			if v, ok := wm["*"]; ok && v > 0 {
				threshold = v
				break
			}
		}
	}
	action := config.StandbyActionTell
	for _, candidate := range []string{accountKey, provider} {
		if candidate == "" {
			continue
		}
		if am, ok := sb.Actions[candidate]; ok {
			if v, ok := am[windowID]; ok && (v == config.StandbyActionTell || v == config.StandbyActionInterrupt) {
				action = v
				break
			}
			if v, ok := am["*"]; ok && (v == config.StandbyActionTell || v == config.StandbyActionInterrupt) {
				action = v
				break
			}
		}
	}
	return threshold, action
}

type parsedEntry struct {
	cacheKey      string
	provider      string
	providerLabel string
	isError       bool
	state         string
	errMsg        string
	lastSuccessAt int64
	groups        []AccountGroup
}

type rawMeter struct {
	ID              string     `json:"id"`
	Key             string     `json:"key"`
	Label           string     `json:"label"`
	Title           string     `json:"title"`
	Name            string     `json:"name"`
	UsedPercent     *float64   `json:"usedPercent"`
	Used            *float64   `json:"used"`
	Limit           *float64   `json:"limit"`
	ResetsAt        any        `json:"resetsAt"`
	ResetAt         any        `json:"resetAt"`
	ResetTime       any        `json:"resetTime"`
	Account         string     `json:"account"`
	Plan            string     `json:"plan"`
	Email           string     `json:"email"`
	WindowMinutes   int        `json:"windowMinutes"`
	DurationSeconds int        `json:"durationSeconds"`
	Ring            string     `json:"ring"`
	Windows         []rawMeter `json:"windows"`
}

func parseProviderEntry(rawKey string, rawVal json.RawMessage, fileUpdatedAt int64, now time.Time) (parsedEntry, bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(rawVal, &obj) != nil || obj == nil {
		return parsedEntry{}, false
	}
	if rawEnabled, ok := obj["enabled"]; ok {
		var enabled bool
		if json.Unmarshal(rawEnabled, &enabled) == nil && !enabled {
			return parsedEntry{}, false
		}
	}

	provider := normalizeProviderID(rawKey, firstNonEmptyStr(strField(obj["provider"]), strField(obj["id"])))
	providerLabel := firstNonEmptyStr(strField(obj["name"]), displayProviderName(provider))
	state := strings.TrimSpace(strField(obj["state"]))
	if state == "" {
		state = strings.TrimSpace(strField(obj["status"]))
	}
	errMsg := extractError(obj["error"])
	updatedAt := firstNonZero(parseTimestamp(obj["updatedAt"]), parseTimestamp(obj["checkedAt"]), fileUpdatedAt)
	lastSuccessAt := firstNonZero(parseTimestamp(obj["lastSuccessAt"]), parseTimestamp(obj["lastGoodAt"]), parseTimestamp(obj["lastReadyAt"]))

	plan := firstNonEmptyStr(strField(obj["plan"]), strField(obj["planName"]), strField(obj["tier"]))
	accountName := firstNonEmptyStr(strField(obj["account"]), strField(obj["accountName"]), strField(obj["email"]), strField(obj["accountEmail"]), plan)

	isErr := isErrorState(state, errMsg)
	windows := extractWindows(obj, now)

	// Group meters by account/plan when individual meters carry distinct account/plan fields.
	byAcct := map[string]*AccountGroup{}
	order := []string{}
	addWindow := func(acctName, acctPlan string, w Window) {
		if acctName == "" {
			acctName = accountName
		}
		if acctPlan == "" {
			acctPlan = plan
		}
		acctSlug := sanitizeKey(firstNonEmptyStr(acctName, acctPlan, "default"))
		groupKey := provider
		if acctSlug != "" && acctSlug != "default" {
			groupKey = provider + ":" + acctSlug
		}
		g, ok := byAcct[groupKey]
		if !ok {
			label := providerLabel
			if acctName != "" && !strings.EqualFold(acctName, providerLabel) {
				label = providerLabel + " · " + acctName
			}
			g = &AccountGroup{
				Key:           groupKey,
				Provider:      provider,
				ProviderLabel: providerLabel,
				AccountID:     acctSlug,
				AccountLabel:  label,
				Plan:          acctPlan,
				State:         defaultStr(state, "ready"),
				UpdatedAt:     updatedAt,
				Windows:       []Window{},
			}
			byAcct[groupKey] = g
			order = append(order, groupKey)
		}
		g.Windows = append(g.Windows, w)
	}

	for _, mw := range windows {
		addWindow(mw.account, mw.plan, mw.window)
	}

	if len(order) == 0 && !isErr {
		return parsedEntry{}, false
	}

	groups := make([]AccountGroup, 0, len(order))
	for _, k := range order {
		groups = append(groups, *byAcct[k])
	}

	return parsedEntry{
		cacheKey:      rawKey,
		provider:      provider,
		providerLabel: providerLabel,
		isError:       isErr,
		state:         defaultStr(state, "error"),
		errMsg:        errMsg,
		lastSuccessAt: lastSuccessAt,
		groups:        groups,
	}, true
}

type windowWithAccount struct {
	account string
	plan    string
	window  Window
}

func extractWindows(obj map[string]json.RawMessage, now time.Time) []windowWithAccount {
	var out []windowWithAccount
	seen := map[string]bool{}
	appendUnique := func(acct, plan string, w Window) {
		k := acct + "\x00" + w.ID
		if seen[k] {
			w.ID = fmt.Sprintf("%s-%d", w.ID, len(seen)+1)
			k = acct + "\x00" + w.ID
		}
		seen[k] = true
		out = append(out, windowWithAccount{account: acct, plan: plan, window: w})
	}

	// 1. Named top-level / windows map entries supported by V1 & provider plugins.
	namedKeys := []struct {
		jsonKey string
		id      string
		label   string
	}{
		{"fiveHour", "session", "Session"},
		{"session", "session", "Session"},
		{"primary", "session", "Session"},
		{"sevenDay", "weekly", "Weekly"},
		{"weekly", "weekly", "Weekly"},
		{"secondary", "weekly", "Weekly"},
		{"sonnet", "sonnet", "Sonnet"},
		{"opus", "opus", "Opus"},
	}

	searchMap := func(m map[string]json.RawMessage) {
		for _, spec := range namedKeys {
			raw, ok := m[spec.jsonKey]
			if !ok || len(raw) == 0 {
				continue
			}
			var rm rawMeter
			if json.Unmarshal(raw, &rm) != nil {
				continue
			}
			if w, ok := buildWindow(spec.id, firstNonEmptyStr(rm.Label, rm.Name, spec.label), rm, now); ok {
				appendUnique(rm.Account, rm.Plan, w)
			}
		}
	}

	searchMap(obj)
	if rawWin, ok := obj["windows"]; ok {
		var wm map[string]json.RawMessage
		if json.Unmarshal(rawWin, &wm) == nil && wm != nil {
			searchMap(wm)
			// Include any custom keys in windows map.
			keys := make([]string, 0, len(wm))
			for k := range wm {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if isStandardAlias(k) {
					continue
				}
				var rm rawMeter
				if json.Unmarshal(wm[k], &rm) != nil {
					continue
				}
				id := normalizeWindowID(firstNonEmptyStr(rm.ID, rm.Key, k, rm.Label))
				label := firstNonEmptyStr(rm.Label, rm.Name, k)
				if w, ok := buildWindow(id, label, rm, now); ok {
					appendUnique(rm.Account, rm.Plan, w)
				}
			}
		}
	}

	// 2. Meters array (real ~/.maestri/usage/providers/.status.json format: flat meters or meters[].windows[]).
	if rawMeters, ok := obj["meters"]; ok {
		var meters []rawMeter
		if json.Unmarshal(rawMeters, &meters) == nil {
			multiNested := 0
			for _, rm := range meters {
				if len(rm.Windows) > 0 {
					multiNested++
				}
			}
			for i, rm := range meters {
				if len(rm.Windows) > 0 {
					groupAcct := firstNonEmptyStr(rm.Account, rm.Email)
					if groupAcct == "" && multiNested > 1 {
						groupAcct = firstNonEmptyStr(rm.Label, rm.Name, rm.ID)
					}
					for j, rw := range rm.Windows {
						rawName := firstNonEmptyStr(rw.Ring, rw.ID, rw.Key, rw.Title, rw.Label, rw.Name)
						id := normalizeWindowID(rawName)
						if rw.ID == "" && rw.Key == "" && rw.Ring == "" {
							mins := rw.WindowMinutes
							if mins == 0 && rw.DurationSeconds > 0 {
								mins = rw.DurationSeconds / 60
							}
							if mins >= 240 && mins <= 360 {
								id = "session"
							} else if mins >= 9000 && mins <= 11000 {
								id = "weekly"
							}
						}
						if id == "" {
							id = fmt.Sprintf("window-%d-%d", i+1, j+1)
						}
						label := firstNonEmptyStr(rw.Title, rw.Label, rw.Name, rw.ID, id)
						if w, ok := buildWindow(id, label, rw, now); ok {
							acct := firstNonEmptyStr(rw.Account, rw.Email, groupAcct)
							pl := firstNonEmptyStr(rw.Plan, rm.Plan)
							appendUnique(acct, pl, w)
						}
					}
					continue
				}
				rawName := firstNonEmptyStr(rm.Ring, rm.ID, rm.Key, rm.Title, rm.Label, rm.Name)
				id := normalizeWindowID(rawName)
				if rm.ID == "" && rm.Key == "" {
					if rm.WindowMinutes >= 240 && rm.WindowMinutes <= 360 {
						id = "session"
					} else if rm.WindowMinutes >= 9000 && rm.WindowMinutes <= 11000 {
						id = "weekly"
					}
				}
				if id == "" {
					id = fmt.Sprintf("meter-%d", i+1)
				}
				label := firstNonEmptyStr(rm.Title, rm.Label, rm.Name, rm.Key, id)
				if w, ok := buildWindow(id, label, rm, now); ok {
					appendUnique(rm.Account, rm.Plan, w)
				}
			}
		}
	}

	return out
}

func buildWindow(id, label string, rm rawMeter, now time.Time) (Window, bool) {
	var usedPct float64
	switch {
	case rm.UsedPercent != nil:
		usedPct = *rm.UsedPercent
	case rm.Used != nil && rm.Limit != nil && *rm.Limit > 0:
		usedPct = (*rm.Used / *rm.Limit) * 100
	default:
		return Window{}, false
	}
	usedPct = clampPct(usedPct)
	rounded := int(math.Round(usedPct))
	rem := math.Max(0, 100-usedPct)

	var resetPtr *int64
	for _, cand := range []any{rm.ResetsAt, rm.ResetAt, rm.ResetTime} {
		if ts := parseTimestampAny(cand); ts > 0 {
			resetPtr = &ts
			break
		}
	}
	var resetLabel string
	if resetPtr != nil {
		resetLabel = FormatResetETA(time.UnixMilli(*resetPtr), now)
	}

	return Window{
		ID:           id,
		Label:        label,
		UsedPercent:  math.Round(usedPct*10) / 10,
		RoundedPct:   rounded,
		RemainingPct: math.Round(rem*10) / 10,
		ResetAt:      resetPtr,
		ResetLabel:   resetLabel,
	}, true
}

func normalizeProviderID(rawKey, declared string) string {
	cand := strings.ToLower(strings.TrimSpace(firstNonEmptyStr(declared, rawKey)))
	if i := strings.IndexByte(cand, ':'); i > 0 {
		cand = cand[:i]
	}
	return sanitizeKey(cand)
}

func displayProviderName(id string) string {
	switch strings.ToLower(id) {
	case "claude":
		return "Claude"
	case "codex":
		return "Codex"
	case "antigravity":
		return "Antigravity"
	case "gemini":
		return "Gemini"
	default:
		if id == "" {
			return "Provider"
		}
		return strings.ToUpper(id[:1]) + id[1:]
	}
}

func normalizeWindowID(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch s {
	case "5h", "fivehour", "five_hour", "session", "primary":
		return "session"
	case "7d", "sevenday", "seven_day", "weekly", "secondary":
		return "weekly"
	default:
		return sanitizeKey(strings.ReplaceAll(s, " ", "-"))
	}
}

func isStandardAlias(k string) bool {
	switch k {
	case "fiveHour", "session", "primary", "sevenDay", "weekly", "secondary", "sonnet", "opus":
		return true
	default:
		return false
	}
}

func sanitizeKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = nonKeyRE.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-.:")
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

func isErrorState(state, errMsg string) bool {
	ls := strings.ToLower(strings.TrimSpace(state))
	if ls == "error" || ls == "executablenotfound" || ls == "unavailable" || ls == "failed" || ls == "unauthenticated" {
		return true
	}
	if errMsg != "" && ls != "ready" && ls != "ok" && ls != "stale" {
		return true
	}
	return false
}

func extractError(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var obj struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return firstNonEmptyStr(obj.Message, obj.Code)
	}
	return ""
}

func strField(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	return ""
}

func parseTimestamp(raw json.RawMessage) int64 {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var anyVal any
	if json.Unmarshal(raw, &anyVal) != nil {
		return 0
	}
	return parseTimestampAny(anyVal)
}

func parseTimestampAny(v any) int64 {
	switch t := v.(type) {
	case float64:
		if t <= 0 {
			return 0
		}
		if t < 1e12 {
			return int64(t * 1000)
		}
		return int64(t)
	case int64:
		if t <= 0 {
			return 0
		}
		if t < 1e12 {
			return t * 1000
		}
		return t
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0
		}
		if n, err := strconv.ParseFloat(s, 64); err == nil {
			return parseTimestampAny(n)
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if parsed, err := time.Parse(layout, s); err == nil {
				return parsed.UnixMilli()
			}
		}
	}
	return 0
}

// FormatResetETA formats the remaining time until reset concisely (e.g. "2h 14m", "3d 4h").
func FormatResetETA(resetAt, now time.Time) string {
	d := resetAt.Sub(now)
	if d <= 0 {
		return "soon"
	}
	totalMins := int(math.Ceil(d.Minutes()))
	if totalMins < 60 {
		return fmt.Sprintf("%dm", totalMins)
	}
	hours := totalMins / 60
	mins := totalMins % 60
	if hours < 24 {
		if mins == 0 {
			return fmt.Sprintf("%dh", hours)
		}
		return fmt.Sprintf("%dh %dm", hours, mins)
	}
	days := hours / 24
	remHours := hours % 24
	if remHours == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd %dh", days, remHours)
}

func nearestMarkStep(pct int) int {
	if pct < 50 {
		pct = 50
	}
	if pct > 100 {
		pct = 100
	}
	return ((pct + 2) / 5) * 5
}

func clampPct(p float64) float64 {
	if math.IsNaN(p) || math.IsInf(p, 0) || p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func firstNonZero(vals ...int64) int64 {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}

func defaultStr(s, fb string) string {
	if s != "" {
		return s
	}
	return fb
}

func loadCache(db *sql.DB) map[string]cachedProvider {
	out := map[string]cachedProvider{}
	if db == nil {
		return out
	}
	raw, ok, err := store.GetKV(db, lastGoodKVKey)
	if err != nil || !ok || raw == "" {
		return out
	}
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

func saveCache(db *sql.DB, cache map[string]cachedProvider) {
	if db == nil {
		return
	}
	b, err := json.Marshal(cache)
	if err != nil {
		return
	}
	_ = store.SetKV(db, lastGoodKVKey, string(b))
}

func cloneGroups(in []AccountGroup) []AccountGroup {
	out := make([]AccountGroup, len(in))
	for i, g := range in {
		g.Windows = append([]Window(nil), g.Windows...)
		out[i] = g
	}
	return out
}
