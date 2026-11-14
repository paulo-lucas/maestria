# Maestria v2: product spec

Status: agreed direction, design phase starting. V1 (the local panel in this repo) stays in use until v2 replaces it.

## Architecture

- **Connector** (Go, one binary per OS: macOS and Windows at least, the hosts Maestri Wire supports). It runs on the user's machine. It talks to Maestri over **Maestri Wire** on localhost (pinned cert) and to **the Maestria system** over an outbound WSS connection: no open port, no tunnel.
  - Holds the Wire tokens locally (0600 file or OS keychain). Pairing with Wire is local. It reads with a guest token, and an owner token is used only for allowlisted actions.
  - Executes only allowlisted high-level commands from the system: prompt a workspace's Maestro, approve/reject a Y/n, pause/resume. Never arbitrary Wire routes (no git push, kill, floor delete...).
  - Local responsibilities that keep working even if the cloud is down: reading `<maestriDir>/usage/providers/.status.json` (read-only), the standby guard (pause on limit, wake the Maestro on reset), reading the project `docs/` folder, local audit log, local kill switch.
  - Serves the Maestro-side protocol locally (`maestria post|say|wait|...`, same CLI contract as V1) and relays it to the system.
- **Maestria system** (Go monolith, web). Accounts, web panel, storage. The user creates an account, links it to their connector (one-time pairing code), and sees their workspaces from the web.
- Security (hosted): email + password (argon2id) + **mandatory 2FA** (TOTP + recovery codes), no Cloudflare Access. Rate limiting/lockout, secure sessions, CSRF, strict CSP, audit log. Email verification and a password reset that never bypasses 2FA.
- Threat-model note: the system can prompt the Maestro, and the Maestro runs commands, so a compromised system is dangerous. Signing commands in the browser doesn't help fully because the system serves the JS. So the real guard is connector-side policy: allowlist, rate limits, optional local confirmation for sensitive actions, local kill switch, local audit.
- Open source (MIT). One `maestria` binary: `maestria connect` (hosted system) or `maestria serve` (local mode, connector + panel in one process on 127.0.0.1, like V1). The hosted multi-tenant server is a separate build from the same repo.

## Views

- **Overview**: all workspaces aggregated, built for monitoring several at once (compact cards, workspace color tags, filters).
- **Workspace**: one workspace in detail.

## Modules

| Module | Overview (all workspaces) | Workspace view | Source |
| --- | --- | --- | --- |
| Usage | Yes (global, per provider/window) | Usage + standby config | Connector reads Maestri usage providers |
| Human tasks | Yes, all of them (workspace tag) | Yes | Maestro via `maestria post` |
| Agent activity | Yes: count working + one line each | Full list per floor, short brief | Wire feed (status, attention, Y/n) + brief |
| Chat with Maestro | No | Yes | Wire chat stream + prompt |
| Floors (roadmap) | Compact: active floors and progress (proposal) | Past / present / future | Wire floors + Maestro-managed plan |
| Docs | No | Yes | Project `docs/` folder (README.md + subfolders) |

### Usage
- Gauges per provider/window, reset times, errors hidden or flagged.
- Standby guard configurable in the panel: session limit threshold per provider/window (e.g. pause at N%). On limit, pause activity (tell the Maestro / interrupt agents). On reset, **wake the Maestro** (Wire prompt). Enforced by the connector.

### Human tasks
- Keep the V1 format: approve / reject / postpone / input / choice, Needs you + Updates + History.

### Agent activity
- Per agent: name, floor, state (working / idle / needs attention / waiting on Y/n), short brief of what it is doing.
- Brief: the Maestro sets it via CLI (e.g. `maestria agent <name> "brief"`), falling back to Wire's status/preview (last terminal lines) as an expandable detail.

### Chat
- Per workspace, with the workspace's Maestro, through Wire (real send). Not shown in the overview.

### Floors (replaces the numbered roadmap/sprints)
- No numbers. Each floor has a name and a **brief** (its purpose).
- **Past**: delivered and decommissioned (landed/deleted). Collapsed by default.
- **Present**: active floors (live from Wire), the agents working on each, progress when the floor has one (Wire floor status progress 0–1, working/blocked/review/done), no progress bar for indefinite/recurring floors.
- **Future**: planned features not started yet.
- Storage: future and past floors are structured data in the system, managed by the Maestro via CLI (`maestria floor plan|start|done ...`). Maestri forgets a floor once it is deleted, so the connector records history when a floor lands or is cancelled (deleted without landing), with brief and outcome.

### Docs
- Project knowledge (what the project is, decisions), not planning. Mirrors the real `docs/` folder on the user's machine: main README.md + subfolders. Read-only in the panel and managed by the Maestro; the user asks the Maestro for changes.
- Note: hosted mode means docs, chats and agent previews leave the machine. Encrypt at rest, and say so clearly in onboarding.

## Order

1. Design: visual base, responsive/mobile-first, screens for overview + workspace + every module + onboarding (account, 2FA, connector pairing).
2. Build: connector + system in Go (V1 CLI contract kept).
3. Hardening, publish (signed binaries for macOS/Windows).

## Decisions (2026-11-13)

- All security lives in the connector, on the user's machine. The connector is the authority. The user configures policies there, including login session duration (and the usage pause limit). Each browser/phone is paired once with the connector (approved locally) and gets a device key. The connector accepts only commands signed by a paired device within the session limit. The website login (email + password + 2FA) is the first layer.
- 100% open source (MIT). **One binary**, `maestria`, with two modes: `maestria connect` (hosted system) and `maestria serve` (local panel on 127.0.0.1, V1-style). Size cost is small. The hosted multi-tenant server is a separate build from the same repo that only we run.
- Floor history is recorded on landing AND on cancel (floor deleted without landing), with brief and outcome.
- Agent brief: written by the Maestro via CLI. Wire `preview` (last terminal lines) is a fallback/expandable detail. Present-floor progress comes from Maestri's native floor status (state + text + progress 0–1).

## Decisions (design review, 2026-11-13)

- **Live mirror.** The panel mirrors Maestri running on the user's machines. An offline machine's workspaces disappear from every view. With no machine online, the panel shows no data, only a "No machine online" state. The Connectors page still lists offline machines (last seen, revoke). Nothing is queued for offline machines.
- Consequence: workspace data (human tasks, floor history, chat, docs, usage) lives on the connector (local SQLite), not in the hosted system. The hosted system stores only accounts, 2FA, linked connectors and paired devices, and relays live traffic. That's simpler and much better for privacy.
- Usage is grouped by provider account. A machine switch appears only when machines use different accounts for the same provider.
- Devices are paired on each machine, since each connector is the authority.
- Standby on limit: per window, "Tell the Maestro" (default) or "Interrupt agents".
- Wire Y/n prompts appear in Needs you as "Agent prompt".
