# Troubleshooting

Find the **message you see** (or what happens), then follow the fix. The messages are quoted exactly as Maestria prints them. Quick check first:

```sh
maestria status
maestria service status
maestria service logs
```

Jump to: [The panel](#the-panel) · [Signing in](#signing-in) · [Maestri Wire](#maestri-wire) · [Hosted mode: your machine](#hosted-mode-your-machine) · [Hosted mode: the browser](#hosted-mode-the-browser) · [Hosted server (for operators)](#hosted-server-for-operators) · [Commands](#commands) · [Usage](#usage)

## The panel

### `Server: down (http://127.0.0.1:4777 not reachable — start with: maestria serve)`

The panel is not running.

1. Start it as a service: `maestria service start`. Or in the foreground: `maestria serve`.
2. Still down? `maestria service logs` shows why.

### The page at `http://127.0.0.1:4777` does not load

- Run `maestria status`. If it says `Server: running`, the panel is up and your browser is the problem. Try another browser.
- The panel might use another port. Look for `"port"` in `config.json` (in `~/.config/maestria` or `%AppData%\maestria`). The default is `4777`.
- Another program might hold the port. Set a free one: `"port": 4780` in `config.json`, then `maestria service restart`.
- Use `127.0.0.1` or `localhost`. Your machine's network address does not work. This is on purpose: the panel never listens outside your machine.

### `Misdirected request: this Maestria panel does not answer to <host>.`

You opened the panel with a name it does not know (your tunnel address, for example). It refuses unknown names to stop DNS rebinding.

On the machine that runs Maestria:

```sh
maestria config allow-host <host>
maestria service restart
```

Use the host only, like `maestria.example.com` (no `https://`). Check the list with `maestria config hosts`. See [Self-host](self-host.md#step-6-tell-maestria-about-the-address).

### `another Maestria process is already running on this config directory: maestria serve (pid 1234, started 09:41).`

Only one `maestria serve` or `maestria connect` may run per config directory. You started a second one, perhaps next to the background service.

- Stop the first one: `maestria service stop`. Or close the terminal where `maestria serve` runs.
- Or just use the one that is already running. The message says its process number.
- On purpose, to run two: give the second its own `MAESTRIA_CONFIG_DIR`.

Commands such as `post`, `say`, `wait`, `status` and `devices` are not affected. They run next to the panel.

The lock clears by itself if the first process died. It goes stale after about 90 seconds with no sign of life.

### `maestria listening on http://127.0.0.1:4777` then `cookieSecure is on: sign in over https (your tunnel). For plain http in Safari, set "cookieSecure": false in config.json.`

Not an error. It explains the next item.

### `config: unknown key "listen" (note: the listen address is fixed to 127.0.0.1)`

`config.json` has a setting that does not exist. The address the panel listens on is fixed on purpose and cannot be set. Remove the key. Other typos give a similar message that names the key.

### `file permissions are too open: <file> has permissions 644; it must be 600 (run: chmod 600 <file>)`

Maestria refuses to read a secrets file that other users could read. Run the `chmod 600` command it shows. This applies to `config.json` on a hosted server, `auth-v2.json`, `wire.json`, `cloud.json` and `server.key`.

### The background service does not start

1. `maestria service status`: is it installed?
2. `maestria service logs -n 200`.
3. Reinstall: `maestria service uninstall`, `maestria service install`, `maestria service start`.
4. On Linux, the service is a `systemd --user` unit. It runs while you are logged in.

## Signing in

### I cannot sign in on `http://127.0.0.1:4777` in Safari

Safari does not keep the panel's `Secure` cookie on plain `http`. Use Chrome or Firefox for that address, or open `config.json` and set:

```json
{ "cookieSecure": false }
```

Then `maestria service restart`. Leave it `true` for any `https` address (a tunnel).

### `no credentials configured. Run "maestria auth setup" first`

`maestria serve` needs a password and an authenticator app. Run:

```sh
maestria auth setup
```

### `Too many failed attempts. Try again in 15 minutes.`

Sign-in is locked after too many wrong attempts (5 from one address, or 20 in total, by default). Wait for the time it says. A correct password does not help while the lock is on.

### I lost my phone or my authenticator app

Use one of your **recovery codes** on the second sign-in page. Each works once. Then set up a new authenticator: on the machine, `maestria auth setup --force` (it asks for a new password too, and signs out every session).

### I have no recovery codes left and cannot sign in (local mode)

On the machine that runs Maestria:

```sh
maestria auth setup --force
```

You choose a new password, scan a new QR code and get new recovery codes. Whoever has the machine can do this. That is why your machine's own security matters.

### `Sign-in refused: kill switch is on` (in the audit log), or the panel signs everyone out

The kill switch is on. On the machine:

```sh
maestria tunnel status
maestria tunnel on
```

Then sign in again. See [Self-host: the kill switch](self-host.md#4-the-kill-switch).

### I forgot my password (hosted)

Choose **Forgot password?** on the log-in page. You need the email link and a code from your authenticator app, or a recovery code. After a reset, every browser must be approved again on each machine.

## Maestri Wire

Start with `maestria wire status`. The `Link:` line tells the state. The panel shows the same on its pages.

### `Maestri Wire isn't connected`

Not paired. In Maestri turn Wire on (**Settings → Wire**), then run `maestria wire pair`. See [Wire](wire.md).

### `Maestri Wire is off or can't be reached`

Maestri is not running, or Wire is off. In Maestri open **Settings → Wire** and switch it on. Maestria keeps trying by itself. Nothing is stale: the data is hidden until Wire returns.

### `Maestri's security key changed`

State `pin_mismatch`. Maestria pinned the key of your Maestri when you paired, and the one that answers now is different. It refuses to talk.

- If **you** reset Wire in Maestri: `maestria wire unpair`, then `maestria wire pair`.
- If you did **not**, do not re-pair. Something else is answering where Maestri should be.

### `Maestri no longer accepts this connector`

State `revoked`. The device was removed in Maestri, or it expired after 30 idle days. Get a new code in **Settings → Wire → Pairing**, then `maestria wire pair`.

### `This Maestri Wire version isn't supported yet`

Wire is in beta and its protocol changed. Update Maestria ([Self-host: update](self-host.md#5-update)).

### `Wire access has the wrong rights`

The read-only device has more rights than expected. `maestria wire unpair`, then `maestria wire pair`.

### `Maestri Wire isn't connected, so nothing was sent.`

You tried to send a chat message with Wire off. Fix Wire (above) and send again.

### While pairing

| Message | Fix |
| --- | --- |
| `Wrong pairing code or password.` | Codes last 5 minutes and work once. Read a fresh one. |
| `Too many attempts. Wait … and try again.` | Wait. |
| `The security key does not match the host's certificate. Copy it again from Maestri: Settings → Wire → Manual → Security key.` | Copy the key again. |
| `Could not reach Maestri Wire. Check that it is on in Settings → Wire, and the host and port.` | Turn Wire on, check the host. The default is `localhost:7434`. |
| `Password pairing is not enabled on this host. Use a pairing code from Settings → Wire → Pairing.` | Use a code. |
| `That looks like the 6-digit pairing code: it comes in the next step. Here we need the long security key.` | Paste the long key from the Manual tab. |
| `That key does not match the one Wire presents on this machine. Check the Manual tab and try again.` | The key you typed is not the one in Maestri. Copy it again. |

## Hosted mode: your machine

### `No cloud to link to. Pass --cloud https://your-maestria-cloud (or set "cloud" in config.json).`

Add the server address: `maestria connect --link CODE --cloud https://maestria.example.com`.

### `the cloud URL must be https (plain http is only allowed for localhost)`

Use an `https://` address. Plain `http` only works for `localhost`, for testing.

### `this machine is already linked to <cloud> as "<name>"; run "maestria connect unlink" first to link it somewhere else`

Each machine links to one server. Unlink first: `maestria connect unlink`.

### `the code expired; run the command again`

Link codes last 10 minutes and work once. Make a new one (**Connectors → Link a machine**, or run `maestria connect` again).

### `cancelled; nothing was linked`

You answered no, or pressed Ctrl+C, at the confirmation. Run the command again.

### `This machine was unlinked or revoked in the cloud. Run "maestria connect unlink", then link it again.`

Someone chose **Revoke** on the Connectors page, or the account was disabled. Run `maestria connect unlink`, then link again.

### `Disconnected from the cloud (…); retrying.`

The connector lost its connection and will retry on its own (waiting 1 second, then more, up to 1 minute). Check your internet connection and `maestria connect status`. If it never reconnects, the server may be down. The reason is shown in brackets.

### `maestria serve (pid 1234) is already running on this config directory. Restart it ("maestria service restart", or stop and start it) and it will connect to the cloud as well.`

You linked while `maestria serve` was running. It is fine. Restart it and it adds the cloud connection.

### `this machine is not linked to a hosted Maestria. Run "maestria connect --link CODE --cloud URL" first`

You used `maestria serve --connect` before linking. Link first.

### `the relay is turned off in config.json ("relay": {"enabled": false}); turn it on or drop --connect`

Set `"relay": { "enabled": true }` in `config.json`, or leave out `--connect`.

### `no pairing is waiting with that code`

`maestria devices approve <code>` found nothing with that code.

- The code lasts 5 minutes. Start again from the web app.
- You typed it for the wrong machine. Each machine has **its own code**.
- Run `maestria devices pending` and use the code it shows.

### `too many pairings are waiting; approve or reject one first`

Up to 5 can wait at once. `maestria devices pending`, then `approve` or `reject` them.

### `more than one pairing matches that code; reject the extra ones first`

Rare. `maestria devices reject <code>` for the ones you do not recognise.

## Hosted mode: the browser

These are the messages the web app shows when a machine refuses a request.

| You see | What it means | What to do |
| --- | --- | --- |
| **No machine online** | None of your machines is connected. | On the machine, run `maestria connect status`. Start it with `maestria service start`, or `maestria connect`. |
| **That machine is offline.** (`connector_offline`) | That machine is not connected. Nothing is queued. | Check it is on, has internet and runs `maestria connect` or `maestria serve`. |
| **Approve this browser** / **This browser is not approved on that machine yet.** (`unknown_device`) | This browser has no approval there. | [Approve it](hosted.md#3-approve-your-browser-on-the-machine). |
| **This browser's approval on that machine expired.** (`approval_expired`) | Approvals last 14 days by default. | Approve again. |
| **This browser was not used for a while** (`idle_expired`) | Unused for 3 days by default. | Approve again. |
| **This machine paused pairing after too many tries.** (`pairing_paused`) | The machine allows 5 pairings per 10 minutes, and pauses for an hour after a key that did not match. | Wait, then start again. `maestria devices pending` says how long. If you did not cause it, see the audit log (`pair_*` entries) and consider [who runs your server](security.md#what-a-hostile-or-hacked-server-can-do). |
| `this changes who can control this machine, so it must be run by you in an interactive terminal` | `devices approve`, `tunnel on` and linking a machine only run in a terminal you type in, and ask for a random word. | Run the command yourself in a terminal window. It cannot be scripted. |
| **Too many sign-in attempts right now.** (`429`) | The panel limits password checks (about 10 a minute per address, 2 at a time). It is not a lockout and does not mean your password was wrong. | Wait a moment and try again. |
| **This browser was signed out of that machine.** (`device_revoked`) | Someone revoked it there. | Approve again if it was a mistake. |
| **The kill switch is on for that machine.** (`kill_switch`) | The machine refuses everything. | On that machine: `maestria tunnel on`. |
| **Remote access is turned off on that machine.** (`relay_disabled`) | `relay.enabled` is `false` there. | Set it to `true` in that machine's `config.json`. |
| **This device's clock is off by more than a minute.** (`stale_timestamp`) | The clocks differ by more than 60 seconds. | Turn on automatic time on both the phone or computer with the browser **and** the machine you are reaching. The server's clock does not matter here. |
| **Too many requests. Wait a moment.** (`rate_limited`) | The device went over its limit (120 a minute by default; changes count double). | Wait a minute. The limit is `relay.ratePerMinute` in that machine's `config.json`. |
| **That machine is busy. Try again in a moment.** (`connector_busy`) | Too many requests in flight. | Wait and retry. |
| **That machine did not answer in time.** (`gateway_timeout`) | It took more than 30 seconds. | Retry. Check the machine is not asleep. |
| **That machine does not allow this action from here.** (`route_forbidden`) | The action is not on the allowlist for remote use. | Do it on the machine. It is blocked on purpose. |
| **The request was not signed.** (`unsigned`) / **The machine did not accept this browser's signature.** (`bad_signature`) | The browser's key is missing or wrong. This happens after clearing the browser's site data, or in a private window. | Approve the browser again. Do not use private windows. |

**I approved my phone but it asks again every time.** Browsers store the key in site data. If your phone clears that (private mode, "clear history"), the key is lost. Approve again and keep site data.

**The machine is online but I see no workspaces.** Check that you ran `maestria init` and added workspaces (`maestria workspace list`). Wire shows live agents.

## Hosted server (for operators)

### `Misdirected request` (HTTP 421) when you open the server

The host name does not match `publicUrl`. Open the server by the exact name in `MAESTRIA_CLOUD_PUBLIC_URL`, not by its IP address. To allow more names, set `allowedHosts` in `config.json`. Only `/healthz` ignores this.

### `publicUrl must be https (plain http is only allowed for localhost)`

Set `MAESTRIA_CLOUD_PUBLIC_URL` to your `https://` address.

### `warning: publicUrl is not set; set it so cookies, email links and the Host allowlist are right`

Set `MAESTRIA_CLOUD_PUBLIC_URL`.

### `warning: no email configured (smtp.* or devEmail); sign-up and password reset cannot send mail`

Set the `MAESTRIA_CLOUD_SMTP_*` variables ([Run your own hosted server, step 4](deploy-cloud.md#4-set-up-email)).

### Sign-up emails do not arrive

1. `journalctl -u maestria-cloud -n 100` and look for mail errors.
2. The sender address must be on a domain verified with your mail provider.
3. Port `465` needs `MAESTRIA_CLOUD_SMTP_TLS=true`. Port `587` uses STARTTLS.
4. `the SMTP server does not offer STARTTLS; refusing to send credentials in clear text`: pick a provider and port that offer TLS.
5. For a quick test, `MAESTRIA_CLOUD_DEV_EMAIL=stdout` prints the mail in the log.
6. Check the spam folder.

### Everyone is locked out ("Too many failed attempts")

The global limit (100 failures per 15 minutes by default) was reached, perhaps because every visitor looks like the tunnel. Set `MAESTRIA_CLOUD_TRUST_PROXY=cf` when you use a Cloudflare Tunnel.

### Nobody can sign in after a restore

`server.key` is missing or from another install. It must be the one that went with the database. Without it, two-factor secrets cannot be read.

## Commands

### `no registered workspace covers <folder> (pass --workspace <id>)`

The folder you are in is not in a registered workspace. Add it: `maestria workspace add <name> <path>`. Or pass `-w <id>`. `maestria workspace list` shows the ids.

### `invalid inbox input: choice requires 2..4 options`

A `choice` task needs 2 to 4 `--option` values.

### `floor "<name>" not found`

The floor does not exist in that workspace. `maestria floor list --all`.

### `credentials already exist. Re-run with --force to replace them (this signs out every session)`

`maestria auth setup` will not overwrite your password by accident. Use `--force` if you mean it.

## Usage

### `Usage: unavailable (missing_status_file: Waiting for usage telemetry)`

Maestri has not written its usage file yet. In Maestri open **Settings → Agents → Usage** and turn usage on. Maestria never writes to that folder.

### `Maestri data directory not found` or `Usage providers folder missing`

Maestria cannot find Maestri's data. Check the folder: `~/.maestri`, or `MAESTRI_HOME` if you set it. If it is elsewhere, set `"maestriDir"` in `config.json`, or run `maestria init --maestri-dir <path>`.

## Still stuck?

- Run `maestria --help` and check the exact command.
- Look at the logs: `maestria service logs`.
- Ask in the project's discussions, and include the exact message and `maestria version`. **Remove secrets, hostnames and personal paths first.**
