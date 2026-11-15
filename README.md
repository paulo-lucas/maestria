<p align="center">
  <a href="https://github.com/paulo-lucas/maestria/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/paulo-lucas/maestria/actions/workflows/ci.yml/badge.svg"></a>
  <a href="https://github.com/paulo-lucas/maestria/releases"><img alt="Release" src="https://img.shields.io/github/v/release/paulo-lucas/maestria?include_prereleases&sort=semver&label=release"></a>
  <img alt="Go" src="https://img.shields.io/github/go-mod/go-version/paulo-lucas/maestria?logo=go&logoColor=white">
  <img alt="Platforms" src="https://img.shields.io/badge/platforms-macOS%20%7C%20Linux%20%7C%20Windows-555">
  <a href="LICENSE"><img alt="License: MIT" src="https://img.shields.io/github/license/paulo-lucas/maestria"></a>
  <a href="https://www.themaestri.app"><img alt="Built for Maestri" src="https://img.shields.io/badge/built%20for-Maestri-6b4fbb"></a>
</p>

<p align="center">
  <img src="docs/images/logo.svg" width="88" height="88" alt="">
</p>

<h1 align="center">Maestria</h1>

<p align="center">
  <strong>Your AI agents, in your pocket.</strong><br>
  A control panel for <a href="https://www.themaestri.app">Maestri</a>: see what every agent is doing, answer their questions,<br>
  and keep an eye on floors and usage, from your computer or your phone.
</p>

<p align="center">
  <a href="docs/getting-started.md"><strong>Get started</strong></a> ·
  <a href="docs/concepts.md">How it works</a> ·
  <a href="docs/self-host.md">Self-host</a> ·
  <a href="docs/hosted.md">Hosted</a> ·
  <a href="docs/security.md">Security</a>
</p>

---

When agents work for hours on several projects, they stop and ask: *"Can I merge this?"*, *"Which database?"*. Maestria collects those questions in one place, so you can answer in seconds and let the work go on. It is open source (MIT), a single Go binary, and your data stays on your machine.

> [!TIP]
> **Skip the reading: let your agent set it up.** Paste this into your Maestro (or any coding agent):
>
> ```text
> Read https://raw.githubusercontent.com/paulo-lucas/maestria/main/docs/getting-started.md
> and https://raw.githubusercontent.com/paulo-lucas/maestria/main/docs/for-agents.md,
> then set up Maestria on this machine for me. Follow the "If you are an AI agent" section.
> ```
>
> The agent installs it and walks you through the rest. You only type your password and scan the authenticator code yourself.

![The Maestria overview](docs/images/overview.png)

| Needs you | Activity | Floors | Usage and standby |
| --- | --- | --- | --- |
| ![Human tasks](docs/images/workspace-tasks.png) | ![Activity](docs/images/workspace-activity.png) | ![Floors](docs/images/workspace-floors.png) | ![Usage](docs/images/usage.png) |

## What you get

- **Needs you:** approve, reject, answer or choose, from anywhere.
- **Live activity and chat:** which agent does what, and a chat with your Maestro (through Maestri Wire).
- **Floors:** what is planned, what is in progress, what landed.
- **Usage and standby:** your AI limits, and an automatic pause before you hit them.
- **A kill switch** that cuts off remote access in one click.

## Choose your setup

There are two ways to run it. Both give you the same panel.

| | **Self-host** | **Hosted** |
| --- | --- | --- |
| It runs | On your own machine | On a Maestria server, with accounts |
| Your machine | Serves the panel on `127.0.0.1` | Connects *out* to the server |
| From your phone | Through a free Cloudflare Tunnel | Sign in, nothing else to set up |
| Several machines | One panel each | One panel for all |
| Guide | [Self-host](docs/self-host.md) | [Use a hosted Maestria](docs/hosted.md) |

**Self-hosted:** the panel lives on your machine. A tunnel is optional.

```mermaid
flowchart LR
  B["Browser or phone"] -- "HTTPS" --> T["Cloudflare Tunnel<br/>(optional)<br/>maestria.example.com"]
  T --> C
  subgraph M["Your machine"]
    C["Connector and panel<br/>127.0.0.1:4777"] --> W["Maestri Wire"] --> A["Maestri<br/>agents, Maestro, floors"]
  end
```

**Hosted:** the server only relays. Your machine holds the data and decides what is allowed.

