# Maestria

A self-hosted control panel and CLI protocol add-on for [Maestri](https://maestri.app) workspaces. One local Maestria process serves every Maestri workspace on your machine from a single hardened web panel:

- **Home (unified Inbox & Usage):** See every item needing your input (`approve`, `input`, `choice`) on top, informational `notice` updates below, workspace filter chips with color tags, and global usage gauges read directly from Maestri's Agent Usage providers.
- **Per-workspace pages:** Dedicated **Inbox**, **Chat**, and read-only **Roadmap** views per project.
- **Maestro protocol (`maestria` CLI):** Your Maestro agents post items (`maestria post`), read answers (`maestria answers`), reply in chat (`maestria say`), and sleep efficiently in the background on `maestria wait`.
- **Usage Standby Guard:** Optional per-provider/window thresholds that emit `standby` / `resume` events into workspace event streams automatically.

## Requirements

- **Node.js >= 24.7**
- **Maestri** with **Settings → Agents → Usage** enabled (so Maestri writes `<maestriDir>/usage/providers/.status.json`).

## Install & Setup

Run the interactive setup wizard (or pass flags like `--skill claude,antigravity --yes` for non-interactive runs):

```sh
npx maestria init
```

Or install globally:

```sh
npm install -g maestria
maestria init
```

Re-running `maestria init` is idempotent: existing config, workspaces, auth credentials, and installed skills are detected and can be kept or redone individually.

What `maestria init` does:

1. Creates your config directory (`$MAESTRIA_CONFIG_DIR`, `%APPDATA%\maestria` on Windows, or `~/.config/maestria` with mode `0700` on macOS/Linux; files inside are restricted to `0600` on POSIX and inherit your user profile ACL on Windows).
2. Detects and validates Maestri's data directory (priority order: `maestriDir` in `config.json` > `MAESTRI_HOME` env > `~/.maestri`, plus `%APPDATA%\Maestri` on Windows), verifying that `usage/providers` exists.
3. Discovers your Maestri workspaces and roadmaps (`docs/roadmap.md`, `ROADMAP.md`, etc.) and writes `workspaces.json` in your config directory.
4. Runs `maestria auth setup` (argon2id password + TOTP authenticator QR code).
5. Optionally installs the Maestro protocol skill (`SKILL.md`) for Claude Code (`~/.claude/skills/maestria/`) and/or Antigravity (`~/.gemini/.../skills/maestria/`).

For local HTTP testing without HTTPS (never use this when exposed on the internet), create `config.json` in your config directory with:

```json
{
  "cookieSecure": false
}
```

Start the panel:

```sh
maestria serve
```

Then open `http://127.0.0.1:4777` (only listens on `127.0.0.1`).

## Keep It Running (Background Service)

`maestria init` offers at the end to install and start a background user service so the panel starts automatically on login and restarts if it crashes. You can manage it anytime with:

```sh
maestria service install    # install LaunchAgent (macOS), systemd --user unit (Linux), or Task Scheduler task (Windows)
maestria service start      # start the background service
maestria service stop       # stop the background service
maestria service restart    # reload/restart (e.g. after updating maestria)
maestria service status     # inspect service state and panel reachability
maestria service uninstall  # remove the service registration and generated script/unit
```

- **macOS (`launchd`):** Installs `~/Library/LaunchAgents/dev.maestria.panel.plist` (`RunAtLoad` + `KeepAlive`) pointing to your current `node` and `dist/cli.js` paths, with logs in `~/Library/Logs/maestria/panel.{out,err}.log` (or customize [`docs/launchd.plist.example`](docs/launchd.plist.example)).
- **Linux (`systemd --user`):** Installs `~/.config/systemd/user/maestria.service` (`Restart=always`) and enables it for user login sessions.
- **Windows (Task Scheduler):** Generates `%APPDATA%\maestria\maestria-service.cmd` and registers user-level Scheduled Task `MaestriaPanel` (`ONLOGON`, limited privileges — no Administrator prompt required), writing logs to `%APPDATA%\maestria\logs\panel.log`. Alternatively, wrap `node <path-to-dist>\cli.js serve` with [NSSM](https://nssm.cc/) or run `maestria serve` inside Windows Terminal.
- You can also check whether the panel is reachable from any workspace via `maestria status`.

## Secure Remote Exposure (Cloudflare Tunnel + Access)

Maestria binds strictly to `127.0.0.1` (`port` defaults to `4777`). To reach it away from your desk:

1. Point a **Cloudflare Tunnel** (`cloudflared`) hostname (e.g. `panel.example.com`) at `http://127.0.0.1:4777`.
2. Protect that hostname with **Cloudflare Access** (an email/identity policy at Cloudflare's edge before traffic ever hits your Mac).
3. Configure `config.json` in your config directory (`~/.config/maestria/config.json` on macOS/Linux, `%APPDATA%\maestria\config.json` on Windows):

```json
{
  "port": 4777,
  "cookieSecure": true,
  "allowedHosts": ["panel.example.com"],
  "trustCfConnectingIp": true,
  "cloudflareAccess": {
    "teamDomain": "yourteam.cloudflareaccess.com",
    "audience": "<your-cloudflare-access-application-aud-hex>"
  }
}
```

Security properties enforced on every request:

- **Argon2id + TOTP MFA** (`maestria auth setup`) with per-IP and global lockout rate limits.
- **Host header allowlist** (`127.0.0.1:<port>`, `localhost:<port>`, `allowedHosts`) to defeat DNS rebinding.
- **Cloudflare Access JWT verification** (`Cf-Access-Jwt-Assertion`) when configured, verified against your team's JWKS.
- **HttpOnly + Secure (`__Host-`) + SameSite=Strict cookies**, explicit CSRF tokens + `Origin` / `Sec-Fetch-Site` checks on writes, strict CSP (`default-src 'none'`, no inline scripts/styles), `X-Frame-Options: DENY`.
- **Append-only audit log** of logins, failed attempts, rejections, writes, standby transitions, and kill-switch events.

## Standby Guard & Usage

Maestria reads `<maestriDir>/usage/providers/.status.json` read-only and displays every provider configured in Maestri (Claude Code, Antigravity, Codex, and custom providers). Erroring providers with no recent reading are hidden under a toggle, while failing providers with a reading within the last 24 hours remain visible as `stale`. If Maestri stops updating `.status.json` (for example while its rings are off-screen) past `usage.staleMinutes` (default `15`), Maestria can optionally execute the provider's `<id>.json` specification as a read-only fallback.

Standby is evaluated per workspace against that workspace's `maestroProvider` (default `"claude"` in `workspaces.json`, settable via `maestria workspace add ... --provider <id>`), so workspaces running on other providers are not paused when Claude hits its limits. By default, Maestria guards `claude` at `session: 95%` and `weekly: 90%`, and emits `resume` automatically when the triggering window resets or usage drops below `resumeBelowPercent` (`80`). You can confirm or customize these thresholds during `maestria init` or in `config.json`:

```json
{
  "usage": {
    "staleMinutes": 15,
    "fallbackEnabled": true,
    "pollSeconds": 30
  },
  "standby": {
    "resumeBelowPercent": 80,
    "notifyViaCli": false,
    "thresholds": {
      "claude": { "session": 95, "weekly": 90 },
      "antigravity": { "weekly": 90 }
    }
  }
}
```

Maestria performs no agent control itself — it appends `standby` / `resume` events (listing the triggering provider/window) to the workspace event stream, and the Maestro interrupts/resumes its running teammates via `maestria wait`.

## Kill Switch & Recovery

The **Kill switch** button in the panel header (with confirmation) — or `maestria tunnel off` locally:

1. Immediately sets `tunnel_killed` in SQLite.
2. Invalidates every active session across all browsers/devices.
3. Executes `tunnel.stopCommand` (without a shell) if configured in `config.json`, for example:

```json
{
  "tunnel": {
    "stopCommand": ["launchctl", "bootout", "gui/501/com.cloudflare.cloudflared"],
    "startCommand": ["launchctl", "bootstrap", "gui/501", "/Users/you/Library/LaunchAgents/com.cloudflare.cloudflared.plist"],
    "statusCommand": ["pgrep", "-x", "cloudflared"]
  }
}
```

While killed, **all web logins are locked** (`HTTP 403`). Re-enabling is only possible locally from the machine itself:

```sh
maestria tunnel on
```

## Maestro Protocol

See [`docs/protocol.md`](docs/protocol.md) and [`skills/maestria-maestro/SKILL.md`](skills/maestria-maestro/SKILL.md).

## Cross-Platform CI Matrix

If you run CI for forks or contributions, test Maestria across macOS, Linux, and Windows using a matrix on Node 24:

```yaml
strategy:
  matrix:
    os: [ubuntu-latest, macos-latest, windows-latest]
steps:
  - uses: actions/checkout@v4
  - uses: pnpm/action-setup@v4
  - uses: actions/setup-node@v4
    with:
      node-version: 24
      cache: pnpm
  - run: pnpm install --frozen-lockfile
  - run: pnpm typecheck
  - run: pnpm test
  - run: pnpm build
```

## License

[MIT](LICENSE)
