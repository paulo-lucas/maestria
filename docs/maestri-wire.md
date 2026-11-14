# Maestri Wire: notes for Maestria (summary of the public docs at https://www.themaestri.app/en/docs/wire, read 2026-11-13)

Beta, `protocolVersion` 1, shapes may change. Hosts: Mac and Windows (Linux not mentioned). Enabled in-app: Settings → Wire. No CLI.

## Transport and auth
- HTTPS/WSS on port 7434, self-signed cert, clients pin SHA-256 of SPKI (`serverKeyHash` in the QR, hex in the Manual tab). HTTP/1.1, `Connection: close`.
- `Host` must be an IP, `localhost`, `.local` or `.ts.net`. No CORS. 8 MiB body cap, 20 s request timeout, 64 connections, 1 MiB WS frame.
- Pairing: `POST /pair` `{deviceName, code}` (6-digit code, 5 min, Pairing tab open) or `{deviceName, password}`, plus optional `deviceIdentifier`. Returns `{token, deviceId, role}`. Throttled.
- Token: 64 hex, `Authorization: Bearer`, `?token=` only on GET/WS. Expires after 30 days idle.
- Roles: `owner` (full control) and `guest` (read-only). Pairing gives owner by default; demote with `PUT /api/devices/{id}/role`. A leaked token can do anything its role allows.
- Gate on `GET /api/info` `capabilities`, never probe routes.

