package cli

import (
	"fmt"

	"github.com/paulo-lucas/maestria/internal/insights"
	"github.com/paulo-lucas/maestria/internal/store"
)

func cmdInsights(args []string, env *Env) error {
	parsed, err := parseFlexArgs(args, flagSpec{Bools: map[string]bool{"json": true}})
	if err != nil {
		return usagef("%v", err)
	}
	if len(parsed.Pos) > 0 {
		return usagef("Usage: maestria insights [--json]")
	}
	db, err := store.Open(configDir(env))
	if err != nil {
		return err
	}
	defer db.Close()
	rep, err := insights.Compute(db, env.now())
	if err != nil {
		return err
	}
	if parsed.Bool("json") {
		return writeJSONLine(env, rep)
	}
	if rep.Level > 0 {
		fmt.Fprintf(env.Stdout, "Overall: level %d of %d (last %d days)\n", rep.Level, insights.MaxLevel, insights.Days)
	} else {
		fmt.Fprintf(env.Stdout, "Overall: not rated yet (needs two rated areas; last %d days)\n", insights.Days)
	}
	for _, a := range rep.Areas {
		fmt.Fprintln(env.Stdout)
		if a.Level == 0 {
			fmt.Fprintf(env.Stdout, "%s: not rated yet\n  %s\n", a.Title, a.Missing)
			continue
		}
		start := ""
		if a.ID == rep.Focus {
			start = " (start here)"
		}
		fmt.Fprintf(env.Stdout, "%s: level %d of %d%s\n  %s\n", a.Title, a.Level, insights.MaxLevel, start, a.Value)
		if a.Next != "" {
			fmt.Fprintf(env.Stdout, "  %s\n", a.Next)
		}
		if a.Tip != "" {
			fmt.Fprintf(env.Stdout, "  Try: %s\n", a.Tip)
		}
		if a.Note != "" {
			fmt.Fprintf(env.Stdout, "  Estimate: %s\n", a.Note)
		}
	}
	return nil
}
