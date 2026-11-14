package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/maestria-dev/maestria/internal/audit"
	"github.com/maestria-dev/maestria/internal/auth"
	"github.com/maestria-dev/maestria/internal/config"
	"github.com/maestria-dev/maestria/internal/store"
	"github.com/maestria-dev/maestria/internal/tunnel"
	"github.com/maestria-dev/maestria/internal/web"
)

// ErrNoCredentials is returned when `auth setup` has not been run.
var ErrNoCredentials = errors.New(`no credentials configured. Run "maestria auth setup" first`)

// BuildServer wires the panel from the config directory. The caller closes the returned DB.
func BuildServer(env *Env) (*web.Server, *config.Config, func(), error) {
	dir := configDir(env)
	cfg, err := config.Load(dir, env.Config)
	if err != nil {
		return nil, nil, nil, err
	}
	secrets, err := auth.LoadSecrets(dir)
	if err != nil {
		return nil, nil, nil, err
	}
	if secrets == nil {
		return nil, nil, nil, ErrNoCredentials
	}
	db, err := store.Open(dir)
	if err != nil {
		return nil, nil, nil, err
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
	srv, err := web.New(web.Deps{
		Config: cfg, DB: db, Secrets: secrets, Audit: aud, Sessions: sessions, Limiter: limiter,
		Tunnel: tun, Access: access, Env: env.Config, Now: env.Now,
	})
	if err != nil {
		db.Close()
		return nil, nil, nil, err
	}
	return srv, cfg, func() { db.Close() }, nil
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
