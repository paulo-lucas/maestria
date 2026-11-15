package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/paulo-lucas/maestria/internal/activity"
	"github.com/paulo-lucas/maestria/internal/audit"
	"github.com/paulo-lucas/maestria/internal/auth"
	"github.com/paulo-lucas/maestria/internal/config"
	"github.com/paulo-lucas/maestria/internal/connect"
	"github.com/paulo-lucas/maestria/internal/devices"
	"github.com/paulo-lucas/maestria/internal/runlock"
	"github.com/paulo-lucas/maestria/internal/store"
	"github.com/paulo-lucas/maestria/internal/tunnel"
	"github.com/paulo-lucas/maestria/internal/update"
	"github.com/paulo-lucas/maestria/internal/web"
	"github.com/paulo-lucas/maestria/internal/workspaces"
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
	// Updates tells the panel (and the startup line) that a newer release exists.
	Updates *update.Notifier
	Close   func()
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
	notifier := env.updateNotifier(cfg)
	deps.Updates = notifier
	srv, err := web.New(deps)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &built{Srv: srv, Cfg: cfg, DB: db, Tun: tun, Audit: aud, Updates: notifier, Close: func() { db.Close() }}, nil
}

// BuildServer wires the panel from the config directory. The caller closes the returned DB.
func BuildServer(env *Env) (*web.Server, *config.Config, func(), error) {
	b, err := buildServer(env, buildOpts{RequireCredentials: true})
	if err != nil {
		return nil, nil, nil, err
	}
	return b.Srv, b.Cfg, b.Close, nil
}

const serveUsage = "Usage: maestria serve [--connect | --no-connect]"

// cmdServe runs the local panel. When this machine is linked to a hosted Maestria and
// relay.enabled is on (the default), the same process also keeps the cloud connection, so the
// panel and the relay share one Wire subscription, one standby guard and one database handle.
// --connect insists on it (and fails if it cannot), --no-connect keeps the panel local only.
func cmdServe(args []string, env *Env) error {
	parsed, err := parseFlexArgs(args, flagSpec{Bools: map[string]bool{"connect": true, "no-connect": true}})
	if err != nil || len(parsed.Pos) > 0 || (parsed.Bool("connect") && parsed.Bool("no-connect")) {
		return usagef("%s", serveUsage)
	}
	dir := configDir(env)
	cfg, err := config.Load(dir, env.Config)
	if err != nil {
		return err
	}
	link, err := connect.LoadLink(dir)
	if err != nil {
		return err
	}
	withCloud := false
	switch {
	case parsed.Bool("no-connect"):
	case parsed.Bool("connect"):
		if link == nil {
			return fmt.Errorf(`this machine is not linked to a hosted Maestria. Run "maestria connect --link CODE --cloud URL" first`)
		}
		if !cfg.Relay.Enabled {
			return fmt.Errorf(`the relay is turned off in config.json ("relay": {"enabled": false}); turn it on or drop --connect`)
		}
		withCloud = true
	default:
		withCloud = link != nil && cfg.Relay.Enabled
	}

	mode := "serve"
	if withCloud {
		mode = "serve+connect"
	}
	lock, err := runlock.Acquire(dir, mode)
	if err != nil {
		return err
	}
	defer lock.Release()

	var (
		b    *built
		conn *Connector
	)
	if withCloud {
		var dm *devices.Manager
		opts := hostedOpts(env, link, &dm)
		opts.RequireCredentials = true
		if b, err = buildServer(env, opts); err != nil {
			return err
		}
		conn = newConnector(env, b, dm, link, func(s string) { fmt.Fprintln(env.Stdout, s) })
	} else if b, err = buildServer(env, buildOpts{RequireCredentials: true}); err != nil {
		return err
	}
	defer b.Close()
	srv := b.Srv

	// Loopback only, on purpose: the internet reaches the panel through the tunnel.
	ln, err := web.Listen(config.Host, b.Cfg.Port)
	if err != nil {
		return err
	}
	httpSrv := srv.HTTPServer()
	fmt.Fprintf(env.Stdout, "maestria listening on http://%s\n", ln.Addr())
	if b.Cfg.CookieSecure {
		fmt.Fprintln(env.Stdout, `cookieSecure is on: sign in over https (your tunnel). For plain http in Safari, set "cookieSecure": false in config.json.`)
	}

	ctx, stop := env.signalCtx()
	defer stop()
	srv.StartBackground(ctx) // once: shared by the panel and the relay
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		watchForUpdates(ctx, b.Updates, env)
	}()
	if conn != nil {
		fmt.Fprintf(env.Stdout, "Also connecting to %s as %q. Approve browsers with \"maestria devices\".\n", link.Cloud, link.Name)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := conn.Client.Run(ctx); errors.Is(err, connect.ErrRevoked) {
				fmt.Fprintln(env.Stdout, "The cloud link was revoked; the local panel keeps running.")
			}
		}()
	}
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.Serve(ln) }()
	var serveErr error
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = err
		}
	case <-ctx.Done():
	}
	stop()
	wg.Wait() // the cloud connection closes cleanly before the database does
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdown); serveErr == nil {
		serveErr = err
	}
	return serveErr
}
