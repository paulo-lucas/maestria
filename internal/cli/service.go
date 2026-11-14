package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/paulo-lucas/maestria/internal/auth"
	"github.com/paulo-lucas/maestria/internal/connect"
	"github.com/paulo-lucas/maestria/internal/service"
)

const serviceUsage = "Usage: maestria service install|uninstall|start|stop|restart|status|logs [options]"

func cmdService(args []string, env *Env) error {
	if len(args) == 0 {
		return usagef(serviceUsage)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "help", "--help", "-h":
		fmt.Fprintln(env.Stdout, serviceUsage)
		fmt.Fprintln(env.Stdout, "\nOptions:")
		fmt.Fprintln(env.Stdout, "  --mode serve|connect|both   Service mode for install (default: serve). both = local panel and hosted connection in one process")
		fmt.Fprintln(env.Stdout, "  --lines N, -n N        Number of trailing log lines for logs (default: 100)")
		fmt.Fprintln(env.Stdout, "  --path                 Print only log directory path for logs")
		fmt.Fprintln(env.Stdout, "  --json                 Machine-readable JSON output")
		return nil
	case "install":
		return cmdServiceInstall(rest, env)
	case "uninstall":
		return cmdServiceUninstall(rest, env)
	case "start":
		return cmdServiceStart(rest, env)
	case "stop":
		return cmdServiceStop(rest, env)
	case "restart":
		return cmdServiceRestart(rest, env)
	case "status":
		return cmdServiceStatus(rest, env)
	case "logs":
		return cmdServiceLogs(rest, env)
	default:
		return usagef("Unknown service subcommand %q\n\n%s", sub, serviceUsage)
	}
}

func (e *Env) serviceOpts(mode string) service.Options {
	return service.Options{
		ConfigDir:         configDir(e),
		HomeDir:           e.Config.Home,
		Env:               e.Config,
		BinaryPath:        e.ServiceBinaryPath,
		Mode:              mode,
		Runner:            e.ServiceRunner,
		ReachabilityCheck: e.ServiceReachability,
	}
}

func cmdServiceInstall(args []string, env *Env) error {
	parsed, err := parseFlexArgs(args, flagSpec{
		Bools:   map[string]bool{"json": true},
		Strings: map[string]bool{"mode": true},
	})
	if err != nil {
		return usagef("%v", err)
	}
	if len(parsed.Pos) > 0 {
		return usagef("Usage: maestria service install [--mode serve|connect|both] [--json]")
	}
	mode, err := service.ValidateMode(parsed.String("mode"))
	if err != nil {
		return usagef("%v", err)
	}
	res, err := service.Install(context.Background(), env.serviceOpts(mode))
	if err != nil {
		return err
	}
	if parsed.Bool("json") {
		return writeJSONLine(env, map[string]any{
			"ok":       true,
			"action":   "install",
			"unitPath": res.UnitPath,
			"logDir":   res.LogDir,
			"mode":     res.Mode,
		})
	}
	fmt.Fprintf(env.Stdout, "Installed Maestria service (%s): %s\n", res.Mode, res.UnitPath)
	fmt.Fprintf(env.Stdout, "Logs: %s\n", res.LogDir)
	if res.Mode == service.ModeServe || res.Mode == service.ModeBoth {
		if sec, err := auth.LoadSecrets(configDir(env)); err == nil && sec == nil {
			fmt.Fprintln(env.Stdout, `Note: credentials are not configured yet. Run "maestria auth setup" before starting.`)
		}
	}
	if res.Mode == service.ModeConnect || res.Mode == service.ModeBoth {
		if l, err := connect.LoadLink(configDir(env)); err == nil && l == nil {
			fmt.Fprintln(env.Stdout, `Note: this machine is not linked yet. Run "maestria connect --link CODE --cloud URL" once before starting.`)
		}
	}
	fmt.Fprintln(env.Stdout, "Start now with: maestria service start")
	return nil
}

func cmdServiceUninstall(args []string, env *Env) error {
	parsed, err := parseFlexArgs(args, flagSpec{Bools: map[string]bool{"json": true}})
	if err != nil {
		return usagef("%v", err)
	}
	if len(parsed.Pos) > 0 {
		return usagef("Usage: maestria service uninstall [--json]")
	}
	removed, unitPath, err := service.Uninstall(context.Background(), env.serviceOpts(""))
	if err != nil {
		return err
	}
	if parsed.Bool("json") {
		return writeJSONLine(env, map[string]any{
			"ok":       true,
			"action":   "uninstall",
			"removed":  removed,
			"unitPath": unitPath,
		})
	}
	if removed {
		fmt.Fprintf(env.Stdout, "Removed service: %s\n", unitPath)
	} else {
		fmt.Fprintf(env.Stdout, "Service was not installed (%s).\n", unitPath)
	}
	return nil
}

