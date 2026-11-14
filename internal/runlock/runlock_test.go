package runlock

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAcquireRefuseRelease(t *testing.T) {
	dir := t.TempDir()
	l, err := Acquire(dir, "serve")
	if err != nil {
		t.Fatal(err)
	}
	_, err = Acquire(dir, "connect")
	var held *HeldError
	if !errors.As(err, &held) {
		t.Fatalf("a second process must be refused, got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"already running", "maestria serve", "pid " + itoa(os.Getpid()), "one process", "MAESTRIA_CONFIG_DIR", FileName} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	l.Release()
	l.Release() // idempotent
	l2, err := Acquire(dir, "serve+connect")
	if err != nil {
		t.Fatalf("the lock must be free after release: %v", err)
	}
	l2.Release()
	if _, err := os.Stat(filepath.Join(dir, FileName)); !os.IsNotExist(err) {
		t.Fatal("release must remove the file")
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestOnlyOneOfManyWins(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	var wins []*Lock
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l, err := Acquire(dir, "serve"); err == nil {
				mu.Lock()
				wins = append(wins, l)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(wins) != 1 {
		t.Fatalf("%d processes got the lock", len(wins))
	}
	wins[0].Release()
}

func writeLock(t *testing.T, dir string, info Info, age time.Duration) string {
	t.Helper()
	path := filepath.Join(dir, FileName)
	b, _ := json.Marshal(info)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-age)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStaleLocksAreTakenOver(t *testing.T) {
	// The owner process is gone.
	dir := t.TempDir()
	writeLock(t, dir, Info{PID: 2147483646, Mode: "serve", Started: time.Now().UnixMilli(), Token: "x"}, 0)
	l, err := Acquire(dir, "connect")
	if err != nil {
		t.Fatalf("a lock of a dead process must be taken over: %v", err)
	}
	l.Release()

	// The pid looks alive (it is us) but nothing refreshed the lock for longer than StaleAfter.
	dir = t.TempDir()
	writeLock(t, dir, Info{PID: os.Getpid(), Mode: "serve", Started: time.Now().UnixMilli(), Token: "x"}, 2*StaleAfter)
	if l, err = Acquire(dir, "serve"); err != nil {
		t.Fatalf("a lock that stopped refreshing must be taken over: %v", err)
	}
	l.Release()

	// A fresh lock of a live process is not stolen, and neither is a garbled one that is fresh
	// (its writer may still be writing).
	dir = t.TempDir()
	writeLock(t, dir, Info{PID: os.Getpid(), Mode: "serve", Started: time.Now().UnixMilli(), Token: "x"}, time.Second)
	if _, err := Acquire(dir, "serve"); err == nil {
		t.Fatal("a live lock was stolen")
	}
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(dir, "serve"); err == nil {
		t.Fatal("a fresh unreadable lock was stolen")
	}
}

func TestHeartbeatKeepsALockFresh(t *testing.T) {
	old := Heartbeat
	Heartbeat = 20 * time.Millisecond
	defer func() { Heartbeat = old }()
	dir := t.TempDir()
	l, err := Acquire(dir, "serve")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	path := filepath.Join(dir, FileName)
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := os.Stat(path); err == nil && time.Since(st.ModTime()) < time.Minute {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the heartbeat did not refresh the lock")
}

func TestReleaseDoesNotRemoveSomeoneElsesLock(t *testing.T) {
	dir := t.TempDir()
	l, err := Acquire(dir, "serve")
	if err != nil {
		t.Fatal(err)
	}
	// Another process took over (this one looked stale) and wrote its own lock.
	writeLock(t, dir, Info{PID: os.Getpid(), Mode: "connect", Started: time.Now().UnixMilli(), Token: "someone-else"}, 0)
	l.Release()
	if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
		t.Fatal("a released lock removed another owner's file")
	}
}

// A real second process: it holds the lock while it lives, and the lock is free after it dies.
func TestAcrossProcesses(t *testing.T) {
	if os.Getenv("RUNLOCK_HELPER_DIR") != "" {
		return
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperHoldsLock")
	cmd.Env = append(os.Environ(), "RUNLOCK_HELPER_DIR="+dir)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line := make(chan string, 1)
	go func() { s, _ := bufio.NewReader(out).ReadString('\n'); line <- s }()
	select {
	case s := <-line:
		if !strings.Contains(s, "locked") {
			t.Fatalf("helper said %q", s)
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("helper did not take the lock")
	}
	_, err = Acquire(dir, "serve")
	var held *HeldError
	if !errors.As(err, &held) || held.Info.PID != cmd.Process.Pid || held.Info.Mode != "serve+connect" {
		t.Fatalf("expected the helper (pid %d) to hold it: %v", cmd.Process.Pid, err)
	}
	// Killed without releasing: the lock stays on disk but is stale.
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	l, err := Acquire(dir, "serve")
	if err != nil {
		t.Fatalf("a crashed owner must not block the next start: %v", err)
	}
	l.Release()
}

func TestHelperHoldsLock(t *testing.T) {
	dir := os.Getenv("RUNLOCK_HELPER_DIR")
	if dir == "" {
		t.Skip("helper process only")
	}
	if _, err := Acquire(dir, "serve+connect"); err != nil {
		os.Stdout.WriteString("error " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Stdout.WriteString("locked\n")
	time.Sleep(time.Minute)
}
