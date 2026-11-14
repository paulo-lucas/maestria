package activity

import (
	"testing"

	"github.com/paulo-lucas/maestria/internal/wire"
)

func term(id, name string, running bool) wire.TerminalCard {
	return wire.TerminalCard{ID: id, Name: name, IsRunning: running, Preview: []string{"first", "\x1b[32m✓\x1b[0m tests passed", "", ""}}
}

func TestBuildGroupsByFloorAndPrefersTheMaestrosBrief(t *testing.T) {
	maestro := term("m", "Maestro", true)
	maestro.IsManager = true
	orin := term("o", "Orin", true)
	orin.FloorID, orin.FloorName = "f1", "billing"
	vesper := term("v", "Vesper", true)
	vesper.FloorID, vesper.FloorName = "f1", "billing"
	wren := term("w", "Wren", false)
	wren.FloorID, wren.FloorName = "f2", "invoice"
	p := 0.85
	snap := &wire.FeedSnapshot{
		Floors: []wire.FloorMeta{{Name: "main"}, {ID: "f1", Name: "billing", Branch: "billing", Status: &wire.FloorStatus{State: "review", Progress: &p}}, {ID: "f2", Name: "invoice"}},
		Items: []wire.FeedItem{
			{Kind: wire.ItemTerminal, Terminal: &maestro},
			{Kind: wire.ItemPendingPrompt, Terminal: &orin, Prompt: "Install zod? (Y/n)"},
			{Kind: wire.ItemTerminal, Terminal: &vesper},
			{Kind: wire.ItemTerminal, Terminal: &wren},
		},
	}
	act := Build(snap, map[string]string{"vesper": "Writing retry tests"})
	if len(act.Groups) != 3 || act.Groups[0].Name != "billing" {
		t.Fatalf("floors needing you must come first: %+v", act.Groups)
	}
	if act.Counts.Total != 4 || act.Counts.Needs != 1 || act.Counts.NotRunning != 1 || act.Counts.Idle != 2 {
		t.Fatalf("counts = %+v", act.Counts)
	}
	b := act.Groups[0]
	if b.Branch != "billing" || b.StatusLabel != "Review" || b.Progress == nil || *b.Progress != 0.85 {
		t.Fatalf("floor = %+v", b)
	}
	var orinA, vesperA Agent
	for _, a := range b.Agents {
		if a.Name == "Orin" {
			orinA = a
		}
		if a.Name == "Vesper" {
			vesperA = a
		}
	}
	if orinA.State != StateYN || orinA.PromptHash == "" || orinA.Tone != "needs" {
		t.Fatalf("orin = %+v", orinA)
	}
	if vesperA.Brief != "Writing retry tests" || vesperA.BriefSource != "maestro" {
		t.Fatalf("vesper = %+v", vesperA)
	}
	// No brief from the Maestro: fall back to the last preview line, stripped of escape codes.
	act = Build(snap, nil)
	for _, g := range act.Groups {
		for _, a := range g.Agents {
			if a.Name == "Vesper" && (a.Brief != "✓ tests passed" || a.BriefSource != "wire") {
				t.Fatalf("fallback brief = %q (%s)", a.Brief, a.BriefSource)
			}
			if a.Name == "Wren" && a.Brief != "" {
				t.Fatalf("a closed terminal has no brief: %+v", a)
			}
		}
	}
}

func TestClassifyAgent(t *testing.T) {
	mk := func(running bool, status string, active, attn bool) wire.FeedTerminal {
		c := wire.TerminalCard{IsRunning: running, IsActive: active, NeedsAttention: attn}
		if status != "" {
			c.Status = []byte(`"` + status + `"`)
		}
		return wire.FeedTerminal{TerminalCard: c}
	}
	cases := []struct {
		t    wire.FeedTerminal
		want string
	}{
		{mk(false, "working", true, false), StateNotRunning},
		{mk(true, "working", false, false), StateWorking},
		{mk(true, "Idle", false, false), StateIdle},
		{mk(true, "", true, false), StateWorking},
		{mk(true, "", false, false), StateIdle},
		{mk(true, "idle", false, true), StateNeeds},
		{mk(true, "mystery", false, false), StateIdle},
	}
	for i, c := range cases {
		if got := ClassifyAgent(c.t); got != c.want {
			t.Errorf("case %d: %s, want %s", i, got, c.want)
		}
	}
	p := mk(true, "idle", false, false)
	p.Pending = true
	if ClassifyAgent(p) != StateYN {
		t.Error("pending must be yn")
	}
	obj := wire.TerminalCard{IsRunning: true, Status: []byte(`{"state":"working"}`)}
	if ClassifyAgent(wire.FeedTerminal{TerminalCard: obj}) != StateWorking {
		t.Error("object status")
	}
}
