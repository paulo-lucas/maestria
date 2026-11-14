package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/paulo-lucas/maestria/internal/audit"
	"github.com/paulo-lucas/maestria/internal/auth"
	"github.com/paulo-lucas/maestria/internal/config"
	"github.com/paulo-lucas/maestria/internal/store"
	"github.com/paulo-lucas/maestria/internal/tunnel"
)

// newTunnel opens the database and builds the controller. Callers must close the returned DB.
func newTunnel(env *Env, runner tunnel.Runner) (*tunnel.Controller, *config.Config, func(), error) {
	dir := configDir(env)
	cfg, err := config.Load(dir, env.Config)
	if err != nil {
		return nil, nil, nil, err
	}
	db, err := store.Open(dir)
	if err != nil {
		return nil, nil, nil, err
	}
	sessions := auth.NewSessionStore(db, 0, 0, env.Now)
	c := &tunnel.Controller{DB: db, Audit: audit.New(db, env.Now), Sessions: sessions, Config: cfg.Tunnel, Runner: runner, Now: env.Now}
	return c, cfg, func() { db.Close() }, nil
}

func cmdTunnel(args []string, env *Env) error {
	const usage = "Usage: maestria tunnel on|off|status [--json]"
	if len(args) == 0 {
		return usagef(usage)
	}
	sub := args[0]
	if sub != "on" && sub != "off" && sub != "status" {
		return usagef(usage)
	}
	fs := newFlagSet("tunnel", env)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := parseFlags(fs, args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usagef(usage)
	}
	ctrl, cfg, closeDB, err := newTunnel(env, nil)
	if err != nil {
		return err
	}
	defer closeDB()
	ctx := context.Background()
	out := env.Stdout
	emit := func(v any) error {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "%s\n", b)
		return err
	}

	switch sub {
	case "status":
		st := ctrl.Status(ctx)
		if *asJSON {
			return emit(st)
		}
		if st.Killed {
			fmt.Fprintln(out, `Kill switch: LOCKED (re-enable with "maestria tunnel on")`)
		} else {
			fmt.Fprintln(out, "Kill switch: inactive")
		}
		fmt.Fprintf(out, "Stop command: %s\n", describeCommand(st.StopConfigured, cfg.Tunnel.StopCommand))
		fmt.Fprintf(out, "Start command: %s\n", describeCommand(st.StartConfigured, cfg.Tunnel.StartCommand))
		if st.Running != nil {
			fmt.Fprintf(out, "Tunnel running: %s\n", yesNo(*st.Running))
		}
		return nil
	case "off":
		res, err := ctrl.Kill(ctx, "cli", "")
		if err != nil {
			return err
		}
		if *asJSON {
			return emit(map[string]any{"ok": true, "sessionsInvalidated": res.SessionsInvalidated, "stopResult": res.StopResult})
		}
		fmt.Fprintf(out, "Kill switch activated. Invalidated %d session(s).", res.SessionsInvalidated)
		if res.StopResult != nil {
			if res.StopResult.OK {
				fmt.Fprint(out, " Stop command succeeded.")
			} else {
				fmt.Fprintf(out, " Stop command exited with error (%s).", res.StopResult.Stderr)
			}
		}
		fmt.Fprintln(out)
		return nil
	}
	res, err := ctrl.EnableLocal(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		if err := emit(map[string]any{"ok": true, "wasKilled": res.WasKilled, "startResult": res.StartResult}); err != nil {
			return err
		}
	} else {
		suffix := ""
		if res.WasKilled {
			suffix = " (kill switch cleared)"
		}
		fmt.Fprintf(out, "Tunnel access re-enabled locally%s.", suffix)
		if res.StartResult != nil {
			if res.StartResult.OK {
				fmt.Fprint(out, " Start command succeeded.")
			} else {
				fmt.Fprintf(out, " Start command failed: %s.", res.StartResult.Stderr)
			}
		}
		fmt.Fprintln(out)
	}
	if res.StartResult != nil && !res.StartResult.OK {
		return fmt.Errorf("the start command failed")
	}
	return nil
}

func describeCommand(configured bool, argv []string) string {
	if !configured {
		return "not configured"
	}
	return strings.Join(argv, " ")
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
