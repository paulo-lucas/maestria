#!/usr/bin/env bash
# Set up maestria-cloud on a fresh Debian/Ubuntu VM: swap (small VMs), a service user, the
# binary, settings, a hardened systemd unit, a daily database backup and, optionally,
# cloudflared with a tunnel token from the Cloudflare dashboard.
#
# Usage (as root, from the folder holding the maestria-cloud binary):
#   sudo ./setup-cloud.sh --public-url https://maestria.example.com \
#        [--binary ./maestria-cloud] [--tunnel-token <token>] [--dev-email] [--no-auto-update]
#
# Updates: by default a systemd timer runs `maestria-cloud update --yes` every ~15 minutes, so
# pushing a release tag updates this machine (root is needed to replace the binary; the service
# itself keeps running as the maestria user). Pass --no-auto-update to skip it, or to turn it
# off on a machine that has it. Releases are checksum-verified; a new version that does not
# answer /healthz within 30 s is rolled back.
#
# Safe to re-run: it updates the binary and unit, and keeps existing settings and data.
set -euo pipefail

PUBLIC_URL=""
BINARY="./maestria-cloud"
TUNNEL_TOKEN=""
DEV_EMAIL=0
AUTO_UPDATE=1

while [ $# -gt 0 ]; do
  case "$1" in
    --public-url) PUBLIC_URL="$2"; shift 2 ;;
    --binary) BINARY="$2"; shift 2 ;;
    --tunnel-token) TUNNEL_TOKEN="$2"; shift 2 ;;
    --dev-email) DEV_EMAIL=1; shift ;;
    --no-auto-update) AUTO_UPDATE=0; shift ;;
    -h|--help) sed -n '2,15p' "$0"; exit 0 ;;
    *) echo "Unknown option: $1" >&2; exit 2 ;;
  esac
done

[ "$(id -u)" -eq 0 ] || { echo "Run as root (sudo)." >&2; exit 1; }
case "$PUBLIC_URL" in https://*) ;; *) echo "--public-url must start with https://" >&2; exit 2 ;; esac
[ -x "$BINARY" ] || { echo "Binary not found or not executable: $BINARY" >&2; exit 1; }

DATA=/var/lib/maestria-cloud
ETC=/etc/maestria-cloud
ENVFILE=$ETC/env

step() { printf '\n==> %s\n' "$*"; }

step "Swap (only if the machine has none and less than 2 GB of RAM)"
if ! swapon --show | grep -q . && [ "$(awk '/MemTotal/ {print $2}' /proc/meminfo)" -lt 2000000 ]; then
  fallocate -l 1G /swapfile && chmod 600 /swapfile && mkswap /swapfile >/dev/null && swapon /swapfile
  grep -q '^/swapfile ' /etc/fstab || echo '/swapfile none swap sw 0 0' >> /etc/fstab
  echo "1 GB swap added."
else
  echo "Skipped."
fi

step "Service user and folders"
id maestria >/dev/null 2>&1 || useradd --system --home "$DATA" --shell /usr/sbin/nologin maestria
install -d -m 0700 -o maestria -g maestria "$DATA"
install -d -m 0750 -o root -g maestria "$ETC"
install -d -m 0700 -o root -g root /var/backups/maestria-cloud

step "Binary"
install -m 0755 -o root -g root "$BINARY" /usr/local/bin/maestria-cloud
/usr/local/bin/maestria-cloud version || true

step "Settings ($ENVFILE)"
if [ ! -f "$ENVFILE" ]; then
  {
    echo "MAESTRIA_CLOUD_DIR=$DATA"
    echo "MAESTRIA_CLOUD_PUBLIC_URL=$PUBLIC_URL"
    echo "MAESTRIA_CLOUD_LISTEN=127.0.0.1:8080"
    echo "MAESTRIA_CLOUD_TRUST_PROXY=cf"
    if [ "$DEV_EMAIL" -eq 1 ]; then
      echo "# Trial mode: verification and reset emails are printed in the log (journalctl -u maestria-cloud)."
      echo "MAESTRIA_CLOUD_DEV_EMAIL=stdout"
    else
      echo "# Email (fill these in, then: sudo systemctl restart maestria-cloud)"
      echo "MAESTRIA_CLOUD_SMTP_HOST="
      echo "MAESTRIA_CLOUD_SMTP_PORT=587"
      echo "MAESTRIA_CLOUD_SMTP_USER="
      echo "MAESTRIA_CLOUD_SMTP_PASSWORD="
      echo "MAESTRIA_CLOUD_SMTP_FROM="
    fi
  } > "$ENVFILE"
  chown root:maestria "$ENVFILE"
  chmod 0640 "$ENVFILE"
  echo "Written. Edit it with: sudo nano $ENVFILE"
else
  echo "Kept the existing settings."
fi

step "systemd unit"
cat > /etc/systemd/system/maestria-cloud.service <<'UNIT'
[Unit]
Description=Maestria hosted server
Documentation=https://github.com/paulo-lucas/maestria
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
UNIT

