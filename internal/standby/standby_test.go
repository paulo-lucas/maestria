package standby

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/maestria-dev/maestria/internal/audit"
	"github.com/maestria-dev/maestria/internal/config"
	"github.com/maestria-dev/maestria/internal/events"
	"github.com/maestria-dev/maestria/internal/store"
	"github.com/maestria-dev/maestria/internal/usage"
	"github.com/maestria-dev/maestria/internal/workspaces"
)

func makeSnap(used float64, resetAt time.Time) usage.Snapshot {
	resetMs := resetAt.UnixMilli()
	return usage.Snapshot{
		Available: true,
		Accounts: []usage.AccountGroup{{
			Key:           "claude:pro",
			Provider:      "claude",
			ProviderLabel: "Claude",
			AccountLabel:  "Claude · pro",
			State:         "ready",
			Windows: []usage.Window{{
				ID:          "session",
				Label:       "Session",
				UsedPercent: used,
				ResetAt:     &resetMs,
			}},
		}},
	}
}

func TestEvaluateStandbyActionsHysteresisResetAndManualResume(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	auditLog := audit.New(db, time.Now)

	resumeBelow := 80
	cfg := &config.Config{
		Standby: config.Standby{
			Thresholds:         map[string]map[string]int{"claude": {"session": 90}},
			Actions:            map[string]map[string]string{"claude": {"session": config.StandbyActionInterrupt}},
			ResumeBelowPercent: &resumeBelow,
			WakeOnReset:        true,
		},
	}
	wsList := []workspaces.Workspace{
		{ID: "ws-claude", Name: "Claude WS", MaestroProvider: "claude"},
		{ID: "ws-any", Name: "Any WS", MaestroProvider: ""},
		{ID: "ws-codex", Name: "Codex WS", MaestroProvider: "codex"},
	}

	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(2 * time.Hour)

	// 1. Below threshold -> no standby.
	tr, err := Evaluate(db, cfg, auditLog, wsList, makeSnap(85, resetAt), now, nil)
	if err != nil || tr.Summary.Active || len(tr.Events) != 0 {
		t.Fatalf("unexpected standby at 85%%: %+v err=%v", tr, err)
	}

	// 2. Hit threshold 92% >= 90% -> triggers standby with action="interrupt" for claude & any, not codex.
	tr, err = Evaluate(db, cfg, auditLog, wsList, makeSnap(92, resetAt), now.Add(time.Minute), nil)
	if err != nil || !tr.Summary.Active || tr.Summary.Action != config.StandbyActionInterrupt || len(tr.Events) != 2 {
		t.Fatalf("expected active interrupt standby with 2 events: %+v err=%v", tr, err)
	}
	var payload map[string]any
	if err := json.Unmarshal(tr.Events[0].Payload, &payload); err != nil || payload["action"] != config.StandbyActionInterrupt {
		t.Fatalf("unexpected event payload: %s err=%v", tr.Events[0].Payload, err)
	}
	codexEvs, _ := events.Since(db, "ws-codex", 0, 10)
	if len(codexEvs) != 0 {
		t.Fatalf("codex workspace should not receive claude standby: %+v", codexEvs)
	}

	// 3. Re-evaluating at 93% must NOT re-emit duplicate standby events.
	tr, err = Evaluate(db, cfg, auditLog, wsList, makeSnap(93, resetAt), now.Add(2*time.Minute), nil)
	if err != nil || len(tr.Events) != 0 {
		t.Fatalf("duplicate standby emitted: %+v", tr)
	}

	// 4. Hysteresis: dropping to 85% (> resumeBelow 80%) keeps standby active.
	tr, err = Evaluate(db, cfg, auditLog, wsList, makeSnap(85, resetAt), now.Add(3*time.Minute), nil)
	if err != nil || !tr.Summary.Active {
		t.Fatalf("expected hysteresis to hold standby at 85%%: %+v", tr)
	}

	// 5. Dropping to 78% (<= 80%) resumes with reason="below_threshold".
	tr, err = Evaluate(db, cfg, auditLog, wsList, makeSnap(78, resetAt), now.Add(4*time.Minute), nil)
	if err != nil || tr.Summary.Active || len(tr.Events) != 2 || tr.Events[0].Kind != events.KindResume {
		t.Fatalf("expected hysteresis resume at 78%%: %+v err=%v", tr, err)
	}

	// 6. Trigger standby again with default action="tell", then wake on reset when resetAt passes.
	cfg.Standby.Actions["claude"]["session"] = config.StandbyActionTell
	cfg.Standby.ResumeBelowPercent = nil
	if _, err := Evaluate(db, cfg, auditLog, wsList, makeSnap(95, resetAt), now.Add(5*time.Minute), nil); err != nil {
		t.Fatal(err)
	}
	nextReset := resetAt.Add(5 * time.Hour)
	tr, err = Evaluate(db, cfg, auditLog, wsList, makeSnap(85, nextReset), resetAt.Add(time.Minute), nil)
	if err != nil || tr.Summary.Active || len(tr.Events) != 2 || tr.Events[0].Kind != events.KindResume {
		t.Fatalf("expected wake on reset: %+v err=%v", tr, err)
	}

	// 7. Manual resume clears active guard immediately and stays suppressed for the current window.
	if _, err := Evaluate(db, cfg, auditLog, wsList, makeSnap(96, nextReset), resetAt.Add(2*time.Minute), nil); err != nil {
		t.Fatal(err)
	}
	tr, err = ResumeNow(db, auditLog, wsList, "", resetAt.Add(3*time.Minute), "127.0.0.1", nil)
	if err != nil || tr.Summary.Active || len(tr.Events) != 2 {
		t.Fatalf("manual resume failed: %+v err=%v", tr, err)
	}
	tr, err = Evaluate(db, cfg, auditLog, wsList, makeSnap(96, nextReset), resetAt.Add(4*time.Minute), nil)
	if err != nil || tr.Summary.Active || len(tr.Events) != 0 {
		t.Fatalf("expected manual resume to suppress immediate re-standby while window unchanged: %+v err=%v", tr, err)
	}
	// Dropping below threshold re-arms the guard, so crossing again triggers standby.
	if _, err := Evaluate(db, cfg, auditLog, wsList, makeSnap(50, nextReset), resetAt.Add(5*time.Minute), nil); err != nil {
		t.Fatal(err)
	}
	tr, err = Evaluate(db, cfg, auditLog, wsList, makeSnap(94, nextReset), resetAt.Add(6*time.Minute), nil)
	if err != nil || !tr.Summary.Active || len(tr.Events) != 2 {
		t.Fatalf("expected standby to re-arm after dropping below threshold: %+v err=%v", tr, err)
	}
}
