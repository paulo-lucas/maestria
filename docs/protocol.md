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
- On a `standby` event, interrupt running teammates (`maestri ask <agent> --raw '\e'`), stop starting work, and re-arm `maestria wait` until a `resume` event arrives, then tell interrupted agents to continue.
```

## CLI Reference

| Command | Purpose |
| --- | --- |
| `maestria post --kind notice\|approve\|input\|choice [--option ...] [--by <agent>] <markdown>` | Post an Inbox item for the human. |
| `maestria inbox [--status open\|history\|all] [--json]` | List workspace Inbox items. |
| `maestria answers [--all] [--json]` | Read resolved Inbox answers/decisions (human-readable or JSON). |
| `maestria say [--by <agent>] <text>` | Append a Maestro reply to the workspace Chat. |
| `maestria wait [--cursor <name>] [--since <id>] [--from-now] [--timeout <sec>] [--json]` | Block until new events arrive for the workspace, advance the named SQLite cursor, print events, and exit. |
| `maestria status [--json]` | Show workspace counts (`needsYou`, `updates`, `history`) and panel server reachability. |

## Agent briefs

| Command | Purpose |
| --- | --- |
| `maestria agent brief <name> "<text>" [-w <workspace>]` | Set the one-line brief (up to 160 characters) the Activity page and the Overview "Agents at work" panel show for an agent. `-` reads the text from stdin. |
| `maestria agent brief <name> --clear` | Remove it; the panel falls back to the last readable line of the agent's terminal. |

Keep briefs current: when you hand work to a teammate, set what it is doing (`maestria agent brief Vesper "Writing retry tests for webhook signature failures"`), and update it when that changes.

## Maestri Wire (live agents, chat, floors)

When the human has paired the connector with Maestri Wire (`maestria wire pair`), the panel mirrors your terminal, your teammates, and the floors live, and its Chat sends straight into your terminal. Nothing changes for you: `say`, `post` and `wait` work the same, whether Wire is on or off. What differs:

- A message the human types in the panel Chat arrives in your terminal as a normal prompt (no `chat` event) while Wire is live; with Wire off it is a `chat` event for `maestria wait`, as before.
- Y/n prompts an agent waits on appear in the human's Needs you as "Agent prompt" and can be answered from the panel. You do not need to post them.
- A floor that disappears from Maestri (landed or deleted) is recorded in Past automatically, with an empty outcome. Run `maestria floor land|cancel <name> --outcome "..."` before you land it in Maestri to keep the outcome you want (it is not recorded twice), or fill it in later with `maestria floor edit`.
- On a `standby` event whose action is "Interrupt agents", the panel sends Esc to the working agents of that provider itself. On `resume` after a reset, with wake-on-reset on, it prompts you in your terminal.

Human-side commands: `maestria wire pair`, `maestria wire status`, `maestria wire unpair`.

## Floors

Floors track planned (**Future**), active (**Present**), and landed or cancelled (**Past**) work in the workspace. Every `maestria floor` command infers the workspace from the current working directory (`-w` / `--workspace` overrides it) and supports `--json`.

| Command | Purpose |
| --- | --- |
| `maestria floor plan <name> --brief <md> [--ongoing]` | Add a Future floor in planned order (`--ongoing` marks recurring/indefinite work with no progress bar). |
| `maestria floor start <name> [--brief <md>] [--ongoing]` | Move a planned floor to Present (or create and start one directly when `--brief` is passed). |
| `maestria floor status <name> [--state working\|blocked\|review\|done] [--progress 0.4\|2/5\|40%] [--text <msg>] [--agents a,b]` | Update a Present floor's status, progress, status text (≤120 chars), or agents; omit flags to inspect it. |
| `maestria floor land <name> [--outcome <md>] [--force]` | Record a floor as Landed in Past. Planned floors that never started require `--force`. |
| `maestria floor cancel <name> [--outcome <md>]` | Record a planned or active floor as Cancelled in Past. |
| `maestria floor list [--all]` | List Present and Future floors (pass `--all` to include Past). |
| `maestria floor edit <name> [--brief <md>] [--rename <new>] [--ongoing[=true\|false]]` | Edit a planned or active floor's brief, name, or ongoing flag. |
| `maestria floor reorder <name> --before\|--after <other>` | Move a Future floor before or after another Future floor. |

## Event Types (`maestria wait`)

Each event row belongs to a single workspace and carries an auto-incrementing `id`, `type`, `payload`, and `createdAt` (epoch ms):

- `chat`: `{ messageId, author, body, createdAt }` — emitted when the human sends a Chat message in the panel.
- `inbox_response`: `{ itemId, kind, decision, response }` — emitted when the human responds to an `approve`, `input`, or `choice` item.
- `inbox_dismiss`: `{ itemId, kind, decision }` — emitted when the human clicks "Got it" on a `notice` or dismisses an open item.
- `standby`: `{ provider, message, triggers }` — emitted for a workspace when usage on its configured `maestroProvider` (default `claude`) reaches a standby threshold; `triggers` lists each `{ providerId, providerName, windowId, windowTitle, usedPercent, thresholdPercent, resetsAt }`.
- `resume`: `{ provider, reason, message }` — emitted once the triggering window resets or usage drops below `resumeBelowPercent`.

Using `--cursor <name>` (default `"default"`) persists `last_event_id` per workspace in SQLite (`wait_cursors`), so events arriving while the Maestro is between `maestria wait` runs are delivered immediately on the next call without being dropped.
