package audit

import (
	"strings"
	"testing"
	"time"

	"github.com/maestria-dev/maestria/internal/store"
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
