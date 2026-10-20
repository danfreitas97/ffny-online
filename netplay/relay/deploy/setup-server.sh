#!/bin/sh
# Sets up a fresh Ubuntu server for the online play relay and the launcher's update service.
# Run as root on the server after copying the relay binary and the unit to /tmp:
#
#     scp dist/relay deploy/defjam-relay.service deploy/setup-server.sh root@SERVER:/tmp/
#     ssh root@SERVER sh /tmp/setup-server.sh
#
# Safe to run again (it updates the binary and the unit in place). It never touches anything
# but its own user, /opt/defjam-relay, its unit and its firewall rules. The update signing key
# never goes on the server: only signed manifests and setups are copied into
# /opt/defjam-relay/updates (scripts/publish-update.ps1).
set -eu

[ "$(id -u)" = 0 ] || { echo "run as root"; exit 1; }
[ -f /tmp/relay ] && [ -f /tmp/defjam-relay.service ] || { echo "copy relay and defjam-relay.service to /tmp first"; exit 1; }

export DEBIAN_FRONTEND=noninteractive
apt-get update -q
apt-get upgrade -y -q
apt-get install -y -q ufw unattended-upgrades
# security updates install themselves
dpkg-reconfigure -f noninteractive unattended-upgrades

id defjam-relay >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin defjam-relay

install -d -m 0755 -o root -g root /opt/defjam-relay /opt/defjam-relay/updates
install -m 0755 -o root -g root /tmp/relay /opt/defjam-relay/relay.new
mv -f /opt/defjam-relay/relay.new /opt/defjam-relay/relay
install -m 0644 -o root -g root /tmp/defjam-relay.service /etc/systemd/system/defjam-relay.service
rm -f /tmp/relay /tmp/defjam-relay.service

# SSH first, so enabling the firewall never locks the session out
ufw allow OpenSSH comment 'ssh'
ufw allow 27777/udp comment 'defjam relay'
ufw allow 27778/tcp comment 'defjam updates'
ufw --force enable

systemctl daemon-reload
systemctl enable defjam-relay
systemctl restart defjam-relay
sleep 2
systemctl is-active defjam-relay
journalctl -u defjam-relay -n 3 --no-pager -o cat
