# FFNY Online Discord bot

The project's own bot for the FFNY Online Discord server. It runs on the project server beside
the relay (`deploy/ffny-bot.service`) and needs nothing but its token.

**What it does**

- **Builds the server** on its first start: the roles `FFNY Team` and `Build Alerts`, the
  categories and channels in `ffny_core.LAYOUT`, and the welcome, rules, FAQ and help texts.
  It only adds what is missing and never deletes anything. Later starts edit its own messages
  to the current texts, so a text change is a redeploy. `/setup` does the same on demand.
- **Tickets**: the panel in #get-help has a button per kind (bug report, setup, online,
  something else). Each opens a short form; the bot then opens a private channel for the player
  and the team. **Claim** and **Close** sit under the first message; closing posts a transcript
  to #ticket-log and sends it to the player. `/add` lets the team bring someone in.
- **Build announcements**: every five minutes it reads the update manifest
  (`latest.json`) and posts a new build in #announcements, pinging `Build Alerts` (a button there
  toggles the role).
- **Status**: the status channel's name shows players and rooms from the relay's summary line
  (`journalctl -u defjam-relay`), refreshed every ten minutes.
- `/status` shows the newest build, the server and the links.

**Setting it up**

1. In the Discord Developer Portal: an application with a bot; under **Bot**, turn on
   **Message Content Intent** (transcripts) and turn off **Public Bot**.
2. Invite it with `https://discord.com/oauth2/authorize?client_id=<APPLICATION_ID>&scope=bot+applications.commands&permissions=8`.
3. On the server: `scp -r community/discord-bot root@SERVER:/tmp/ffny-bot`, then
   `ssh root@SERVER sh /tmp/ffny-bot/deploy/setup-bot.sh`.
4. Put the token into `/etc/ffny-bot/token` and `systemctl restart ffny-bot`. The token never
   goes into the repo.

Give the team the `FFNY Team` role; they see every ticket and the team channels.
Logs: `journalctl -u ffny-bot`.
