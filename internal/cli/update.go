package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/paulo-lucas/maestria/internal/config"
	"github.com/paulo-lucas/maestria/internal/service"
	"github.com/paulo-lucas/maestria/internal/update"
)

const updateUsage = "Usage: maestria update [--check] [--yes] [--force] [--channel stable|prerelease] [--version TAG]"

// updater builds the release updater for this binary. Env.UpdateHook lets tests point it at a
// fake GitHub and a temporary "installed" binary.
func (e *Env) updater() *update.Updater {
	u := &update.Updater{Binary: update.BinaryMaestria, Current: e.Version, Verify: verifyNewBinary}
	if e.UpdateHook != nil {
		e.UpdateHook(u)
	}
	return u
}

// verifyNewBinary runs the downloaded binary's `version` before it replaces the current one, so
// a build for the wrong platform is caught while the old binary is still in place.
func verifyNewBinary(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("`version` failed: %w", err)
	}
	if !strings.HasPrefix(string(out), update.BinaryMaestria+" ") {
		return fmt.Errorf("unexpected `version` output %q", strings.TrimSpace(string(out)))
	}
	return nil
}

func cmdUpdate(args []string, env *Env) error {
	parsed, err := parseFlexArgs(args, flagSpec{
		Bools:   map[string]bool{"check": true, "yes": true, "force": true, "help": true},
		Strings: map[string]bool{"channel": true, "version": true},
		Shorts:  map[string]string{"y": "yes"},
	})
	if err != nil || len(parsed.Pos) > 0 {
		return usagef("%s", updateUsage)
	}
	if parsed.Bool("help") {
		fmt.Fprintln(env.Stdout, updateUsage)
		return nil
	}
	ch, err := update.ParseChannel(parsed.String("channel"))
	if err != nil {
		return usagef("%v", err)
	}
	ctx, stop := env.signalCtx()
	defer stop()

	u := env.updater()
	if !parsed.Bool("check") {
		env.attachService(ctx, u)
	}
	err = update.RunCommand(ctx, u, update.CommandOpts{
		Check: parsed.Bool("check"), Yes: parsed.Bool("yes"), Force: parsed.Bool("force"),
		Channel: ch, Version: parsed.String("version"),
		IsTTY: env.isTTY(), Stdin: env.Stdin, Stdout: env.Stdout,
	})
	var ue *update.UsageError
	if errors.As(err, &ue) {
		return usagef("%s", ue.Msg)
	}
	return err
}

// attachService makes the update restart the background service when one is installed and
// running, and wait for it to come back before the update counts as done. A panel that was
// answering before must answer again; a connect-only service just has to be running.
func (e *Env) attachService(ctx context.Context, u *update.Updater) {
	if u.Restart != nil {
		return
	}
	opts := e.serviceOpts("")
	st, err := service.Status(ctx, opts)
	if err != nil || !st.Installed || !st.Running {
		return
	}
	panel := st.Server.Reachable
	u.Restart = func(ctx context.Context) error { return service.Restart(ctx, opts) }
	u.Health = func(ctx context.Context) error {
		s, err := service.Status(ctx, opts)
		switch {
		case err != nil:
			return err
		case !s.Running:
			return errors.New("the service is not running")
		case panel && !s.Server.Reachable:
			return fmt.Errorf("the panel does not answer at %s", s.Server.URL)
		}
		return nil
	}
}

// updateNotifier checks (at most daily) for a newer release. nil when the check is turned off.
func (e *Env) updateNotifier(cfg *config.Config) *update.Notifier {
	return &update.Notifier{
		Updater: e.updater(), Channel: update.ChannelStable, Dir: configDir(e),
		Enabled: cfg.Updates.Check, Now: e.Now,
	}
}

// watchForUpdates prints a one-line notice when the panel starts if a newer release exists, then
// keeps the cached answer fresh. The notifier itself limits the network to one request a day
// (and none for development builds or with updates.check turned off).
func watchForUpdates(ctx context.Context, n *update.Notifier, env *Env) {
	if n == nil {
		return
	}
	first := true
	for {
		rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		notice, _ := n.Refresh(rctx)
		cancel()
		if first && notice != nil {
			fmt.Fprintf(env.Stdout, "Maestria v%s is available: maestria update\n", notice.Version)
		}
		first = false
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Hour):
		}
	}
}
