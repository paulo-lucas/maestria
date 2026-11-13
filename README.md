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

Run the interactive setup wizard:

```sh
npx maestria init
```

Or install globally:

```sh
npm install -g maestria
maestria init
```

What `maestria init` does:

1. Creates your config directory (`$MAESTRIA_CONFIG_DIR` or `~/.config/maestria`, mode `0700`).
2. Detects and validates Maestri's data directory (priority order: `maestriDir` in `config.json` > `MAESTRI_HOME` env > `~/.maestri`), verifying that `usage/providers` exists.
3. Discovers your Maestri workspaces and roadmaps (`docs/roadmap.md`, `ROADMAP.md`, etc.) and writes `~/.config/maestria/workspaces.json`.
4. Runs `maestria auth setup` (argon2id password + TOTP authenticator QR code).
5. Optionally installs the Maestro protocol skill (`SKILL.md`) for Claude Code (`~/.claude/skills/maestria/`) and/or Antigravity (`~/.gemini/.../skills/maestria/`).

For local HTTP testing without HTTPS (never use this when exposed on the internet), create `~/.config/maestria/config.json` with:

```json
{
  "cookieSecure": false
}
```

Start the panel:

```sh
maestria serve
```

Then open `http://127.0.0.1:4777` (only listens on `127.0.0.1`). To run continuously on macOS login, adapt [`docs/launchd.plist.example`](docs/launchd.plist.example).

## Secure Remote Exposure (Cloudflare Tunnel + Access)

Maestria binds strictly to `127.0.0.1` (`port` defaults to `4777`). To reach it away from your desk:

1. Point a **Cloudflare Tunnel** (`cloudflared`) hostname (e.g. `panel.example.com`) at `http://127.0.0.1:4777`.
2. Protect that hostname with **Cloudflare Access** (an email/identity policy at Cloudflare's edge before traffic ever hits your Mac).
3. Configure `~/.config/maestria/config.json`:

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

Maestria reads `<maestriDir>/usage/providers/.status.json` read-only and displays every provider configured in Maestri (Claude Code, Antigravity, Codex, and custom providers). If Maestri stops updating `.status.json` (for example while its rings are off-screen) past `usage.staleMinutes` (default `15`), Maestria can optionally execute the provider's `<id>.json` specification as a read-only fallback.

Configure automatic standby/resume thresholds per provider and window in `config.json`:

```json
{
  "usage": {
    "staleMinutes": 15,
    "fallbackEnabled": true,
    "pollSeconds": 30
  },
  "standby": {
    "resumeBelowPercent": 80,
    "notifyViaCli": true,
    "thresholds": {
      "claude": { "five_hour": 95, "seven_day": 90 },
      "antigravity": { "weekly": 90 }
    }
  }
}
```

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

## License

[MIT](LICENSE)
