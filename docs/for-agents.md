# For Maestro agents and power users

This page is the working guide to the `maestria` command line, for **Maestro agents** (the AI agent that runs a workspace) and for people who script things. It shows what each command does, with examples. The exact protocol and event formats are in [protocol.md](protocol.md). This page does not repeat them.

If you are a person who just wants the panel, you do not need this page. Start with [Getting started](getting-started.md).

## The idea in one minute

- The Maestro talks to the human **only through `maestria`**. It posts tasks, answers in chat, and waits for replies.
- Commands run **from inside the workspace folder**. Maestria works out the workspace from the current directory. Pass `-w <id|name>` (or `--workspace`) to override it.
- Everything is local: the commands write to the connector's database on the same machine. No network is needed.
- Add `--json` to most commands for machine-readable output.
- A failed command prints `Error: …` and exits with status `1`.

`maestria init` installs a **skill** that teaches Claude Code and Antigravity all of this. To add it by hand, paste this snippet into the Maestro's instructions:

```text
This workspace uses Maestria (`maestria` CLI) to talk to the human:
- Post updates (`maestria post --kind notice "..."`), approvals (`--kind approve`), questions (`--kind input`), and choices (`--kind choice --option A --option B "..."`).
- Reply in chat with `maestria say "..."`.
- Run `maestria wait` in the background to receive human chat messages, inbox answers, and usage standby/resume events; after handling an event, re-run `maestria wait` in the background.
- On a `standby` event, interrupt running teammates (`maestri ask <agent> --raw '\e'`), stop starting work, and re-arm `maestria wait` until a `resume` event arrives, then tell interrupted agents to continue.
```

The skill is at [skills/maestria-maestro/SKILL.md](../skills/maestria-maestro/SKILL.md).

## Before anything: is the workspace registered?

```sh
maestria workspace list
maestria workspace add Atlas /Users/you/code/atlas
maestria workspace remove atlas
```

`maestria init` adds the workspaces it finds. The commands below fail with `no registered workspace covers <folder>` when the folder is not in the list.

## Post to the human

`maestria post` takes a **kind** and a markdown body. It prints `Posted <kind> #<id> to <workspace>`.

| Kind | Use it for | What the human gets |
| --- | --- | --- |
| `notice` | Information | Text and a "Got it" button |
| `approve` | A yes/no decision | Approve and Reject buttons, plus an optional comment |
| `input` | An open question | A text box |
| `choice` | A pick from 2 to 4 options | One button per option |

```sh
maestria post --kind notice "Deployed staging build \`abc123\`."
maestria post --kind approve "Merge PR #42 into \`main\`?"
maestria post --kind input "Which domain should we bind for staging?"
maestria post --kind choice --option "PostgreSQL" --option "SQLite" "Which database for this service?"
```

The short form works too: `maestria post approve "Merge PR #42?"`.

Useful flags:

- `--by <agent>`: show which agent is asking (default `Maestro`).
- `-w <workspace>`: pick the workspace.
- `--json`: print the created item.

A `choice` needs **between 2 and 4** options, or the command fails with `choice requires 2..4 options`.

Write bodies so that the human can answer **from a phone** without more context: say what, why, and what happens next.

## Read what is open and what was answered

```sh
maestria inbox                    # open items (needs you, updates)
maestria inbox --status history   # answered items
maestria inbox --status all --json
```

```sh
maestria answers                  # answers not yet acknowledged
maestria answers --ack            # same, and mark them acknowledged
maestria answers --all            # everything ever answered
maestria answers --json
```

Example output:

```text
#12 (approve): approved
#13 (choice): SQLite
```

Use `answers --ack` so that you do not act on the same answer twice.

## Talk in chat

```sh
maestria say "All 3 steps passed. Waiting for your approval in Human tasks."
maestria say --by Orin "Tests are green."
```

`say` adds a Maestro message to the workspace chat.

## Wait for the human: `maestria wait`

The Maestro does not poll. It starts `maestria wait` in the **background** and goes on working. The command blocks until something happens, prints the events, and exits.

```sh
maestria wait
```

It prints one line per event: the event number, its type, and its payload as JSON.

```text
[#<id>] <type> <payload>
```

If nothing happens before `--timeout`, it prints `Timed out waiting for events.`

When it exits:

1. Read the events it printed.
2. Act on them. `maestria answers` gives you the replies, and `maestria say` answers a chat message.
3. **Start `maestria wait` again** in the background.

Options:

| Flag | Meaning |
| --- | --- |
| `--cursor <name>` | Use a named position (default `default`). Events that arrive between two runs are delivered by the next run, never lost. |
| `--since <id>` | Start after event `<id>` without using a cursor. |
| `--from-now` | Skip everything that already happened. |
| `--timeout <sec>` | Give up after this long (`30`, or `30s`, or `2m`). Prints `Timed out waiting for events.` and exits `0`. Without it, it waits as long as needed. |
| `--json` | Print `{ workspace, cursor, lastEventId, events, timedOut }`. |