func cmdServiceStart(args []string, env *Env) error {
	parsed, err := parseFlexArgs(args, flagSpec{Bools: map[string]bool{"json": true}})
	if err != nil {
		return usagef("%v", err)
	}
	if len(parsed.Pos) > 0 {
		return usagef("Usage: maestria service start [--json]")
	}
	if err := service.Start(context.Background(), env.serviceOpts("")); err != nil {
		return err
	}
	if parsed.Bool("json") {
		return writeJSONLine(env, map[string]any{"ok": true, "action": "start"})
	}
	fmt.Fprintln(env.Stdout, "Started Maestria service.")
	return nil
}

func cmdServiceStop(args []string, env *Env) error {
	parsed, err := parseFlexArgs(args, flagSpec{Bools: map[string]bool{"json": true}})
	if err != nil {
		return usagef("%v", err)
	}
	if len(parsed.Pos) > 0 {
		return usagef("Usage: maestria service stop [--json]")
	}
	stopped, err := service.Stop(context.Background(), env.serviceOpts(""))
	if err != nil {
		return err
	}
	if parsed.Bool("json") {
		return writeJSONLine(env, map[string]any{"ok": true, "action": "stop", "stopped": stopped})
	}
	if stopped {
		fmt.Fprintln(env.Stdout, "Stopped Maestria service.")
	} else {
		fmt.Fprintln(env.Stdout, "Maestria service is not running.")
	}
	return nil
}

func cmdServiceRestart(args []string, env *Env) error {
	parsed, err := parseFlexArgs(args, flagSpec{Bools: map[string]bool{"json": true}})
	if err != nil {
		return usagef("%v", err)
	}
	if len(parsed.Pos) > 0 {
		return usagef("Usage: maestria service restart [--json]")
	}
	if err := service.Restart(context.Background(), env.serviceOpts("")); err != nil {
		return err
	}
	if parsed.Bool("json") {
		return writeJSONLine(env, map[string]any{"ok": true, "action": "restart"})
	}
	fmt.Fprintln(env.Stdout, "Restarted Maestria service.")
	return nil
}

func cmdServiceStatus(args []string, env *Env) error {
	parsed, err := parseFlexArgs(args, flagSpec{Bools: map[string]bool{"json": true}})
	if err != nil {
		return usagef("%v", err)
	}
	if len(parsed.Pos) > 0 {
		return usagef("Usage: maestria service status [--json]")
	}
	report, err := service.Status(context.Background(), env.serviceOpts(""))
	if err != nil {
		return err
	}
	if parsed.Bool("json") {
		return writeJSONLine(env, report)
	}
	inst := "not installed"
	if report.Installed {
		inst = fmt.Sprintf("installed (%s)", report.UnitPath)
	}
	state := "stopped"
	if report.Running {
		state = "running"
	} else if report.Loaded {
		state = "loaded (not running)"
	}
	reach := fmt.Sprintf("unreachable (%s)", report.Server.URL)
	if report.Server.Reachable {
		reach = fmt.Sprintf("reachable (%s)", report.Server.URL)
	}
	fmt.Fprintf(env.Stdout, "Service (%s): %s\n", report.Manager, inst)
	fmt.Fprintf(env.Stdout, "State: %s\n", state)
	fmt.Fprintf(env.Stdout, "Panel server: %s\n", reach)
	fmt.Fprintf(env.Stdout, "Logs: %s\n", report.LogDir)
	return nil
}

func cmdServiceLogs(args []string, env *Env) error {
	parsed, err := parseFlexArgs(args, flagSpec{
		Bools:   map[string]bool{"json": true, "path": true},
		Strings: map[string]bool{"lines": true},
		Shorts:  map[string]string{"n": "lines"},
	})
	if err != nil {
		return usagef("%v", err)
	}
	if len(parsed.Pos) > 0 {
		return usagef("Usage: maestria service logs [--lines N | -n N] [--path] [--json]")
	}
	lines := 100
	if parsed.HasString("lines") {
		n, err := strconv.Atoi(strings.TrimSpace(parsed.String("lines")))
		if err != nil || n <= 0 {
			return usagef("--lines must be a positive integer")
		}
		lines = n
	}
	files, logDir, err := service.ReadLogs(env.serviceOpts(""), lines)
	if err != nil {
		return err
	}
	if parsed.Bool("path") {
		if parsed.Bool("json") {
			return writeJSONLine(env, map[string]any{"logDir": logDir, "files": files})
		}
		fmt.Fprintln(env.Stdout, logDir)
		return nil
	}
	if parsed.Bool("json") {
		return writeJSONLine(env, map[string]any{"logDir": logDir, "files": files})
	}
	anyExist := false
	for _, f := range files {
		if !f.Exists {
			continue
		}
		anyExist = true
		fmt.Fprintf(env.Stdout, "==> %s <==\n", f.Path)
		if f.Content == "" {
			fmt.Fprintln(env.Stdout, "(empty)")
		} else {
			fmt.Fprint(env.Stdout, f.Content)
		}
	}
	if !anyExist {
		fmt.Fprintf(env.Stdout, "No service logs found in %s.\n", logDir)
	}
	return nil
}
