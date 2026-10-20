"""FFNY Online Discord bot.

Builds the server's layout on its first start (roles, categories, channels, the welcome, rules,
FAQ and help texts; it only adds what is missing and never deletes), runs the ticket system
(a panel in #get-help: a form per kind, then a private channel with the team; closing posts a
transcript to #ticket-log), announces new builds from the update manifest and keeps the
player count from the relay's summary line in the status channel's name.

Configuration (environment):
  FFNY_BOT_TOKEN_FILE  file with the bot token (default /etc/ffny-bot/token)
  FFNY_BOT_STATE       state file (default /var/lib/ffny-bot/state.json)
  FFNY_BOT_GUILD       server id; without it the first server the bot is in
"""
import asyncio
import datetime
import io
import json
import logging
import os
import sys

import aiohttp
import discord
from discord import app_commands
from discord.ext import tasks

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import ffny_core as core  # noqa: E402

log = logging.getLogger("ffny-bot")
STATE_PATH = os.environ.get("FFNY_BOT_STATE", "/var/lib/ffny-bot/state.json")
TOKEN_FILE = os.environ.get("FFNY_BOT_TOKEN_FILE", "/etc/ffny-bot/token")
ORANGE = discord.Colour(0xF28C28)


# ---- state ------------------------------------------------------------------------------------

def load_state():
    try:
        with open(STATE_PATH, encoding="utf-8") as f:
            return json.load(f)
    except (OSError, ValueError):
        return {}


def save_state(state):
    tmp = STATE_PATH + ".new"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(state, f, indent=1)
    os.replace(tmp, STATE_PATH)


# ---- the client -------------------------------------------------------------------------------

