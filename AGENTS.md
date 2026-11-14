# Maestria

Control panel and protocol add-on for Maestri, written in Go. One `maestria` binary is the connector: it serves the panel locally (`maestria serve`) and/or links to a hosted server (`maestria connect`, server in `cmd/maestria-cloud`). `docs/v2-spec.md` is the product spec, `docs/v2-architecture.md` the architecture, `docs/protocol.md` the Maestro-side CLI protocol, `docs/hosted-protocol.md` the hosted relay protocol, and `skills/maestria-maestro/SKILL.md` the skill `maestria init` installs.

## Before declaring work done

`go vet ./... && go test ./...` (add `-race` for `internal/relay`, `internal/devices`, `internal/connect`, `internal/cloud`, `internal/runlock`), and `go mod tidy -diff` must be empty. After changing anything in `design/` that the panel uses, run `go generate ./internal/ui` (a test fails on drift).

## Rules

- Everything in English (code, UI, docs, commits). MIT, open source: nothing specific to one user, machine or workspace in code, defaults, tests or docs. No personal data, hostnames, emails or secrets in the repo.
- Maestri's data dir is resolved (`maestriDir` in config > `MAESTRI_HOME` > `~/.maestri`), never hard-coded. `<maestriDir>/usage/providers/` is strictly read-only.
- User config and state live in the config dir (`~/.config/maestria/`, 0600 secrets), never in the repo.
- The connector is the security authority. Local mode: bind 127.0.0.1 only, argon2id password + TOTP, CSRF on every write, strict CSP, no CORS, Host allowlist, optional Cloudflare Access JWT check, a kill switch that can only be re-enabled locally, audit log of every write. Hosted mode: signed requests from paired devices checked by the connector against its allowlist (see `docs/hosted-protocol.md`); the cloud only relays. Add tests for any change touching auth, sessions, CSRF, device signing, the relay allowlist, Wire scopes or the kill switch.
- Maestri Wire: reads and subscriptions use the guest token. The owner token is only for the allowlisted writes in `internal/wire/allow.go`.
- Few, well-known, pinned dependencies.
- Must work on macOS, Linux and Windows (paths, services, shell commands).
- Small commits on `main`.
- The panel in use runs the binary at `~/.local/bin/maestria`: after merging, rebuild it (`go build -o ~/.local/bin/maestria ./cmd/maestria`) and run `maestria service restart`.
