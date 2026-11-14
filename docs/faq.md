# Frequently asked questions

Short answers. Each links to the full guide. Something not working? Go to [Troubleshooting](troubleshooting.md).

## The basics

### What is Maestria?

A control panel for [Maestri](https://www.themaestri.app). You open it in a browser, on your computer or your phone. You see what your agents do, answer their questions, follow your floors and your AI usage. See [Concepts](concepts.md).

### Do I need Maestri?

Yes. Maestria shows and steers work that happens in Maestri.

### Does it cost anything?

Maestria is free and open source (MIT license). Running it costs nothing beyond what you already use. A Cloudflare Tunnel and a small hosted server can both be free (see [Self-host](self-host.md) and [Run your own hosted server](deploy-cloud.md)). Free offers from other companies can change.

### Which systems does it run on?

macOS, Linux and Windows. Maestri Wire (live agents and chat) needs a Maestri host, which runs on macOS and Windows.

### What is the difference between self-hosted and hosted?

- **Self-hosted:** the panel runs on your own machine. You reach it at `http://127.0.0.1:4777`, or from outside through a Cloudflare Tunnel.
- **Hosted:** a Maestria server on the internet gives you accounts and one panel for all your machines. Your machines connect out to it.

See [Which setup should I pick?](concepts.md#which-setup-should-i-pick).

### Can I use both?

Yes. After `maestria connect`, `maestria serve` also keeps the hosted connection. Or install the service with `maestria service install --mode both`.

## Using it

### What does "Needs you" mean?

Things an agent waits on you for: an approval, a question, a choice, or a yes/no prompt in a terminal. See [Human tasks](concepts.md#human-tasks).

### The panel is empty. Is it broken?

Probably not. It shows what your Maestro posts. Try `maestria post --kind notice "Hello"` in a project folder ([Getting started, step 7](getting-started.md#7-try-it)). Also check that the workspace is registered (`maestria workspace list`) and that Wire is on if you expect live agents ([Wire](wire.md)).

### Why do I not see live agents or chat?

You need to pair Maestria with Maestri Wire (`maestria wire pair`). Without it the panel still shows tasks, usage and floors the Maestro plans. See [Wire](wire.md).

### How do I make my Maestro use Maestria?

`maestria init` installs a skill for Claude Code and Antigravity. For other agents, paste the snippet from [For agents](for-agents.md#the-idea-in-one-minute) into the Maestro's instructions.

### What is standby?

A brake. When your AI usage nears a limit you set, Maestria tells the Maestro, or interrupts the agents, and wakes the Maestro when the limit resets. See [Standby](concepts.md#standby). Change it on the **Usage & standby** page.

### Can I answer from my phone?

Yes. Use a Cloudflare Tunnel ([Self-host](self-host.md#2-reach-it-from-your-phone-with-a-cloudflare-tunnel)) or a hosted server ([Hosted](hosted.md)). The panel works on small screens.

### Can several people use one panel?

The panel has **one owner**: one password, one authenticator. It is built for a single person steering their own machines. A hosted server has many accounts, and each account sees only its own machines.

### How do I update?

Run the install command again, then `maestria service restart`. See [Self-host: update](self-host.md#5-update).

### How do I remove it?

[Self-host: uninstall](self-host.md#7-uninstall).

## Security and privacy

### Is it safe to reach from the internet?

The panel never listens outside your machine. A tunnel dials out, and you sign in with a password and a 6-digit code. You can add Cloudflare Access as a second lock, and use the kill switch in an emergency. See [Security](security.md).

### Does the hosted server see my data?

What you look at passes through it while you look. It does not store it or log it, and it cannot forge commands or read it later. But it also serves the web page, so use a server you trust or run your own. Details: [Hosted: what leaves your machine](hosted.md#what-leaves-your-machine-and-what-never-does).

### Can the website run commands on my computer?

Not by itself. Each request is signed by a browser **you approved on your machine**, and your machine checks it against a short allowlist. See [Security](security.md#hosted-mode).

### What can Maestria do to Maestri through Wire?

Read agents, floors and chat. Send a message to your Maestro, answer a yes/no, and press Esc on an agent. Nothing else. See [Wire](wire.md#what-maestria-can-and-cannot-do-through-wire).

### What is the kill switch?

An emergency stop for remote access. Turn it on with `maestria tunnel off` or the power button. Only the local command `maestria tunnel on` turns it off. See [Self-host](self-host.md#4-the-kill-switch).

### I lost my phone. What now?

1. Turn on the kill switch (`maestria tunnel off`) if you reach the panel remotely.
2. In hosted mode, revoke the phone on each machine (`maestria devices revoke <id>`).
3. Sign in with a recovery code and set up a new authenticator, or `maestria auth setup --force` on the machine.

### Where is my data?

In the config directory on your machine: `~/.config/maestria` (macOS, Linux) or `%AppData%\maestria` (Windows). Back it up as in [Self-host: back up](self-host.md#6-back-up). Maestria never changes Maestri's own data folder.

## Hosted mode

### Why must I approve each browser on each machine?

So a hacked server cannot add its own browser. The approval happens on your own machine, where the server cannot reach. See [Hosted, step 3](hosted.md#3-approve-your-browser-on-the-machine).

### What if my machine is off?

Its workspaces disappear from the panel. Nothing is saved for later and nothing is sent to it. It shows again when it is back online.

### Can I run my own server?

Yes. See [Run your own hosted server](deploy-cloud.md). Cloudflare Workers cannot run it. A small free VM with a Cloudflare Tunnel can.

### Can I use Cloudflare Access with the hosted server?

No, not in front of the server's address. Your machines connect with their own credential and cannot pass an Access login. Accounts already require two-factor sign-in.

## Technical

### Which port does it use?

`4777` on `127.0.0.1` for the panel. Change it with `"port"` in `config.json`. The hosted server uses `8080` on `127.0.0.1` by default.

### Why can I not make the panel listen on my network?

On purpose. Anything on your network could try to sign in. To go beyond your machine, use a tunnel, which dials out.

### Where are the logs?

`maestria service logs`, or the `logs/` folder in your config directory.

### What does it store?

A SQLite database (`maestria-v2.db`) with tasks, chat, floors and the audit log, plus small files for settings and secrets. See [Self-host: back up](self-host.md#6-back-up).

### How do I use it in scripts?

Most commands take `--json`. See [For agents](for-agents.md#scripting-notes).

### Can I try it without touching my real setup?

Yes. Set `MAESTRIA_CONFIG_DIR` to an empty folder before running any `maestria` command. It uses that folder instead of your real one.

### Where do I report a bug or a security problem?

Bugs: the project's issue tracker. Security problems: GitHub's private "Report a vulnerability" on the repository's Security tab. See [Security](security.md#reporting-a-problem).
