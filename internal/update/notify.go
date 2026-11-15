package update

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/paulo-lucas/maestria/internal/privfile"
)

// StateFile is the notifier's cache in the config directory.
const StateFile = "update-check.json"

const (
	checkInterval = 24 * time.Hour
	retryInterval = time.Hour
)

// Notice says a newer release exists.
type Notice struct {
	Version string `json:"version"`
	URL     string `json:"url,omitempty"`
}

type notifierState struct {
	// Latest is the newest release seen on the last successful check.
	Latest string `json:"latest,omitempty"`
	URL    string `json:"url,omitempty"`
	// NextCheck is the earliest time the network may be asked again.
	NextCheck time.Time `json:"nextCheck"`
	// Dismissed is the version whose note the user closed.
	Dismissed string `json:"dismissed,omitempty"`
}

// Notifier tells a long-running process that an update exists without nagging GitHub: at most one
// request a day (an hour after a failure), the answer cached in the config directory. Development
// builds and a disabled check never touch the network.
type Notifier struct {
	Updater *Updater
	Channel Channel
	// Dir is the config directory holding the cache.
	Dir string
	// Enabled is false when the user turned the check off (updates.check: false).
	Enabled bool
	Now     func() time.Time

	mu sync.Mutex
}

func (n *Notifier) now() time.Time {
	if n.Now != nil {
		return n.Now()
	}
	return time.Now()
}

func (n *Notifier) path() string { return filepath.Join(n.Dir, StateFile) }

func (n *Notifier) load() notifierState {
	var s notifierState
	if b, err := os.ReadFile(n.path()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

func (n *Notifier) save(s notifierState) {
	b, err := json.Marshal(s)
	if err != nil {
		return
	}
	_ = privfile.WriteAtomic(n.path(), b)
}

func (n *Notifier) off() bool { return !n.Enabled || IsDevBuild(n.Updater.Current) }

// notice turns a cached state into a Notice when it is newer than the running version.
func (n *Notifier) notice(s notifierState) *Notice {
	if s.Latest == "" || CleanVersion(s.Latest) == CleanVersion(s.Dismissed) {
		return nil
	}
	st, err := n.Updater.Compare(Release{Tag: s.Latest})
	if err != nil || st != StatusAvailable {
		return nil
	}
	return &Notice{Version: CleanVersion(s.Latest), URL: s.URL}
}

// Cached returns the pending notice from the last check, with no network access.
func (n *Notifier) Cached() *Notice {
	if n.off() {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.notice(n.load())
}

// Refresh asks GitHub when the last answer is older than a day, then returns the pending notice.
func (n *Notifier) Refresh(ctx context.Context) (*Notice, error) {
	if n.off() {
		return nil, nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	s := n.load()
	if now := n.now(); now.Before(s.NextCheck) {
		return n.notice(s), nil
	}
	rel, err := n.Updater.Latest(ctx, n.Channel)
	if err != nil {
		wait := retryInterval
		var rl *RateLimitError
		if errors.As(err, &rl) && rl.Reset.After(n.now()) {
			wait = rl.Reset.Sub(n.now())
		}
		s.NextCheck = n.now().Add(wait)
		n.save(s)
		return n.notice(s), err
	}
	s.Latest, s.URL = rel.Tag, rel.HTMLURL
	s.NextCheck = n.now().Add(checkInterval)
	n.save(s)
	return n.notice(s), nil
}

// Dismiss hides the note for version until a newer release appears.
func (n *Notifier) Dismiss(version string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	s := n.load()
	s.Dismissed = CleanVersion(version)
	n.save(s)
}

// LatestKnown is the newest release seen on the last successful check, from the cache with no
// network access ("" when none was seen, the check is off, or this is a development build). Unlike
// Cached it ignores a dismissed note: it answers "what is the newest release", not "should I nag".
func (n *Notifier) LatestKnown() string {
	if n.off() {
		return ""
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return CleanVersion(n.load().Latest)
}