## What matters for Maestria
- **Workspace view (#4):** `GET /api/workspaces`, `GET /api/workspaces/{ws}/feed?floor=`, `WS /api/feed/stream?ws=&floor=`. A `TerminalCard` has `name, agentType, status, preview, needsAttention, isActive, isRunning, roleName, accountName, maestroRequest`. Item kinds: `terminal`, `pendingPrompt` (Y/n pending), `note`. `FloorMeta` has `status{state working|blocked|review|done, text, progress}`, `branch`, `attentionCount`, `hasActivity`. `WorkspaceMeta` has counts and `attentionCount`. Snapshots arrive at most every 750 ms. Caveat: an owner subscribing to a feed wakes its terminals, a guest subscription wakes nothing.
- **Chat mirror (#7):** `GET /api/terminals/{id}/chat`, `WS …/chat/stream` (snapshots; never wakes the terminal), `…/chat/threads`, `…/chat/history`. **Send** with `POST /api/terminals/{id}/prompt` `{text}` or `{segments}` (works in chat mode, owner only; don't auto-retry ambiguous sends). Attachments: `POST …/attachments` then `attachmentId`.
- **Approvals:** `POST /api/terminals/{id}/approve|reject` (Y/n prompts), Computer Use approvals socket and answer route (owner).
- **Widgets / portals (#3):** `GET /api/portals/{id}/snapshot` (JPEG, polled with ETag), `POST …/navigate`.
- **Virtual office (#5):** `CanvasSnapshot` (nodes, connections, groups, positions), floors, presence messages.
- Also: notes read/write with `ifRevision`, floor status `POST …/floors/{id}/status`, routines, file trees + named git actions, floor land/delete, terminal create/kill. Most of these are owner only and out of scope for Maestria.

## Chat threads (verified against a real Maestri on 2026-11-14, guest token, GET only)

A terminal's chat keeps **seven threads, one per colour**, slots `0..6` in the fixed order blue, purple, pink, red, orange, yellow, green (the docs also list `graphite`; a real host returned seven items). Capabilities: `terminalChat` (snapshot, socket, history, thread routes) and `chatThreadPreviews` (`GET …/chat/threads`).

- **`GET /api/terminals/{id}/chat`** (and each frame of `WS …/chat/stream`, `{"type":"chat","snapshot":…}`) returns `terminalId, conversationId, enabled, status, entries[], unreadCount, canClear, hasOlderOnRecord, showsShellWindow, turnInterrupted, appearance{…}` and:
  `threads: {activeSlot: 0..6, canSelect: bool, items: [{id: 0..6, color: "blue"…, hasMessages: bool}]}`.
  `entries` are always those of the **selected** (active) thread, the one chosen in the Maestri app. `threads` is absent on a host without thread support (single conversation: show no thread controls). Items carry no unread count and no preview: only `unreadCount`, and it belongs to the selected thread.
- **`GET /api/terminals/{id}/chat/threads`** (guest, read-only; selects nothing, marks nothing read, works while switching is blocked) returns `{threads: [{id: slot, conversationId, entries: [ChatEntry]}]}`: **every thread other than the selected one that holds messages**, newest 40 entries each, oldest first like a snapshot. A slot that is missing is empty; the selected thread is never listed (read it from the snapshot). On a real host with only the selected thread in use it answers `{"threads": []}`.
- **`GET /api/terminals/{id}/chat/history`** returns `{conversationId, entries[]}` for the selected thread only (the whole retained transcript; the snapshot tail holds the newest 80). `POST …/chat/history` pages the selected conversation backwards and answers on the socket. Neither takes a slot, so they cannot read another thread. Maestria does not use them.
- `ChatEntry`: `id, role (user|agent), kind (message|progress|final|divider), text, segments[], ts` (ISO 8601 string, e.g. `2026-11-13T11:29:01Z`), plus `authorName` and `agentType` on agent entries. A `divider` has empty text.
- **Reading a thread without switching the app**: use the snapshot for the selected thread and `…/chat/threads` for the others. Never `PUT …/chat/thread {slot}` (it changes the active thread in the app, owner only) or `DELETE …/chat/thread` (clears a slot); Maestria's allowlist refuses both.
- **Sending**: `POST …/prompt` has no thread or slot field and "prompts all resolve against the selected thread". So Maestria can only send to the thread that is selected in Maestri; on any other thread the composer is disabled with an explanation.
- A switch changes `conversationId`: replace the transcript, do not merge.

## Design implications
- Maestria would become a Wire client running on the same host (`https://localhost:7434`, pinned key). Keep the V1 CLI path as a fallback when Wire is off or the OS is unsupported.
- Security: an owner token is far more powerful than V1's "append text to an event stream". Proposal: one **guest** token for every read, and an owner token only if the human opts in to sending. The server would use it for an allowlist of routes only (prompt to the workspace's Maestro terminal, approve/reject). Both tokens go in the config dir with mode 0600, never in the browser. Audit every Wire write.
- Push relay is not documented enough to use.

## Implementation notes (Maestria v2, `internal/wire`)

Read from the public docs on 2026-11-14. Wire is not enabled on the development machine yet, so everything below is verified against a fake host (`internal/wire/wiretest`, `tools/wirefake`) only. Check these against a real Maestri first:

- **Tokens.** `maestria wire pair` pairs twice (device identifiers `maestria-owner` and `maestria-guest`, so a re-pair replaces the records). Wire hands out owner by default, so the second device is demoted to guest with the first device's authority (`PUT /api/devices/{id}/role`, setup scope only); both roles are then verified with `GET /api/info`. A six-digit code is single use, so a code pairing asks for two codes; a password pairing reuses the password. Tokens live in `wire.json` (0600) in the config dir.
- **Allowlist** (`internal/wire/allow.go`, one place, every request goes through it): guest: `GET /api/info`, `/api/workspaces`, `/api/workspaces/{id}/feed`, `WS /api/feed/stream`, `/api/terminals/{id}/chat`, `WS .../chat/stream`; owner: `POST /api/terminals/{id}/prompt|approve|reject`, `WS /api/terminals/{id}/stream` (one frame: `inputBytes` carrying a lone Esc), `GET /api/info`; either token: `DELETE /api/devices/{own id}`. The owner token never reads feeds (an owner subscription wakes terminals).
- **Capability gating**: chat and prompts need `terminalChat`, the feed `feedSnapshots`, interrupts `terminalStreaming` + `terminalInputBytes`, Y/n answers `feedSnapshots`. The docs name no capability for approve/reject; `feedSnapshots` is a guess (pending prompts only exist in the feed).
- **Guesses to verify**: `GET /api/info` with an unknown token answers 200 without `role` (the client treats a missing role as a revoked token); the terminal `status` values (not enumerated: `ClassifyAgent` falls back to `isActive`); the chat entry `ts` type (read as ISO string, ms, seconds or Apple reference seconds); that Esc through `inputBytes` interrupts an agent; how a floor leaving Maestri is told apart: landed when its branch tip is an ancestor of the workspace's default branch (`origin/HEAD`, else `main`, else `master`, else the checked-out branch; read-only git with a timeout, run in the workspace path; a fast-forwarded floor that never had commits also reads as landed) or Wire last showed `done` / a merged pull request; cancelled otherwise, including when the branch was deleted.
- **Mapping** of Maestri workspaces to Maestria workspaces: working directory first (needs `workspaceActions`), then name (case-insensitive, only when unambiguous).
