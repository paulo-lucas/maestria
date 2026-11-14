package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/maestria-dev/maestria/internal/audit"
	"github.com/maestria-dev/maestria/internal/chat"
	"github.com/maestria-dev/maestria/internal/config"
	"github.com/maestria-dev/maestria/internal/events"
	"github.com/maestria-dev/maestria/internal/inbox"
	"github.com/maestria-dev/maestria/internal/standby"
	"github.com/maestria-dev/maestria/internal/store"
	"github.com/maestria-dev/maestria/internal/usage"
	"github.com/maestria-dev/maestria/internal/workspaces"
)

func cmdPost(args []string, env *Env) error {
	parsed, err := parseFlexArgs(args, flagSpec{
		Bools:   map[string]bool{"json": true},
		Strings: map[string]bool{"workspace": true, "author": true},
		Multi:   map[string]bool{"option": true},
	})
	if err != nil {
		return usagef("%v", err)
	}
	if len(parsed.Pos) < 2 {
		return usagef("Usage: maestria post <notice|approve|input|choice> <body> [--option ...] [--author <name>] [--workspace <ws>] [--json]")
	}
	kind := parsed.Pos[0]
	body := strings.Join(parsed.Pos[1:], " ")
	if body == "-" {
		raw, err := io.ReadAll(env.Stdin)
		if err != nil {
			return err
		}
		body = string(raw)
	}

	ws, _, _, err := resolveWorkspaceCLI(env, parsed.String("workspace"))
	if err != nil {
		return err
	}
	db, err := store.Open(configDir(env))
	if err != nil {
		return err
	}
	defer db.Close()

	item, err := inbox.Post(db, ws.ID, inbox.PostInput{
		Kind:    kind,
		Body:    body,
		Options: parsed.MultiVal("option"),
		Author:  parsed.String("author"),
	}, env.now().UnixMilli())
	if err != nil {
		return err
	}
	if parsed.Bool("json") {
		return writeJSONLine(env, item)
	}
	fmt.Fprintf(env.Stdout, "Posted %s #%d to %s (%s)\n", item.Kind, item.ID, ws.ID, ws.Name)
	return nil
}

func cmdInbox(args []string, env *Env) error {
	parsed, err := parseFlexArgs(args, flagSpec{
		Bools:   map[string]bool{"json": true},
		Strings: map[string]bool{"workspace": true, "status": true},
	})
	if err != nil {
		return usagef("%v", err)
	}
	status := parsed.String("status")
	if status == "" {
		status = inbox.StatusOpen
	}
	ws, _, _, err := resolveWorkspaceCLI(env, parsed.String("workspace"))
	if err != nil {
		return err
	}
	db, err := store.Open(configDir(env))
	if err != nil {
		return err
	}
	defer db.Close()

	items, err := inbox.List(db, inbox.ListOptions{
		WorkspaceID: ws.ID,
		Status:      status,
		NowMs:       env.now().UnixMilli(),
	})
	if err != nil {
		return err
	}
	if parsed.Bool("json") {
		return writeJSONLine(env, map[string]any{
			"workspace": ws.ID,
			"items":     items,
		})
	}
	if len(items) == 0 {
		fmt.Fprintf(env.Stdout, "No %s inbox items for %s.\n", status, ws.ID)
		return nil
	}
	for _, it := range items {
		tag := it.Kind
		if it.Postponed {
			tag += ", postponed"
		}
		extra := ""
		if it.Response != nil && *it.Response != "" {
			extra = " -> " + *it.Response
		}
		fmt.Fprintf(env.Stdout, "#%d [%s] %s%s\n", it.ID, tag, it.Title, extra)
	}
	return nil
}

