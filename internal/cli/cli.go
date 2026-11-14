// Package cli implements the maestria command line. It is kept apart from cmd/maestria so the
// commands can be tested with fake input, output and clocks.
package cli

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/maestria-dev/maestria/internal/config"
)

const help = `maestria: control panel for Maestri workspaces

Usage:
  maestria serve                     Start the panel (127.0.0.1 only)
  maestria auth setup [--force]      Set the password, enrol an authenticator app, print recovery codes
  maestria auth reset-sessions       Sign out every browser session
  maestria tunnel on|off|status      Turn the kill switch off (locally) or on, or show its state
  maestria version                   Print the version

Config directory: $MAESTRIA_CONFIG_DIR, %AppData%\maestria (Windows), or ~/.config/maestria
`

// Env is everything the commands touch outside the config directory, so tests can replace it.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Config config.Env
	Now    func() time.Time
	// ReadSecret reads a line without echo. nil uses the terminal.
	ReadSecret func(prompt string) (string, error)
	// Serve runs until interrupted. nil uses the real signal-driven serve loop.
	Version string
}

// DefaultEnv returns the real process environment.
func DefaultEnv(version string) Env {
	return Env{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Config: config.OSEnv(), Now: time.Now, Version: version}
}

func (e *Env) now() time.Time {
	if e.Now == nil {
		return time.Now()
	}
	return e.Now()
}

// usageError marks a mistake in how the command was invoked (exit code 2).
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error { return &usageError{fmt.Sprintf(format, args...)} }

func newFlagSet(name string, env *Env) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return usagef("%s", strings.TrimSpace(err.Error()))
	}
	return nil
}

// Run executes the command line and returns the process exit code.
func Run(args []string, env Env) int {
	err := run(args, &env)
	if err == nil {
		return 0
	}
	var ue *usageError
	if errors.As(err, &ue) {
		fmt.Fprintln(env.Stderr, ue.msg)
		return 2
	}
	var ce *config.Error
	if errors.As(err, &ce) {
		fmt.Fprintln(env.Stderr, ce.Error())
		return 1
	}
	fmt.Fprintf(env.Stderr, "Error: %v\n", err)
	return 1
}

func run(args []string, env *Env) error {
	if len(args) == 0 {
		fmt.Fprint(env.Stdout, help)
		return nil
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "help", "--help", "-h":
		fmt.Fprint(env.Stdout, help)
		return nil
	case "version", "--version":
		fmt.Fprintf(env.Stdout, "maestria %s\n", env.Version)
		return nil
	case "serve":
		return cmdServe(rest, env)
	case "auth":
		if len(rest) == 0 {
			return usagef("Usage: maestria auth setup [--force] | reset-sessions")
		}
		switch rest[0] {
		case "setup":
			return cmdAuthSetup(rest[1:], env)
		case "reset-sessions":
			return cmdAuthResetSessions(rest[1:], env)
		}
		return usagef("Unknown auth command %q\n\n%s", rest[0], help)
	case "tunnel":
		return cmdTunnel(rest, env)
	}
	return usagef("Unknown command: %s\n\n%s", strings.Join(args, " "), help)
}

func configDir(env *Env) string { return config.DefaultDir(env.Config) }

// readLine reads one line of visible input.
func (e *Env) readLine(r *bufio.Reader, prompt string) (string, error) {
	fmt.Fprint(e.Stdout, prompt)
	line, err := r.ReadString('\n')
	if err != nil && (err != io.EOF || line == "") {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// readSecret reads a line without echo. On a terminal it uses term.ReadPassword; when stdin is a
// pipe or a file (scripts, tests) nothing is echoed anyway, so it reads a plain line.
func (e *Env) readSecret(r *bufio.Reader, prompt string) (string, error) {
	if e.ReadSecret != nil {
		return e.ReadSecret(prompt)
	}
	fmt.Fprint(e.Stdout, prompt)
	if f, ok := e.Stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(e.Stdout)
		return string(b), err
	}
	line, err := r.ReadString('\n')
	if err != nil && (err != io.EOF || line == "") {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
