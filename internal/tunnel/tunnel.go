// Package tunnel controls the public tunnel and the kill switch.
//
// The kill switch stops the tunnel with the configured command, invalidates every session and
// blocks logins. Only the local CLI (`maestria tunnel on`) can turn it off: there is no HTTP route
// that clears it.
package tunnel

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/maestria-dev/maestria/internal/audit"
	"github.com/maestria-dev/maestria/internal/auth"
	"github.com/maestria-dev/maestria/internal/config"
	"github.com/maestria-dev/maestria/internal/store"
)

// CommandResult is the outcome of a tunnel command.
type CommandResult struct {
	OK     bool   `json:"ok"`
	Code   *int   `json:"code"`
	Stderr string `json:"stderr"`
}

// Runner runs an argv without a shell.
type Runner func(ctx context.Context, argv []string) CommandResult

// ExecRunner runs argv with a 15 s timeout and no shell.
func ExecRunner(ctx context.Context, argv []string) CommandResult {
	if len(argv) == 0 {
		return CommandResult{OK: true}
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{buf: &stderr, max: 64 * 1024}
	hideWindow(cmd)
	err := cmd.Run()
	msg := strings.TrimSpace(stderr.String())
	if err == nil {
		zero := 0
		return CommandResult{OK: true, Code: &zero, Stderr: msg}
	}
	var res CommandResult
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		c := exitErr.ExitCode()
		res.Code = &c
	}
	if msg == "" {
		msg = err.Error()
	}
	if len(msg) > 400 {
		msg = msg[:400]
	}
	res.Stderr = msg
	return res
}

type limitedWriter struct {
	buf *bytes.Buffer
	max int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		if len(p) > room {
			w.buf.Write(p[:room])
		} else {
			w.buf.Write(p)
		}
	}
	return len(p), nil
}

const (
	keyKilled    = "tunnel_killed"
	keyKilledAt  = "tunnel_killed_at"
	keyEnabledAt = "tunnel_enabled_at"
)

// Status is the kill switch and tunnel state.
type Status struct {
	Killed           bool   `json:"killed"`
	KilledAt         *int64 `json:"killedAt"`
	EnabledAt        *int64 `json:"enabledAt"`
	StopConfigured   bool   `json:"stopConfigured"`
	StartConfigured  bool   `json:"startConfigured"`
	StatusConfigured bool   `json:"statusConfigured"`
	Running          *bool  `json:"running"`
}

// Controller runs tunnel commands and holds the kill switch state in the database.
type Controller struct {
	DB       *sql.DB
	Audit    *audit.Log
	Sessions *auth.SessionStore
	Config   config.Tunnel
	Runner   Runner
	Now      func() time.Time
}

func (c *Controller) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
}

func (c *Controller) run(ctx context.Context, argv []string) CommandResult {
	if c.Runner == nil {
		return ExecRunner(ctx, argv)
	}
	return c.Runner(ctx, argv)
}

func (c *Controller) kvInt(key string) *int64 {
	v, ok, err := store.GetKV(c.DB, key)
	if err != nil || !ok {
		return nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

// Killed reports whether the kill switch is on. Errors count as killed (fail closed).
func (c *Controller) Killed() bool {
	v, _, err := store.GetKV(c.DB, keyKilled)
	return err != nil || v == "1"
}

// Status reports the state, running the status command when one is configured.
func (c *Controller) Status(ctx context.Context) Status {
	killed := c.Killed()
	st := Status{
		Killed:           killed,
		KilledAt:         c.kvInt(keyKilledAt),
		EnabledAt:        c.kvInt(keyEnabledAt),
		StopConfigured:   len(c.Config.StopCommand) > 0,
		StartConfigured:  len(c.Config.StartCommand) > 0,
		StatusConfigured: len(c.Config.StatusCommand) > 0,
	}
	if st.StatusConfigured {
		running := false
		if !killed {
			running = c.run(ctx, c.Config.StatusCommand).OK
		}
		st.Running = &running
	}
	return st
}

// KillResult is what the kill switch did.
type KillResult struct {
	SessionsInvalidated int            `json:"sessionsInvalidated"`
	StopResult          *CommandResult `json:"stopResult"`
}

// Kill turns the kill switch on: the state is saved first (so logins are refused at once),
// every session is invalidated, then the stop command runs.
func (c *Controller) Kill(ctx context.Context, via, ip string) (KillResult, error) {
	if err := store.SetKV(c.DB, keyKilled, "1"); err != nil {
		return KillResult{}, err
	}
	if err := store.SetKV(c.DB, keyKilledAt, strconv.FormatInt(c.now().UnixMilli(), 10)); err != nil {
		return KillResult{}, err
	}
	n, err := c.Sessions.DestroyAll()
	if err != nil {
		return KillResult{}, err
	}
	res := KillResult{SessionsInvalidated: n}
	if len(c.Config.StopCommand) > 0 {
		r := c.run(ctx, c.Config.StopCommand)
		res.StopResult = &r
	}
	detail := map[string]any{"via": via, "sessionsInvalidated": n, "stopExecuted": res.StopResult != nil}
	if res.StopResult != nil {
		detail["stopOk"] = res.StopResult.OK
		detail["stopCode"] = res.StopResult.Code
	}
	return res, c.Audit.Write("kill_switch", ip, detail)
}

// EnableResult is what `tunnel on` did.
type EnableResult struct {
	WasKilled   bool           `json:"wasKilled"`
	StartResult *CommandResult `json:"startResult"`
}

// EnableLocal clears the kill switch and runs the start command. Call it only from the local CLI.
func (c *Controller) EnableLocal(ctx context.Context) (EnableResult, error) {
	res := EnableResult{WasKilled: c.Killed()}
	if err := store.SetKV(c.DB, keyKilled, "0"); err != nil {
		return res, err
	}
	if err := store.SetKV(c.DB, keyEnabledAt, strconv.FormatInt(c.now().UnixMilli(), 10)); err != nil {
		return res, err
	}
	if len(c.Config.StartCommand) > 0 {
		r := c.run(ctx, c.Config.StartCommand)
		res.StartResult = &r
	}
	detail := map[string]any{"via": "cli", "wasKilled": res.WasKilled, "startExecuted": res.StartResult != nil}
	if res.StartResult != nil {
		detail["startOk"] = res.StartResult.OK
		detail["startCode"] = res.StartResult.Code
	}
	return res, c.Audit.Write("tunnel_on", "", detail)
}
