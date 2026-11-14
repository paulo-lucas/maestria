package tunnel

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/maestria-dev/maestria/internal/audit"
	"github.com/maestria-dev/maestria/internal/auth"
	"github.com/maestria-dev/maestria/internal/config"
	"github.com/maestria-dev/maestria/internal/store"
)

func TestKillSwitchAndLocalEnable(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sessions := auth.NewSessionStore(db, time.Hour, 12*time.Hour, nil)
	var calls [][]string
	c := &Controller{
		DB: db, Audit: audit.New(db, nil), Sessions: sessions,
		Config: config.Tunnel{StopCommand: []string{"stop-tunnel"}, StartCommand: []string{"start-tunnel"}, StatusCommand: []string{"tunnel-status"}},
		Runner: func(_ context.Context, argv []string) CommandResult {
			calls = append(calls, argv)
			return CommandResult{OK: argv[0] != "start-tunnel", Stderr: "boom"}
		},
	}
	_, _, _ = sessions.Create("ip", "ua")
	_, _, _ = sessions.Create("ip", "ua")

	if c.Killed() {
		t.Fatal("off by default")
	}
	if st := c.Status(context.Background()); st.Running == nil || !*st.Running {
		t.Fatalf("status: %+v", st)
	}
	res, err := c.Kill(context.Background(), "web", "127.0.0.1")
	if err != nil || res.SessionsInvalidated != 2 || res.StopResult == nil || !res.StopResult.OK {
		t.Fatalf("kill: %+v %v", res, err)
	}
	if !c.Killed() {
		t.Fatal("killed")
	}
	if n, _ := sessions.Count(); n != 0 {
		t.Fatal("sessions must be invalidated")
	}
	st := c.Status(context.Background())
	if !st.Killed || st.KilledAt == nil || st.Running == nil || *st.Running {
		t.Fatalf("status after kill: %+v", st)
	}

	en, err := c.EnableLocal(context.Background())
	if err != nil || !en.WasKilled || en.StartResult == nil || en.StartResult.OK {
		t.Fatalf("enable: %+v %v", en, err)
	}
	if c.Killed() {
		t.Fatal("enabled")
	}
	got := []string{}
	for _, call := range calls {
		got = append(got, call[0])
	}
	if strings.Join(got, ",") != "tunnel-status,stop-tunnel,start-tunnel" {
		t.Fatalf("calls: %v", got)
	}
	entries, _ := audit.New(db, nil).Recent(10)
	if len(entries) != 2 || entries[0].Event != "tunnel_on" || entries[1].Event != "kill_switch" {
		t.Fatalf("audit: %+v", entries)
	}
}

func TestExecRunner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX commands")
	}
	if r := ExecRunner(context.Background(), []string{"true"}); !r.OK || r.Code == nil || *r.Code != 0 {
		t.Fatalf("true: %+v", r)
	}
	if r := ExecRunner(context.Background(), []string{"sh", "-c", "echo nope >&2; exit 3"}); r.OK || *r.Code != 3 || r.Stderr != "nope" {
		t.Fatalf("exit 3: %+v", r)
	}
	if r := ExecRunner(context.Background(), []string{"/definitely/not/a/command"}); r.OK || r.Stderr == "" {
		t.Fatalf("missing: %+v", r)
	}
	if r := ExecRunner(context.Background(), nil); !r.OK {
		t.Fatal("empty argv is a no-op")
	}
}
