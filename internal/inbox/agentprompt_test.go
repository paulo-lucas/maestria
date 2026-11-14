package inbox

import (
	"database/sql"
	"testing"

	"github.com/maestria-dev/maestria/internal/store"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.OpenPath(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSyncAgentPromptsCreatesAndResolves(t *testing.T) {
	db := openTestDB(t)
	changed, err := SyncAgentPrompts(db, "ws", []AgentPrompt{{Ref: "t1:aaa", Agent: "Orin", Prompt: "Allow pnpm add zod? (Y/n)"}}, 1000)
	if err != nil || !changed {
		t.Fatalf("first sync: changed=%v err=%v", changed, err)
	}
	// Same prompt again: nothing changes, no duplicate.
	if changed, _ = SyncAgentPrompts(db, "ws", []AgentPrompt{{Ref: "t1:aaa", Agent: "Orin", Prompt: "x"}}, 2000); changed {
		t.Fatal("unchanged sync reported a change")
	}
	items, _ := List(db, ListOptions{WorkspaceID: "ws", Status: "open"})
	if len(items) != 1 || items[0].Kind != KindAgentPrompt {
		t.Fatalf("items = %+v", items)
	}
	if c, _ := CountsByWorkspace(db, 3000); c["ws"].NeedsYou != 1 {
		t.Fatalf("agent prompt must count as Needs you: %+v", c["ws"])
	}
	// The prompt disappears from the feed: item is closed automatically.
	if changed, _ = SyncAgentPrompts(db, "ws", nil, 4000); !changed {
		t.Fatal("disappearance not detected")
	}
	items, _ = List(db, ListOptions{WorkspaceID: "ws", Status: "open"})
	if len(items) != 0 {
		t.Fatalf("still open: %+v", items)
	}
	// And Maestro `answers` never sees it.
	if ans, _ := Answers(db, "ws", false, 5000); len(ans) != 0 {
		t.Fatalf("answers = %+v", ans)
	}
	// A new prompt on the same terminal (different hash) opens a new item.
	if _, err := SyncAgentPrompts(db, "ws", []AgentPrompt{{Ref: "t1:bbb", Agent: "Orin", Prompt: "next"}}, 6000); err != nil {
		t.Fatal(err)
	}
	items, _ = List(db, ListOptions{WorkspaceID: "ws", Status: "open"})
	if len(items) != 1 {
		t.Fatalf("items = %+v", items)
	}
}

func TestResolveAgentPromptEmitsNoEvent(t *testing.T) {
	db := openTestDB(t)
	_, _ = SyncAgentPrompts(db, "ws", []AgentPrompt{{Ref: "t1:aaa", Agent: "Orin", Prompt: "Y/n"}}, 1000)
	items, _ := List(db, ListOptions{WorkspaceID: "ws", Status: "open"})
	id := items[0].ID
	if _, err := ResolveAgentPrompt(db, "ws", id, "bogus", 2000); err == nil {
		t.Fatal("bad response accepted")
	}
	it, err := ResolveAgentPrompt(db, "ws", id, ResponseApproved, 2000)
	if err != nil || it.Status != StatusAnswered || it.Response == nil || *it.Response != ResponseApproved {
		t.Fatalf("resolve: %+v %v", it, err)
	}
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d events emitted", n)
	}
	if _, err := ResolveAgentPrompt(db, "ws", id, ResponseRejected, 3000); err != ErrNotOpen {
		t.Fatalf("second resolve: %v", err)
	}
}

func TestDismissAllAgentPrompts(t *testing.T) {
	db := openTestDB(t)
	_, _ = SyncAgentPrompts(db, "a", []AgentPrompt{{Ref: "1:x", Prompt: "p"}}, 1)
	_, _ = SyncAgentPrompts(db, "b", []AgentPrompt{{Ref: "2:x", Prompt: "p"}}, 1)
	n, err := DismissAllAgentPrompts(db, 10)
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}
