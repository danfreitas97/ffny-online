#!/bin/sh
# Installs or updates the account service and its HTTPS proxy on the project server.
# Run as root after copying the files to /tmp:
#
#     scp dist/accounts deploy/ffny-accounts.service deploy/Caddyfile deploy/setup-accounts.sh root@SERVER:/tmp/
#     ssh root@SERVER sh /tmp/setup-accounts.sh
#
# Safe to run again. It touches only its own user, /opt/ffny-accounts, /etc/ffny-accounts, its
# data in /var/lib/ffny-accounts, its unit, Caddy (installed here for HTTPS) and the firewall's
# web ports. The relay is left alone. The Discord client secret is never in the repo: it goes
# to /etc/ffny-accounts/client-secret (root:ffny-accounts, 0640) separately.
set -eu

[ "$(id -u)" = 0 ] || { echo "run as root"; exit 1; }
for f in accounts ffny-accounts.service Caddyfile; do
    [ -f /tmp/$f ] || { echo "copy $f to /tmp first"; exit 1; }
done

export DEBIAN_FRONTEND=noninteractive
command -v caddy >/dev/null 2>&1 || { apt-get update -q; apt-get install -y -q caddy; }

id ffny-accounts >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin ffny-accounts
install -d -m 0755 -o root -g root /opt/ffny-accounts
install -d -m 0750 -o root -g ffny-accounts /etc/ffny-accounts
install -m 0755 -o root -g root /tmp/accounts /opt/ffny-accounts/accounts.new
mv -f /opt/ffny-accounts/accounts.new /opt/ffny-accounts/accounts
install -m 0644 -o root -g root /tmp/ffny-accounts.service /etc/systemd/system/ffny-accounts.service

# Only replace a Caddyfile that is the package's own or one of ours.
if [ ! -f /etc/caddy/Caddyfile ] || grep -q "managed by setup-accounts.sh" /etc/caddy/Caddyfile \
   || grep -q "^:80" /etc/caddy/Caddyfile; then
    install -m 0644 -o root -g root /tmp/Caddyfile /etc/caddy/Caddyfile
else
    echo "/etc/caddy/Caddyfile was changed by hand; left as it is"
fi
rm -f /tmp/accounts /tmp/ffny-accounts.service /tmp/Caddyfile

ufw allow 80/tcp comment 'https certificate' >/dev/null
ufw allow 443/tcp comment 'ffny accounts' >/dev/null

systemctl daemon-reload
systemctl enable ffny-accounts >/dev/null 2>&1
if [ -s /etc/ffny-accounts/client-secret ]; then
    chown root:ffny-accounts /etc/ffny-accounts/client-secret
    chmod 0640 /etc/ffny-accounts/client-secret
    systemctl restart ffny-accounts
else
    echo "installed; put the Discord client secret in /etc/ffny-accounts/client-secret, then: systemctl restart ffny-accounts"
fi
caddy validate --config /etc/caddy/Caddyfile >/dev/null 2>&1 && systemctl reload-or-restart caddy
sleep 2
systemctl is-active ffny-accounts caddy
