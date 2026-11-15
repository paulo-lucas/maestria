package update

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// Status is how the running version compares with a release.
type Status int

const (
	// StatusDev: a development build; it never updates by itself.
	StatusDev Status = iota
	// StatusUpToDate: running exactly the release.
	StatusUpToDate
	// StatusAvailable: the release is newer.
	StatusAvailable
	// StatusAhead: the running version is newer than the release (a prerelease, or a pulled release).
	StatusAhead
)

// Check is the result of comparing the running version with the newest release.
type Check struct {
	Status  Status
	Current string
	Release Release
}

// Updater finds and installs releases of one binary. The zero value plus Binary and Current
// is usable; the other fields are hooks and overrides (mostly for tests).
type Updater struct {
	// Binary is the cmd name: "maestria" or "maestria-cloud".
	Binary string
	// Current is the running version, from -ldflags.
	Current string

	Repo    string // default Repo
	APIBase string // default DefaultAPIBase
	HTTP    *http.Client
	// Insecure allows http URLs (a local test server). Never set in production code.
	Insecure bool

	GOOS, GOARCH string // default runtime.GOOS / runtime.GOARCH
	// ExePath is the executable to replace. Default: the running one, symlinks resolved.
	ExePath string

	// Verify runs the staged binary before it replaces the current one (e.g. `<new> version`).
	Verify func(ctx context.Context, path string) error
	// BeforeSwap runs after Verify and before the binary is replaced (a backup, for instance). An
	// error stops the update with the old binary untouched.
	BeforeSwap func(ctx context.Context) error
	// Restart restarts what runs the binary; nil when nothing needs it. Health then has to pass
	// within HealthTimeout, or the previous binary is put back and Restart runs again.
	Restart func(ctx context.Context) error
	Health  func(ctx context.Context) error

	HealthTimeout time.Duration // default 30s
	HealthPoll    time.Duration // default 1s

	// RequireSignature makes a missing cosign an error instead of a warning (the check itself
	// always runs, and is strict, whenever cosign is installed).
	RequireSignature bool
	// LookPath finds cosign; default exec.LookPath. RunCosign runs it; default os/exec. Both are
	// hooks for tests.
	LookPath  func(file string) (string, error)
	RunCosign func(ctx context.Context, bin string, args []string) ([]byte, error)

	// Log receives progress lines. nil is silent.
	Log func(format string, args ...any)
}

func (u *Updater) logf(format string, args ...any) {
	if u.Log != nil {
		u.Log(format, args...)
	}
}

func (u *Updater) goos() string {
	if u.GOOS != "" {
		return u.GOOS
	}
	return runtime.GOOS
}

func (u *Updater) goarch() string {
	if u.GOARCH != "" {
		return u.GOARCH
	}
	return runtime.GOARCH
}

func (u *Updater) exePath() (string, error) {
	if u.ExePath != "" {
		return u.ExePath, nil
	}
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return p, nil
}

// Compare says how the running version stands against rel.
func (u *Updater) Compare(rel Release) (Status, error) {
	if IsDevBuild(u.Current) {
		return StatusDev, nil
	}
	cur, err := ParseVersion(u.Current)
	if err != nil {
		return StatusDev, nil
	}
	rv, err := rel.Version()
	if err != nil {
		return StatusUpToDate, fmt.Errorf("release tag %q is not a version", rel.Tag)
	}
	switch c := rv.Compare(cur); {
	case c > 0:
		return StatusAvailable, nil
	case c < 0:
		return StatusAhead, nil
	}
	return StatusUpToDate, nil
}

// Check fetches the newest release of the channel and compares it with the running version.
func (u *Updater) Check(ctx context.Context, ch Channel) (Check, error) {
	rel, err := u.Latest(ctx, ch)
	if err != nil {
		return Check{Current: u.Current}, err
	}
	st, err := u.Compare(rel)
	return Check{Status: st, Current: u.Current, Release: rel}, err
}

// ErrRolledBack wraps the cause when a new binary failed to start and the previous one was restored.
type ErrRolledBack struct{ Cause error }

func (e *ErrRolledBack) Error() string {
	return "the new version did not come up healthy (" + e.Cause.Error() + "); the previous version was restored"
}
func (e *ErrRolledBack) Unwrap() error { return e.Cause }

// Result is what Apply did.
type Result struct {
	From, To  string
	Restarted bool
	// Previous is where the replaced binary was kept.
	Previous string
}

// Apply installs rel over the running executable: download, verify the checksum, extract, swap
// atomically, restart, wait for health, and roll back if it is not healthy. Callers decide
// whether rel is newer (see Compare); Apply installs what it is given.
func (u *Updater) Apply(ctx context.Context, rel Release) (Result, error) {
	res := Result{From: u.Current, To: CleanVersion(rel.Tag)}
	exe, err := u.exePath()
	if err != nil {
		return res, err
	}
	staged, err := u.fetchBinary(ctx, rel, filepath.Dir(exe))
	if err != nil {
		return res, err
	}
	defer os.Remove(staged) // a no-op once it was renamed into place
	if u.Verify != nil {
		if err := u.Verify(ctx, staged); err != nil {
			return res, fmt.Errorf("the downloaded binary does not run here: %w", err)
		}
	}
	if u.BeforeSwap != nil {
		if err := u.BeforeSwap(ctx); err != nil {
			return res, fmt.Errorf("before the update: %w", err)
		}
	}
	prev, err := swapIn(exe, staged, u.goos())
	if err != nil {
		return res, err
	}
	res.Previous = prev
	u.logf("Installed %s over %s", res.To, exe)
	if u.Restart == nil {
		return res, nil
	}
	if err := u.restartAndWait(ctx); err != nil {
		u.logf("Rolling back: %v", err)
		if rbErr := restorePrevious(exe, prev, u.goos()); rbErr != nil {
			return res, fmt.Errorf("%w; and the rollback failed: %v (the previous binary is at %s)", err, rbErr, prev)
		}
		if rsErr := u.Restart(context.WithoutCancel(ctx)); rsErr != nil {
			return res, fmt.Errorf("%w; the previous binary is back but restarting it failed: %v", &ErrRolledBack{Cause: err}, rsErr)
		}
		return res, &ErrRolledBack{Cause: err}
	}
	res.Restarted = true
	return res, nil
}

func (u *Updater) restartAndWait(ctx context.Context) error {
	if err := u.Restart(ctx); err != nil {
		return fmt.Errorf("restart: %w", err)
	}
	if u.Health == nil {
		return nil
	}
	timeout, poll := u.HealthTimeout, u.HealthPoll
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if poll <= 0 {
		poll = time.Second
	}
	deadline := time.Now().Add(timeout)
	var last error
	for {
		if last = u.Health(ctx); last == nil {
			return nil
		}
		if time.Now().Add(poll).After(deadline) {
			return fmt.Errorf("not healthy after %s: %w", timeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// Rollback puts the `.previous` binary back over exePath (the running executable when empty).
func Rollback(exePath string) error {
	if exePath == "" {
		p, err := os.Executable()
		if err != nil {
			return err
		}
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		exePath = p
	}
	prev := exePath + ".previous"
	if _, err := os.Stat(prev); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("there is no previous version at %s", prev)
		}
		return err
	}
	return restorePrevious(exePath, prev, runtime.GOOS)
}
