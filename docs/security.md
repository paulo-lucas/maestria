# Security

Maestria lets you steer AI agents that run commands on your computer. That is powerful, so it is built to be hard to misuse. This page explains how, first in plain words and then in detail.

## In plain words

1. **Your data stays on your machine.** Tasks, chat, floors and docs live in a small database on your computer. A hosted server never stores them.
2. **Nothing listens to the internet.** The panel only answers on `127.0.0.1`, an address that works from your own machine and nowhere else. The hosted setup connects *out*. No port is opened.
3. **Signing in takes two things.** A password you know and a 6-digit code from an app you own. There are no accounts without the code.
4. **Your machine has the last word.** In the hosted setup the website cannot give orders by itself. Each order is signed by a browser *you approved on your own machine*, and your machine checks it. But the hosted server also sends that browser its page, so only use a hosted server you trust, or run your own (see [what a hostile server can do](#what-a-hostile-or-hacked-server-can-do)).
5. **There is an emergency stop.** The kill switch cuts off remote access in one click. Only someone at your machine can turn it back on.
6. **Everything is written down.** Every change goes into an audit log you can read in the panel.

You do not have to understand the rest to use Maestria safely. Use a long password, keep your recovery codes in a password manager, and use the kill switch if a phone is lost.

## What Maestria can and cannot do to your machine

- It **can**: show you what agents do, let you answer them, send a message to your Maestro, interrupt agents, and pause work near your usage limits.
- It **cannot**, by itself: delete floors, push code, or read your files. The only things it may do through Maestri Wire are on a short fixed list (see [Wire](wire.md#what-maestria-can-and-cannot-do-through-wire)).
- It **never changes** Maestri's usage files. That folder is read-only for Maestria.

**Read this part twice.** Anyone who is signed in to the panel, and any browser you approved in hosted mode, can **type a message to any of your agents and answer their Y/n questions**. An agent is a program that runs commands on your computer with your rights. So a signed-in session can, in practice, **make an agent run commands on your machine**. The fixed list above limits what Maestria does *itself*; it does not limit what an agent does when it is told to. Treat a signed-in session, and an approved browser, like a terminal on your computer:

- protect the password, the authenticator app and the recovery codes accordingly;
- approve only browsers you control;
- use the [kill switch](#the-kill-switch) the moment a phone or laptop with a signed-in session is lost.

## Local mode (self-hosted)

### What protects the panel

| Protection | What it means |
| --- | --- |
| **Loopback only** | The panel binds `127.0.0.1` and refuses anything else. This cannot be changed in settings, on purpose. To go beyond your machine, put a tunnel in front. |
| **Password** | Hashed with **argon2id** (a slow hash that makes guessing expensive). At least 12 characters. |
| **Two-factor, always** | A **TOTP** code (6 digits, 30 seconds, from an authenticator app). Each code works once. You get **10 recovery codes**, each usable once. |
| **Login limits** | By default, 5 failed sign-ins from one address, or 20 in total, lock sign-in for 15 minutes. A request counts as "you, at the machine" only when it is addressed to `127.0.0.1:<port>` or `localhost:<port>` and carries no proxy header (`X-Forwarded-For`, `Forwarded`, `Via`, `CF-…` and the like). Those requests are not refused by the global lock and are not counted into it, so strangers' failures never lock you out of the panel at your own machine. Everything else that reaches the panel over loopback through an HTTP tunnel or reverse proxy (Cloudflare Tunnel, ngrok http, Tailscale Funnel, nginx, Caddy) is counted per visitor address when the tunnel reports one, and otherwise shares one bucket, so strangers can lock *that* bucket, and with it other tunnel visitors, but not you at the machine. **A raw TCP forward is different** (`ssh -R`, `ngrok tcp`, `socat`): it passes the visitor's bytes through untouched, so a visitor can send `Host: 127.0.0.1:<port>` and no proxy header and cannot be told apart from you at the machine, and no code can fix that. Do not expose the panel through a raw TCP forward; use an HTTP tunnel, preferably Cloudflare Tunnel (see [Self-host](self-host.md)). |
| **Password-check limits** | Checking a password is expensive on purpose, so a stranger could use it to exhaust memory. Each address may start about 10 password checks a minute (the whole server about 300), and only 2 run at the same time. Past that the panel answers "try again in a moment" (HTTP `429` with `Retry-After`), which says nothing about whether the password was right. |
| **Real visitor address** | Behind a tunnel, every visitor reaches the panel from your own machine. When you allow a public host name, the panel reads the visitor's address from Cloudflare's `CF-Connecting-IP` header, so one stranger cannot lock everyone out. (Turn this off with `"trustCfConnectingIp": false` if your tunnel is not Cloudflare's and lets visitors set the header themselves.) |
| **No hints** | A wrong password and a wrong code look the same, so nobody learns which was right. |
| **Sessions** | A random cookie, stored on the server only as a hash. `HttpOnly`, `SameSite=Strict`, and `Secure` over https. By default it ends after 60 minutes idle or 12 hours in total. You can sign out everywhere (**Security** page, or `maestria auth reset-sessions`). |
| **CSRF protection** | Every change needs a secret token tied to your session, and a same-origin request. A page on another website cannot make your browser change anything. |
| **Strict CSP** | The panel's content-security rules forbid inline scripts and styles and any outside content. |
| **No CORS** | Other websites cannot read the panel's answers. |
| **Host allowlist** | The panel only answers to names you approved (`maestria config allow-host`). This defeats **DNS rebinding**, a trick where a hostile website points its own name at `127.0.0.1`. |
| **Cloudflare Access (optional)** | If you set it up, the panel also checks Cloudflare's signed token on every request. See [Self-host](self-host.md#3-add-cloudflare-access-optional). |
| **Audit log** | Every change, sign-in, refusal and kill-switch action is recorded in the local database and shown on the **Security** page. |
| **Private files** | Secrets are stored in files only you can read (`0600`). Maestria refuses to use them if their permissions are too open (on macOS and Linux). `config.json` holds commands that run (the tunnel's stop, start and status commands), so Maestria refuses it when anyone can write it and warns when its group can. On Windows these checks are not possible; keep the config folder writable only by you, especially if you set `MAESTRIA_CONFIG_DIR`. |

### The kill switch

`maestria tunnel off` (or the power button in the panel) does three things: it signs out every browser, it refuses every sign-in, and it runs your stop command for the tunnel if you set one. **Only the local command `maestria tunnel on` clears it.** There is no web page or API route that can turn it off. See [Self-host](self-host.md#4-the-kill-switch).

### Commands that need you at the keyboard

A few commands change who may control your machine: `maestria devices approve`, `maestria tunnel on`, linking the machine to a hosted server (`maestria connect --link …` or the code flow), and replacing the panel's password and authenticator (`maestria auth setup --force`, or `auth setup` when credentials already exist, which also covers the same choice in `maestria init`). Only a genuinely fresh install, where no setup was ever done, skips the question. That is read from the database, so deleting `auth-v2.json` does not make a replacement look like a first setup. On a computer that also runs AI agents, an agent that read a hostile web page or file could be talked into running them. So each one:

- **refuses unless it is run in an interactive terminal** (not from a script, a pipe or a service);
- prints what is about to happen and a **short random word**, and goes on only when you type that word back;
- has **no flag that skips this**, `--yes` included;
- writes the result (confirmed, wrong word, refused) to the audit log.

Be honest about the limit: an agent that has been given a real terminal session can read the word and type it. The word is random and printed at that moment so it cannot be put in a script or a prompt ahead of time, and every confirmation is audited, but this is friction and a trail, not a lock. Never tell an agent to run these commands for you.

## Hosted mode

### Who is trusted with what

The key idea: **the connector on your machine is the authority.** The server is a relay. It cannot make your machine run a request that a browser you approved did not sign. But it is **not** an innocent bystander: it sends your browser its web page and it sees what you look at while you look. Choose the server the way you would choose who to give a terminal on your computer to.

| Party | Trusted for | Not trusted for |
| --- | --- | --- |
| **Connector** (your machine) | Everything on that machine: your data, the rules, the allowlist, the kill switch | n/a |
| **Browser device key** | Signing the requests you make | n/a (the key cannot be exported from the browser) |
| **Server** | Accounts, sign-in, who is online, routing | Forging a request *without* a browser you approved, or reaching a route you did not allow. **Not** safe from: seeing what you view, showing you altered pages, and (through the page it serves) getting a key approved when you start a pairing. See below. |

### What a hostile or hacked server can do

Be clear-eyed about this. In hosted mode the server operator ships the code of the web page that runs in your browser and that signs your requests. So a dishonest or hacked server can, **without breaking any of the protocol's cryptography**:

- **Abuse a browser you already approved, while its page is open.** It can serve a hostile page that uses that browser's key to send anything on the allowlist: prompt your agents and answer their Y/n questions, which can make an agent run commands (see [above](#what-maestria-can-and-cannot-do-to-your-machine)).
- **Get its own key approved when you start a pairing.** Pairing asks you to compare a code and then confirm on your machine. The page that shows you the code is also served by the server, so a hostile page can pair a key it holds and show you the matching code. You would approve a key you think is your browser's.
- **See what you look at** (chat, tasks, docs, the audit log) while it passes through, and show you altered answers. Responses are not signed by your machine, so it could show you a harmless Y/n prompt while the real one is different. Read the full prompt text, and prefer to approve important prompts where you can see them on the machine.

What your machine still enforces whatever the server does: only requests signed by an approved key run; only the allowlisted routes; a rate limit; approvals that expire (14 days, 3 idle by default); a pairing attempt budget (see below); the kill switch; and a local audit log of every change and every pairing.

**If you do not trust the operator, do not use a server you do not control.** Run [your own hosted server](deploy-cloud.md), or use local mode, where the page comes from your own machine and none of this applies.

### Pairing: how attempts are limited

Approving a browser is the most sensitive step, so your machine limits it itself, whatever the server sends:

- At most **5 pairings offered per 10 minutes** and 20 per day.
- After a pairing whose key did not match its commitment, **no new pairing for an hour**.
- The comparison code is **8 digits** (`1234 · 5678`), so even the few allowed tries are hopeless to grind.
- 5 wrong codes typed into `maestria devices approve` within 10 minutes pause it.
- Every offer, refusal, failure and approval is in the local audit log. `maestria devices pending` shows how many pairings were offered recently, and a running connector prints a line when a browser asks to pair.
- `devices approve` shows the browser's name, its **key fingerprint**, the time and the code, and asks you to type a random word. The pairing page in your browser shows the same fingerprint. If you did not just start a pairing, do not approve it. `maestria devices list` shows the fingerprint of every approved browser.

### How a request is protected

1. Your browser holds a private key that **never leaves it**.
2. You approve that browser **on each machine** by comparing a short code and the key fingerprint (step 3 of [Use a hosted Maestria](hosted.md#3-approve-your-browser-on-the-machine)). The code is built so that a server that only relays cannot swap the key without the codes differing. A server that also serves the page can still do more: see [What a hostile or hacked server can do](#what-a-hostile-or-hacked-server-can-do).
3. Every request is **signed** by the browser, covering the method, path, query, body, machine, device, time and a one-time number.
4. Your machine checks the signature, the time (within 60 seconds), that the number was not used before, that the approval has not expired, that the action is on its **allowlist**, and that the device is not over its **rate limit**. Only then it runs the request.
5. The machine records every change in its own audit log, with the device, method, path and result, never the content.

So a server that only relays **cannot invent, change or replay a command**. It can only refuse to relay. (A server that also serves the page can do more; see the next section.)

### What the server stores

Your email, a hash of your password, your encrypted 2FA secret, your linked machines (name, system, version, last seen), the public keys of your approved browsers, and an account audit log. **No workspace data is stored.** What you look at passes through the server in clear while you look, is not logged or stored by Maestria's server, and could be read or changed by a hostile operator.

### The allowlist

Through the relay, the web app can read the panel's pages and data, answer tasks, chat, answer agent prompts, change the standby settings, turn the kill switch **on**, and revoke devices. It can **never**: sign in or out for you, set up passwords, approve a device, turn the kill switch **off**, change settings or Wire pairing, or reach anything not on the list. The full list is in [hosted-protocol.md](hosted-protocol.md#8-relay-allowlist-connector-side).

### Account security (server)

- Sign-up and sign-in require **two-factor**. An account without it cannot sign in.
- Sign-up is **by invitation** by default (`signup: invite`; `open` and `closed` exist). An invitation is a random 256-bit token, stored only as a hash, that works once, expires in 1 hour to 30 days, can be bound to one email address, and is spent by the sign-up that uses it. Every way it can fail (unknown, expired, used, revoked, wrong address) shows the same page, and a token is spent on every accepted sign-up (a new address, a pending one or an existing one), so holding one reveals nothing about who has an account. Administrators are made only from the server's command line (`maestria-cloud user promote`), never from the web; the web checks it on every request and shows the invitation pages to nobody else (404). Invitation emails are fixed text, link to the configured public URL, and are limited per address, per administrator and by the daily cap.
- The sign-up page answers the same whether or not an email is already used.
- Password reset needs the email link **and** a 2FA or recovery code. It never turns 2FA off. A reset signs out every session and makes every machine forget its approved browsers.
- Sign-in limits apply per address, per account and for the whole server, even for accounts that do not exist.
- Cookies use the `__Host-` prefix, `HttpOnly` and `SameSite=Strict`. The server uses the same CSRF, CSP and no-CORS rules as local mode, and answers `421` to unknown host names.

## Threat model summary

| Threat | What stops it |
| --- | --- |
| Stolen password | Two-factor is mandatory. Recovery codes work once. Reset needs 2FA too. |
| Password guessing | argon2id, per-address, per-account and global limits, lockouts, no hints. |
| Stolen session | `HttpOnly`, `SameSite=Strict`, `__Host-` prefix, hashed on the server, idle and absolute limits. |
| A malicious website using your browser | CSRF token and origin checks on every write. No CORS. |
| DNS rebinding | Host allowlist. |
| Hostile server forges or edits a command | Device signature. The connector refuses it. |
| Hostile server replays a command | 60-second window, one-time numbers, and the machine id inside the signature. |
| Relay-only server swaps a browser key | The code and key fingerprint you compare on your machine, and the pairing attempt budget. |
| **Hostile server that also serves the page** (pairs its own key, abuses an open session) | **Not stopped by the protocol.** Bounded by the allowlist, rate limit, short approvals, the pairing budget, the kill switch and the audit log. Run your own server, or use local mode. |
| Hostile server reaches other actions | The allowlist on your machine. |
| Hostile server reads or alters what you view | **Not stopped.** It stores none and logs no bodies, but it sees what passes through. Run your own server for anything sensitive. |
| An AI agent on your machine approves a device, links a cloud or lifts the kill switch | Those commands need an interactive terminal and a random word typed back, and are audited. Not a lock against an agent with a real terminal. |
| A stranger exhausts memory with sign-in attempts | Password checks are rate limited per address and globally, and at most 2 run at once. |
| A stranger locks you out | Visitor addresses are used behind a tunnel that reports them (Cloudflare). Behind an HTTP tunnel or proxy that does not, strangers share one "tunnel" bucket and can lock out other tunnel visitors, but not you at the machine (loopback Host, no proxy header). **Raw TCP forwards (`ssh -R`, `ngrok tcp`, `socat`) cannot be told apart from you at the machine**, and neither can a proxy that rewrites the Host to `127.0.0.1` and adds no header at all. Use an HTTP tunnel (Cloudflare Tunnel), or a proxy that adds `X-Forwarded-For`. |
| Stolen phone or browser | Revoke the device on the machine, or use the kill switch. Approvals expire (14 days, or 3 idle). |
| Leaked link code | Lasts 10 minutes, works once, needs confirmation at your machine. |
| Leaked connector credential | It only lets a machine connect. It cannot sign for a user. Revoke it in the web app. |
| Flooding | Limits on message size, requests in flight, per-device rate and cache size. |
| A tampered or swapped release download | SHA-256 check always. With cosign, a signature pinned to this repository's release workflow and the exact tag, plus build provenance ([Verify a download](#verify-a-download)). **Not stopped:** someone who can push a `v*` tag, and the updater on a machine without cosign. |
| Wire key theft | Keys are in a `0600` file, never sent to a browser. The read-only key cannot act. |

### The one limit we cannot remove

In hosted mode, **the server sends your browser the web page itself**. A server that has been hacked, or is run by someone dishonest, can always send a hostile page, and use a key you approved while the page is open, or get its own key approved when you start a pairing (see [What a hostile or hacked server can do](#what-a-hostile-or-hacked-server-can-do)). No protocol can prevent that. It is limited by the rules your machine enforces: the allowlist, a rate limit, short approvals, the pairing budget, the kill switch and the local audit log. The practical advice:

- **Use a hosted server you trust completely, or run your own** ([Run your own hosted server](deploy-cloud.md)). If you do not trust the operator, use local mode or self-host the cloud.
- Use the **kill switch** if something looks wrong.
- Check the **audit log** on the Security page from time to time.

Local mode has no such limit. The page comes from your own machine.

## Verify a download

A checksum file that sits next to the archive only proves the download is intact: whoever could replace the archive could replace the checksum too. So every release also carries proof of where it came from, made by the release workflow (`.github/workflows/release.yml`) with GitHub's keyless signing, which needs no secret key to be kept or leaked.

**What a release holds**

| File | What it is |
| --- | --- |
| `maestria_<version>_<os>_<arch>.tar.gz` / `.zip`, `maestria-cloud_…` | The programs. |
| `SHA256SUMS` | The SHA-256 of every archive. |
| `SHA256SUMS.sigstore.json` | A [cosign](https://docs.sigstore.dev/cosign/) signature on `SHA256SUMS`, with the certificate and the public log entry. |
| `maestria.rb` | The Homebrew formula. |
| (GitHub's attestation store) | A signed provenance statement for every file above: which workflow, which commit, which tag. |

**Check an archive by hand** (needs the [GitHub CLI](https://cli.github.com)):

```sh
gh attestation verify maestria_X.Y.Z_linux_amd64.tar.gz -R paulo-lucas/maestria \
  --signer-workflow paulo-lucas/maestria/.github/workflows/release.yml
```

It prints the commit the file was built from. A file that was built anywhere else, or changed afterwards, fails.

**Check `SHA256SUMS`** (needs cosign), then the archive against it:

```sh
VERSION=vX.Y.Z
cosign verify-blob --bundle SHA256SUMS.sigstore.json \
  --certificate-identity "https://github.com/paulo-lucas/maestria/.github/workflows/release.yml@refs/tags/$VERSION" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-github-workflow-repository paulo-lucas/maestria \
  --certificate-github-workflow-trigger push \
  SHA256SUMS
sha256sum --check --ignore-missing SHA256SUMS
```

The identity is the exact one, with the tag. A looser pattern would accept another release's signed file, or a workflow with the same file name in another repository.

**What the tools do for you**

- `install.sh`, `install.ps1` and `maestria update` / `maestria-cloud update` run exactly that `cosign verify-blob` check **when cosign is installed**, before they read a single line of `SHA256SUMS`. If cosign is installed and anything about the signature is missing or wrong, they stop. They never fall back to the checksum then, because a missing signature file would otherwise be a downgrade. If you run a mirror (`MAESTRIA_BASE_URL`), files come from it but the identity that must have signed them does not change. For the two installer scripts only, `MAESTRIA_REPO` changes the repository the identity names (a fork you chose to trust); the updater's repository is compiled in, and nothing in the environment can move it.
- **Without cosign** they print a clear warning and use the checksum alone. Set `MAESTRIA_REQUIRE_SIGNATURE=1` to make a missing cosign an error instead.
- Releases are signed with cosign v3. If your cosign is too old to read the bundle, the check fails with a message that says to upgrade; it does not install anyway.
- Downloads use https only, TLS 1.2 or newer, and a redirect may not leave https. The installer scripts run nothing until their last line, so a download cut short does nothing. The one-liners ask for the same on the `curl` and PowerShell side.
- Homebrew checks the SHA-256 in the formula. The workflow only publishes the formula to the tap after verifying its provenance and that every URL and checksum in it belongs to this repository's release.

**What this proves, and what it does not**

It proves the files were built by this repository's release workflow, from a `v*` tag, at the commit named in the provenance. It does **not** stop someone who can push a tag: a stolen token with write access can tag a malicious commit that leaves the workflow alone, and the workflow will build and sign it. The commit is recorded, so it is auditable after the fact, but it is not prevented. To narrow that:

- Add a tag ruleset (only maintainers can create `v*` tags) and require two-factor sign-in for them.
- Keep the Homebrew deploy key in a protected `homebrew` environment restricted to `v*` tags ([Releasing](releasing.md#publishing-the-homebrew-formula)).

Two more limits, both small:

- **The identity is made of names, not ids.** Never rename the owner or the repository: an old name that someone re-registers could produce a workflow that matches old one-liners. If a rename is ever needed, update every one-liner, the installers and the updater's identity first.
- **Rollback.** Pinning the exact tag stops anyone swapping another release's `SHA256SUMS` into this one. It does not stop someone with release rights marking an older, validly signed release as "latest". Updates never go backwards on their own (`--force` is needed), but a fresh install of "latest" follows whatever GitHub calls latest.

**The self-updater, honestly**

`maestria update` verifies the signature by running `cosign` when it is on the `PATH`; it does not verify sigstore bundles itself. Doing that means embedding Fulcio and Rekor trust roots, verifying the transparency-log proof and the certificate extensions, and surviving root rotation, several hundred lines of delicate crypto in the binary that updates itself, for a dependency-free tool. So today, **on a machine without cosign the updater trusts the checksum, which comes from the same GitHub release as the archive.** Someone who can publish to the repository could ship a release that machines update to. The hosted VM updates itself on a timer, so install cosign there and put `MAESTRIA_REQUIRE_SIGNATURE=1` in `/etc/maestria-cloud/env`: the check then runs on every update, and a cosign that later disappears is an error, not a quiet downgrade to the checksum ([Check who built each update](deploy-cloud.md#9-update)).

Proposed follow-up, the owner's decision: an ed25519 signature on `SHA256SUMS` with the public key built into the binary, signed with a key kept offline or in a protected environment. It is a few lines of standard library, and it is the only option that adds trust outside GitHub, at the cost of managing a key.

## Good habits

- Use a long, unique password, and keep your recovery codes in a password manager.
- Do not share a Maestria account.
- Revoke devices you no longer use: `maestria devices list` and `maestria devices revoke <id>`.
- Keep Maestria up to date ([Self-host: update](self-host.md#5-update)).
- Never post your `wire.json`, `auth-v2.json` or `cloud.json` anywhere. They are secrets.

## Reporting a problem

Found a security problem? Please **do not** open a public issue. Use GitHub's private reporting on the repository's **Security** tab ("Report a vulnerability"). Include what you did, what you expected, and what happened.

## More detail

- [hosted-protocol.md](hosted-protocol.md): the complete contract and threat model.
- [v2-spec.md](v2-spec.md) and [v2-architecture.md](v2-architecture.md): the decisions behind it.
