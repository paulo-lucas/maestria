# Roadmap

## Done

| #   | Milestone             | Result                                                                                                     |
| --- | --------------------- | ---------------------------------------------------------------------------------------------------------- |
| 1   | Panel core            | Fastify server on 127.0.0.1, argon2id password + TOTP, sessions, CSRF, strict CSP, audit log, kill switch |
| 2   | Inbox                 | notice / approve / input / choice items, Needs you + Updates + History, per-workspace and Home views      |
| 3   | Protocol + CLI        | `post`, `inbox`, `answers`, `say`, `wait` (cursors), `status`; docs/protocol.md and the Maestro skill     |
| 4   | Usage + standby       | Reads Maestri Agent Usage providers, hides failing providers, standby/resume events with thresholds       |
| 5   | Distribution          | `maestria init` wizard, workspace discovery, skill install, `maestria service` (macOS, Linux, Windows)    |
| 6   | Roadmap + fixes       | Sanitized GFM roadmap view, tabs hidden on Home, CSRF cookie reuse, `wait` stays alive                    |

## Pipeline

| #   | Milestone         | Scope                                                                                                                                     |
| --- | ----------------- | ----------------------------------------------------------------------------------------------------------------------------------------- |
| 7   | Chat mirror       | Read-only mirror of the Maestri Chat threads (one per color) plus a badged composer that wakes the Maestro, until Maestri has a send API |
| 8   | Access hardening  | Guide and checks for Cloudflare Access (JWT verification on by default when configured), kill switch with a user-level cloudflared      |
| 9   | Publish           | README polish, screenshots, CI, npm package and GitHub release                                                                           |
