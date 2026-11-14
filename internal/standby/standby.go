// Package standby evaluates quota thresholds against read-only usage and emits standby/resume events.
package standby

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/maestria-dev/maestria/internal/audit"
	"github.com/maestria-dev/maestria/internal/config"
	"github.com/maestria-dev/maestria/internal/events"
	"github.com/maestria-dev/maestria/internal/store"
	"github.com/maestria-dev/maestria/internal/usage"
	"github.com/maestria-dev/maestria/internal/workspaces"
)

const stateKVKey = "standby.state_v1"

// ActiveGuard describes one quota window currently holding workspaces on standby.
type ActiveGuard struct {
	GuardKey     string  `json:"guardKey"`
	AccountKey   string  `json:"accountKey"`
	Provider     string  `json:"provider"`
	AccountLabel string  `json:"accountLabel"`
	WindowID     string  `json:"windowId"`
	WindowLabel  string  `json:"windowLabel"`
	Action       string  `json:"action"`
	Threshold    int     `json:"threshold"`
	UsedPercent  float64 `json:"usedPercent"`
	ResetAt      *int64  `json:"resetAt,omitempty"`
	EnteredAt    int64   `json:"enteredAt"`
	Reason       string  `json:"reason"`
}

// Summary represents the current standby guard state across providers.
type Summary struct {
	Active bool          `json:"active"`
	Action string        `json:"action,omitempty"`
	Guards []ActiveGuard `json:"guards"`
	Banner string        `json:"banner,omitempty"`
}

// Transition records events emitted during one Evaluate or ResumeNow run.
type Transition struct {
	Changed bool
	Events  []events.Event
	Summary Summary
}

// CLINotifier optionally dispatches standby/resume notices when notifyViaCli is enabled.
type CLINotifier func(ev events.Event)

type suppressedGuard struct {
	ResetAt   *int64 `json:"resetAt,omitempty"`
	Threshold int    `json:"threshold"`
	ResumedAt int64  `json:"resumedAt"`
}

type persistedState struct {
	Active     map[string]ActiveGuard     `json:"active"`
	Suppressed map[string]suppressedGuard `json:"suppressed,omitempty"`
}

// Current returns the persisted standby summary.
func Current(db *sql.DB) (Summary, error) {
	st, err := loadState(db)
	if err != nil {
		return Summary{}, err
	}
	return summarize(st), nil
}

