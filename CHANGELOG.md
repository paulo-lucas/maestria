# Changelog

All notable changes to Maestria are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org/). How to ship a release: [docs/releasing.md](docs/releasing.md).

## [Unreleased]

### Added

- **Insights.** A new page (a tab next to Usage & standby, and System → Insights in the sidebar) and `maestria insights [--json]` rate from 1 to 7 how agent work on a machine turns usage into landed floors over the last 30 days: delivery speed, work that holds, usage that ships and your response time. Each area shows its measure, the threshold for the next level and one thing to try; an area without enough data says what it needs. Thresholds are fixed and listed in [docs/insights.md](docs/insights.md). Local panel and hosted site.
- The connector keeps a usage history for Insights: one sample per usage window every 5 minutes while it runs, kept 90 days, in its own database, with the provider account stored as a hash. Maestri's usage files are still only read.

### Fixed

- A floor created in Maestri that the Maestro never recorded is now kept in Past with the time Maestria first saw it as its start, instead of starting and ending at the same moment.

## [0.1.4] - 2026-11-15

### Fixed

- **A sent chat message did not show until a reload.** When a redraw was already on its way while the message was sent (common on a slow connection), the redraw that should show it was dropped, and the composer then held back the next ones. It now always runs.
- **Scrolling the chat stopped with a jolt** when the page redrew during a swipe. The message list now stays the same element across a redraw, so a scroll in progress carries on, and its position is only changed when something above the reader actually moved.

## [0.1.3] - 2026-11-15

### Fixed

- **The chat jumped to its first messages** while you were reading older ones, typically after switching back to the tab. When a redraw came while the chat was briefly empty (reconnecting), the saved place read as the very top. The chat now remembers the message you were reading, not a pixel offset, and puts you back on it.

## [0.1.2] - 2026-11-15

### Fixed

- **The hosted Overview was stuck on "Approve this browser"** for an approved browser, while the sidebar and every other page worked. Building the new Updates card threw an error, so the page kept what it showed while connecting. A test now catches this kind of mistake in the browser code.
- The hosted Overview shows **Connecting to your machines…** while a machine has not answered yet, instead of asking to approve the browser.
- When a machine is online but does not answer (a slow or dropped connection, a clock that is off), the hosted Overview says **Could not reach your machine**, with the reason and a **Try again** button, instead of asking to approve the browser. It also tries again by itself, waiting longer each time, when the tab comes back into view and when the device reconnects.
- On Windows, saving a settings file (such as `wire.json`) while another program had it open failed with "Access is denied". The save now retries for about a second.

## [0.1.1] - 2026-11-15

### Added

- **Overview shows every update.** Notices from all workspaces now sit in an **Updates** card under Needs you, newest first, with workspace filter chips, **Got it** on each one and a **Mark all as read** button that acts on the chosen workspace (or all of them). Local panel and hosted site.
- **The version on a phone.** The scope menu ends with the version (`maestria vX` locally, `Server vX` on the hosted site) and Security has an **About** line with it, both with the same "newer release" dot as the desktop sidebar.
- **`setup-cloud.sh` installs cosign** (a pinned release, its SHA-256 checked) so the VM's automatic updates verify who built each release. Skip it with `--no-cosign`; an existing cosign is kept.

### Changed

- **Inbox actions answer once.** Approve, Reject, Send, a choice, Later and Got it disable the item on the first click and show a spinner until the server answers; a failure re-enables the item and says why. A second submit of an answered item is refused (409), which is now covered by tests.
- A fenced command of one shell line in a Maestro's message (inbox and chat) is shown as the panel's command block with a **Copy** button; longer code stays a scrollable block.
- GitHub Actions use their Node 24 releases (`checkout` v7.0.1, `setup-go` v7.0.0, `action-gh-release` v3.0.4), pinned by commit SHA.

### Fixed

- The **Later** menu was clipped by the card on the last item. It is now a popover that opens upward when there is no room below, and a bottom sheet on a phone.
- Long code or commands in an inbox item no longer widen the card or push the time and buttons out of view.
- A floor that landed and had its branch deleted at once was recorded as cancelled. The panel now remembers the branch tip while the floor is alive and uses that commit to tell it was merged. (A floor that never made a commit of its own still reads as landed, as before: git cannot tell the two apart.)
- Two commands opening a brand-new database at the same moment (the panel and an agent's `maestria post` on a fresh config dir) could fail with "table sessions already exists" or "database is locked". The first open now takes the write lock before reading the schema version and applies the missing migrations in one transaction, and waits while another process creates the file.
- On a phone, the Security audit log's date no longer overlaps the event text.
- A one-line notice longer than the title limit shows its full text under the shortened title.
- After **Got it**, **Mark all as read** or an answer removes an item, keyboard focus moves to that list's heading instead of being lost.
- The hosted sidebar's machine card no longer ends with a bare "·" ("offline ·") when the last-seen time is unknown.
- The systemd environment file example, the `smtp.from` documentation and the start-up error now say to put a `Name <address>` From value in double quotes.

## [0.1.0] - 2026-11-15

First public release. Same contents as 0.1.0-rc.1, checked by hand: signed checksums verified with cosign, a clean install through `install.sh` with the signature required, and the updater.

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
