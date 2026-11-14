package chat

import (
	"errors"
	"strings"
	"testing"

	"github.com/paulo-lucas/maestria/internal/events"
	"github.com/paulo-lucas/maestria/internal/store"
)

func TestSendAndList(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, _, err := Send(db, "ws1", SenderHuman, "   ", 100); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty chat must fail: %v", err)
	}
	if _, _, err := Send(db, "ws1", SenderHuman, strings.Repeat("x", MaxBodyChars+1), 100); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("oversized chat must fail: %v", err)
	}

	m1, ev1, err := Send(db, "ws1", SenderHuman, "Hello <script>alert(1)</script> **Maestro**", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if ev1 == nil || ev1.Kind != events.KindChat {
		t.Fatalf("human send must emit chat event, got %+v", ev1)
	}
	if strings.Contains(m1.BodyHTML, "<script") || !strings.Contains(m1.BodyHTML, "<strong>Maestro</strong>") {
		t.Fatalf("unexpected BodyHTML: %q", m1.BodyHTML)
	}

	m2, ev2, err := Send(db, "ws1", SenderMaestro, "On it.", 1001)
	if err != nil {
		t.Fatal(err)
	}
	if ev2 != nil {
		t.Fatalf("maestro reply must NOT emit event, got %+v", ev2)
	}

	msgs, err := List(db, "ws1", 0, 50)
	if err != nil || len(msgs) != 2 || msgs[0].ID != m1.ID || msgs[1].ID != m2.ID {
		t.Fatalf("List: %+v err=%v", msgs, err)
	}
	since, err := List(db, "ws1", m1.ID, 50)
	if err != nil || len(since) != 1 || since[0].ID != m2.ID {
		t.Fatalf("List afterID: %+v err=%v", since, err)
	}
}
