# Maestria

Self-hosted control panel and protocol add-on for Maestri: one panel (inbox, chat, roadmap, usage, standby, kill switch, audit) for every Maestri workspace on the machine. `BRIEF.md` is the product spec; `docs/protocol.md` is the Maestro-side protocol; `skills/maestria/SKILL.md` is the skill `maestria init` installs.

## Commands

| Task | Command |
| --- | --- |
| Build | `pnpm build` (outputs `dist/`, which the running panel serves) |
| Types | `pnpm typecheck` |
| Tests | `pnpm test` (builds first; Vitest) |
| Dev server | `pnpm dev` (watch mode, port from config) |

Before declaring work done: `pnpm typecheck && pnpm test`.

## Rules

- Everything in English (code, UI, docs, commits). MIT, open source: nothing specific to one user, machine or workspace in code, defaults, tests or docs. No personal data, hostnames, emails or secrets in the repo.
- Maestri's data dir is resolved (`maestriDir` in config > `MAESTRI_HOME` > `~/.maestri`), never hard-coded. `<maestriDir>/usage/providers/` is strictly read-only.
- User config and state live in the config dir (`~/.config/maestria/`, 0600 secrets), never in the repo.
- Security model is fixed (see BRIEF.md): bind 127.0.0.1 only, argon2id password + TOTP, CSRF on every write, strict CSP, no CORS, optional Cloudflare Access JWT check, kill switch that can only be re-enabled locally, audit log of every write. Add tests for any change touching auth, sessions, CSRF or the kill switch.
- Few, well-known, pinned dependencies.
- Must work on macOS, Linux and Windows (paths, services, shell commands).
- Small commits on `main`.
- The panel in use runs from `dist/`: after merging, `pnpm build` and restart the server (`maestria service restart` if installed as a service).
