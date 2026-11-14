# Security

Maestria lets you steer AI agents that run commands on your computer. That is powerful, so it is built to be hard to misuse. This page explains how, first in plain words and then in detail.

## In plain words

1. **Your data stays on your machine.** Tasks, chat, floors and docs live in a small database on your computer. A hosted server never stores them.
2. **Nothing listens to the internet.** The panel only answers on `127.0.0.1`, an address that works from your own machine and nowhere else. The hosted setup connects *out*. No port is opened.
3. **Signing in takes two things.** A password you know and a 6-digit code from an app you own. There are no accounts without the code.
4. **Your machine has the last word.** In the hosted setup the website cannot give orders by itself. Each order is signed by a browser *you approved on your own machine*, and your machine checks it.
5. **There is an emergency stop.** The kill switch cuts off remote access in one click. Only someone at your machine can turn it back on.
6. **Everything is written down.** Every change goes into an audit log you can read in the panel.

You do not have to understand the rest to use Maestria safely. Use a long password, keep your recovery codes in a password manager, and use the kill switch if a phone is lost.

## What Maestria can and cannot do to your machine

- It **can**: show you what agents do, let you answer them, send a message to your Maestro, interrupt agents, and pause work near your usage limits.
- It **cannot**: run arbitrary commands, delete floors, push code, or read your files. The only things it may do through Maestri Wire are on a short fixed list (see [Wire](wire.md#what-maestria-can-and-cannot-do-through-wire)).
- It **never changes** Maestri's usage files. That folder is read-only for Maestria.

## Local mode (self-hosted)

### What protects the panel

| Protection | What it means |
| --- | --- |
| **Loopback only** | The panel binds `127.0.0.1` and refuses anything else. This cannot be changed in settings, on purpose. To go beyond your machine, put a tunnel in front. |
| **Password** | Hashed with **argon2id** (a slow hash that makes guessing expensive). At least 12 characters. |
| **Two-factor, always** | A **TOTP** code (6 digits, 30 seconds, from an authenticator app). Each code works once. You get **10 recovery codes**, each usable once. |
| **Login limits** | By default, 5 failed sign-ins from one address, or 20 in total, lock sign-in for 15 minutes. |
| **No hints** | A wrong password and a wrong code look the same, so nobody learns which was right. |
| **Sessions** | A random cookie, stored on the server only as a hash. `HttpOnly`, `SameSite=Strict`, and `Secure` over https. By default it ends after 60 minutes idle or 12 hours in total. You can sign out everywhere (**Security** page, or `maestria auth reset-sessions`). |
| **CSRF protection** | Every change needs a secret token tied to your session, and a same-origin request. A page on another website cannot make your browser change anything. |
| **Strict CSP** | The panel's content-security rules forbid inline scripts and styles and any outside content. |
| **No CORS** | Other websites cannot read the panel's answers. |
| **Host allowlist** | The panel only answers to names you approved (`maestria config allow-host`). This defeats **DNS rebinding**, a trick where a hostile website points its own name at `127.0.0.1`. |
| **Cloudflare Access (optional)** | If you set it up, the panel also checks Cloudflare's signed token on every request. See [Self-host](self-host.md#3-add-cloudflare-access-optional). |
| **Audit log** | Every change, sign-in, refusal and kill-switch action is recorded in the local database and shown on the **Security** page. |
| **Private files** | Secrets are stored in files only you can read (`0600`). Maestria refuses to use them if their permissions are too open (on macOS and Linux). |

### The kill switch

`maestria tunnel off` (or the power button in the panel) does three things: it signs out every browser, it refuses every sign-in, and it runs your stop command for the tunnel if you set one. **Only the local command `maestria tunnel on` clears it.** There is no web page or API route that can turn it off. See [Self-host](self-host.md#4-the-kill-switch).

## Hosted mode

### Who is trusted with what

The key idea: **the connector on your machine is the authority.** The server is a relay that is not trusted for anything that matters.

| Party | Trusted for | Not trusted for |
| --- | --- | --- |
| **Connector** (your machine) | Everything on that machine: your data, the rules, the allowlist, the kill switch | n/a |
| **Browser device key** | Signing the requests you make | n/a (the key cannot be exported from the browser) |
| **Server** | Accounts, sign-in, who is online, routing | Forging a command, reading your data, reaching a route you did not allow, choosing which keys are approved |

### How a request is protected

1. Your browser holds a private key that **never leaves it**.
2. You approve that browser **on each machine** by comparing a short code (step 3 of [Use a hosted Maestria](hosted.md#3-approve-your-browser-on-the-machine)). The code is built so that a hostile server cannot swap the key without the codes differing.
3. Every request is **signed** by the browser, covering the method, path, query, body, machine, device, time and a one-time number.
4. Your machine checks the signature, the time (within 60 seconds), that the number was not used before, that the approval has not expired, that the action is on its **allowlist**, and that the device is not over its **rate limit**. Only then it runs the request.
5. The machine records every change in its own audit log, with the device, method, path and result, never the content.

So a hostile or hacked server **cannot invent, change or replay a command**. It can only refuse to relay.

### What the server stores

Your email, a hash of your password, your encrypted 2FA secret, your linked machines (name, system, version, last seen), the public keys of your approved browsers, and an account audit log. **No workspace data.** What you look at passes through while you look, and is not logged or stored.

### The allowlist

Through the relay, the web app can read the panel's pages and data, answer tasks, chat, answer agent prompts, change the standby settings, turn the kill switch **on**, and revoke devices. It can **never**: sign in or out for you, set up passwords, approve a device, turn the kill switch **off**, change settings or Wire pairing, or reach anything not on the list. The full list is in [hosted-protocol.md](hosted-protocol.md#8-relay-allowlist-connector-side).

### Account security (server)

- Sign-up and sign-in require **two-factor**. An account without it cannot sign in.
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
| Hostile server swaps a browser key | The code you compare on your machine. |
| Hostile server reaches other actions | The allowlist on your machine. |
| Hostile server reads your data | It stores none, and logs no bodies. |
| Stolen phone or browser | Revoke the device on the machine, or use the kill switch. Approvals expire (30 days, or 7 idle). |
| Leaked link code | Lasts 10 minutes, works once, needs confirmation at your machine. |
| Leaked connector credential | It only lets a machine connect. It cannot sign for a user. Revoke it in the web app. |
| Flooding | Limits on message size, requests in flight, per-device rate and cache size. |
| Wire key theft | Keys are in a `0600` file, never sent to a browser. The read-only key cannot act. |

### The one limit we cannot remove

In hosted mode, **the server sends your browser the web page itself**. A server that has been hacked, or is run by someone dishonest, could send a hostile page to a browser you already approved, and use its key while the page is open. No protocol can prevent that. It is limited by the rules your machine enforces: the allowlist, a rate limit, short approvals, the kill switch and the local audit log. The practical advice:

- **Use a hosted server you trust**, or run your own ([Run your own hosted server](deploy-cloud.md)).
- Use the **kill switch** if something looks wrong.
- Check the **audit log** on the Security page from time to time.

Local mode has no such limit. The page comes from your own machine.

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
