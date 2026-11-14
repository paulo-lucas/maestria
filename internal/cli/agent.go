package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/paulo-lucas/maestria/internal/activity"
	"github.com/paulo-lucas/maestria/internal/store"
)

const agentHelp = `Usage: maestria agent brief <name> "<text>" [-w <workspace>] [--json]
       maestria agent brief <name> --clear [-w <workspace>]

Set the one-line brief the panel shows for an agent on the Activity page (what it is doing now,
up to 160 characters). Without a brief the panel shows the last line of the agent's terminal.
`

func cmdAgent(args []string, env *Env) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(env.Stdout, agentHelp)
		return nil
	}
	if args[0] != "brief" {
		return usagef("Unknown agent subcommand %q\n\n%s", args[0], agentHelp)
	}
	parsed, err := parseFlexArgs(args[1:], flagSpec{
		Bools:   map[string]bool{"clear": true, "json": true},
		Strings: map[string]bool{"workspace": true},
		Shorts:  map[string]string{"w": "workspace"},
	})
	if err != nil {
		return usagef("%v", err)
	}
	clear := parsed.Bool("clear")
	if len(parsed.Pos) < 1 || (!clear && len(parsed.Pos) < 2) || (clear && len(parsed.Pos) > 1) {
		return usagef("%s", agentHelp)
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

	if clear {
		had, err := activity.ClearBrief(db, ws.ID, name)
		if err != nil {
			return err
		}
		if parsed.Bool("json") {
			return writeJSONLine(env, map[string]any{"workspace": ws.ID, "agent": name, "cleared": had})
		}
		if had {
			fmt.Fprintf(env.Stdout, "Cleared the brief for %s.\n", name)
		} else {
			fmt.Fprintf(env.Stdout, "%s had no brief.\n", name)
		}
		return nil
	}
	text := strings.Join(parsed.Pos[1:], " ")
	if text == "-" {
		raw, err := readStdinIfDash("-", env)
		if err != nil {
			return err
		}
		text = raw
	}
	if err := activity.SetBrief(db, ws.ID, name, text, env.now().UnixMilli()); err != nil {
		return err
	}
	if parsed.Bool("json") {
		b, _ := json.Marshal(map[string]any{"workspace": ws.ID, "agent": name, "brief": strings.Join(strings.Fields(text), " ")})
		fmt.Fprintf(env.Stdout, "%s\n", b)
		return nil
	}
	fmt.Fprintf(env.Stdout, "Brief set for %s.\n", name)
	return nil
}