Event types: `chat`, `inbox_response`, `inbox_dismiss`, `standby` and `resume`. The fields of each are in [protocol.md](protocol.md#event-types-maestria-wait).

## Handle usage standby

Maestria watches AI usage. At the limits you set, a `standby` event reaches `maestria wait`.

On **`standby`**:

1. Read which provider and window triggered it (`triggers`).
2. Interrupt every running teammate:

   ```sh
   maestri ask <agent> --raw '\e'
   ```

3. Remember who you interrupted. Do not start or hand out new work.
4. Run `maestria wait` again.

On **`resume`**:

1. Tell each interrupted agent to continue:

   ```sh
   maestri ask <agent> "Usage standby cleared; please continue your task."
   ```

2. Run `maestria wait` again and go on.

If the human chose **Interrupt agents** for a window and Wire is connected, the panel sends Esc to the working agents of that provider itself. You still re-arm `wait` and watch for `resume`.

## Show what each agent is doing

The **Activity** page and the Overview show one line per agent. Keep it current:

```sh
maestria agent brief Vesper "Writing retry tests for webhook signature failures"
maestria agent brief Vesper --clear
echo "Reviewing the diff" | maestria agent brief Vesper -
```

A brief is one line, up to 160 characters. Without one, the panel shows the last readable line of the agent's terminal. Set it when you hand work out and update it when the work changes.

## Track floors

The **Floors** page has three columns: **Future** (planned), **Present** (active) and **Past** (landed or cancelled). The Maestro keeps it current.

```sh
maestria floor plan billing-webhooks --brief "Signature checks and retries for payment webhooks."
maestria floor reorder billing-webhooks --before sso-login
maestria floor start billing-webhooks
maestria floor status billing-webhooks --state review --progress 4/5 --text "Tests green; ready to land" --agents Orin,Vesper
maestria floor land billing-webhooks --outcome "Added webhook signature checks and a retry queue."
maestria floor cancel old-experiment --outcome "Dropped in favor of REST endpoints."
maestria floor list --all
maestria floor edit billing-webhooks --rename billing-hooks --brief "…"
```

- `--ongoing` on `plan` and `start` marks recurring work with no progress bar.
- `--state` is `working`, `blocked`, `review` or `done`. `--progress` takes `0.4`, `2/5` or `40%`. `--text` is up to 120 characters.
- `land --force` is needed for a planned floor that never started.
- `floor status <name>` with no flags just shows the status.

**Order matters when you land.** Maestri forgets a floor once it is deleted. Maestria notices that a floor has gone and records it in **Past** by itself, with an empty outcome. Run `maestria floor land` (or `cancel`) with `--outcome "…"` **before** you land the floor in Maestri to keep the outcome you want. You can also fill it in later with `maestria floor edit`.

## Check that it all works

```sh
maestria status
```

Example:

```text
Workspace: Atlas (atlas)
Tasks:     3 needs you, 0 postponed, 1 updates, 0 history
Standby:   clear
Usage:     3 provider accounts tracked
Server:    running (http://127.0.0.1:4777)
```

If it says `Server: down`, start the panel:

```sh
maestria service start
```

If the environment does not let you manage services, **tell the human the exact command**. After the human updates `maestria`, `maestria service restart` makes the running panel pick up the new version.

## When Maestri Wire is on

Nothing changes for the Maestro: `say`, `post` and `wait` work the same. Wire adds:

- A chat message typed in the panel **arrives in your terminal as a normal prompt**. You get no `chat` event. With Wire off, it is a `chat` event for `maestria wait`.
- `say` messages still go to the local log. The panel chat shows them in the same conversation, with a small "via maestria" badge.
- An agent waiting on a yes/no appears in the human's **Needs you** as an **Agent prompt**. You do not need to post it.

See [Connect to Maestri Wire](wire.md).

## Scripting notes

- Use `--json` and read fields, not text. The text may change.
- Exit status is `0` on success and `1` on any error. A `wait` that times out is a success.
- The commands do not need the panel to be running. They write to the same database it reads.
- `post`, `say`, `wait`, `inbox`, `answers`, `status`, `floor`, `agent` and `devices` also keep working while `maestria serve` or `maestria connect` runs. Only `serve` and `connect` take the run lock.
- Config: `MAESTRIA_CONFIG_DIR` picks another config directory. Use it to try things without touching your real data.

## Command cheat sheet

| Command | Purpose |
| --- | --- |
| `maestria post [--kind notice\|approve\|input\|choice] [--option …] [--by <agent>] <markdown>` | Post a human task |
| `maestria inbox [--status open\|history\|all] [--json]` | List tasks |
| `maestria answers [--all] [--ack] [--json]` | Read answers |
| `maestria say [--by <agent>] <text>` | Chat message |
| `maestria wait [--cursor <name>] [--since <id>] [--from-now] [--timeout <sec>] [--json]` | Block for events |
| `maestria status [--json]` | Counts, standby, usage, panel |
| `maestria agent brief <name> "<text>"` / `--clear` | One-line agent brief |
| `maestria floor plan\|start\|status\|land\|cancel\|list\|edit\|reorder` | Floors |
| `maestria workspace list\|add\|remove` | Registered workspaces |

Full protocol: [protocol.md](protocol.md). For the human-side commands (`init`, `serve`, `service`, `wire`, `connect`, `devices`, `tunnel`, `config`, `auth`), see the other guides, or run `maestria --help`.