```mermaid
flowchart LR
  B["Browser or phone"] -- "HTTPS" --> S["Maestria service<br/>maestria.example.com<br/>relay only, no data"]
  subgraph M["Your machine"]
    C["Connector<br/>your data and rules"] --> W["Maestri Wire"] --> A["Maestri<br/>agents, Maestro, floors"]
  end
  C -- "outbound WSS, no open port" --> S
```

Not sure which one? Start with self-host. Words you do not know are explained in [Concepts](docs/concepts.md).

## Quick start

**1. Install** (macOS and Linux):

```sh
curl --proto '=https' --tlsv1.2 -fsSL https://raw.githubusercontent.com/paulo-lucas/maestria/main/install.sh | sh
```

On Windows (PowerShell):

```powershell
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor 3072; irm https://raw.githubusercontent.com/paulo-lucas/maestria/main/install.ps1 | iex
```

With Homebrew: `brew install paulo-lucas/tap/maestria`. Other ways and more detail: [Getting started](docs/getting-started.md#1-install-maestria).

**Verify your download:** releases carry build provenance and a signature that prove they were built by this repository's release workflow. With [cosign](https://docs.sigstore.dev/cosign/system_config/installation/) installed the installers check it for you; to check by hand, see [Verify a download](docs/security.md#verify-a-download).

**2. Set up** (open a new terminal first):

```sh
maestria init
```

The wizard asks a few questions and explains each one. Press Enter to accept the suggestion. It sets your password and authenticator app, and can start Maestria in the background.

**3. Open the panel** at <http://127.0.0.1:4777>.

Using a hosted server? Link your machine instead, with the code the server shows you:

```sh
maestria connect --link XXXX-XXXX --cloud https://maestria.example.com
```

**Stay current:** `maestria update` installs the latest release (checksum-verified, signature-verified when cosign is installed, and it rolls back if the new version does not start). See [Self-host](docs/self-host.md#5-update).

New to the terminal? [Getting started](docs/getting-started.md) walks through every step and shows what you should see.

## Guides

| Guide | For |
| --- | --- |
| [Getting started](docs/getting-started.md) | Everyone. Install and first run, step by step. |
| [Concepts](docs/concepts.md) | Everyone. What the words mean, with diagrams. |
| [Self-host](docs/self-host.md) | Run it on your machine, reach it from your phone, update, back up. |
| [Use a hosted Maestria](docs/hosted.md) | Sign in to a server, link machines, approve browsers. |
| [Run your own hosted server](docs/deploy-cloud.md) | Operators. A free VM, a systemd unit, a Cloudflare Tunnel. |
| [Connect to Maestri Wire](docs/wire.md) | Live agents and chat. |
| [For Maestro agents](docs/for-agents.md) | The `maestria` command line for agents and scripts. |
| [Security](docs/security.md) | How it protects you, in plain words and in detail. |
| [FAQ](docs/faq.md) and [Troubleshooting](docs/troubleshooting.md) | Questions, and what to do about an error message. |

All guides are listed in [docs/README.md](docs/README.md).

## Security in three lines

- **Nothing listens to the internet.** The panel binds `127.0.0.1` only, and the hosted setup connects out. No port is opened.
- **Two locks to sign in:** a password (argon2id) and an authenticator code, with limits on failed attempts. In hosted mode, each browser is also approved once on your machine, and every request is signed.
- **You can pull the plug.** The kill switch cuts off remote access, and only someone at your machine can turn it back on. Every change is in an audit log.

Read more in [Security](docs/security.md).

## Development

Go 1.27 or newer.

```sh
go vet ./...
go test ./...
go mod tidy -diff
go build -o maestria ./cmd/maestria
go build -o maestria-cloud ./cmd/maestria-cloud
```

After changing the panel's design in `design/`, run `go generate ./internal/ui`. Release archives for all five targets, with checksums and the Homebrew formula, come from `go run ./tools/release`.

Want to help? Read [CONTRIBUTING.md](CONTRIBUTING.md). Security problems go through [SECURITY.md](SECURITY.md), not public issues.

More: the design of the panel is in [design/](design/README.md), the product decisions in [docs/v2-spec.md](docs/v2-spec.md), the architecture in [docs/v2-architecture.md](docs/v2-architecture.md), the Maestro protocol in [docs/protocol.md](docs/protocol.md) and the hosted protocol in [docs/hosted-protocol.md](docs/hosted-protocol.md).

## License

[MIT](LICENSE)
