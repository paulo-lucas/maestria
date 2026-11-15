package audit

import (
	"strings"
	"testing"
	"time"

	"github.com/paulo-lucas/maestria/internal/store"
)

func TestWriteRecentAndThrottle(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.UnixMilli(1_700_000_000_000)
	l := New(db, func() time.Time { return now })

	_ = l.Write("login_ok", "127.0.0.1", map[string]any{"method": "totp"})
	_ = l.Write("logout", "", nil)
	_ = l.Write("long", "", strings.Repeat("é", 2000))
	for i := 0; i < 5; i++ {
		_ = l.WriteThrottled("k", time.Minute, "origin_reject", "1.2.3.4", nil)
	}
	now = now.Add(2 * time.Minute)
	_ = l.WriteThrottled("k", time.Minute, "origin_reject", "1.2.3.4", nil)

	entries, err := l.Recent(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 5 {
		t.Fatalf("want 5 entries (2 throttled rejects), got %d", len(entries))
	}
	if entries[len(entries)-1].Event != "login_ok" || entries[len(entries)-1].Detail != `{"method":"totp"}` {
		t.Fatalf("oldest entry: %+v", entries[len(entries)-1])
	}
	for _, e := range entries {
		if e.Event == "long" && (len(e.Detail) > 1010 || !strings.HasSuffix(e.Detail, "…")) {
			t.Fatalf("detail not truncated cleanly: %d bytes", len(e.Detail))
		}
	}
}

func TestThrottleMapIsBoundedWithoutForgettingLiveKeys(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.UnixMilli(1_700_000_000_000)
	l := New(db, func() time.Time { return now })

	if !l.Allow("victim", time.Hour) || l.Allow("victim", time.Hour) {
		t.Fatal("a key acts once per interval")
	}
	// A flood of new keys (attacker-chosen addresses) with shorter intervals must neither grow the
	// map without bound nor make it forget the key that is still throttled.
	for i := 0; i < 5000; i++ {
		l.Allow("flood:"+string(rune('a'+i%26))+time.Duration(i).String(), time.Second)
		now = now.Add(time.Millisecond)
	}
	if len(l.throttle) > maxThrottleKeys {
		t.Fatalf("throttle map grew to %d keys", len(l.throttle))
	}
	if l.Allow("victim", time.Hour) {
		t.Fatal("a flood of other keys reset a throttled key")
	}
}

func TestRecentSignificantLeavesOutFloodedRefusals(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	l := New(db, nil)
	_ = l.Write("login_ok", "1.2.3.4", nil)
	_ = l.Write("device_approved", "", nil)
	for i := 0; i < 300; i++ {
		_ = l.Write("csrf_reject", "6.6.6.6", nil)
		_ = l.Write("origin_reject", "6.6.6.6", nil)
		_ = l.Write("login_blocked", "6.6.6.6", nil)
	}
	all, _ := l.Recent(50)
	for _, e := range all {
		if e.Event == "login_ok" {
			t.Fatal("test setup: the flood should have pushed login_ok out of the plain view")
		}
	}
	sig, _ := l.RecentSignificant(50)
	if len(sig) != 2 || sig[0].Event != "device_approved" || sig[1].Event != "login_ok" {
		t.Fatalf("significant view: %+v", sig)
	}
}
