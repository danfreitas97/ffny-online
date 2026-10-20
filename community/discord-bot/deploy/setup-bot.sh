#!/bin/sh
# Installs or updates the FFNY Online Discord bot on the project's server, beside the relay.
# Run as root after copying the bot's files to /tmp/ffny-bot:
#
#     scp -r community/discord-bot root@SERVER:/tmp/ffny-bot
#     ssh root@SERVER sh /tmp/ffny-bot/deploy/setup-bot.sh
#
# Safe to run again. It touches only its own user, /opt/ffny-bot, /etc/ffny-bot, its state in
# /var/lib/ffny-bot and its unit; the relay is left alone. The bot token is never in the repo:
# it goes to /etc/ffny-bot/token (root:ffny-bot, 0640) separately. Without it the bot is
# installed but not started.
set -eu

[ "$(id -u)" = 0 ] || { echo "run as root"; exit 1; }
src=/tmp/ffny-bot
[ -f $src/ffny_bot.py ] || { echo "copy the bot to $src first"; exit 1; }

export DEBIAN_FRONTEND=noninteractive
dpkg -s python3-venv >/dev/null 2>&1 || { apt-get update -q; apt-get install -y -q python3-venv; }

id ffny-bot >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin ffny-bot

install -d -m 0755 -o root -g root /opt/ffny-bot
install -d -m 0750 -o root -g ffny-bot /etc/ffny-bot
install -m 0644 -o root -g root $src/ffny_bot.py $src/ffny_core.py $src/requirements.txt /opt/ffny-bot/
[ -x /opt/ffny-bot/venv/bin/python ] || python3 -m venv /opt/ffny-bot/venv
/opt/ffny-bot/venv/bin/pip install -q --disable-pip-version-check -r /opt/ffny-bot/requirements.txt
install -m 0644 -o root -g root $src/deploy/ffny-bot.service /etc/systemd/system/ffny-bot.service
rm -rf $src

systemctl daemon-reload
systemctl enable ffny-bot >/dev/null 2>&1
if [ -s /etc/ffny-bot/token ]; then
    chown root:ffny-bot /etc/ffny-bot/token
    chmod 0640 /etc/ffny-bot/token
    systemctl restart ffny-bot
    sleep 3
    systemctl is-active ffny-bot
else
    echo "installed; put the bot token in /etc/ffny-bot/token, then: systemctl restart ffny-bot"
fi
