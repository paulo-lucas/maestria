package events

import (
	"context"
	"testing"
	"time"

	"github.com/paulo-lucas/maestria/internal/store"
)

func TestAppendSinceAndCursors(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ev1, err := Append(db, "ws1", KindChat, map[string]any{"body": "hello"}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Append(db, "ws2", KindChat, map[string]any{"body": "other ws"}, 1001); err != nil {
		t.Fatal(err)
	}
	ev2, err := Append(db, "ws1", KindInboxResponse, map[string]any{"id": 7, "response": "Yes"}, 1002)
	if err != nil {
		t.Fatal(err)
	}

	got, err := Since(db, "ws1", 0, 50)
	if err != nil || len(got) != 2 || got[0].ID != ev1.ID || got[1].ID != ev2.ID {
		t.Fatalf("Since ws1: %+v err=%v", got, err)
	}

	// First Wait on default cursor returns unread history and advances cursor.
	got, lastID, err := Wait(context.Background(), db, "ws1", WaitOptions{
		Cursor:       "c1",
		PollInterval: 10 * time.Millisecond,
	})
	if err != nil || len(got) != 2 || lastID != ev2.ID {
		t.Fatalf("Wait c1: len=%d last=%d err=%v", len(got), lastID, err)
	}

	saved, ok, err := GetCursor(db, "ws1", "c1")
	if err != nil || !ok || saved != ev2.ID {
		t.Fatalf("cursor saved=%d ok=%v err=%v", saved, ok, err)
	}

	// Emitting while offline is caught on next Wait with the same cursor.
	ev3, err := Append(db, "ws1", KindStandby, map[string]any{"action": "tell"}, 2000)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err = Wait(context.Background(), db, "ws1", WaitOptions{Cursor: "c1"})
	if err != nil || len(got) != 1 || got[0].ID != ev3.ID {
		t.Fatalf("Wait c1 missed event: %+v err=%v", got, err)
	}
}

func TestWaitFromNowTimeoutAndBlocking(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Unread historical event.
	if _, err := Append(db, "ws1", KindChat, map[string]any{"body": "old"}, 1000); err != nil {
		t.Fatal(err)
	}

	// --from-now ignores old event and returns empty on short timeout.
	got, _, err := Wait(context.Background(), db, "ws1", WaitOptions{
		Cursor:       "c-now",
		FromNow:      true,
		Timeout:      40 * time.Millisecond,
		PollInterval: 10 * time.Millisecond,
	})
	if err != nil || len(got) != 0 {
		t.Fatalf("expected timeout empty, got %+v err=%v", got, err)
	}

	// Blocking wait wakes up when another writer appends an event.
	done := make(chan []Event, 1)
	errCh := make(chan error, 1)
	go func() {
		evs, _, werr := Wait(context.Background(), db, "ws1", WaitOptions{
			Cursor:       "c-now",
			Timeout:      2 * time.Second,
			PollInterval: 15 * time.Millisecond,
		})
		errCh <- werr
		done <- evs
	}()

	time.Sleep(50 * time.Millisecond)
	live, err := Append(db, "ws1", KindResume, map[string]any{"reason": "manual"}, 3000)
	if err != nil {
		t.Fatal(err)
	}

	if werr := <-errCh; werr != nil {
		t.Fatal(werr)
	}
	got = <-done
	if len(got) != 1 || got[0].ID != live.ID {
		t.Fatalf("expected live event %+v, got %+v", live, got)
	}
}
