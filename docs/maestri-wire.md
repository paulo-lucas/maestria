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

## Design implications
- Maestria would become a Wire client running on the same host (`https://localhost:7434`, pinned key). Keep the V1 CLI path as a fallback when Wire is off or the OS is unsupported.
- Security: an owner token is far more powerful than V1's "append text to an event stream". Proposal: one **guest** token for every read, and an owner token only if the human opts in to sending. The server would use it for an allowlist of routes only (prompt to the workspace's Maestro terminal, approve/reject). Both tokens go in the config dir with mode 0600, never in the browser. Audit every Wire write.
- Push relay is not documented enough to use.
