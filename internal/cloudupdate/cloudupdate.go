// Package cloudupdate is `maestria-cloud update`: the hosted server updating itself from GitHub
// releases. It lives apart from internal/cloud (the server) and only reads its configuration to
// know where to ask whether the new version is healthy.
package cloudupdate

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/paulo-lucas/maestria/internal/cloud"
	"github.com/paulo-lucas/maestria/internal/update"
)

// Unit is the systemd service the hosted server runs as (see packaging/linux/setup-cloud.sh).
const Unit = "maestria-cloud.service"

// BackupScript is the database backup setup-cloud.sh installs; it runs before the binary changes.
const BackupScript = "/usr/local/sbin/maestria-cloud-backup"

const usage = "Usage: maestria-cloud update [--check] [--yes] [--force] [--channel stable|prerelease] [--version TAG]"

// Runner runs a command and returns its combined output.
type Runner func(ctx context.Context, argv []string) (string, error)

// Env is what the command touches outside the binary itself, so tests can replace it.
type Env struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Getenv         func(string) string
	Home           string
	Version        string
	// IsTTY reports whether Stdin is a terminal. nil checks Stdin.(*os.File).
	IsTTY func() bool
	// Runner runs systemctl and the backup script. nil runs the real commands.
	Runner Runner
	// BackupScript overrides the database backup script path (tests).
	BackupScript string
	// GOOS decides whether systemd is expected. Default runtime.GOOS.
	GOOS string
	// Hook adjusts the updater (tests point it at a fake GitHub).
	Hook func(*update.Updater)
	// HealthTimeout and HealthPoll override the 30 s / 1 s defaults (tests).
	HealthTimeout, HealthPoll time.Duration
}

// DefaultEnv is the real process environment.
func DefaultEnv(version string) Env {
	home, _ := os.UserHomeDir()
	return Env{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Getenv: os.Getenv, Home: home, Version: version}
}

func (e *Env) goos() string {
	if e.GOOS != "" {
		return e.GOOS
	}
	return runtime.GOOS
}

func (e *Env) isTTY() bool {
	if e.IsTTY != nil {
		return e.IsTTY()
	}
	if f, ok := e.Stdin.(*os.File); ok {
		if fi, err := f.Stat(); err == nil {
			return fi.Mode()&os.ModeCharDevice != 0
		}
	}
	return false
}

func (e *Env) run(ctx context.Context, argv ...string) (string, error) {
	if e.Runner != nil {
		return e.Runner(ctx, argv)
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

// Run executes `maestria-cloud update ...` (args after "update") and returns the exit code.
func Run(args []string, env Env) int {
	if err := run(args, &env); err != nil {
		var ue *update.UsageError
		if errors.As(err, &ue) {
			fmt.Fprintln(env.Stderr, ue.Msg)
			return 2
		}
		fmt.Fprintf(env.Stderr, "Error: %v\n", err)
		return 1
	}
	return 0
}

func run(args []string, env *Env) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	check := fs.Bool("check", false, "")
	yes := fs.Bool("yes", false, "")
	fs.BoolVar(yes, "y", false, "")
	force := fs.Bool("force", false, "")
	channel := fs.String("channel", "", "")
	version := fs.String("version", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return &update.UsageError{Msg: usage}
	}
	ch, err := update.ParseChannel(*channel)
	if err != nil {
		return &update.UsageError{Msg: err.Error()}
	}

	u := &update.Updater{Binary: update.BinaryCloud, Current: env.Version, Verify: verifyNewBinary, RequireSignature: update.RequireSignatureFromEnv()}
	if !*check {
		env.attach(u)
	}
	if env.Hook != nil {
		env.Hook(u)
	}
	return update.RunCommand(context.Background(), u, update.CommandOpts{
		Check: *check, Yes: *yes, Force: *force, Channel: ch, Version: *version,
		IsTTY: env.isTTY(), Stdin: env.Stdin, Stdout: env.Stdout,
	})
}

// verifyNewBinary runs the downloaded binary's `version` before it replaces the current one.
func verifyNewBinary(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("`version` failed: %w", err)
	}
	if !strings.HasPrefix(string(out), update.BinaryCloud+" ") {
		return fmt.Errorf("unexpected `version` output %q", strings.TrimSpace(string(out)))
	}
	return nil
}

// attach makes the update take a backup, restart the systemd service when it is running, and
// wait for /healthz before the update counts as done (otherwise it rolls back).
func (e *Env) attach(u *update.Updater) {
	if e.goos() != "linux" {
		return
	}
	ctx := context.Background()
	if out, err := e.run(ctx, "systemctl", "is-active", Unit); err != nil || out != "active" {
		return // not running under systemd here: replace the binary only
	}
	backup := e.BackupScript
	if backup == "" {
		backup = BackupScript
	}
	if _, err := os.Stat(backup); err == nil {
		// A new version may migrate the database; keep a copy from just before it ran.
		u.BeforeSwap = func(ctx context.Context) error {
			if out, err := e.run(ctx, backup); err != nil {
				return fmt.Errorf("database backup failed: %v %s", err, out)
			}
			return nil
		}
	}
	u.Restart = func(ctx context.Context) error {
		if out, err := e.run(ctx, "systemctl", "restart", Unit); err != nil {
			return fmt.Errorf("systemctl restart: %v %s", err, out)
		}
		return nil
	}
	u.Health = e.health
	u.HealthTimeout, u.HealthPoll = e.HealthTimeout, e.HealthPoll
}

// healthURL is the server's /healthz on its own listen address (config.json and
// MAESTRIA_CLOUD_* variables, the same way `serve` reads them).
func (e *Env) healthURL() (string, error) {
	getenv := e.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	cfg, err := cloud.LoadConfig(cloud.DefaultDataDir(getenv, e.Home), getenv)
	if err != nil {
		return "", err
	}
	host, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return "", fmt.Errorf("listen address %q: %w", cfg.Listen, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz", nil
}

func (e *Env) health(ctx context.Context) error {
	target, err := e.healthURL()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"ok"`) {
		return fmt.Errorf("%s answered %d", filepath.Base(target), resp.StatusCode)
	}
	return nil
}
