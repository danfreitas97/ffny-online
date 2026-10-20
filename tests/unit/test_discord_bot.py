"""The FFNY Online Discord bot's Discord-free part (community/discord-bot/ffny_core.py)."""
import importlib.util
import json
import os
import re

HERE = os.path.dirname(os.path.abspath(__file__))
PATH = os.path.join(HERE, "..", "..", "community", "discord-bot", "ffny_core.py")
spec = importlib.util.spec_from_file_location("ffny_core", PATH)
core = importlib.util.module_from_spec(spec)
spec.loader.exec_module(core)


def test_layout_names_are_valid_discord_names():
    seen = set()
    for category, channels in core.LAYOUT:
        assert 0 < len(category) <= 100
        for name, kind in channels:
            assert kind in {"read", "chat", "team", "voice", "stat"}
            if kind in {"read", "chat", "team"}:
                assert re.fullmatch(r"[a-z0-9-]{1,100}", name), name
            assert name not in seen
            seen.add(name)
    for needed in (core.TICKET_LOG, core.ANNOUNCEMENTS, core.HELP_CHANNEL):
        assert needed in seen
    assert core.TICKET_CATEGORY in [c for c, _ in core.LAYOUT]


def test_ticket_forms_fit_discord_limits():
    for kind, info in core.TICKET_TYPES.items():
        assert 1 <= len(info["fields"]) <= 5
        assert len(info["label"]) <= 45
        keys = [f[0] for f in info["fields"]]
        assert len(keys) == len(set(keys))
        for key, label, style, required, placeholder in info["fields"]:
            assert len(label) <= 45 and len(placeholder) <= 100
            assert style in {"short", "paragraph"}


def test_texts_fit_a_message():
    for text in (core.WELCOME, core.RULES, core.HELP_PANEL, core.ALERTS_TEXT):
        assert len(text) <= 2000
    assert len(core.FAQ) <= 25
    for q, a in core.FAQ:
        assert len(q) <= 256 and len(a) <= 1024


def test_ticket_channel_name():
    assert core.ticket_channel_name("bug", 7, "Masa_Fan 99") == "bug-0007-masa-fan-99"
    assert core.ticket_channel_name("other", 12, "!!!") == "other-0012-player"
    assert len(core.ticket_channel_name("online", 1, "x" * 80)) <= 100


def test_relay_summary_takes_the_newest_line():
    text = ("2026/10/20 21:18:57 summary rooms=0 clients=1 in=1.0pkt/s\n"
            "2026/10/20 21:19:57 summary rooms=2 clients=5 in=1.0pkt/s\n")
    assert core.parse_relay_summary(text) == (2, 5)
    assert core.parse_relay_summary("no summary yet") is None
    assert core.status_name((1, 3)) == "🟢 3 online · 1 room"
    assert core.status_name((2, 5)) == "🟢 5 online · 2 rooms"
    assert core.status_name(None) == "🔴 Server offline"


def test_build_announcements():
    m = core.parse_manifest(json.dumps({"build": 11, "version": "0.7.2-remaster.11", "notes": "x"}))
    assert m == {"build": 11, "version": "0.7.2-remaster.11", "notes": "x"}
    assert core.parse_manifest("<html>") is None
    assert core.parse_manifest('{"version": "1"}') is None
    assert not core.is_new_build(None, m)          # first look only records the build
    assert not core.is_new_build(11, m)
    assert core.is_new_build(10, m)
    assert not core.is_new_build(10, None)


def test_transcript():
    text = core.transcript("bug-0001-a", [("2026-10-20 21:00", "a", "it crashed", ["https://x/y.png"])])
    assert text.startswith("Transcript of #bug-0001-a\n")
    assert "[2026-10-20 21:00] a: it crashed" in text and "attachment: https://x/y.png" in text