func cmdAnswers(args []string, env *Env) error {
	parsed, err := parseFlexArgs(args, flagSpec{
		Bools:   map[string]bool{"json": true, "ack": true},
		Strings: map[string]bool{"workspace": true},
	})
	if err != nil {
		return usagef("%v", err)
	}
	ws, _, _, err := resolveWorkspaceCLI(env, parsed.String("workspace"))
	if err != nil {
		return err
	}
	db, err := store.Open(configDir(env))
	if err != nil {
		return err
	}
	defer db.Close()

	items, err := inbox.Answers(db, ws.ID, parsed.Bool("ack"), env.now().UnixMilli())
	if err != nil {
		return err
	}
	if parsed.Bool("json") {
		return writeJSONLine(env, map[string]any{
			"workspace": ws.ID,
			"answers":   items,
		})
	}
	if len(items) == 0 {
		fmt.Fprintf(env.Stdout, "No unacknowledged answers for %s.\n", ws.ID)
		return nil
	}
	for _, it := range items {
		resp := ""
		if it.Response != nil {
			resp = *it.Response
		}
		fmt.Fprintf(env.Stdout, "#%d (%s): %s\n", it.ID, it.Kind, resp)
	}
	return nil
}

func cmdSay(args []string, env *Env) error {
	parsed, err := parseFlexArgs(args, flagSpec{
		Bools:   map[string]bool{"json": true},
		Strings: map[string]bool{"workspace": true},
	})
	if err != nil {
		return usagef("%v", err)
	}
	if len(parsed.Pos) == 0 {
		return usagef("Usage: maestria say <message> [--workspace <ws>] [--json]")
	}
	msgText := strings.Join(parsed.Pos, " ")
	if msgText == "-" {
		raw, err := io.ReadAll(env.Stdin)
		if err != nil {
			return err
		}
		msgText = string(raw)
	}

	ws, _, _, err := resolveWorkspaceCLI(env, parsed.String("workspace"))
	if err != nil {
		return err
	}
	db, err := store.Open(configDir(env))
	if err != nil {
		return err
	}
	defer db.Close()

	msg, _, err := chat.Send(db, ws.ID, chat.SenderMaestro, msgText, env.now().UnixMilli())
	if err != nil {
		return err
	}
	if parsed.Bool("json") {
		return writeJSONLine(env, msg)
	}
	fmt.Fprintf(env.Stdout, "Sent to %s (#%d)\n", ws.ID, msg.ID)
	return nil
}