// Evaluate checks usage against cfg.Standby thresholds, triggers standby or resume events cleanly, and persists state.
func Evaluate(
	db *sql.DB,
	cfg *config.Config,
	auditLog *audit.Log,
	wsList []workspaces.Workspace,
	snap usage.Snapshot,
	now time.Time,
	notifier CLINotifier,
) (Transition, error) {
	if now.IsZero() {
		now = time.Now()
	}
	st, err := loadState(db)
	if err != nil {
		return Transition{}, err
	}
	if !snap.Available {
		return Transition{Summary: summarize(st)}, nil
	}

	nowMs := now.UnixMilli()
	var emitted []events.Event
	changed := false

	seenWindows := map[string]usage.Window{}
	seenAccount := map[string]usage.AccountGroup{}

	for _, g := range snap.Accounts {
		if g.Stale {
			continue
		}
		for _, w := range g.Windows {
			gk := g.Key + ":" + w.ID
			seenWindows[gk] = w
			seenAccount[gk] = g

			th, act := usage.ResolveWindowRule(cfg.Standby, g.Provider, g.Key, w.ID)
			if sup, ok := st.Suppressed[gk]; ok {
				resetCleared := sup.ResetAt != nil && *sup.ResetAt > 0 &&
					(nowMs >= *sup.ResetAt || (w.ResetAt != nil && *w.ResetAt > *sup.ResetAt))
				thresholdCleared := th == 0 || w.UsedPercent < float64(th) || th != sup.Threshold
				if resetCleared || thresholdCleared {
					delete(st.Suppressed, gk)
					changed = true
				} else {
					continue
				}
			}
			cur, isActive := st.Active[gk]

			if th > 0 && w.UsedPercent >= float64(th) {
				if !isActive || cur.Action != act {
					guard := ActiveGuard{
						GuardKey:     gk,
						AccountKey:   g.Key,
						Provider:     g.Provider,
						AccountLabel: g.AccountLabel,
						WindowID:     w.ID,
						WindowLabel:  w.Label,
						Action:       act,
						Threshold:    th,
						UsedPercent:  w.UsedPercent,
						ResetAt:      cloneInt64Ptr(w.ResetAt),
						EnteredAt:    nowMs,
						Reason: fmt.Sprintf("%s %s reached %.0f%% (threshold %d%%)",
							g.AccountLabel, w.Label, w.UsedPercent, th),
					}
					st.Active[gk] = guard
					changed = true
					evs, err := emitProviderEvent(db, wsList, g.Provider, events.KindStandby, map[string]any{
						"provider":    g.Provider,
						"account":     g.AccountLabel,
						"window":      w.ID,
						"usedPercent": w.UsedPercent,
						"threshold":   th,
						"resetAt":     w.ResetAt,
						"action":      act,
						"reason":      guard.Reason,
					}, nowMs, cfg.Standby.NotifyViaCLI, notifier)
					if err != nil {
						return Transition{}, err
					}
					emitted = append(emitted, evs...)
					if auditLog != nil {
						_ = auditLog.Write("standby.enter", "guard", map[string]any{
							"guardKey":    gk,
							"action":      act,
							"usedPercent": w.UsedPercent,
							"threshold":   th,
						})
					}
				} else {
					cur.UsedPercent = w.UsedPercent
					if cur.ResetAt == nil && w.ResetAt != nil {
						cur.ResetAt = cloneInt64Ptr(w.ResetAt)
						st.Active[gk] = cur
						changed = true
					}
				}
				continue
			}

			if !isActive {
				continue
			}

			reason := wakeReason(cfg.Standby, cur, w, th, nowMs)
			if reason == "" {
				if cur.UsedPercent != w.UsedPercent {
					cur.UsedPercent = w.UsedPercent
					st.Active[gk] = cur
					changed = true
				}
				continue
			}

			delete(st.Active, gk)
			changed = true
			evs, err := emitProviderEvent(db, wsList, cur.Provider, events.KindResume, map[string]any{
				"provider":           cur.Provider,
				"account":            cur.AccountLabel,
				"window":             cur.WindowID,
				"usedPercent":        w.UsedPercent,
				"resumeBelowPercent": cfg.Standby.ResumeBelowPercent,
				"reason":             reason,
			}, nowMs, cfg.Standby.NotifyViaCLI, notifier)
			if err != nil {
				return Transition{}, err
			}
			emitted = append(emitted, evs...)
			if auditLog != nil {
				_ = auditLog.Write("standby.resume", "guard", map[string]any{
					"guardKey":    gk,
					"reason":      reason,
					"usedPercent": w.UsedPercent,
				})
			}
		}
	}

	// If thresholds were removed for an active guard whose window vanished or no longer has threshold, wake cleanly.
	for gk, cur := range st.Active {
		if _, ok := seenWindows[gk]; ok {
			continue
		}
		th, _ := usage.ResolveWindowRule(cfg.Standby, cur.Provider, cur.AccountKey, cur.WindowID)
		if th == 0 {
			delete(st.Active, gk)
			changed = true
			evs, err := emitProviderEvent(db, wsList, cur.Provider, events.KindResume, map[string]any{
				"provider": cur.Provider,
				"account":  cur.AccountLabel,
				"window":   cur.WindowID,
				"reason":   "threshold_cleared",
			}, nowMs, cfg.Standby.NotifyViaCLI, notifier)
			if err != nil {
				return Transition{}, err
			}
			emitted = append(emitted, evs...)
		}
	}

	if changed {
		if err := saveState(db, st); err != nil {
			return Transition{}, err
		}
	}
	return Transition{Changed: changed, Events: emitted, Summary: summarize(st)}, nil
}