class FfnyBot(discord.Client):
    def __init__(self):
        intents = discord.Intents.default()
        intents.message_content = True      # ticket transcripts
        super().__init__(intents=intents)
        self.tree = app_commands.CommandTree(self)
        self.state = load_state()
        self.state.setdefault("tickets", {})
        self.state.setdefault("messages", {})
        self.ready_once = False

    async def setup_hook(self):
        self.add_view(HelpPanel())
        self.add_view(TicketControls())
        self.add_view(AlertToggle())

    def guild(self):
        want = os.environ.get("FFNY_BOT_GUILD")
        if want:
            return self.get_guild(int(want))
        return self.guilds[0] if self.guilds else None

    async def on_ready(self):
        log.info("logged in as %s in %d server(s)", self.user, len(self.guilds))
        guild = self.guild()
        if guild is None:
            log.warning("not in a server yet: waiting for the invite")
            return
        await self.start_in(guild)

    async def on_guild_join(self, guild):
        log.info("joined %s", guild.name)
        if guild == self.guild():
            await self.start_in(guild)

    async def start_in(self, guild):
        if self.ready_once:
            return
        self.ready_once = True
        await self.build_server(guild)
        self.tree.copy_global_to(guild=guild)
        await self.tree.sync(guild=guild)
        watch_builds.start(self)
        update_status.start(self)

    # -- layout --

    def role(self, guild, name):
        return discord.utils.get(guild.roles, name=name)

    async def build_server(self, guild):
        team = self.role(guild, core.TEAM_ROLE) or await guild.create_role(
            name=core.TEAM_ROLE, colour=ORANGE, hoist=True, mentionable=True, reason="FFNY bot layout")
        if not self.role(guild, core.ALERT_ROLE):
            await guild.create_role(name=core.ALERT_ROLE, mentionable=True, reason="FFNY bot layout")
        everyone, me = guild.default_role, guild.me
        bot_all = discord.PermissionOverwrite(view_channel=True, send_messages=True, manage_channels=True,
                                              manage_messages=True, embed_links=True, attach_files=True,
                                              read_message_history=True, connect=True)
        for cat_name, channels in core.LAYOUT:
            category = discord.utils.get(guild.categories, name=cat_name)
            private = cat_name == core.TICKET_CATEGORY or any(k == "team" for _, k in channels)
            if category is None:
                over = {me: bot_all}
                if private:
                    over[everyone] = discord.PermissionOverwrite(view_channel=False)
                    over[team] = discord.PermissionOverwrite(view_channel=True, send_messages=True,
                                                             read_message_history=True)
                category = await guild.create_category(cat_name, overwrites=over, reason="FFNY bot layout")
                log.info("created category %s", cat_name)
            for name, kind in channels:
                await self.ensure_channel(guild, category, name, kind, team, bot_all)
        save_state(self.state)
        await self.post_texts(guild)

    async def ensure_channel(self, guild, category, name, kind, team, bot_all):
        everyone, me = guild.default_role, guild.me
        if kind == "stat":
            known = guild.get_channel(self.state.get("status_channel", 0))
            if known:
                return known
            ch = await guild.create_voice_channel(
                core.status_name(None), category=category, reason="FFNY bot layout",
                overwrites={everyone: discord.PermissionOverwrite(connect=False), me: bot_all})
            self.state["status_channel"] = ch.id
            return ch
        pool = category.voice_channels if kind == "voice" else category.text_channels
        existing = discord.utils.get(pool, name=name)
        if existing:
            return existing
        over = {me: bot_all}
        if kind == "read":
            over[everyone] = discord.PermissionOverwrite(send_messages=False, add_reactions=True,
                                                         create_public_threads=False)
            over[team] = discord.PermissionOverwrite(send_messages=True)
        elif kind == "team":
            over[everyone] = discord.PermissionOverwrite(view_channel=False)
            over[team] = discord.PermissionOverwrite(view_channel=True, send_messages=True)
        if kind == "voice":
            ch = await guild.create_voice_channel(name, category=category, overwrites=over, reason="FFNY bot layout")
        else:
            ch = await guild.create_text_channel(name, category=category, overwrites=over, reason="FFNY bot layout")
        log.info("created #%s", name)
        return ch

    def text_channel(self, guild, name):
        return discord.utils.get(guild.text_channels, name=name)

    async def put(self, guild, key, channel_name, content=None, embed=None, view=None):
        """Posts a fixed message once and edits it to the current text on later starts."""
        channel = self.text_channel(guild, channel_name)
        if channel is None:
            return
        mid = self.state["messages"].get(key)
        if mid:
            try:
                msg = await channel.fetch_message(mid)
                await msg.edit(content=content, embed=embed, view=view)
                return
            except discord.NotFound:
                pass
        msg = await channel.send(content=content, embed=embed, view=view)
        self.state["messages"][key] = msg.id
        save_state(self.state)

    async def post_texts(self, guild):
        await self.put(guild, "welcome", "welcome", content=core.WELCOME)
        await self.put(guild, "rules", "rules", content=core.RULES)
        faq = discord.Embed(title="FAQ", colour=ORANGE)
        for q, a in core.FAQ:
            faq.add_field(name=q, value=a, inline=False)
        await self.put(guild, "faq", "faq", embed=faq)
        await self.put(guild, "help", core.HELP_CHANNEL, content=core.HELP_PANEL, view=HelpPanel())
        await self.put(guild, "alerts", core.ANNOUNCEMENTS, content=core.ALERTS_TEXT, view=AlertToggle())

    # -- tickets --

    def open_tickets(self, user_id):
        return [int(cid) for cid, t in self.state["tickets"].items() if t["owner"] == user_id]

    async def open_ticket(self, interaction, kind, answers):
        guild = interaction.guild
        info = core.TICKET_TYPES[kind]
        team = self.role(guild, core.TEAM_ROLE)
        category = discord.utils.get(guild.categories, name=core.TICKET_CATEGORY)
        number = self.state.get("ticket_counter", 0) + 1
        self.state["ticket_counter"] = number
        user = interaction.user
        over = {
            guild.default_role: discord.PermissionOverwrite(view_channel=False),
            user: discord.PermissionOverwrite(view_channel=True, send_messages=True, attach_files=True,
                                              embed_links=True, read_message_history=True),
            guild.me: discord.PermissionOverwrite(view_channel=True, send_messages=True, manage_channels=True,
                                                  read_message_history=True, attach_files=True),
        }
        if team:
            over[team] = discord.PermissionOverwrite(view_channel=True, send_messages=True, attach_files=True,
                                                     manage_messages=True, read_message_history=True)
        channel = await guild.create_text_channel(
            core.ticket_channel_name(info["prefix"], number, user.name), category=category, overwrites=over,
            topic="%s ticket #%d by %s (%d)" % (info["label"], number, user, user.id), reason="FFNY ticket")
        self.state["tickets"][str(channel.id)] = {"owner": user.id, "kind": kind, "number": number,
                                                  "opened": datetime.datetime.now(datetime.timezone.utc).isoformat()}
        save_state(self.state)
        embed = discord.Embed(title="%s %s #%d" % (info["emoji"], info["label"], number), colour=ORANGE)
        for key, label, _, _, _ in info["fields"]:
            value = answers.get(key, "").strip()
            if value:
                embed.add_field(name=label, value=value[:1024], inline=False)
        embed.set_footer(text="Add screenshots or logs here. Setup logs: %LOCALAPPDATA%\\DefJamSetup\\logs")
        ping = user.mention + (" " + team.mention if team else "")
        await channel.send(ping, embed=embed, view=TicketControls(),
                           allowed_mentions=discord.AllowedMentions(users=True, roles=True))
        await self.ticket_log(guild, "📬 %s opened %s (%s)" % (user.mention, channel.mention, info["label"]))
        return channel

    async def ticket_log(self, guild, text, file=None):
        channel = self.text_channel(guild, core.TICKET_LOG)
        if channel:
            await channel.send(text, file=file, allowed_mentions=discord.AllowedMentions.none())

    def is_team(self, member):
        return any(r.name == core.TEAM_ROLE for r in getattr(member, "roles", [])) or \
            getattr(member, "guild_permissions", None) and member.guild_permissions.administrator

    async def close_ticket(self, interaction):
        channel = interaction.channel
        ticket = self.state["tickets"].get(str(channel.id))
        if ticket is None:
            await interaction.response.send_message("This is not a ticket channel.", ephemeral=True)
            return
        if interaction.user.id != ticket["owner"] and not self.is_team(interaction.user):
            await interaction.response.send_message("Only the ticket's owner or the team can close it.", ephemeral=True)
            return
        await interaction.response.send_message("Closing this ticket in 5 seconds; the transcript is saved.")
        lines = []
        async for m in channel.history(limit=None, oldest_first=True):
            content = m.content
            if m.embeds and not content:
                e = m.embeds[0]
                content = (e.title or "") + " | " + " | ".join("%s: %s" % (f.name, f.value) for f in e.fields)
            lines.append((m.created_at.strftime("%Y-%m-%d %H:%M"), str(m.author), content,
                          [a.url for a in m.attachments]))
        text = core.transcript(channel.name, lines).encode("utf-8")
        owner = interaction.guild.get_member(ticket["owner"])
        await self.ticket_log(interaction.guild, "📪 %s closed #%s (opened by <@%d>)" % (
            interaction.user.mention, channel.name, ticket["owner"]),
            file=discord.File(io.BytesIO(text), filename=channel.name + ".txt"))
        if owner:
            try:
                await owner.send("Your FFNY Online ticket #%s is closed. Thanks for the report! Transcript:" % channel.name,
                                 file=discord.File(io.BytesIO(text), filename=channel.name + ".txt"))
            except discord.HTTPException:
                pass        # DMs closed
        del self.state["tickets"][str(channel.id)]
        save_state(self.state)
        await asyncio.sleep(5)
        await channel.delete(reason="FFNY ticket closed")