func cmdWait(args []string, env *Env) error {
	parsed, err := parseFlexArgs(args, flagSpec{
		Bools:   map[string]bool{"json": true, "from-now": true},
		Strings: map[string]bool{"workspace": true, "cursor": true, "since": true, "timeout": true},
	})
	if err != nil {
		return usagef("%v", err)
	}

	var sinceID int64
	sinceSet := parsed.HasString("since")
	if sinceSet {
		v, err := strconv.ParseInt(parsed.String("since"), 10, 64)
		if err != nil || v < 0 {
			return usagef("invalid --since value: %q", parsed.String("since"))
		}
		sinceID = v
	}

	var timeout time.Duration
	if parsed.HasString("timeout") {
		d, err := parseWaitTimeout(parsed.String("timeout"))
		if err != nil {
			return usagef("%v", err)
		}
		timeout = d
	}

	ws, _, _, err := resolveWorkspaceCLI(env, parsed.String("workspace"))
	if err != nil {
		return err
	}
	db, err := store.Open(configDir(env))
	if err != nil {
		return err
	}
	defer db.Close()

	cursorName := parsed.String("cursor")
	if cursorName == "" && !sinceSet {
		cursorName = events.DefaultCursor
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	evs, lastID, err := events.Wait(ctx, db, ws.ID, events.WaitOptions{
		Cursor:   cursorName,
		SinceID:  sinceID,
		SinceSet: sinceSet,
		FromNow:  parsed.Bool("from-now"),
		Timeout:  timeout,
		Now:      func() int64 { return env.now().UnixMilli() },
	})
	if err != nil && err != context.Canceled {
		return err
	}

	if parsed.Bool("json") {
		return writeJSONLine(env, map[string]any{
			"workspace":   ws.ID,
			"cursor":      cursorName,
			"lastEventId": lastID,
			"events":      evs,
			"timedOut":    len(evs) == 0,
		})
	}
	if len(evs) == 0 {
		fmt.Fprintln(env.Stdout, "Timed out waiting for events.")
		return nil
	}
	for _, ev := range evs {
		fmt.Fprintf(env.Stdout, "[#%d] %s %s\n", ev.ID, ev.Kind, string(ev.Payload))
	}
	return nil
}

func cmdStatus(args []string, env *Env) error {
	parsed, err := parseFlexArgs(args, flagSpec{
		Bools:   map[string]bool{"json": true},
		Strings: map[string]bool{"workspace": true},
	})
	if err != nil {
		return usagef("%v", err)
	}

	list, cfg, err := loadMergedWorkspaces(env)
	if err != nil {
		return err
	}
	db, err := store.Open(configDir(env))
	if err != nil {
		return err
	}
	defer db.Close()

	now := env.now()
	nowMs := now.UnixMilli()
	counts, err := inbox.CountsByWorkspace(db, nowMs)
	if err != nil {
		return err
	}
	sb, err := standby.Current(db)
	if err != nil {
		return err
	}
	usageSnap := usage.ReadSnapshot(db, cfg, now)

	var resolved *workspaces.Workspace
	if wsArg := parsed.String("workspace"); wsArg != "" {
		w, err := workspaces.Resolve(list, wsArg, "")
		if err != nil {
			return err
		}
		resolved = w
	} else if w, err := workspaces.Resolve(list, "", ""); err == nil {
		resolved = w
	}

	if parsed.Bool("json") {
		out := map[string]any{
			"standby": sb,
			"usage":   usageSnap,
			"counts":  counts,
		}
		if resolved != nil {
			out["workspace"] = resolved
			out["workspaceCounts"] = counts[resolved.ID]
		} else {
			out["workspaces"] = list
		}
		return writeJSONLine(env, out)
	}

	if resolved != nil {
		c := counts[resolved.ID]
		fmt.Fprintf(env.Stdout, "Workspace: %s (%s)\n", resolved.Name, resolved.ID)
		fmt.Fprintf(env.Stdout, "Tasks:     %d needs you, %d postponed, %d updates, %d history\n",
			c.NeedsYou, c.Postponed, c.Updates, c.History)
	} else {
		fmt.Fprintf(env.Stdout, "Workspaces: %d total\n", len(list))
		for _, w := range list {
			c := counts[w.ID]
			fmt.Fprintf(env.Stdout, "  - %s (%s): %d needs you, %d updates\n", w.ID, w.Name, c.NeedsYou, c.Updates)
		}
	}
	if sb.Active {
		fmt.Fprintf(env.Stdout, "Standby:   ACTIVE (%s) — %s\n", sb.Action, sb.Banner)
	} else {
		fmt.Fprintln(env.Stdout, "Standby:   clear")
	}
	if usageSnap.Issue != nil {
		fmt.Fprintf(env.Stdout, "Usage:     unavailable (%s: %s)\n", usageSnap.Issue.Code, usageSnap.Issue.Title)
	} else {
		fmt.Fprintf(env.Stdout, "Usage:     %d provider accounts tracked\n", len(usageSnap.Accounts))
	}
	return nil
}

func cmdImportV1(args []string, env *Env) error {
	parsed, err := parseFlexArgs(args, flagSpec{
		Bools:   map[string]bool{"json": true},
		Strings: map[string]bool{"from": true},
	})
	if err != nil {
		return usagef("%v", err)
	}
	dir := configDir(env)
	db, err := store.Open(dir)
	if err != nil {
		return err
	}
	defer db.Close()

	report, err := store.ImportV1(db, dir, parsed.String("from"))
	if err != nil {
		return err
	}
	auditLog := audit.New(db, env.Now)
	_ = auditLog.Write("import_v1", "cli", report)

	if parsed.Bool("json") {
		return writeJSONLine(env, report)
	}
	fmt.Fprintf(env.Stdout, "Imported from %s:\n  inbox: %d imported, %d skipped\n  chat:  %d imported, %d skipped\n  events:%d imported, %d skipped\n",
		report.V1Path,
		report.InboxImported, report.InboxSkipped,
		report.ChatImported, report.ChatSkipped,
		report.EventsImported, report.EventsSkipped,
	)
	return nil
}

func parseWaitTimeout(raw string) (time.Duration, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, nil
	}
	if secs, err := strconv.ParseFloat(s, 64); err == nil {
		if secs < 0 {
			return 0, fmt.Errorf("--timeout must not be negative: %q", raw)
		}
		return time.Duration(secs * float64(time.Second)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid --timeout %q (use duration like 5s or numeric seconds)", raw)
	}
	return d, nil
}

// Keep unused config import check clean if expanded.
var _ = (*config.Config)(nil)
