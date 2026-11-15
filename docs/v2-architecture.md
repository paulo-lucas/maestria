# Maestria v2: architecture and build plan

Companion to `docs/v2-spec.md` (product) and `design/` (approved UI). This file says how v2 is built.

## Principles

- **One Go codebase, one `maestria` binary** per OS: `serve` (local mode), `connect` (hosted mode), plus the Maestro CLI (`post`, `inbox`, `answers`, `say`, `wait`, `status`) (see `docs/protocol.md`).
- **The connector owns data and security.** Workspace data (human tasks, events, chat, floor plans and history, audit, standby state) lives in the connector's SQLite in the config dir. The hosted system stores only accounts, 2FA, linked connectors and paired devices, and relays live traffic.
- **Live mirror.** No machine online means no data in the panel. Nothing is queued for offline machines.
- **One API, two transports.** The connector exposes one JSON API + event stream. In local mode the browser calls it directly over HTTP on 127.0.0.1. In hosted mode the browser calls the cloud, which relays each request as a signed envelope over the connector's outbound WebSocket. The frontend is the same.
- Security model from `AGENTS.md` still applies to local mode (127.0.0.1 only, argon2id + TOTP, CSRF, strict CSP, no CORS, audit, kill switch). Tests for every change to auth, sessions, CSRF, device signing, the allowlist or the kill switch.

## Stack

- Go (latest stable; `go.mod` pins it, `GOTOOLCHAIN=auto` fetches it). Stdlib `net/http` (1.22+ routing), `log/slog`, `embed`.
- Pinned dependencies, few and well known:
  - `modernc.org/sqlite`: pure Go, no CGO, trivial cross-compile
  - `golang.org/x/crypto`: argon2id
  - `github.com/coder/websocket`
  - `github.com/yuin/goldmark` + `github.com/microcosm-cc/bluemonday`: sanitized Markdown
  - a small QR encoder for terminal and PNG QR codes
- Frontend: vanilla JS + the CSS from `design/` (tokens, components, shell), embedded with `embed.FS`. No build step beyond copying. Strict CSP: no inline script or style.

## Layout

```
cmd/maestria/            main: CLI dispatch
internal/config          config dir, config.json, maestriDir resolution (config > MAESTRI_HOME > ~/.maestri)
internal/store           SQLite open, migrations
internal/auth            argon2id, TOTP + recovery codes, sessions, CSRF, rate limit/lockout
internal/audit           append-only audit log
internal/web             HTTP server, security headers/CSP, JSON API, SSE/WS event stream, embedded UI
internal/ui              embedded static assets (from design/)
internal/workspaces      workspace registry (Maestri workspaces discovery + config)
internal/inbox           human tasks (notice/approve/input/choice + postpone), history
internal/events          per-workspace event log + wait cursors
internal/usage           read-only reader of <maestriDir>/usage/providers/.status.json, grouping by provider account
internal/standby         thresholds per provider/window, Tell the Maestro / Interrupt agents, wake on reset
internal/insights        usage samples + 1-7 rating of delivery, quality, usage that ships and response time
internal/wire            Maestri Wire client: pairing, SPKI-pinned TLS, REST, feed/chat sockets, capability gating
internal/floors          floor plans (future), live floors (Wire), history (landed/cancelled) + Maestro CLI
internal/docs            read-only mirror of the project's docs/ folder (path-safe, size-limited)
internal/relay           connector <-> cloud protocol (outbound WSS, envelopes, signatures, allowlist)
internal/devices         device pairing and keys, per-machine approval, session policy
internal/service         launchd / systemd --user / Windows service install
cmd/maestria-cloud/      hosted multi-tenant server (accounts, email, 2FA, linking, relay fan-out)
```

## Hosted mode protocol (summary; detailed in the cloud milestone)

- **Linking:** a one-time code links a connector to an account. The connector then holds a long-lived connector credential (0600 / OS keychain) and dials `wss://<cloud>/connector`.
- **Device pairing (per machine):** the browser creates a non-extractable WebCrypto key pair (ECDSA P-256), stored in IndexedDB. The cloud forwards the public key and a short comparison code to every online connector. The user approves it locally on each machine (`maestria devices approve <code>`, or a local prompt or notification).
- **Requests:** the browser signs `{connectorId, method, path, bodyHash, ts, nonce, sessionId}`. The connector verifies the device key, the timestamp window and nonce replay, the session policy (idle/absolute limits set on the connector) and the route allowlist, then executes. The cloud can only relay. It cannot forge commands.
- **Threat note:** the cloud serves the frontend JS, so a compromised cloud could abuse a page that is already open. The real guards stay on the connector: allowlist, rate limits, short sessions, local kill switch, local audit, and optional local confirmation for sensitive actions.
- **Live events:** the connector pushes event deltas to the cloud, which fans them out to that account's open browser sockets and stores nothing.

## Build order (floors; no numbers)

| Floor | Scope | Agent |
| --- | --- | --- |
| `v2-foundation` | Go module, config, store + migrations, CLI skeleton, auth (password, TOTP, recovery codes), sessions with configurable policy, CSRF, CSP/security headers, rate limit, audit, `serve` with the design shell and login screens, kill switch. Security tests for each of these. | Claude (security-critical) |
| `v2-connector-core` | Workspaces, inbox, events/`wait`, `post/say/inbox/answers/status`, usage reader, standby guard, Overview/Tasks/Usage UI wired to the API | Antigravity |
| `v2-wire` | Wire client (pairing, pinning, feed, chat, prompt, approve/reject), Activity + Chat UI, live floors | Antigravity, Claude review |
| `v2-floors-docs` | Floors plan/history CLI + UI, docs mirror + UI | Antigravity |
| `v2-cloud` | `maestria-cloud`, linking, relay, device pairing + signing, hosted auth (email, 2FA, reset), Connectors and Devices UI | Claude (security-critical) |
| `v2-ship` | Service install (macOS, Windows, Linux), release builds, checksums, Homebrew, README | Antigravity |

Each floor is a git-isolated branch that lands on `main`.
## Prerequisites on the owner's side

- Maestri Wire must be enabled (Settings → Wire) before `v2-wire`. It is off today.