bot = FfnyBot()


# ---- views and forms --------------------------------------------------------------------------

class TicketForm(discord.ui.Modal):
    def __init__(self, kind):
        info = core.TICKET_TYPES[kind]
        super().__init__(title=info["label"])
        self.kind = kind
        self.inputs = {}
        for key, label, style, required, placeholder in info["fields"]:
            field = discord.ui.TextInput(
                label=label, required=required, placeholder=placeholder or None,
                style=discord.TextStyle.paragraph if style == "paragraph" else discord.TextStyle.short,
                max_length=1000 if style == "paragraph" else 100)
            self.inputs[key] = field
            self.add_item(field)

    async def on_submit(self, interaction):
        await interaction.response.defer(ephemeral=True, thinking=True)
        channel = await bot.open_ticket(interaction, self.kind, {k: f.value for k, f in self.inputs.items()})
        await interaction.followup.send("Your ticket is open: %s" % channel.mention, ephemeral=True)


class HelpPanel(discord.ui.View):
    def __init__(self):
        super().__init__(timeout=None)
        styles = {"bug": discord.ButtonStyle.danger, "setup": discord.ButtonStyle.primary,
                  "online": discord.ButtonStyle.success, "other": discord.ButtonStyle.secondary}
        for kind, info in core.TICKET_TYPES.items():
            button = discord.ui.Button(label=info["label"], emoji=info["emoji"], style=styles[kind],
                                       custom_id="ffny:ticket:" + kind)
            button.callback = self.opener(kind)
            self.add_item(button)

    @staticmethod
    def opener(kind):
        async def callback(interaction):
            mine = [c for c in bot.open_tickets(interaction.user.id) if interaction.guild.get_channel(c)]
            if len(mine) >= core.MAX_OPEN_TICKETS:
                await interaction.response.send_message(
                    "You already have open tickets: %s. Close one first." % " ".join("<#%d>" % c for c in mine),
                    ephemeral=True)
                return
            await interaction.response.send_modal(TicketForm(kind))
        return callback


