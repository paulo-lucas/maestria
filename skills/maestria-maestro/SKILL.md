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
2. Act on them (`maestria answers` for inbox replies, `maestria say` for chat replies).
3. If `standby`, pause active work cleanly until a `resume` event arrives.
4. Start `maestria wait` in the background again.
