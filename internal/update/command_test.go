package update

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runCmd(t *testing.T, u *Updater, o CommandOpts, stdin string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	o.Stdout, o.Stdin = &out, strings.NewReader(stdin)
	err := RunCommand(context.Background(), u, o)
	return out.String(), err
}

func cmdFixture(t *testing.T, current string) (*fakeGitHub, installed, *Updater) {
	g := newFakeGitHub(t)
	g.add(release(t, "maestria", "v1.2.0", "linux", "amd64", "NEW"))
	ins := install(t, "maestria", "OLD")
	return g, ins, updater(g, ins, current)
}

func TestCommandCheckOnly(t *testing.T) {
	_, ins, u := cmdFixture(t, "1.0.0")
	out, err := runCmd(t, u, CommandOpts{Check: true}, "")
	if err != nil || !strings.Contains(out, "maestria 1.2.0 is available (you have 1.0.0)") {
		t.Fatalf("%q %v", out, err)
	}
	if ins.read(t) != "OLD" {
		t.Error("--check must not install")
	}
	if _, err := os.Stat(ins.exe + ".previous"); err == nil {
		t.Error("--check must not touch the folder")
	}
}

func TestCommandNeedsYesWithoutTerminal(t *testing.T) {
	_, ins, u := cmdFixture(t, "1.0.0")
	_, err := runCmd(t, u, CommandOpts{}, "")
	var ue *UsageError
	if !errors.As(err, &ue) || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("err = %v", err)
	}
	if ins.read(t) != "OLD" {
		t.Error("nothing may be installed without consent")
	}
}

func TestCommandPromptsOnTerminal(t *testing.T) {
	for stdin, want := range map[string]string{"y\n": "NEW", "yes\n": "NEW", "n\n": "OLD", "\n": "OLD", "": "OLD"} {
		_, ins, u := cmdFixture(t, "1.0.0")
		out, err := runCmd(t, u, CommandOpts{IsTTY: true}, stdin)
		if err != nil {
			t.Fatalf("%q: %v", stdin, err)
		}
		if ins.read(t) != want {
			t.Errorf("answer %q: binary = %q, want %q (%s)", stdin, ins.read(t), want, out)
		}
	}
}

func TestCommandYesUpdates(t *testing.T) {
	_, ins, u := cmdFixture(t, "1.0.0")
	out, err := runCmd(t, u, CommandOpts{Yes: true}, "")
	if err != nil || ins.read(t) != "NEW" || !strings.Contains(out, "maestria is now 1.2.0") {
		t.Fatalf("%q %v binary=%q", out, err, ins.read(t))
	}
}

func TestCommandSameAndOlderAreNoOps(t *testing.T) {
	for _, cur := range []string{"1.2.0", "1.3.0"} {
		_, ins, u := cmdFixture(t, cur)
		out, err := runCmd(t, u, CommandOpts{Yes: true}, "")
		if err != nil || ins.read(t) != "OLD" {
			t.Fatalf("current %s: %q %v binary=%q", cur, out, err, ins.read(t))
		}
	}
}

func TestCommandDevBuildNeverUpdates(t *testing.T) {
	g, ins, u := cmdFixture(t, "2.0.0-dev+abc1234")
	out, err := runCmd(t, u, CommandOpts{Yes: true}, "")
	if err != nil || ins.read(t) != "OLD" || !strings.Contains(out, "development build") {
		t.Fatalf("%q %v", out, err)
	}
	g.mu.Lock()
	n := len(g.hits)
	g.mu.Unlock()
	if n != 0 {
		t.Errorf("a development build must not even ask GitHub (%d requests)", n)
	}
	// --force is the explicit way out.
	if _, err := runCmd(t, u, CommandOpts{Yes: true, Force: true}, ""); err != nil || ins.read(t) != "NEW" {
		t.Fatalf("forced: %v binary=%q", err, ins.read(t))
	}
}

func TestCommandDowngradeNeedsForce(t *testing.T) {
	g := newFakeGitHub(t)
	g.add(release(t, "maestria", "v1.2.0", "linux", "amd64", "V2"))
	g.add(release(t, "maestria", "v1.1.0", "linux", "amd64", "V1.1"))
	ins := install(t, "maestria", "CURRENT")
	u := updater(g, ins, "1.2.0")

	_, err := runCmd(t, u, CommandOpts{Yes: true, Version: "v1.1.0"}, "")
	var ue *UsageError
	if !errors.As(err, &ue) || !strings.Contains(err.Error(), "--force") || ins.read(t) != "CURRENT" {
		t.Fatalf("err = %v binary=%q", err, ins.read(t))
	}
	out, err := runCmd(t, u, CommandOpts{Yes: true, Force: true, Version: "1.1.0"}, "")
	if err != nil || ins.read(t) != "V1.1" || !strings.Contains(out, "is now 1.1.0") {
		t.Fatalf("%q %v binary=%q", out, err, ins.read(t))
	}
	// And --force reinstalls the same version.
	u.Current = "1.1.0"
	if _, err := runCmd(t, u, CommandOpts{Yes: true, Force: true, Version: "v1.1.0"}, ""); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
}