class TicketControls(discord.ui.View):
    def __init__(self):
        super().__init__(timeout=None)

    @discord.ui.button(label="Claim", emoji="🙋", style=discord.ButtonStyle.secondary, custom_id="ffny:claim")
    async def claim(self, interaction, button):
        if not bot.is_team(interaction.user):
            await interaction.response.send_message("Only the FFNY Team can claim tickets.", ephemeral=True)
            return
        await interaction.response.send_message("%s is on it." % interaction.user.mention)

    @discord.ui.button(label="Close", emoji="🔒", style=discord.ButtonStyle.danger, custom_id="ffny:close")
    async def close(self, interaction, button):
        await bot.close_ticket(interaction)


class AlertToggle(discord.ui.View):
    def __init__(self):
        super().__init__(timeout=None)

    @discord.ui.button(label="Build alerts", emoji="🔔", style=discord.ButtonStyle.primary, custom_id="ffny:alerts")
    async def toggle(self, interaction, button):
        role = bot.role(interaction.guild, core.ALERT_ROLE)
        if role is None:
            await interaction.response.send_message("The alert role is missing.", ephemeral=True)
        elif role in interaction.user.roles:
            await interaction.user.remove_roles(role, reason="build alerts off")
            await interaction.response.send_message("No more build pings.", ephemeral=True)
        else:
            await interaction.user.add_roles(role, reason="build alerts on")
            await interaction.response.send_message("You get a ping for every new build. 🔔", ephemeral=True)


# ---- commands ---------------------------------------------------------------------------------

async def fetch_manifest():
    try:
        async with aiohttp.ClientSession(timeout=aiohttp.ClientTimeout(total=15)) as s:
            async with s.get(core.UPDATE_URL) as r:
                return core.parse_manifest(await r.text()) if r.status == 200 else None
    except (aiohttp.ClientError, asyncio.TimeoutError):
        return None


