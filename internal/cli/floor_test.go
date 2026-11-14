package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paulo-lucas/maestria/internal/floors"
)

func TestCLIFloorCommandsAndErrors(t *testing.T) {
	now := time.Date(2026, 11, 14, 15, 0, 0, 0, time.UTC)
	env, _, wsDir, out, errBuf := newConnectorEnv(t, now)

	runCLI(t, env, out, errBuf, "workspace", "add", wsDir, "--name", "Atlas", "--json")

	// Plan two floors (one ongoing) and verify JSON output.
	planOut := runCLI(t, env, out, errBuf, "floor", "plan", "billing-webhooks", "--brief", "Add webhook delivery", "-w", "atlas", "--json")
	var planned floors.Floor
	if err := json.Unmarshal([]byte(planOut), &planned); err != nil || planned.State != floors.StatePlanned || planned.Name != "billing-webhooks" {
		t.Fatalf("floor plan json=%q err=%v", planOut, err)
	}

	runCLI(t, env, out, errBuf, "floor", "plan", "dep-watch", "--brief", "Weekly checks", "--ongoing", "-w", "atlas", "--json")
	runCLI(t, env, out, errBuf, "floor", "plan", "sso-login", "--brief", "OIDC login", "-w", "atlas", "--json")

	// Reorder sso-login before dep-watch -> billing-webhooks, sso-login, dep-watch.
	reOut := runCLI(t, env, out, errBuf, "floor", "reorder", "sso-login", "--before", "dep-watch", "-w", "atlas", "--json")
	var reRes struct {
		Floors []floors.Floor `json:"floors"`
	}
	if err := json.Unmarshal([]byte(reOut), &reRes); err != nil || len(reRes.Floors) != 3 || reRes.Floors[1].Name != "sso-login" {
		t.Fatalf("floor reorder json=%q err=%v", reOut, err)
	}

	// Edit rename + brief.
	editOut := runCLI(t, env, out, errBuf, "floor", "edit", "sso-login", "--rename", "oidc-login", "--brief", "OIDC admin login", "-w", "atlas", "--json")
	var edited floors.Floor
	if err := json.Unmarshal([]byte(editOut), &edited); err != nil || edited.Name != "oidc-login" || edited.Brief != "OIDC admin login" {
		t.Fatalf("floor edit json=%q err=%v", editOut, err)
	}

	// Cannot land planned floor without --force.
	out.Reset()
	errBuf.Reset()
	if code := Run([]string{"floor", "land", "oidc-login", "-w", "atlas"}, *env); code == 0 {
		t.Fatalf("expected non-zero exit landing planned floor without --force")
	}

	// Start billing-webhooks and update status.
	startOut := runCLI(t, env, out, errBuf, "floor", "start", "billing-webhooks", "-w", "atlas", "--json")
	var started floors.Floor
	if err := json.Unmarshal([]byte(startOut), &started); err != nil || started.State != floors.StateActive {
		t.Fatalf("floor start json=%q err=%v", startOut, err)
	}

	stOut := runCLI(t, env, out, errBuf, "floor", "status", "billing-webhooks", "--state", "review", "--progress", "2/5", "--text", "Tests green", "--agents", "Orin,Vesper", "-w", "atlas", "--json")
	var stFloor floors.Floor
	if err := json.Unmarshal([]byte(stOut), &stFloor); err != nil || stFloor.Status != floors.StatusReview || stFloor.Progress == nil || *stFloor.Progress != 0.4 || len(stFloor.Agents) != 2 {
		t.Fatalf("floor status json=%q err=%v", stOut, err)
	}

	// Status inspect with no flags prints current status.
	inspectOut := runCLI(t, env, out, errBuf, "floor", "status", "billing-webhooks", "-w", "atlas")
	if !strings.Contains(inspectOut, "[active/review] billing-webhooks") || !strings.Contains(inspectOut, "40%") {
		t.Fatalf("floor status inspect=%q", inspectOut)
	}

	// Land billing-webhooks and cancel oidc-login.
	landOut := runCLI(t, env, out, errBuf, "floor", "land", "billing-webhooks", "--outcome", "Shipped retries", "-w", "atlas", "--json")
	var landed floors.Floor
	if err := json.Unmarshal([]byte(landOut), &landed); err != nil || landed.State != floors.StateLanded || landed.Outcome != "Shipped retries" {
		t.Fatalf("floor land json=%q err=%v", landOut, err)
	}

	cancelOut := runCLI(t, env, out, errBuf, "floor", "cancel", "oidc-login", "--outcome", "Deferred", "-w", "atlas", "--json")
	var cancelled floors.Floor
	if err := json.Unmarshal([]byte(cancelOut), &cancelled); err != nil || cancelled.State != floors.StateCancelled {
		t.Fatalf("floor cancel json=%q err=%v", cancelOut, err)
	}

	// list without --all shows only dep-watch; list --all shows all 3.
	listCur := runCLI(t, env, out, errBuf, "floor", "list", "-w", "atlas", "--json")
	var curRes struct {
		Floors []floors.Floor `json:"floors"`
	}
	if err := json.Unmarshal([]byte(listCur), &curRes); err != nil || len(curRes.Floors) != 1 || curRes.Floors[0].Name != "dep-watch" {
		t.Fatalf("floor list json=%q err=%v", listCur, err)
	}

	listAll := runCLI(t, env, out, errBuf, "floor", "list", "--all", "-w", "atlas", "--json")
	var allRes struct {
		Floors []floors.Floor `json:"floors"`
	}
	if err := json.Unmarshal([]byte(listAll), &allRes); err != nil || len(allRes.Floors) != 3 {
		t.Fatalf("floor list --all json=%q err=%v", listAll, err)
	}

	// Error cases: missing args -> exit 2, unknown floor -> exit 1, bad progress -> exit 1.
	for _, bad := range [][]string{
		{"floor", "plan", "only-name", "-w", "atlas"},
		{"floor", "reorder", "dep-watch", "-w", "atlas"},
		{"floor", "edit", "dep-watch", "-w", "atlas"},
		{"floor", "bogus"},
	} {
		out.Reset()
		errBuf.Reset()
		if code := Run(bad, *env); code != 2 {
			t.Errorf("Run(%v) exit=%d, want 2", bad, code)
		}
	}

	for _, fail := range [][]string{
		{"floor", "start", "missing-floor", "-w", "atlas"},
		{"floor", "status", "dep-watch", "--progress", "2.0", "-w", "atlas"},
	} {
		out.Reset()
		errBuf.Reset()
		if code := Run(fail, *env); code != 1 {
			t.Errorf("Run(%v) exit=%d, want 1", fail, code)
		}
	}

	// Verify cwd workspace inference.
	oldWd, _ := os.Getwd()
	subDir := filepath.Join(wsDir, "sub")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(subDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	cwdOut := runCLI(t, env, out, errBuf, "floor", "list", "--json")
	if !strings.Contains(cwdOut, `"workspace": "atlas"`) {
		t.Fatalf("expected cwd workspace inference to atlas, got %s", cwdOut)
	}
}
