package cloud

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

const help = `maestria-cloud: the hosted Maestria server (accounts, relay, device pairing)

Usage:
  maestria-cloud serve               Start the server (default 127.0.0.1:8080; put a proxy or tunnel in front)
  maestria-cloud user list           List accounts
  maestria-cloud user disable EMAIL  Disable an account and end its sessions and connector links
  maestria-cloud user enable EMAIL   Enable it again
  maestria-cloud version             Print the version

Config: <data dir>/config.json and MAESTRIA_CLOUD_* variables. Data dir: $MAESTRIA_CLOUD_DIR,
%AppData%\maestria-cloud (Windows) or ~/.config/maestria-cloud.

Settings (config.json key / environment variable):
  publicUrl  MAESTRIA_CLOUD_PUBLIC_URL    https URL users reach the cloud at (sets Host allowlist, cookies, email links)
  listen     MAESTRIA_CLOUD_LISTEN        address to bind (default 127.0.0.1:8080)
  trustProxy MAESTRIA_CLOUD_TRUST_PROXY   "", "xff" or "cf": trust the proxy's client address header
  smtp.*     MAESTRIA_CLOUD_SMTP_HOST, _PORT, _USER, _PASSWORD, _FROM, _TLS
  devEmail   MAESTRIA_CLOUD_DEV_EMAIL     "stdout" or "file:<path>": print emails instead of sending them
`

// Env is what the commands touch outside the data dir, so tests can replace it.
type Env struct {
	Stdout, Stderr io.Writer
	Getenv         func(string) string
	Home           string
	Now            func() time.Time
	// Version, Commit and Date are injected at build time via -ldflags.
	Version string
	Commit  string
	Date    string
	// Ready is called with the listening address once `serve` is up (tests).
	Ready func(addr string)
	// Stop ends `serve` (tests). nil means SIGINT/SIGTERM.
	Stop <-chan struct{}
}

// DefaultEnv returns the real process environment.
func DefaultEnv(version string) Env {
	home, _ := os.UserHomeDir()
	return Env{Stdout: os.Stdout, Stderr: os.Stderr, Getenv: os.Getenv, Home: home, Now: time.Now, Version: version}
}

// Run executes the command line and returns the exit code.
func Run(args []string, env Env) int {
	if err := run(args, &env); err != nil {
		fmt.Fprintf(env.Stderr, "Error: %v\n", err)
		return 1
	}
	return 0
}

func openAll(env *Env) (*Config, *Server, func(), error) {
	dir := DefaultDataDir(env.Getenv, env.Home)
	cfg, err := LoadConfig(dir, env.Getenv)
	if err != nil {
		return nil, nil, nil, err
	}
	db, err := OpenDB(dir)
	if err != nil {
		return nil, nil, nil, err
	}
	var mailer Mailer
	if cfg.mailConfigured() {
		mailer, err = NewMailer(cfg, env.Stdout)
		if err != nil {
			db.Close()
			return nil, nil, nil, err
		}
	} else {
		mailer = &failMailer{}
	}
	log := slog.New(slog.NewTextHandler(env.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	srv, err := New(Deps{Config: cfg, DB: db, Mailer: mailer, Log: log, Now: env.Now})
	if err != nil {
		db.Close()
		return nil, nil, nil, err
	}
	return cfg, srv, func() { srv.Close(); db.Close() }, nil
}

type failMailer struct{}

func (failMailer) Send(Message) error { return ErrNoMailer }

func run(args []string, env *Env) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(env.Stdout, help)
		return nil
	}
	switch args[0] {
	case "version", "--version":
		var meta []string
		if env.Commit != "" {
			meta = append(meta, "commit "+env.Commit)
		}
		if env.Date != "" {
			meta = append(meta, "built "+env.Date)
		}
		if len(meta) == 0 {
			fmt.Fprintf(env.Stdout, "maestria-cloud %s\n", env.Version)
		} else {
			fmt.Fprintf(env.Stdout, "maestria-cloud %s (%s)\n", env.Version, strings.Join(meta, ", "))
		}
		return nil
	case "serve":
		return cmdServe(args[1:], env)
	case "user":
		return cmdUser(args[1:], env)
	}
	return fmt.Errorf("unknown command %q\n\n%s", args[0], help)
}

func cmdServe(args []string, env *Env) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return errors.New("usage: maestria-cloud serve")
	}
	cfg, srv, closeAll, err := openAll(env)
	if err != nil {
		return err
	}
	defer closeAll()
	if cfg.PublicURL == "" {
		fmt.Fprintln(env.Stderr, "warning: publicUrl is not set; set it so cookies, email links and the Host allowlist are right")
	}
	if !cfg.mailConfigured() {
		fmt.Fprintln(env.Stderr, `warning: no email configured (smtp.* or devEmail); sign-up and password reset cannot send mail`)
	}
	ln, err := listen(cfg.Listen)
	if err != nil {
		return err
	}
	httpSrv := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      90 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 * 1024,
	}
	fmt.Fprintf(env.Stdout, "maestria-cloud listening on http://%s\n", ln.Addr())
	if env.Ready != nil {
		env.Ready(ln.Addr().String())
	}
	var ctx context.Context
	var stop context.CancelFunc
	if env.Stop != nil {
		ctx, stop = context.WithCancel(context.Background())
		go func() { <-env.Stop; stop() }()
	} else {
		ctx, stop = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	}
	defer stop()
	srv.StartBackground(ctx)
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.Serve(ln) }()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdown)
	return nil
}

func cmdUser(args []string, env *Env) error {
	if len(args) == 0 {
		return errors.New("usage: maestria-cloud user list | disable EMAIL | enable EMAIL")
	}
	_, srv, closeAll, err := openAll(env)
	if err != nil {
		return err
	}
	defer closeAll()
	acc := srv.Accounts()
	switch args[0] {
	case "list":
		users, err := acc.ListUsers()
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(env.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "EMAIL\tSTATE\tCREATED")
		for _, u := range users {
			state := "active"
			switch {
			case u.DisabledAt != 0:
				state = "disabled"
			case u.VerifiedAt == 0:
				state = "unverified"
			case u.EnrolledAt == 0:
				state = "no 2fa yet"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", u.Email, state, time.UnixMilli(u.CreatedAt).Format("2006-01-02"))
		}
		return tw.Flush()
	case "disable", "enable":
		if len(args) != 2 {
			return fmt.Errorf("usage: maestria-cloud user %s EMAIL", args[0])
		}
		u, err := acc.SetDisabled(strings.TrimSpace(args[1]), args[0] == "disable")
		if err != nil {
			return fmt.Errorf("no such account")
		}
		if args[0] == "disable" {
			// A running server notices within 15 seconds and drops the sockets; sessions are gone now.
			fmt.Fprintf(env.Stdout, "Disabled %s. Sessions ended; its connectors are disconnected within 15 seconds and refused afterwards.\n", u.Email)
		} else {
			fmt.Fprintf(env.Stdout, "Enabled %s.\n", u.Email)
		}
		return nil
	}
	return fmt.Errorf("unknown user command %q", args[0])
}
