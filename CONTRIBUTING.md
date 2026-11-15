# Contributing

Thanks for helping. Pull requests are welcome: bug fixes, docs, tests and small features.

For anything bigger than a fix (a new page, a new command, a change to how something works), open an issue first so we can agree on the shape before you write it.

## How

1. Fork the repository and make a branch from `main`.
2. Make the change. Keep it small: one fix or one feature per pull request.
3. Run the checks below. They are what CI runs on macOS, Linux and Windows.
4. Open a pull request against `main` and fill in the template.

A maintainer reviews every pull request. The first time you open one, CI waits for a maintainer to approve the run.

## Checks

Go 1.27 or newer.

```sh
go vet ./...
go test ./...
go test -race ./internal/relay/... ./internal/devices/... ./internal/connect/... ./internal/cloud/... ./internal/runlock/...
go mod tidy -diff   # must print nothing
```

After changing anything in `design/` that the panel uses, run `go generate ./internal/ui` (a test fails if you forget).

To try the panel without touching a Maestria you already use, give your build its own config dir and port:

```sh
go build -o maestria ./cmd/maestria
export MAESTRIA_CONFIG_DIR="$(mktemp -d)"
./maestria init     # a test password and authenticator
./maestria serve
```

Set `"port": 4778` in `$MAESTRIA_CONFIG_DIR/config.json` (a 0600 file) if 4777 is taken.

## Rules

The full list is in [AGENTS.md](AGENTS.md). If you work with a coding agent, it reads that file. The short version:

- **English** everywhere: code, UI text, docs, commit messages.
- **Nothing personal** in the repository: no names, emails, hostnames, paths of your machine or secrets, in code, tests or docs.
- **Works on macOS, Linux and Windows.** Use `filepath`, not hard-coded `/`, and no shell-only commands.
- **Few dependencies.** A new dependency needs a good reason in the pull request.
- **Security changes come with tests.** Anything touching sign-in, sessions, CSRF, device signing, the relay allowlist, Wire scopes or the kill switch must add or update tests.
- Maestri's `usage/providers/` folder is read-only. Maestria never writes there.
- Docs are plain and short. If the change is visible to people, add a line under `## [Unreleased]` in [CHANGELOG.md](CHANGELOG.md).

Commit messages follow the style in the history: `fix(panel): keep the reader's place in the chat`, `docs: …`, `feat(cloud): …`.

## Security problems

Do not open a public issue or pull request for a vulnerability. See [SECURITY.md](SECURITY.md).

## License

By contributing, you agree that your contribution is released under the [MIT License](LICENSE).
