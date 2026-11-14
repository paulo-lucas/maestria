---
name: maestria
description: Communicate with the human via the Maestria control panel (post inbox notices, approvals, questions, and choices; reply in workspace chat; wait for human replies and usage standby events).
---

# Maestria — Maestro Protocol

Maestria connects this workspace to the human through a self-hosted web panel. Always run commands from inside the workspace directory (or pass `--workspace <id>`).

## Commands

### 1. Post to the Human's Inbox

- Informational notice ("Got it" dismiss button):
  ```sh
  maestria post --kind notice "Deployed staging build \`abc123\`."
  ```
- Approval request (`Approve` / `Reject` buttons + optional comment):
  ```sh
  maestria post --kind approve "Merge PR #42 into \`main\`?"
  ```
- Open-ended question (`input` text box):
  ```sh
  maestria post --kind input "Which domain should we bind for staging?"
  ```
- Multiple-choice decision (2–4 buttons):
  ```sh
  maestria post --kind choice --option "PostgreSQL" --option "SQLite" "Which database for this service?"
  ```

### 2. Inspect Inbox & Answers

```sh
maestria inbox
maestria answers
maestria answers --json
```

### 3. Reply in Workspace Chat

```sh
maestria say "Status update: all 3 steps passed; waiting on your approval in Inbox."
```

### 4. Wait for Events (Background Listener)

Run `maestria wait` in the background so you wake automatically when the human sends a chat message, answers/dismisses an inbox item, or when the usage standby guard triggers/resumes:

```sh
maestria wait
```

When `maestria wait` exits:
1. Read its printed event(s) (`chat`, `inbox_response`, `inbox_dismiss`, `standby`, or `resume`).
2. Act on them (`maestria answers` for inbox replies, `maestria say` for chat replies; see **Standby** below for `standby` / `resume`).
3. Start `maestria wait` in the background again.

### 4b. Keep Agent Briefs Current

The panel's **Activity** page (and the Overview "Agents at work" list) shows one line per agent. Set it when you give a teammate work and whenever it changes, so the human sees what each agent is doing at a glance:

```sh
maestria agent brief Vesper "Writing retry tests for webhook signature failures"
maestria agent brief Vesper --clear
```

Briefs are one line, up to 160 characters. Without one, the panel shows the last readable line of the agent's terminal. With Maestri Wire connected, chat messages from the human arrive in your terminal as prompts, and agent Y/n prompts reach the human on their own.

### 5. Track Floors (Future / Present / Past)

Keep the panel's **Floors** page current as you plan, start, update, and finish floors:

```sh
maestria floor plan billing-webhooks --brief "Signature checks and retries for payment webhooks."
maestria floor reorder billing-webhooks --before sso-login
maestria floor start billing-webhooks
maestria floor status billing-webhooks --state review --progress 4/5 --text "Tests green; ready to land" --agents Orin,Vesper
maestria floor land billing-webhooks --outcome "Added webhook signature checks and retry queue."
maestria floor cancel old-experiment --outcome "Dropped in favor of REST endpoints."
maestria floor list --all
```

Use `--ongoing` on `plan` / `start` for recurring work with no progress bar, and `--force` on `land` if landing a planned floor that was never started.

## Standby

Maestria monitors the provider configured for this workspace (`maestroProvider` in `workspaces.json`, default `claude`) and emits `standby` / `resume` events. Each usage window has an action the human picks in the panel: **Tell the Maestro** (the default: only the event below, you own your team) or **Interrupt agents** (when Maestri Wire is paired, the panel itself sends Esc to that provider's working agents; the event still arrives so you stop assigning work). With wake-on-reset on, the panel also prompts you in your terminal when the window resets.

When `maestria wait` delivers a **`standby`** event:
1. Check which provider/window triggered it from the event output (`triggers`: provider, window, `%` vs threshold, and reset time).
2. Immediately interrupt every currently running teammate agent:
   ```sh
   maestri ask <agent> --raw '\e'
   ```
   Remember which agents were running so you can resume them when standby clears.
3. Stop starting or assigning new work until a `resume` event arrives.
4. Re-arm `maestria wait` in the background (it wakes automatically when the triggering window resets or usage drops below `resumeBelowPercent`).

When `maestria wait` delivers a **`resume`** event:
1. Tell each previously interrupted teammate agent to continue its task:
   ```sh
   maestri ask <agent> "Usage standby cleared; please continue your task."
   ```
2. Re-arm `maestria wait` in the background and resume orchestrating work normally.

## Server

- Check panel status with `maestria status`.
- If `maestria status` says the panel is down, start it with:
  ```sh
  maestria service start
  ```
  If your environment does not permit managing user services, tell the human the exact command to run: `maestria service start`.
- After updating `maestria`, restart the background service so the running panel picks up the new version:
  ```sh
  maestria service restart
  ```
