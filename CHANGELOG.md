# Changelog

All notable changes to Maestria are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org/). How to ship a release: [docs/releasing.md](docs/releasing.md).

## [Unreleased]

## [0.1.0-rc.1] - 2026-11-15

Release candidate for the first public release. Maestria 2: a control panel and protocol add-on for Maestri, in Go.

### Added

- **Invite-only sign-up on the hosted server.** `signup` is `invite` by default (`open` and `closed` exist; `MAESTRIA_CLOUD_SIGNUP`, and `open` logs a warning at start-up). Administrators (made only on the server with `maestria-cloud user promote EMAIL`, `demote` to undo) get an **Invite someone** page to create, copy, email and revoke single-use invitation links (7 days by default, 1 hour to 30 days, optionally bound to one email address, with a note), and `maestria-cloud invite create|list|revoke` does the same from the VM. A token is stored hashed, spent in the same transaction as the sign-up that uses it, and every way it can fail shows one page. Invitation emails count against the mail gate and have per-administrator and per-recipient limits. **After updating,** run `maestria-cloud user promote YOU@example.com` once to make your existing account an administrator; existing accounts keep working. See [deploy-cloud.md](docs/deploy-cloud.md#8-first-steps-as-administrator).
- **The panel.** One `maestria` binary serves it on `127.0.0.1` (`maestria serve`): what needs you (approve, reject, answer, choose), live activity and a chat with your Maestro through Maestri Wire, floors, and AI usage with an automatic standby before a limit.
- **Local security.** Password (argon2id) and TOTP sign-in with recovery codes, limits on failed attempts, CSRF on every write, a strict Content Security Policy, a Host allowlist, optional Cloudflare Access JWT checks, a kill switch that can only be turned back on locally, and an audit log of every write.
- **Hosted mode.** `maestria connect` links a machine to a hosted server and stays connected over an outbound WebSocket. Browsers are approved once on your machine (`maestria devices`) and every request is signed; the server only relays and stores no workspace data.
- **`maestria-cloud`,** the hosted server: accounts with email verification and TOTP, connector linking by code, the relay and device pairing. `packaging/linux/setup-cloud.sh` sets it up on a Debian or Ubuntu VM with a hardened systemd unit and a daily database backup.
- **The command line for agents:** `post`, `inbox`, `answers`, `say`, `wait`, `status`, `floor`, `agent`, `workspace`, `wire`, `service`, `config`, `tunnel`, and the `maestria-maestro` skill that `maestria init` installs.
- **Background service** on macOS (launchd), Linux (systemd user) and Windows (Task Scheduler): `maestria service install|start|stop|restart|status|logs`.
- **Installers and releases:** `install.sh`, `install.ps1` and a Homebrew formula, all verifying SHA-256 checksums; release archives for macOS, Linux and Windows built by `tools/release` on every `v*` tag.
- **Automatic updates.** `maestria update` and `maestria-cloud update` find the newest GitHub release (`--channel prerelease` for prereleases), verify its SHA-256 against `SHA256SUMS`, replace the binary atomically, keep the old one as `.previous`, restart the service and roll back if it is not healthy within 30 seconds. `--check` only reports; `--yes` is required without a terminal; nothing downgrades without `--force` (and `--version TAG` picks a release). Development builds never update themselves.
- **Update notices:** `maestria serve` prints a line when a newer release exists and the Security page shows a dismissible note, with no more than one request to GitHub a day. Turn the check off with `"updates": {"check": false}` in `config.json`.
- **Hosted VM timer:** `setup-cloud.sh` installs `maestria-cloud-update.timer` (every ~15 minutes, with a randomized delay) so a pushed tag reaches the server by itself. Opt out with `--no-auto-update`.
- `docs/releasing.md`: cutting a release or prerelease and rolling one back.
- **Signed releases.** The release workflow attests build provenance for every archive, `SHA256SUMS` and the formula, and signs `SHA256SUMS` with cosign (keyless, `SHA256SUMS.sigstore.json`). `install.sh`, `install.ps1` and the updater verify it when cosign is installed, pinned to this repository's release workflow and the exact tag, and refuse to continue if it fails. They download over https only (TLS 1.2+), and the installer scripts run nothing if a download is cut short. See [Verify a download](docs/security.md#verify-a-download).
- **Homebrew tap publishing.** After a stable release, the workflow checks the formula against the release and pushes it to `paulo-lucas/homebrew-tap` with a deploy key.
