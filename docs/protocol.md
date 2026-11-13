# Maestria Maestro Protocol

Any Maestro agent working in a configured Maestri workspace can communicate with the human and respond to panel events using the `maestria` CLI.

Every workspace command infers the workspace automatically from the current working directory (`--workspace <id|name>` overrides it).

## Quick Prompt Snippet

Paste this into a Maestro's instructions (or install the skill via `maestria init`):

```text
This workspace uses Maestria (`maestria` CLI) to talk to the human:
- Post updates (`maestria post --kind notice "..."`), approvals (`--kind approve`), questions (`--kind input`), and choices (`--kind choice --option A --option B "..."`).
- Reply in chat with `maestria say "..."`.
- Run `maestria wait` in the background to receive human chat messages, inbox answers, and usage standby/resume events; after handling an event, re-run `maestria wait` in the background.
- On a `standby` event, pause work until a `resume` event arrives.
```

## CLI Reference

| Command | Purpose |
| --- | --- |
| `maestria post --kind notice\|approve\|input\|choice [--option ...] [--by <agent>] <markdown>` | Post an Inbox item for the human. |
| `maestria inbox [--status open\|history\|all] [--json]` | List workspace Inbox items. |
| `maestria answers [--all] [--json]` | Read resolved Inbox answers/decisions (human-readable or JSON). |
| `maestria say [--by <agent>] <text>` | Append a Maestro reply to the workspace Chat. |
| `maestria wait [--cursor <name>] [--since <id>] [--from-now] [--timeout <sec>] [--json]` | Block until new events arrive for the workspace, advance the named SQLite cursor, print events, and exit. |
| `maestria status [--json]` | Show workspace counts (`needsYou`, `updates`, `history`). |

## Event Types (`maestria wait`)

Each event row belongs to a single workspace and carries an auto-incrementing `id`, `type`, `payload`, and `createdAt` (epoch ms):

- `chat`: `{ messageId, author, body, createdAt }` — emitted when the human sends a Chat message in the panel.
- `inbox_response`: `{ itemId, kind, decision, response }` — emitted when the human responds to an `approve`, `input`, or `choice` item.
- `inbox_dismiss`: `{ itemId, kind, decision }` — emitted when the human clicks "Got it" on a `notice` or dismisses an open item.
- `standby`: `{ message, triggers }` — emitted when configured provider/window usage reaches a standby threshold.
- `resume`: `{ message }` — emitted once monitored usage drops below the resume threshold.

Using `--cursor <name>` (default `"default"`) persists `last_event_id` per workspace in SQLite (`wait_cursors`), so events arriving while the Maestro is between `maestria wait` runs are delivered immediately on the next call without being dropped.
