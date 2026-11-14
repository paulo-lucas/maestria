// Package runlock keeps two Maestria processes from running `serve` or `connect` on the same
// config directory. Both start the same background work (usage polling, the standby guard, the
// Maestri Wire mirror), so a second one would duplicate it. The CLI commands (post, say, wait,
// devices, ...) do not take the lock and stay usable next to a running process.
//
// The lock is a file created with O_EXCL that holds the owner's pid, and a heartbeat refreshes its
// modification time. A lock whose process is gone, or that stopped refreshing (a crash, a reused
// pid), is stale and is taken over.
package runlock

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/maestria-dev/maestria/internal/privfile"
)

// FileName is the lock file in the config dir.
const FileName = "run.lock"

// Heartbeat is how often a live owner refreshes the lock; StaleAfter is how long without a
// refresh makes a lock stale even if its pid looks alive. Tests shorten them.
var (
	Heartbeat  = 15 * time.Second
	StaleAfter = 90 * time.Second
)

// Info is what the lock file says about its owner.
type Info struct {
	PID     int    `json:"pid"`
	Mode    string `json:"mode"` // "serve", "connect" or "serve+connect"
	Started int64  `json:"started"`
	Token   string `json:"token"`
}

// HeldError is returned when another live process owns the lock.
type HeldError struct {
	Info Info
	Path string
	Dir  string
}

func (e *HeldError) Error() string {
	started := time.UnixMilli(e.Info.Started).Format("15:04")
	return fmt.Sprintf("another Maestria process is already running on this config directory: maestria %s (pid %d, started %s).\n"+
		"Only one process may run serve or connect per config directory, because both run the same background work.\n"+
		"Stop it first (for example \"maestria service stop\"), or use another directory with MAESTRIA_CONFIG_DIR.\n"+
		"Lock file: %s", e.Info.Mode, e.Info.PID, started, e.Path)
}

// Lock is a held run lock.
type Lock struct {
	path string
	info Info
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// Acquire takes the lock for mode ("serve", "connect" or "serve+connect"), or returns *HeldError.
func Acquire(dir, mode string) (*Lock, error) {
	if err := privfile.EnsureDir(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, FileName)
	tok := make([]byte, 8)
	if _, err := rand.Read(tok); err != nil {
		return nil, err
	}
	info := Info{PID: os.Getpid(), Mode: mode, Started: time.Now().UnixMilli(), Token: hex.EncodeToString(tok)}
	body, _ := json.Marshal(info)

	for attempt := 0; attempt < 5; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, werr := f.Write(append(body, '\n'))
			cerr := f.Close()
			if werr != nil || cerr != nil {
				_ = os.Remove(path)
				return nil, errors.Join(werr, cerr)
			}
			l := &Lock{path: path, info: info, stop: make(chan struct{}), done: make(chan struct{})}
			go l.beat()
			return l, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		owner, stale := inspect(path)
		if !stale {
			return nil, &HeldError{Info: owner, Path: path, Dir: dir}
		}
		// Take over a stale lock. If two starters race here, only one wins the O_EXCL create.
		_ = os.Remove(path)
	}
	return nil, fmt.Errorf("could not take the run lock %s", path)
}

// inspect reads a lock file and reports whether it is stale.
func inspect(path string) (Info, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return Info{}, true // vanished: free to take
	}
	var info Info
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &info) != nil || info.PID <= 0 {
		// Unreadable or half-written: stale only once it is old, so a starter that is still
		// writing is not stolen from.
		return info, time.Since(st.ModTime()) > StaleAfter
	}
	if !alive(info.PID) {
		return info, true
	}
	return info, time.Since(st.ModTime()) > StaleAfter
}

func (l *Lock) beat() {
	defer close(l.done)
	t := time.NewTicker(Heartbeat)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case now := <-t.C:
			_ = os.Chtimes(l.path, now, now)
		}
	}
}

// Release frees the lock. It only removes the file if it is still this process's.
func (l *Lock) Release() {
	l.once.Do(func() {
		close(l.stop)
		<-l.done
		data, err := os.ReadFile(l.path)
		if err != nil {
			return
		}
		var cur Info
		if json.Unmarshal(data, &cur) == nil && cur.Token == l.info.Token {
			_ = os.Remove(l.path)
		}
	})
}