async def relay_summary():
    """The relay's newest summary line from the journal (the bot runs beside the relay)."""
    try:
        proc = await asyncio.create_subprocess_exec(
            "journalctl", "-u", "defjam-relay", "-n", "30", "--no-pager", "-o", "cat",
            stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.DEVNULL)
        out, _ = await asyncio.wait_for(proc.communicate(), 15)
    except (OSError, asyncio.TimeoutError):
        return None
    return core.parse_relay_summary(out.decode("utf-8", "replace"))


@bot.tree.command(name="status", description="FFNY Online: the newest build and who is online")
async def status_cmd(interaction):
    await interaction.response.defer(thinking=True)
    manifest, summary = await fetch_manifest(), await relay_summary()
    embed = discord.Embed(title="FFNY Online", colour=ORANGE)
    embed.add_field(name="Newest build", value=("%d (%s)" % (manifest["build"], manifest["version"]))
                    if manifest else "unknown")
    embed.add_field(name="Server", value=core.status_name(summary))
    embed.add_field(name="Links", value="[Download](%s) · [Disc guide](%s) · [GitHub](%s)" % (
        core.DOWNLOAD, core.DISC_GUIDE, core.GITHUB), inline=False)
    await interaction.followup.send(embed=embed)


@bot.tree.command(name="close", description="Close this ticket")
async def close_cmd(interaction):
    await bot.close_ticket(interaction)


@bot.tree.command(name="add", description="Add someone to this ticket (team only)")
async def add_cmd(interaction, member: discord.Member):
    if str(interaction.channel.id) not in bot.state["tickets"] or not bot.is_team(interaction.user):
        await interaction.response.send_message("Only the team can add people, in a ticket channel.", ephemeral=True)
        return
    await interaction.channel.set_permissions(member, view_channel=True, send_messages=True, attach_files=True,
                                              read_message_history=True)
    await interaction.response.send_message("Added %s." % member.mention)


@bot.tree.command(name="setup", description="Add missing channels and refresh the bot's texts")
@app_commands.default_permissions(manage_guild=True)
async def setup_cmd(interaction):
    await interaction.response.defer(ephemeral=True, thinking=True)
    await bot.build_server(interaction.guild)
    await interaction.followup.send("Layout and texts are up to date.", ephemeral=True)


# ---- background jobs --------------------------------------------------------------------------

@tasks.loop(minutes=5)
async def watch_builds(client):
    manifest = await fetch_manifest()
    if manifest is None:
        return
    last = client.state.get("last_build")
    if core.is_new_build(last, manifest):
        guild = client.guild()
        channel = client.text_channel(guild, core.ANNOUNCEMENTS)
        role = client.role(guild, core.ALERT_ROLE)
        embed = discord.Embed(title="Build %d is out" % manifest["build"], description=manifest["notes"],
                              colour=ORANGE)
        embed.add_field(name="Get it", value="The launcher offers it at its next start. New here? [Download](%s)"
                        % core.DOWNLOAD)
        if channel:
            await channel.send(role.mention if role else None, embed=embed,
                               allowed_mentions=discord.AllowedMentions(roles=True))
            log.info("announced build %d", manifest["build"])
    if last != manifest["build"]:
        client.state["last_build"] = manifest["build"]
        save_state(client.state)


@tasks.loop(minutes=10)     # Discord allows two channel renames per ten minutes
async def update_status(client):
    guild = client.guild()
    channel = guild.get_channel(client.state.get("status_channel", 0)) if guild else None
    if channel is None:
        return
    name = core.status_name(await relay_summary())
    if channel.name != name:
        try:
            await channel.edit(name=name, reason="player count")
        except discord.HTTPException as e:
            log.warning("status rename failed: %s", e)


def main():
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s: %(message)s")
    with open(TOKEN_FILE, encoding="utf-8") as f:
        token = f.read().strip()
    bot.run(token, log_handler=None)


if __name__ == "__main__":
    main()