step "Daily database backup (keeps 14 days in /var/backups/maestria-cloud)"
command -v sqlite3 >/dev/null || { apt-get update -qq && apt-get install -y -qq sqlite3 >/dev/null; }
cat > /usr/local/sbin/maestria-cloud-backup <<'BACKUP'
#!/bin/sh
set -eu
for db in /var/lib/maestria-cloud/*.db; do
  [ -f "$db" ] || continue
  sqlite3 "$db" ".backup '/var/backups/maestria-cloud/$(basename "$db" .db)-$(date +%F).db'"
done
cp -p /var/lib/maestria-cloud/server.key "/var/backups/maestria-cloud/server.key" 2>/dev/null || true
find /var/backups/maestria-cloud -name '*.db' -mtime +14 -delete
BACKUP
chmod 0700 /usr/local/sbin/maestria-cloud-backup
cat > /etc/systemd/system/maestria-cloud-backup.service <<'UNIT'
[Unit]
Description=Back up the Maestria hosted server database
[Service]
Type=oneshot
ExecStart=/usr/local/sbin/maestria-cloud-backup
UNIT
cat > /etc/systemd/system/maestria-cloud-backup.timer <<'UNIT'
[Unit]
Description=Daily Maestria hosted server backup
[Timer]
OnCalendar=daily
RandomizedDelaySec=1h
Persistent=true
[Install]
WantedBy=timers.target
UNIT

if [ "$AUTO_UPDATE" -eq 1 ]; then
  step "Automatic updates (every ~15 minutes, from GitHub releases)"
  # Runs as root: replacing /usr/local/bin/maestria-cloud needs it. The server itself keeps
  # running as the maestria user. The update command verifies the release checksum, backs up
  # the database, restarts the service and rolls back if /healthz does not answer.
  cat > /etc/systemd/system/maestria-cloud-update.service <<'UNIT'
[Unit]
Description=Update maestria-cloud from the latest GitHub release
Documentation=https://github.com/paulo-lucas/maestria/blob/main/docs/releasing.md
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
EnvironmentFile=/etc/maestria-cloud/env
# If cosign is installed, the update checks the release signature with it. cosign keeps the
# Sigstore trusted root (a TUF cache) in $TUF_ROOT, by default under $HOME/.sigstore, and
# ProtectHome=true hides /root: give it a state directory instead, or every run would fail.
StateDirectory=maestria-cloud-update
Environment=HOME=/var/lib/maestria-cloud-update
Environment=TUF_ROOT=/var/lib/maestria-cloud-update/sigstore
ExecStart=/usr/local/bin/maestria-cloud update --yes
NoNewPrivileges=true
PrivateTmp=true
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
LockPersonality=true
RestrictSUIDSGID=true
UNIT
  cat > /etc/systemd/system/maestria-cloud-update.timer <<'UNIT'
[Unit]
Description=Look for a new maestria-cloud release every 15 minutes

[Timer]
OnBootSec=5min
OnUnitActiveSec=15min
RandomizedDelaySec=5min

[Install]
WantedBy=timers.target
UNIT
else
  step "Automatic updates: off (--no-auto-update)"
  if [ -f /etc/systemd/system/maestria-cloud-update.timer ]; then
    systemctl disable --now maestria-cloud-update.timer >/dev/null 2>&1 || true
    rm -f /etc/systemd/system/maestria-cloud-update.timer /etc/systemd/system/maestria-cloud-update.service
    echo "Removed the update timer."
  fi
  echo "Update by hand with: sudo maestria-cloud update"
fi

systemctl daemon-reload
systemctl enable --now maestria-cloud maestria-cloud-backup.timer >/dev/null
if [ "$AUTO_UPDATE" -eq 1 ]; then
  systemctl enable --now maestria-cloud-update.timer >/dev/null
fi
systemctl restart maestria-cloud

step "Health check"
for _ in $(seq 1 20); do
  if curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1; then echo "maestria-cloud is up."; break; fi
  sleep 1
done
curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1 || { echo "It did not come up. See: journalctl -u maestria-cloud -n 50" >&2; exit 1; }

if [ -n "$TUNNEL_TOKEN" ]; then
  step "cloudflared (Cloudflare's signed apt repository)"
  if ! command -v cloudflared >/dev/null; then
    install -d -m 0755 /usr/share/keyrings
    curl -fsSL https://pkg.cloudflare.com/cloudflare-main.gpg -o /usr/share/keyrings/cloudflare-main.gpg
    echo "deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared any main" \
      > /etc/apt/sources.list.d/cloudflared.list
    apt-get update -qq && apt-get install -y -qq cloudflared >/dev/null
  fi
  cloudflared service install "$TUNNEL_TOKEN" >/dev/null 2>&1 || echo "cloudflared is already installed as a service (re-run 'cloudflared service uninstall' to change the token)."
  systemctl enable --now cloudflared >/dev/null 2>&1 || true
  echo "Tunnel service installed. In the Cloudflare dashboard, set the public hostname to http://127.0.0.1:8080."
fi

step "Done"
echo "Public address: $PUBLIC_URL"
echo "Logs:           journalctl -u maestria-cloud -f"
echo "Settings:       $ENVFILE (then: systemctl restart maestria-cloud)"
if [ "$AUTO_UPDATE" -eq 1 ]; then
  echo "Updates:        automatic (journalctl -u maestria-cloud-update -n 20; opt out: re-run with --no-auto-update)"
else
  echo "Updates:        manual (sudo maestria-cloud update)"
fi
