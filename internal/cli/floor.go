package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/maestria-dev/maestria/internal/floors"
	"github.com/maestria-dev/maestria/internal/store"
)

const floorHelp = `Usage: maestria floor <subcommand> [options]

Subcommands:
  plan <name> --brief <md> [--ongoing]
      Plan a Future floor (does not start it).
  start <name> [--brief <md>] [--ongoing]
      Start a planned floor (moving it to Present), or create and start one if --brief is given.
  status <name> [--state working|blocked|review|done] [--progress 0.4|2/5|40%] [--text <msg>] [--agents a,b]
      Update (or inspect when no flags are given) a Present floor's status.
  land <name> [--outcome <md>] [--force]
      Mark an active floor as Landed in Past. Planned floors require --force.
  cancel <name> [--outcome <md>]
      Cancel a planned or active floor and record it in Past.
  list [--all]
      List Present and Future floors (pass --all to include Past).
  edit <name> [--brief <md>] [--rename <new-name>] [--ongoing[=true|false]]
      Edit a planned or active floor.
  reorder <name> --before <other> | --after <other>
      Reorder a Future (planned) floor relative to another Future floor.

Common flags:
  -w, --workspace <id|name>   Workspace (defaults to cwd)
      --json                  Output JSON
`

func cmdFloor(args []string, env *Env) error {
	if len(args) == 0 {
		return cmdFloorList(nil, env)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "help", "--help", "-h":
		fmt.Fprint(env.Stdout, floorHelp)
		return nil
	case "plan":
		return cmdFloorPlan(rest, env)
	case "start":
		return cmdFloorStart(rest, env)
	case "status":
		return cmdFloorStatus(rest, env)
	case "land":
		return cmdFloorLand(rest, env)
	case "cancel":
		return cmdFloorCancel(rest, env)
	case "list", "ls":
		return cmdFloorList(rest, env)
	case "edit":
		return cmdFloorEdit(rest, env)
	case "reorder":
		return cmdFloorReorder(rest, env)
	default:
		if strings.HasPrefix(sub, "-") {
			return cmdFloorList(args, env)
		}
		return usagef("Unknown floor subcommand: %s\n\n%s", sub, floorHelp)
	}
}

