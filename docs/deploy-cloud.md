# Run your own hosted server

This guide is for people who want to **run a Maestria server** for themselves or their team. If you only want to *use* one, read [Use a hosted Maestria](hosted.md).

The server is a program called **`maestria-cloud`**. It handles accounts, sign-in (with mandatory two-factor), linking machines, and relaying. **It stores no workspace data.** See [Security](security.md) for what that means.

You will end up with:

- a small free server (a **VM**, a computer in a data centre) running `maestria-cloud`,
- a **Cloudflare Tunnel** that gives it an `https://` address and keeps the VM's ports closed,
- email for sign-up links,
- an account for yourself, and a machine linked to it.

**Time needed:** about an hour.

> **Why a VM, and not Cloudflare Workers?** Workers cannot run this server. It is a Go program with a SQLite database that holds long-lived WebSocket connections open. The free path is **a small VM + a Cloudflare Tunnel**.

## What you need

- A free [Cloudflare](https://dash.cloudflare.com/sign-up) account, with a **domain** added to it. Below we use `maestria.example.com`. **Use your own.**
- A free VM (step 1).
- An email service that can send mail by SMTP (step 4).
- Basic comfort typing commands in an SSH session.

## 1. Get a free VM

Any small Linux VM works. The server uses very little memory. Two free options (check each provider's current terms, because free offers change):

| Provider | Free VM | Notes |
| --- | --- | --- |
| **Oracle Cloud "Always Free"** | An Ampere A1 (ARM) or AMD micro instance | Needs a credit card to sign up. Pick an `Ubuntu 24.04` image. |
| **Google Cloud** | `e2-micro` in a US free-tier region | Needs a billing account. Pick `Ubuntu 24.04` and a standard disk. |

Create the VM, add your SSH key, and connect:

```sh
ssh ubuntu@<VM-IP-ADDRESS>
```

**You do not need to open any port** other than SSH. Keep HTTP and HTTPS closed: the tunnel makes an outgoing connection, so nothing needs to reach the VM from outside.

Check your processor type, because it decides which download you need:

```sh
uname -m
```

`x86_64` means `amd64`. `aarch64` means `arm64`.

## 2. Get the program

**Option A: download a release (easiest).** Replace `VERSION` with the release you want (for example `0.2.0`) and `ARCH` with `amd64` or `arm64`:

```sh
cd /tmp
curl -fsSLO https://github.com/maestria-dev/maestria/releases/download/vVERSION/maestria-cloud_VERSION_linux_ARCH.tar.gz
curl -fsSLO https://github.com/maestria-dev/maestria/releases/download/vVERSION/SHA256SUMS
grep " maestria-cloud_VERSION_linux_ARCH.tar.gz" SHA256SUMS | sha256sum -c -
```

**What you should see:** `maestria-cloud_…tar.gz: OK`. If it says anything else, stop. The file is not intact.

Then install it:

```sh
tar -xzf maestria-cloud_VERSION_linux_ARCH.tar.gz maestria-cloud
sudo install -m 0755 maestria-cloud /usr/local/bin/maestria-cloud
maestria-cloud version
```

**Option B: build from source.** You need Go 1.27 or newer ([go.dev/dl](https://go.dev/dl/)):

```sh
git clone https://github.com/maestria-dev/maestria.git
cd maestria
go build -trimpath -o maestria-cloud ./cmd/maestria-cloud
sudo install -m 0755 maestria-cloud /usr/local/bin/maestria-cloud
```

## 3. Create a user and a folder

The server should not run as `root`. Make a dedicated user with no login:

```sh
sudo useradd --system --home-dir /var/lib/maestria-cloud --shell /usr/sbin/nologin maestria
sudo install -d -o maestria -g maestria -m 0700 /var/lib/maestria-cloud
sudo install -d -m 0755 /etc/maestria-cloud
```

`/var/lib/maestria-cloud` is the **data directory**. It holds the database and the server key.

## 4. Set up email

The server sends mail for **email verification** and **password resets**. Without mail, nobody can sign up.

Many providers have a free plan with an SMTP login. Examples (check their current limits):

- Brevo (formerly Sendinblue)
- Resend
- SMTP2GO
- Mailjet

In your provider's dashboard, create an **SMTP login** and note four things: the **host**, the **port**, the **username** and the **password** (sometimes called an API key or SMTP key). You also need a **From address** on a domain you control and have **verified** with the provider (this means adding a couple of DNS records). Unverified senders end up in spam.

Ports: `587` uses **STARTTLS** (the default here). `465` uses implicit TLS and needs `SMTP_TLS=true`. The server refuses to send your password in clear text, so a server without STARTTLS or TLS will not work.

## 5. Write the settings

Settings come from two places. **Environment variables win** over `config.json`. Keep secrets such as the SMTP password in an **environment file** that only root can read.

Create `/etc/maestria-cloud/env`:

```sh
sudo nano /etc/maestria-cloud/env
```

```ini
# Where the data lives.
MAESTRIA_CLOUD_DIR=/var/lib/maestria-cloud

# The address people will type. It must be https. It sets the allowed Host,
# the cookie names and the links in emails.
MAESTRIA_CLOUD_PUBLIC_URL=https://maestria.example.com

# Listen on this machine only. The tunnel connects here.
MAESTRIA_CLOUD_LISTEN=127.0.0.1:8080

# We sit behind a Cloudflare Tunnel, so trust Cloudflare's client-address header.
MAESTRIA_CLOUD_TRUST_PROXY=cf

# Email.
MAESTRIA_CLOUD_SMTP_HOST=smtp.mail.example.com
MAESTRIA_CLOUD_SMTP_PORT=587
MAESTRIA_CLOUD_SMTP_USER=your-smtp-login
MAESTRIA_CLOUD_SMTP_PASSWORD=your-smtp-password
MAESTRIA_CLOUD_SMTP_FROM=maestria@example.com
```

Lock the file:

```sh
sudo chmod 0600 /etc/maestria-cloud/env
```

### All settings

Each setting can be an environment variable or a key in `/var/lib/maestria-cloud/config.json` (which must be readable only by its owner, `0600`). Unknown keys in `config.json` are refused.

| `config.json` key | Environment variable | Default | What it does |
| --- | --- | --- | --- |
| `listen` | `MAESTRIA_CLOUD_LISTEN` | `127.0.0.1:8080` | Address to listen on. Keep it on `127.0.0.1` and put a tunnel or proxy in front. |
| `publicUrl` | `MAESTRIA_CLOUD_PUBLIC_URL` | none | The `https://` address of the server. Plain `http` is accepted only for `localhost`. **Set it.** |
| `allowedHosts` | none | none | More host names the server answers to (a list). The public URL's host is always allowed. |
| `trustProxy` | `MAESTRIA_CLOUD_TRUST_PROXY` | none | `cf` (Cloudflare's `CF-Connecting-IP`), `xff` (`X-Forwarded-For`), or empty. Set it right, or every visitor looks like the tunnel. |
| `proxyHops` | none | `1` | How many trusted proxies sit in front (only for `xff`). |
| `sessionIdleMinutes` | none | `720` (12 h) | Sign-out after this long without use. |
| `sessionAbsoluteHours` | none | `168` (7 days) | Sign-out after this long in total. |
| `login.maxFailuresPerIp` | none | `5` | Failed sign-ins per address before a lockout. |
| `login.maxFailuresPerAccount` | none | `8` | Failed sign-ins per account. |
| `login.maxFailuresGlobal` | none | `100` | Failed sign-ins for the whole server. |
| `login.windowSeconds` | none | `900` | The time window for counting failures. |
| `login.lockoutSeconds` | none | `900` | How long a lockout lasts. |
| `smtp.host` | `MAESTRIA_CLOUD_SMTP_HOST` | none | Mail server. |
| `smtp.port` | `MAESTRIA_CLOUD_SMTP_PORT` | `587` | Mail port. |
| `smtp.username` | `MAESTRIA_CLOUD_SMTP_USER` | none | Mail login. |
| `smtp.password` | `MAESTRIA_CLOUD_SMTP_PASSWORD` | none | Mail password. |
| `smtp.from` | `MAESTRIA_CLOUD_SMTP_FROM` | none | The "From" address. Required with a host. |
| `smtp.tls` | `MAESTRIA_CLOUD_SMTP_TLS` | `false` | `true` for implicit TLS (usually port 465). |
| `devEmail` | `MAESTRIA_CLOUD_DEV_EMAIL` | none | `stdout` or `file:<path>`: print mail instead of sending it. **For trying things out only.** |
| none | `MAESTRIA_CLOUD_DIR` | `~/.config/maestria-cloud` (`%AppData%\maestria-cloud` on Windows) | The data directory. |

The server warns at start-up if `publicUrl` or email is missing.

**Trying it without email?** Set `MAESTRIA_CLOUD_DEV_EMAIL=stdout`. The verification links appear in the server log, where you can copy them. Do not leave it on for real users.

## 6. Run it as a service (systemd)

A **systemd unit** makes the server start at boot and restart if it crashes. Create the file:

```sh
sudo nano /etc/systemd/system/maestria-cloud.service
```

Paste this:

```ini
[Unit]
Description=Maestria hosted server
Documentation=https://github.com/maestria-dev/maestria
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=maestria
Group=maestria
EnvironmentFile=/etc/maestria-cloud/env
ExecStart=/usr/local/bin/maestria-cloud serve
Restart=on-failure
RestartSec=5
TimeoutStopSec=15

# Keep the server in a small box.
NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LockPersonality=true
ReadWritePaths=/var/lib/maestria-cloud
UMask=0077

[Install]
WantedBy=multi-user.target
```

Start it:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now maestria-cloud
sudo systemctl status maestria-cloud
```

**What you should see:** `active (running)`. Then:

```sh
curl -s http://127.0.0.1:8080/healthz
```

**What you should see:** `{"status":"ok"}`.

Read the log any time with `journalctl -u maestria-cloud -f`.

## 7. Add the Cloudflare Tunnel

Now give the server its public address. This is the same tunnel you would use for [self-hosting](self-host.md#2-reach-it-from-your-phone-with-a-cloudflare-tunnel), with one difference: it points at port `8080`.

**Install `cloudflared` on the VM.** Follow [Cloudflare's instructions](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/) for Ubuntu (Debian package).

**Create the tunnel**, as your normal user:

```sh
cloudflared tunnel login
cloudflared tunnel create maestria
cloudflared tunnel route dns maestria maestria.example.com
```

Note the **tunnel ID** it prints.

**Write the config** into the system folder, where the service looks for it. Copy the credentials file there too:

```sh
sudo mkdir -p /etc/cloudflared
sudo cp ~/.cloudflared/<TUNNEL-ID>.json /etc/cloudflared/
sudo nano /etc/cloudflared/config.yml
```

```yaml
tunnel: <TUNNEL-ID>
credentials-file: /etc/cloudflared/<TUNNEL-ID>.json

ingress:
  - hostname: maestria.example.com
    service: http://127.0.0.1:8080
  - service: http_status:404
```

**Run it as a service:**

```sh
sudo cloudflared service install
sudo systemctl enable --now cloudflared
```

WebSockets work through the tunnel with no extra setting, and the connectors need them.

**Check from anywhere:**

```sh
curl -s https://maestria.example.com/healthz
```

**What you should see:** `{"status":"ok"}`. Then open `https://maestria.example.com` in a browser. **What you should see:** the Maestria log-in page.

> **Do not put Cloudflare Access in front of this address.** Machines dial in with their own credential and cannot pass an Access login. Accounts already have a password and mandatory two-factor sign-in.

## 8. First steps as administrator

There is no separate administrator account. The server has accounts, and you manage them from the command line **on the VM**.

1. **Create your own account** in the browser: **Create an account**, verify your email, set up two-factor, save your recovery codes. (Steps in [Use a hosted Maestria](hosted.md#1-create-your-account).)
2. **Check it** on the VM:

   ```sh
   sudo -u maestria env MAESTRIA_CLOUD_DIR=/var/lib/maestria-cloud maestria-cloud user list
   ```

   **What you should see:**

   ```text
   EMAIL              STATE   CREATED
   you@example.com    active  2026-11-14
   ```

   The states are `unverified` (email not confirmed), `no 2fa yet` (confirmed but not enrolled), `active` and `disabled`.

3. **Link your first machine** and approve your browser: [hosted.md, steps 2 and 3](hosted.md#2-link-your-machine).

To block an account (it ends its sessions and disconnects its machines within about 15 seconds):

```sh
sudo -u maestria env MAESTRIA_CLOUD_DIR=/var/lib/maestria-cloud maestria-cloud user disable someone@example.com
sudo -u maestria env MAESTRIA_CLOUD_DIR=/var/lib/maestria-cloud maestria-cloud user enable someone@example.com
```

> **Sign-up is open.** Anyone who can reach your address can create an account. Accounts are isolated from each other, and a stranger's account cannot see or reach your machines, because machines are linked per account and approved on the machine. But they could use your server, and your mail provider sends their verification mails. For a private server, **keep the address to yourself** and use `user list` to watch for accounts you do not know.

## 9. Update

1. Download the new release as in step 2 (or `git pull` and rebuild).
2. [Back up](#10-back-up) first.
3. Replace the program and restart:

   ```sh
   sudo systemctl stop maestria-cloud
   sudo install -m 0755 maestria-cloud /usr/local/bin/maestria-cloud
   sudo systemctl start maestria-cloud
   maestria-cloud version
   ```

Connected machines reconnect on their own, within about a minute.

## 10. Back up

Everything lives in `/var/lib/maestria-cloud`:

| File | What it holds | If you lose it |
| --- | --- | --- |
| `cloud.db` (plus `-wal` and `-shm`) | Accounts, linked machines, public keys | Everyone signs up and links again |
| `server.key` | The key that encrypts two-factor secrets | **Nobody can sign in.** Back it up. |
| `config.json` (if you use one) | Your settings | You re-enter them |

The database is live, so **copy it with SQLite's own tool**, not `cp`:

```sh
sudo apt install sqlite3
sudo mkdir -p /var/backups/maestria-cloud
sudo sqlite3 /var/lib/maestria-cloud/cloud.db ".backup '/var/backups/maestria-cloud/cloud.db'"
sudo cp -p /var/lib/maestria-cloud/server.key /var/backups/maestria-cloud/
sudo chmod -R go-rwx /var/backups/maestria-cloud
```

Then copy `/var/backups/maestria-cloud` **off the VM**. The backup holds password hashes and encrypted two-factor secrets, plus the key to decrypt them, so **keep it private**.

**To restore:** stop the service, put `cloud.db` and `server.key` back in `/var/lib/maestria-cloud` (owned by `maestria`, mode `0600`), and start it.

## Good to know

- **Clocks.** Browsers sign requests with the time, and your users' machines reject any that are more than 60 seconds off. That compares the browser's clock with the machine's, not with your server's. Still, keep the VM's clock right (Ubuntu does by default, check with `timedatectl`).
- **Monitor** with `https://maestria.example.com/healthz`.
- **You are the trust anchor.** Your server sends the web page that your users' browsers run. People who use your server trust you not to send them a hostile page. Be honest about that with them. See [Security](security.md).
- **Release assets.** Each release ships `maestria-cloud` for the same five targets as `maestria`, with checksums in `SHA256SUMS`.

## Next

- [Troubleshooting](troubleshooting.md): `Misdirected request`, email not arriving, and more.
- [Use a hosted Maestria](hosted.md): what your users do next.
