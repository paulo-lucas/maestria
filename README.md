# Maestria

Self-hosted control panel and Maestro protocol add-on for [Maestri](https://github.com/maestria-dev/maestria) workspaces — built in Go as a single binary with an embedded web UI, SQLite state, and optional multi-machine pairing.

![Maestria Overview](docs/images/overview.png)

## Why Maestria?

When several Maestri agents run across workspaces, floors, and machines, Maestria gives you one place to see what needs you and steer work from a desktop or phone:

- **Overview & Human Tasks (`Needs you`)**: Approve prompts, answer Maestro questions, pick between options, and track updates across workspaces.
- **Live Activity & Chat**: Watch active agents, view live terminal previews, and chat directly with Maestro via Maestri Wire.
- **Floors (Present, Future, Past)**: Inspect worktrees, blockers, and queued floors planned by Maestro.
- **Usage & Standby Guard**: Monitor provider quotas read-only and pause work automatically before hitting session or weekly limits, waking Maestro on reset.
- **Security by Default**: Loopback-only HTTP server (`127.0.0.1:4777`), argon2id password + TOTP 2FA, CSRF protection, strict CSP, audit logging, and a host-enforced kill switch (`maestria tunnel off`) for Cloudflare Tunnel deployments.

| Activity | Human tasks |
| --- | --- |
| ![Workspace Activity](docs/images/workspace-activity.png) | ![Workspace Human Tasks](docs/images/workspace-tasks.png) |

| Floors | Usage & Standby |
| --- | --- |
| ![Workspace Floors](docs/images/workspace-floors.png) | ![Usage and Standby](docs/images/usage.png) |

---

## Install

### macOS and Linux (`install.sh`)

Installs `maestria` to `~/.local/bin` and verifies its SHA-256 checksum against `SHA256SUMS`:

```sh
curl -fsSL https://raw.githubusercontent.com/maestria-dev/maestria/main/install.sh | sh
```

Optional environment variables:

- `MAESTRIA_VERSION` — pin a release tag (defaults to `latest`)
- `MAESTRIA_INSTALL_DIR` — install destination (defaults to `~/.local/bin`)

### Windows PowerShell (`install.ps1`)

Installs `maestria.exe` to `%LOCALAPPDATA%\Programs\Maestria` (and adds it to your user `Path`), verifying its SHA-256 checksum:

```powershell
irm https://raw.githubusercontent.com/maestria-dev/maestria/main/install.ps1 | iex
```

### Homebrew (macOS and Linux)

Each tagged release generates a `maestria.rb` formula in the GitHub release assets from [packaging/homebrew/maestria.rb](packaging/homebrew/maestria.rb). Install from a tap or directly from the released formula:

```sh
brew install maestria-dev/tap/maestria
```

### From Source (Go 1.27+)

```sh
go install github.com/maestria-dev/maestria/cmd/maestria@latest
```

Verify installation:

```sh
maestria version
```

---

## Quick Start (`maestria init`)

Run the setup wizard in your terminal:

```sh
maestria init
```

`maestria init` is idempotent and walks through:

1. **Maestri data directory & workspace discovery** — checks `MAESTRI_HOME` or `~/.maestri` (read-only on usage data) and lets you pick which workspaces to register.
2. **Optional V1 import** — detects an existing V1 SQLite database and offers a read-only import of inbox items, chat, and events (`maestria import-v1`).
3. **Authentication setup** — sets your admin password, enrols a TOTP authenticator app, and prints one-time recovery codes (`maestria auth setup`).
4. **Maestri Wire pairing** — optionally pairs with Maestri Wire so Maestria streams live agent status, chat, and floors (`maestria wire pair`).
5. **Standby guard defaults** — optionally configures pause thresholds for provider usage.
6. **Maestro skill install** — installs the embedded `maestria-maestro` skill into Claude (`~/.claude/skills`) and/or Antigravity (`~/.gemini/config/skills`).
7. **Background service** — optionally installs (and starts) the OS user service.

### Non-Interactive Setup

For scripts or headless machines:

```sh
maestria init \
  --maestri-dir ~/.maestri \
  --workspaces all \
  --standby default \
  --skill both \
  --service install \
  --skip-auth \
  --skip-wire \
  --yes
```

Run `maestria init --help` for all flags. Config and SQLite state live in `$MAESTRIA_CONFIG_DIR`, `%AppData%\maestria` (Windows), or `~/.config/maestria` (with `0600` secret permissions).

---

## Background Service (`maestria service`)

Manage Maestria as an OS user service across macOS (`launchd`), Linux (`systemd --user`), and Windows (Task Scheduler):

```sh
maestria service install [--mode serve|connect]
maestria service status
maestria service start
maestria service stop
maestria service restart
maestria service logs [-n 100]
maestria service uninstall
```

- `--mode serve` (default) runs `maestria serve` for local/tunnel hosting.
- `--mode connect` runs a connector process for multi-machine setups.
- Logs are stored in `<configDir>/logs/`.

---

## Local Mode, Cloudflare Tunnel & Kill Switch

By default, `maestria serve` listens exclusively on **`127.0.0.1:4777`** and refuses to expose an unencrypted LAN or public port.

To reach your panel securely from a phone or away from your desk:

1. Point a **Cloudflare Tunnel** (`cloudflared`) at `http://127.0.0.1:4777`.
2. Optionally put **Cloudflare Access** in front of that hostname and configure `cloudflareAccess` (`teamDomain` and `audience`) in `config.json` so Maestria verifies the `Cf-Access-Jwt-Assertion` header on every request.
3. Use the **kill switch** whenever you want to cut off external access immediately:
   - From the panel UI or CLI (`maestria tunnel off`), non-loopback requests are blocked with `503`.
   - Re-enabling tunnel access (`maestria tunnel on`) is **only allowed locally** over CLI or `127.0.0.1` — never from a remote session.
   - Check state anytime with `maestria tunnel status`.

---

## Maestri Wire Pairing

Connect Maestria to a running Maestri instance on the same machine or over Maestri Wire for live agent activity, chat, and floor state:

```sh
maestria wire pair
maestria wire status
maestria wire unpair
```

During setup, `maestria init` also offers to pair right away.

---

## Maestro Protocol & Agent Integration

Agents and Maestros communicate with the panel through the `maestria` CLI (`post`, `inbox`, `answers`, `say`, `wait`, `status`, `floor`, `agent brief`) backed by a clear text/JSON protocol:

- Protocol specification: [docs/protocol.md](docs/protocol.md)
- Installed agent skill: [skills/maestria-maestro/SKILL.md](skills/maestria-maestro/SKILL.md) (embedded directly into the Go binary so `maestria init` always installs the matching skill)

---

## Hosted Mode (Coming Soon)

A managed hosted relay (`maestria-cloud`) for connecting multiple machines without self-managing a Cloudflare Tunnel is planned for a future release (**coming soon**). Self-hosted local mode (`127.0.0.1` + optional Cloudflare Tunnel/Access) is supported today.

---

## Security Model

Maestria is designed for single-owner remote control of local developer machines:

- **Loopback bind only**: `127.0.0.1` is fixed; TLS/edge termination belongs to Cloudflare Tunnel or localhost.
- **No CORS, strict CSP**: cross-origin requests are rejected and inline script execution is locked down.
- **CSRF on every write**: state-changing HTTP endpoints enforce CSRF verification.
- **Strong authentication**: passwords hashed with **argon2id**, mandatory **TOTP 2FA** setup with recovery codes, rate-limited logins with IP + global lockouts, and `maestria auth reset-sessions`.
- **Cloudflare Access JWT verification**: optional second layer checked server-side when configured.
- **Local-only kill switch recovery**: `maestria tunnel off` locks down non-loopback access and cannot be undone remotely.
- **Audit log**: every write action is recorded in SQLite.
- **Read-only provider usage**: `<maestriDir>/usage/providers/` is never modified by Maestria.

For architecture and design details, see [docs/v2-spec.md](docs/v2-spec.md) and [docs/v2-architecture.md](docs/v2-architecture.md).

---

## Development (Go)

Requirements: Go 1.27+.

```sh
go vet ./...
go test -count=1 ./...
go run ./tools/syncdesign -check
go mod tidy -diff
```

Build release archives locally for `darwin/arm64`, `darwin/amd64`, `linux/amd64`, `linux/arm64`, and `windows/amd64` with `-trimpath`, version ldflags, `SHA256SUMS`, and the filled Homebrew formula:

```sh
go run ./tools/release \
  -out dist/release \
  -version v0.2.0 \
  -homebrew-template packaging/homebrew/maestria.rb \
  -homebrew-out dist/release/maestria.rb
```

---

## Legacy (V1)

A legacy Node.js/TypeScript implementation lives alongside v2 under `src/`, `public/`, and `test/` while users migrate. Existing V1 data (`maestria.db` inbox, chat, and events) can be imported read-only during `maestria init` or via:

```sh
maestria import-v1 [--from /path/to/v1-config]
```

V1 development commands:

```sh
pnpm build
pnpm typecheck
pnpm test
```

---

## License

[MIT](LICENSE)