func TestCommandPrereleaseChannel(t *testing.T) {
	g := newFakeGitHub(t)
	rc := release(t, "maestria", "v1.3.0-rc.1", "linux", "amd64", "RC")
	rc.Prerelease = true
	g.add(rc)
	g.add(release(t, "maestria", "v1.2.0", "linux", "amd64", "STABLE"))
	ins := install(t, "maestria", "OLD")
	u := updater(g, ins, "1.2.0")

	out, _ := runCmd(t, u, CommandOpts{Yes: true, Channel: ChannelStable}, "")
	if ins.read(t) != "OLD" || !strings.Contains(out, "up to date") {
		t.Fatalf("stable: %q binary=%q", out, ins.read(t))
	}
	if _, err := runCmd(t, u, CommandOpts{Yes: true, Channel: ChannelPrerelease}, ""); err != nil || ins.read(t) != "RC" {
		t.Fatalf("prerelease: %v binary=%q", err, ins.read(t))
	}
}

func TestCommandNoReleases(t *testing.T) {
	g := newFakeGitHub(t)
	ins := install(t, "maestria", "OLD")
	u := updater(g, ins, "1.0.0")
	if _, err := runCmd(t, u, CommandOpts{Yes: true}, ""); err == nil || !strings.Contains(err.Error(), "no release") {
		t.Fatalf("err = %v", err)
	}
}

// ---- notifier ----

func notifierFixture(t *testing.T, current string) (*fakeGitHub, *Notifier, *time.Time) {
	g := newFakeGitHub(t)
	g.add(release(t, "maestria", "v1.2.0", "linux", "amd64", "NEW"))
	ins := install(t, "maestria", "OLD")
	now := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	return g, &Notifier{
		Updater: updater(g, ins, current), Channel: ChannelStable, Dir: t.TempDir(), Enabled: true,
		Now: func() time.Time { return now },
	}, &now
}

func (g *fakeGitHub) apiHits() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hits["/repos/"+Repo+"/releases/latest"]
}

func TestNotifierChecksOncePerDay(t *testing.T) {
	g, n, now := notifierFixture(t, "1.0.0")
	if n.Cached() != nil {
		t.Fatal("nothing is cached yet")
	}
	notice, err := n.Refresh(context.Background())
	if err != nil || notice == nil || notice.Version != "1.2.0" {
		t.Fatalf("%+v %v", notice, err)
	}
	for i := 0; i < 5; i++ {
		*now = now.Add(time.Hour)
		if got, _ := n.Refresh(context.Background()); got == nil {
			t.Fatal("the cached notice should still show")
		}
	}
	if g.apiHits() != 1 {
		t.Errorf("%d API calls within a day, want 1", g.apiHits())
	}
	if c := n.Cached(); c == nil || c.Version != "1.2.0" {
		t.Errorf("Cached = %+v", c)
	}
	*now = now.Add(25 * time.Hour)
	n.Refresh(context.Background())
	if g.apiHits() != 2 {
		t.Errorf("%d API calls after a day, want 2", g.apiHits())
	}
	info, err := os.Stat(filepath.Join(n.Dir, StateFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("state file mode %v", info.Mode())
	}
}

func TestNotifierDismiss(t *testing.T) {
	g, n, _ := notifierFixture(t, "1.0.0")
	n.Refresh(context.Background())
	n.Dismiss("1.2.0")
	if n.Cached() != nil {
		t.Error("a dismissed version must stay hidden")
	}
	if got, _ := n.Refresh(context.Background()); got != nil {
		t.Error("a dismissed version must stay hidden after a refresh too")
	}
	// A newer release brings the note back.
	g.add(release(t, "maestria", "v1.3.0", "linux", "amd64", "NEWER"))
	g.releases[0], g.releases[1] = g.releases[1], g.releases[0]
	n.Now = func() time.Time { return time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC) }
	if got, _ := n.Refresh(context.Background()); got == nil || got.Version != "1.3.0" {
		t.Errorf("got %+v, want 1.3.0", got)
	}
}

func TestNotifierOffMeansNoNetwork(t *testing.T) {
	for name, mod := range map[string]func(n *Notifier){
		"disabled":  func(n *Notifier) { n.Enabled = false },
		"dev build": func(n *Notifier) { n.Updater.Current = "2.0.0-dev+abc" },
	} {
		g, n, _ := notifierFixture(t, "1.0.0")
		mod(n)
		if got, err := n.Refresh(context.Background()); got != nil || err != nil || g.apiHits() != 0 {
			t.Errorf("%s: %+v %v hits=%d", name, got, err, g.apiHits())
		}
	}
}

func TestNotifierUpToDateAndErrors(t *testing.T) {
	_, n, _ := notifierFixture(t, "1.2.0")
	if got, err := n.Refresh(context.Background()); got != nil || err != nil {
		t.Errorf("up to date: %+v %v", got, err)
	}

	g, n, now := notifierFixture(t, "1.0.0")
	g.limited = true
	if _, err := n.Refresh(context.Background()); err == nil {
		t.Fatal("expected the rate limit error")
	}
	g.limited = false
	*now = now.Add(5 * time.Minute) // backed off: no second request yet
	n.Refresh(context.Background())
	if g.apiHits() != 1 {
		t.Errorf("retried too soon (%d hits)", g.apiHits())
	}
}