func readStdinIfDash(val string, env *Env) (string, error) {
	if val != "-" {
		return val, nil
	}
	raw, err := io.ReadAll(env.Stdin)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func cmdFloorPlan(args []string, env *Env) error {
	parsed, err := parseFlexArgs(args, flagSpec{
		Bools:   map[string]bool{"json": true, "ongoing": true},
		Strings: map[string]bool{"workspace": true, "brief": true},
		Shorts:  map[string]string{"w": "workspace", "b": "brief"},
	})
	if err != nil {
		return usagef("%v", err)
	}
	if len(parsed.Pos) != 1 || !parsed.HasString("brief") {
		return usagef("Usage: maestria floor plan <name> --brief <md> [--ongoing] [-w <ws>] [--json]")
	}
	brief, err := readStdinIfDash(parsed.String("brief"), env)
	if err != nil {
		return err
	}
	ws, _, _, err := resolveWorkspaceCLI(env, parsed.String("workspace"))
	if err != nil {
		return err
	}
	db, err := store.Open(configDir(env))
	if err != nil {
		return err
	}
	defer db.Close()

	fl, err := floors.Plan(db, ws.ID, floors.PlanInput{
		Name:    parsed.Pos[0],
		Brief:   brief,
		Ongoing: parsed.Bool("ongoing"),
	}, env.now().UnixMilli())
	if err != nil {
		return err
	}
	if parsed.Bool("json") {
		return writeJSONLine(env, fl)
	}
	fmt.Fprintf(env.Stdout, "Planned floor %q in %s (%s)\n", fl.Name, ws.ID, ws.Name)
	return nil
}

func cmdFloorStart(args []string, env *Env) error {
	parsed, err := parseFlexArgs(args, flagSpec{
		Bools:   map[string]bool{"json": true, "ongoing": true},
		Strings: map[string]bool{"workspace": true, "brief": true},
		Shorts:  map[string]string{"w": "workspace", "b": "brief"},
	})
	if err != nil {
		return usagef("%v", err)
	}
	if len(parsed.Pos) != 1 {
		return usagef("Usage: maestria floor start <name> [--brief <md>] [--ongoing] [-w <ws>] [--json]")
	}
	brief := ""
	if parsed.HasString("brief") {
		brief, err = readStdinIfDash(parsed.String("brief"), env)
		if err != nil {
			return err
		}
	}
	_, ongoingSet := parsed.Bools["ongoing"]
	ws, _, _, err := resolveWorkspaceCLI(env, parsed.String("workspace"))
	if err != nil {
		return err
	}
	db, err := store.Open(configDir(env))
	if err != nil {
		return err
	}
	defer db.Close()

	fl, err := floors.Start(db, ws.ID, parsed.Pos[0], floors.StartInput{
		Brief:      brief,
		Ongoing:    parsed.Bool("ongoing"),
		OngoingSet: ongoingSet,
	}, env.now().UnixMilli())
	if err != nil {
		return err
	}
	if parsed.Bool("json") {
		return writeJSONLine(env, fl)
	}
	fmt.Fprintf(env.Stdout, "Started floor %q in %s (%s)\n", fl.Name, ws.ID, ws.Name)
	return nil
}

func cmdFloorStatus(args []string, env *Env) error {
	parsed, err := parseFlexArgs(args, flagSpec{
		Bools:   map[string]bool{"json": true, "clear-progress": true},
		Strings: map[string]bool{"workspace": true, "state": true, "progress": true, "text": true, "agents": true},
		Shorts:  map[string]string{"w": "workspace"},
	})
	if err != nil {
		return usagef("%v", err)
	}
	if len(parsed.Pos) != 1 {
		return usagef("Usage: maestria floor status <name> [--state working|blocked|review|done] [--progress 0.4|2/5] [--text ...] [--agents a,b] [-w <ws>] [--json]")
	}
	name := parsed.Pos[0]
	ws, _, _, err := resolveWorkspaceCLI(env, parsed.String("workspace"))
	if err != nil {
		return err
	}
	db, err := store.Open(configDir(env))
	if err != nil {
		return err
	}
	defer db.Close()

	hasUpdate := parsed.HasString("state") || parsed.HasString("progress") ||
		parsed.Bool("clear-progress") || parsed.HasString("text") || parsed.HasString("agents")

	if !hasUpdate {
		all, err := floors.List(db, floors.ListOptions{WorkspaceID: ws.ID, IncludePast: true})
		if err != nil {
			return err
		}
		for _, fl := range all {
			if fl.Name == name && (fl.State == floors.StateActive || fl.State == floors.StatePlanned) {
				if parsed.Bool("json") {
					return writeJSONLine(env, fl)
				}
				printFloorLine(env, fl)
				return nil
			}
		}
		return fmt.Errorf("%w: floor %q not found", floors.ErrNotFound, name)
	}

	in := floors.StatusInput{
		Status:        parsed.String("state"),
		ClearProgress: parsed.Bool("clear-progress"),
		Text:          parsed.String("text"),
		TextSet:       parsed.HasString("text"),
	}
	if parsed.HasString("progress") {
		rawP := strings.TrimSpace(parsed.String("progress"))
		if rawP == "" || strings.EqualFold(rawP, "none") || strings.EqualFold(rawP, "clear") {
			in.ClearProgress = true
		} else {
			p, err := floors.ParseProgress(rawP)
			if err != nil {
				return err
			}
			in.Progress = &p
			in.ProgressSet = true
		}
	}
	if parsed.HasString("agents") {
		in.AgentsSet = true
		rawA := strings.TrimSpace(parsed.String("agents"))
		if rawA != "" {
			in.Agents = []string{rawA}
		}
	}

	fl, err := floors.SetStatus(db, ws.ID, name, in, env.now().UnixMilli())
	if err != nil {
		return err
	}
	if parsed.Bool("json") {
		return writeJSONLine(env, fl)
	}
	printFloorLine(env, fl)
	return nil
}

func cmdFloorLand(args []string, env *Env) error {
	parsed, err := parseFlexArgs(args, flagSpec{
		Bools:   map[string]bool{"json": true, "force": true},
		Strings: map[string]bool{"workspace": true, "outcome": true},
		Shorts:  map[string]string{"w": "workspace", "o": "outcome"},
	})
	if err != nil {
		return usagef("%v", err)
	}
	if len(parsed.Pos) != 1 {
		return usagef("Usage: maestria floor land <name> [--outcome <md>] [--force] [-w <ws>] [--json]")
	}
	outcome := ""
	if parsed.HasString("outcome") {
		outcome, err = readStdinIfDash(parsed.String("outcome"), env)
		if err != nil {
			return err
		}
	}
	ws, _, _, err := resolveWorkspaceCLI(env, parsed.String("workspace"))
	if err != nil {
		return err
	}
	db, err := store.Open(configDir(env))
	if err != nil {
		return err
	}
	defer db.Close()

	fl, err := floors.Land(db, ws.ID, parsed.Pos[0], outcome, parsed.Bool("force"), env.now().UnixMilli())
	if err != nil {
		return err
	}
	if parsed.Bool("json") {
		return writeJSONLine(env, fl)
	}
	fmt.Fprintf(env.Stdout, "Landed floor %q in %s\n", fl.Name, ws.ID)
	return nil
}

func cmdFloorCancel(args []string, env *Env) error {
	parsed, err := parseFlexArgs(args, flagSpec{
		Bools:   map[string]bool{"json": true},
		Strings: map[string]bool{"workspace": true, "outcome": true},
		Shorts:  map[string]string{"w": "workspace", "o": "outcome"},
	})
	if err != nil {
		return usagef("%v", err)
	}
	if len(parsed.Pos) != 1 {
		return usagef("Usage: maestria floor cancel <name> [--outcome <md>] [-w <ws>] [--json]")
	}
	outcome := ""
	if parsed.HasString("outcome") {
		outcome, err = readStdinIfDash(parsed.String("outcome"), env)
		if err != nil {
			return err
		}
	}
	ws, _, _, err := resolveWorkspaceCLI(env, parsed.String("workspace"))
	if err != nil {
		return err
	}
	db, err := store.Open(configDir(env))
	if err != nil {
		return err
	}
	defer db.Close()

	fl, err := floors.Cancel(db, ws.ID, parsed.Pos[0], outcome, env.now().UnixMilli())
	if err != nil {
		return err
	}
	if parsed.Bool("json") {
		return writeJSONLine(env, fl)
	}
	fmt.Fprintf(env.Stdout, "Cancelled floor %q in %s\n", fl.Name, ws.ID)
	return nil
}

func cmdFloorList(args []string, env *Env) error {
	parsed, err := parseFlexArgs(args, flagSpec{
		Bools:   map[string]bool{"json": true, "all": true},
		Strings: map[string]bool{"workspace": true},
		Shorts:  map[string]string{"w": "workspace", "a": "all"},
	})
	if err != nil {
		return usagef("%v", err)
	}
	ws, _, _, err := resolveWorkspaceCLI(env, parsed.String("workspace"))
	if err != nil {
		return err
	}
	db, err := store.Open(configDir(env))
	if err != nil {
		return err
	}
	defer db.Close()

	list, err := floors.List(db, floors.ListOptions{
		WorkspaceID: ws.ID,
		IncludePast: parsed.Bool("all"),
	})
	if err != nil {
		return err
	}
	if parsed.Bool("json") {
		return writeJSONLine(env, map[string]any{
			"workspace": ws.ID,
			"floors":    list,
		})
	}
	if len(list) == 0 {
		fmt.Fprintf(env.Stdout, "No floors for %s.\n", ws.ID)
		return nil
	}
	for _, fl := range list {
		printFloorLine(env, fl)
	}
	return nil
}

func cmdFloorEdit(args []string, env *Env) error {
	parsed, err := parseFlexArgs(args, flagSpec{
		Bools:   map[string]bool{"json": true, "ongoing": true},
		Strings: map[string]bool{"workspace": true, "brief": true, "rename": true},
		Shorts:  map[string]string{"w": "workspace", "b": "brief"},
	})
	if err != nil {
		return usagef("%v", err)
	}
	if len(parsed.Pos) != 1 {
		return usagef("Usage: maestria floor edit <name> [--brief <md>] [--rename <newname>] [--ongoing[=true|false]] [-w <ws>] [--json]")
	}
	brief := ""
	if parsed.HasString("brief") {
		brief, err = readStdinIfDash(parsed.String("brief"), env)
		if err != nil {
			return err
		}
	}
	_, ongoingSet := parsed.Bools["ongoing"]
	if !parsed.HasString("brief") && !parsed.HasString("rename") && !ongoingSet {
		return usagef("provide at least one of --brief, --rename, or --ongoing")
	}

	ws, _, _, err := resolveWorkspaceCLI(env, parsed.String("workspace"))
	if err != nil {
		return err
	}
	db, err := store.Open(configDir(env))
	if err != nil {
		return err
	}
	defer db.Close()

	fl, err := floors.Edit(db, ws.ID, parsed.Pos[0], floors.EditInput{
		Brief:      brief,
		BriefSet:   parsed.HasString("brief"),
		Rename:     parsed.String("rename"),
		Ongoing:    parsed.Bool("ongoing"),
		OngoingSet: ongoingSet,
	}, env.now().UnixMilli())
	if err != nil {
		if errors.Is(err, floors.ErrInvalidInput) {
			return usagef("%v", err)
		}
		return err
	}
	if parsed.Bool("json") {
		return writeJSONLine(env, fl)
	}
	fmt.Fprintf(env.Stdout, "Updated floor %q in %s\n", fl.Name, ws.ID)
	return nil
}

func cmdFloorReorder(args []string, env *Env) error {
	parsed, err := parseFlexArgs(args, flagSpec{
		Bools:   map[string]bool{"json": true},
		Strings: map[string]bool{"workspace": true, "before": true, "after": true},
		Shorts:  map[string]string{"w": "workspace"},
	})
	if err != nil {
		return usagef("%v", err)
	}
	hasBefore := parsed.HasString("before")
	hasAfter := parsed.HasString("after")
	if len(parsed.Pos) != 1 || hasBefore == hasAfter {
		return usagef("Usage: maestria floor reorder <name> --before <other> | --after <other> [-w <ws>] [--json]")
	}
	target := parsed.String("before")
	if hasAfter {
		target = parsed.String("after")
	}
	ws, _, _, err := resolveWorkspaceCLI(env, parsed.String("workspace"))
	if err != nil {
		return err
	}
	db, err := store.Open(configDir(env))
	if err != nil {
		return err
	}
	defer db.Close()

	reordered, err := floors.Reorder(db, ws.ID, parsed.Pos[0], target, hasBefore, env.now().UnixMilli())
	if err != nil {
		return err
	}
	if parsed.Bool("json") {
		return writeJSONLine(env, map[string]any{
			"workspace": ws.ID,
			"floors":    reordered,
		})
	}
	fmt.Fprintf(env.Stdout, "Reordered Future floors for %s:\n", ws.ID)
	for _, fl := range reordered {
		fmt.Fprintf(env.Stdout, "  - %s\n", fl.Name)
	}
	return nil
}

func printFloorLine(env *Env, fl floors.Floor) {
	stateLabel := fl.State
	if fl.State == floors.StateActive {
		if fl.Ongoing {
			stateLabel = "active/ongoing"
		} else {
			stateLabel = "active/" + fl.Status
		}
	} else if fl.Ongoing {
		stateLabel += ", ongoing"
	}
	parts := []string{fmt.Sprintf("[%s] %s", stateLabel, fl.Name)}
	if fl.Progress != nil && !fl.Ongoing {
		parts = append(parts, fmt.Sprintf("(%d%%)", int(*fl.Progress*100+0.5)))
	}
	if len(fl.Agents) > 0 {
		parts = append(parts, "agents: "+strings.Join(fl.Agents, ", "))
	}
	if fl.StatusText != "" && fl.State == floors.StateActive {
		parts = append(parts, "— "+fl.StatusText)
	} else if fl.Outcome != "" && (fl.State == floors.StateLanded || fl.State == floors.StateCancelled) {
		parts = append(parts, "— "+fl.Outcome)
	}
	fmt.Fprintln(env.Stdout, strings.Join(parts, " "))
}
