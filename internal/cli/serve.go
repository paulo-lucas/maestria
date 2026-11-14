package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/maestria-dev/maestria/internal/activity"
	"github.com/maestria-dev/maestria/internal/audit"
	"github.com/maestria-dev/maestria/internal/auth"
	"github.com/maestria-dev/maestria/internal/config"
	"github.com/maestria-dev/maestria/internal/devices"
	"github.com/maestria-dev/maestria/internal/store"
	"github.com/maestria-dev/maestria/internal/tunnel"
	"github.com/maestria-dev/maestria/internal/web"
	"github.com/maestria-dev/maestria/internal/workspaces"
)

// ErrNoCredentials is returned when `auth setup` has not been run.
var ErrNoCredentials = errors.New(`no credentials configured. Run "maestria auth setup" first`)

// built is everything BuildServer and the connector share.
type built struct {
	Srv   *web.Server
	Cfg   *config.Config
	DB    *sql.DB
	Tun   *tunnel.Controller
	Audit *audit.Log
	Close func()
}

type buildOpts struct {
	// RequireCredentials makes a missing `auth setup` an error (local mode). Hosted mode has no
	// local login at all: devices and the relay gate replace it.
	RequireCredentials bool
	// Devices and ConnectorName switch on the hosted-mode API (devices, relay info).
	Devices       func(db *sql.DB, cfg *config.Config) *devices.Manager
	ConnectorName string
}

func buildServer(env *Env, o buildOpts) (*built, error) {
	dir := configDir(env)
	cfg, err := config.Load(dir, env.Config)
	if err != nil {
		return nil, err
	}
	secrets, err := auth.LoadSecrets(dir)
	if err != nil {
		return nil, err
	}
	if secrets == nil {
		if o.RequireCredentials {
			return nil, ErrNoCredentials
		}
		secrets = &auth.Secrets{}
	}
	db, err := store.Open(dir)
	if err != nil {
		return nil, err
	}
	aud := audit.New(db, env.Now)
	sessions := auth.NewSessionStore(db,
		time.Duration(cfg.Session.IdleMinutes)*time.Minute, time.Duration(cfg.Session.AbsoluteHours)*time.Hour, env.Now)
	limiter := auth.NewLoginLimiter(auth.LimiterOptions{
		MaxFailuresPerIP:  cfg.Login.MaxFailuresPerIP,
		MaxFailuresGlobal: cfg.Login.MaxFailuresGlobal,
		Window:            time.Duration(cfg.Login.WindowSeconds) * time.Second,
		Lockout:           time.Duration(cfg.Login.LockoutSeconds) * time.Second,
	}, env.Now)
	tun := &tunnel.Controller{DB: db, Audit: aud, Sessions: sessions, Config: cfg.Tunnel, Now: env.Now}
	var access *auth.AccessVerifier
	if cfg.CloudflareAccess != nil {
		access = auth.NewAccessVerifier(cfg.CloudflareAccess.TeamDomain, cfg.CloudflareAccess.Audience, nil, env.Now)
	}
	// Maestri Wire: stays off (the "not connected" views) until `maestria wire pair` creates wire.json.
	act := activity.New(activity.Options{
		DB: db, Audit: aud, ConfigDir: dir, Now: env.Now,
		Workspaces: func() []workspaces.Workspace {
			list, _ := workspaces.Merged(dir, cfg.MaestriDir, env.Config.Home)
			return list
		},
	})
	deps := web.Deps{
		Config: cfg, DB: db, Secrets: secrets, Audit: aud, Sessions: sessions, Limiter: limiter,
		Tunnel: tun, Access: access, Activity: act, Env: env.Config, Now: env.Now, ConnectorName: o.ConnectorName,
	}
	if o.Devices != nil {
		deps.Devices = o.Devices(db, cfg)
	}
	srv, err := web.New(deps)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &built{Srv: srv, Cfg: cfg, DB: db, Tun: tun, Audit: aud, Close: func() { db.Close() }}, nil
}

// BuildServer wires the panel from the config directory. The caller closes the returned DB.
func BuildServer(env *Env) (*web.Server, *config.Config, func(), error) {
	b, err := buildServer(env, buildOpts{RequireCredentials: true})
	if err != nil {
		return nil, nil, nil, err
	}
	return b.Srv, b.Cfg, b.Close, nil
}

func cmdServe(args []string, env *Env) error {
	fs := newFlagSet("serve", env)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usagef("Usage: maestria serve")
	}
	srv, cfg, closeDB, err := BuildServer(env)
	if err != nil {
		return err
	}
	defer closeDB()

	// Loopback only, on purpose: the internet reaches the panel through the tunnel.
	ln, err := web.Listen(config.Host, cfg.Port)
	if err != nil {
		return err
	}
	httpSrv := srv.HTTPServer()
	fmt.Fprintf(env.Stdout, "maestria listening on http://%s\n", ln.Addr())
	if cfg.CookieSecure {
		fmt.Fprintln(env.Stdout, `cookieSecure is on: sign in over https (your tunnel). For plain http in Safari, set "cookieSecure": false in config.json.`)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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
	return httpSrv.Shutdown(shutdown)
}