// ResumeNow manually clears one guardKey (or all active guards when guardKey == "") and emits resume events.
func ResumeNow(
	db *sql.DB,
	auditLog *audit.Log,
	wsList []workspaces.Workspace,
	guardKey string,
	now time.Time,
	sourceIP string,
	notifier CLINotifier,
) (Transition, error) {
	if now.IsZero() {
		now = time.Now()
	}
	st, err := loadState(db)
	if err != nil {
		return Transition{}, err
	}
	nowMs := now.UnixMilli()
	var targets []ActiveGuard
	for gk, g := range st.Active {
		if guardKey == "" || guardKey == gk {
			targets = append(targets, g)
		}
	}
	if len(targets) == 0 {
		return Transition{Summary: summarize(st)}, nil
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].GuardKey < targets[j].GuardKey })

	var emitted []events.Event
	for _, g := range targets {
		delete(st.Active, g.GuardKey)
		st.Suppressed[g.GuardKey] = suppressedGuard{
			ResetAt:   cloneInt64Ptr(g.ResetAt),
			Threshold: g.Threshold,
			ResumedAt: nowMs,
		}
		evs, err := emitProviderEvent(db, wsList, g.Provider, events.KindResume, map[string]any{
			"provider": g.Provider,
			"account":  g.AccountLabel,
			"window":   g.WindowID,
			"reason":   "manual",
		}, nowMs, notifier != nil, notifier)
		if err != nil {
			return Transition{}, err
		}
		emitted = append(emitted, evs...)
	}
	if err := saveState(db, st); err != nil {
		return Transition{}, err
	}
	if auditLog != nil {
		_ = auditLog.Write("standby.manual_resume", sourceIP, map[string]any{
			"guardKey": guardKey,
			"cleared":  len(targets),
		})
	}
	return Transition{Changed: true, Events: emitted, Summary: summarize(st)}, nil
}

func wakeReason(sb config.Standby, cur ActiveGuard, w usage.Window, currentThreshold int, nowMs int64) string {
	if currentThreshold == 0 {
		return "threshold_cleared"
	}
	if sb.WakeOnReset && cur.ResetAt != nil && *cur.ResetAt > 0 {
		resetPassed := nowMs > *cur.ResetAt
		resetRolledForward := w.ResetAt != nil && *w.ResetAt > *cur.ResetAt
		if (resetPassed || resetRolledForward) && w.UsedPercent < float64(currentThreshold) {
			return "reset"
		}
	}
	if sb.ResumeBelowPercent != nil && w.UsedPercent <= float64(*sb.ResumeBelowPercent) {
		return "below_threshold"
	}
	return ""
}

func emitProviderEvent(
	db *sql.DB,
	wsList []workspaces.Workspace,
	provider string,
	kind string,
	payload map[string]any,
	nowMs int64,
	notifyCLI bool,
	notifier CLINotifier,
) ([]events.Event, error) {
	var out []events.Event
	for _, w := range wsList {
		if w.MaestroProvider != "" && !strings.EqualFold(w.MaestroProvider, provider) {
			continue
		}
		ev, err := events.Append(db, w.ID, kind, payload, nowMs)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
		if notifyCLI && notifier != nil {
			notifier(ev)
		}
	}
	return out, nil
}

func summarize(st persistedState) Summary {
	if len(st.Active) == 0 {
		return Summary{Active: false, Guards: []ActiveGuard{}}
	}
	guards := make([]ActiveGuard, 0, len(st.Active))
	action := config.StandbyActionTell
	for _, g := range st.Active {
		guards = append(guards, g)
		if g.Action == config.StandbyActionInterrupt {
			action = config.StandbyActionInterrupt
		}
	}
	sort.Slice(guards, func(i, j int) bool { return guards[i].GuardKey < guards[j].GuardKey })
	banner := guards[0].Reason
	if len(guards) > 1 {
		banner = fmt.Sprintf("%s (+%d more)", guards[0].Reason, len(guards)-1)
	}
	return Summary{
		Active: true,
		Action: action,
		Guards: guards,
		Banner: banner,
	}
}

func loadState(db *sql.DB) (persistedState, error) {
	raw, ok, err := store.GetKV(db, stateKVKey)
	if err != nil {
		return persistedState{}, err
	}
	st := persistedState{
		Active:     map[string]ActiveGuard{},
		Suppressed: map[string]suppressedGuard{},
	}
	if ok && raw != "" {
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			return persistedState{}, err
		}
		if st.Active == nil {
			st.Active = map[string]ActiveGuard{}
		}
		if st.Suppressed == nil {
			st.Suppressed = map[string]suppressedGuard{}
		}
	}
	return st, nil
}

func saveState(db *sql.DB, st persistedState) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return store.SetKV(db, stateKVKey, string(raw))
}

func cloneInt64Ptr(p *int64) *int64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
