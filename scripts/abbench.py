#!/usr/bin/env python3
"""abbench — live A/B benchmark for LeanProxy across three arms.

Measures what the residency sweep cannot: how many turns a real model takes,
and whether it finds the tools it needs at all. Layer 1
(tests/bench/e2e/residency_test.go) covers residency for free; this script
covers behaviour, and it spends coins to do it.

    LEANPROXY_AB_LIVE=1 python3 scripts/abbench.py --out bench-results

THE THREE ARMS MUST MODEL THE SAME EXPERIMENT AS LAYER 1
--------------------------------------------------------
abreport.py joins the two layers on `ballast_tools` alone and computes

    net_tokens = residency_tokens x turns + output_tokens

so a turn count from a world Layer 1 did not measure gets multiplied by a
residency figure from a world this script did not run. Two ways that used to
go wrong, both fixed here and both guarded by tests:

- Ballast WEIGHT. Layer 1 gives every ballast tool a 568-character
  description; this script used to omit --description entirely and fall back
  to mockmcp's 46-character default, so the same `ballast_tools=100` meant
  ~67.7 KB in one layer and ~15.6 KB in the other. Both layers now read the
  description from tests/bench/e2e/fixtures/ballast.json, and
  TestBallastWeightIsIdenticalAcrossLayers measures both layers' real
  tools/list payloads and fails on a one-byte difference (review C-1).

- Ballast TOPOLOGY. Layer 1's Capture() puts ballast BEHIND the proxy for the
  router and lazy arms and dials it directly only for native. This script used
  to attach ballast directly to the agent in all three arms, so the proxy arms'
  live sessions carried schema weight their residency figure said the proxy had
  hidden — an error that ran one way, in LeanProxy's favour, and for the router
  arm inverted the sign of the comparison. `arm_config` now mirrors Layer 1:
  ballast goes into the arm's leanproxy config for the proxy arms and directly
  onto the agent only for native (review C-2).

Following from that, the native arm attaches the PROXIED servers directly, so
it genuinely tests "same tools, no proxy" rather than "whatever the operator
happened to leave enabled outside the proxy" (review C-4). `preflight()` then
verifies, before a single coin is spent, that every arm can actually reach
every tool the task fixture expects.

Python 3.9+, standard library only.
"""

from __future__ import annotations

import argparse
import copy
import hashlib
import json
import os
import queue
import re
import shutil
import signal
import sqlite3
import subprocess
import sys
import tempfile
import threading
import time

HOME = os.path.expanduser("~")
DEFAULT_BOB_CFG = os.path.join(HOME, ".bob", "settings", "mcp.json")
DEFAULT_LP_CFG = os.path.join(HOME, ".config", "leanproxy_servers.yaml")
DEFAULT_DB = os.path.join(HOME, ".bob", "db", "bob.db")  # default db_path for run_task/read_task_result

_REPO_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DEFAULT_TASKS = os.path.join(_REPO_ROOT, "tests", "bench", "e2e", "fixtures", "tasks.json")
DEFAULT_BALLAST_FIXTURE = os.path.join(_REPO_ROOT, "tests", "bench", "e2e", "fixtures", "ballast.json")

ARMS = ("native", "router", "lazy")
PROXY_ARMS = ("router", "lazy")

# A single surviving pair can't disagree with itself: `signs` has at most one
# element by construction at n=1, so an unguarded consistency check would
# report a lone, possibly noisy, data point as a confident finding. Below
# this many pairs, `paired_deltas` reports "insufficient_pairs" instead.
MIN_PAIRS_FOR_CONSISTENCY = 2


# ---------------------------------------------------------------------------
# shared ballast fixture (Layer 1 / Layer 2 single source of truth)
# ---------------------------------------------------------------------------


def load_ballast_fixture(path: str = DEFAULT_BALLAST_FIXTURE) -> dict:
    """The ballast definition shared with Layer 1, validated on the way in.

    tests/bench/e2e/ballast.go embeds this same file with //go:embed, so the
    two layers cannot describe differently-weighted ballast tools without the
    file itself changing. The recorded `description_chars` is checked against
    the description it ships: a fixture whose own measurement disagrees with
    the thing it measures is the exact defect this file exists to prevent, so
    it raises rather than being quietly believed (mustLoadBallastFixture on
    the Go side panics for the same reason).
    """
    with open(path) as fh:
        doc = json.load(fh)
    desc = doc.get("description")
    if not isinstance(desc, str) or not desc:
        raise ValueError(f"{path}: 'description' must be a non-empty string")
    chars = doc.get("description_chars")
    if chars != len(desc):
        raise ValueError(
            f"{path}: description is {len(desc)} characters but description_chars "
            f"says {chars!r} — fix one of them before trusting either"
        )
    return doc


# ---------------------------------------------------------------------------
# leanproxy config parsing
# ---------------------------------------------------------------------------


_LIST_ITEM_RE = re.compile(r"^-\s+(?P<rest>\S.*)$")
_KEY_RE = re.compile(r"^(?P<key>[A-Za-z_][\w-]*):\s*(?P<value>.*)$")


def _strip_comment(line: str) -> str:
    """Strip a trailing ` # comment`, respecting simple single/double quoting."""
    quote = None
    for i, ch in enumerate(line):
        if quote:
            if ch == quote:
                quote = None
        elif ch in "'\"":
            quote = ch
        elif ch == "#" and (i == 0 or line[i - 1].isspace()):
            return line[:i]
    return line


def _unquote(value: str) -> str:
    v = value.strip()
    if len(v) >= 2 and v[0] == v[-1] and v[0] in "\"'":
        return v[1:-1]
    return v


def _significant_lines(text: str) -> list:
    """(indent, content, raw) for every non-blank, comment-stripped line."""
    out = []
    for raw in text.splitlines():
        line = _strip_comment(raw).rstrip()
        if not line.strip():
            continue
        indent = len(line) - len(line.lstrip())
        out.append((indent, line[indent:], raw))
    return out


def _parse_mapping(lines: list, key_indent: int) -> dict:
    """Parse `lines` as a block mapping whose keys sit at `key_indent`.

    Anything that doesn't fit that shape raises. Strictness is the point:
    this parser feeds a confound guard and, now, the native arm's own server
    list — a parser that can silently under-report either one is worse than
    one that refuses to run.
    """
    out = {}
    i = 0
    while i < len(lines):
        indent, content, raw = lines[i]
        if indent != key_indent:
            raise ValueError(f"unrecognised leanproxy config line: {raw!r}")
        km = _KEY_RE.match(content)
        if not km:
            raise ValueError(f"unrecognised leanproxy config line: {raw!r}")
        key = km.group("key")
        value = km.group("value").strip()

        j = i + 1
        while j < len(lines) and lines[j][0] > key_indent:
            j += 1
        nested = lines[i + 1:j]

        if nested:
            if value:
                # real YAML rejects this outright ("mapping values are not
                # allowed here"); treating it as harmless nesting would
                # silently drop whatever it says.
                raise ValueError(
                    f"unrecognised leanproxy config line: {nested[0][2]!r} — indented "
                    f"past a key that already had a value"
                )
            out[key] = _parse_block(nested)
        else:
            out[key] = value
        i = j
    return out


def _parse_list(lines: list, indent: int) -> list:
    """Parse `lines` as a block list whose `-` markers sit at `indent`."""
    starts = [
        i for i, (ind, content, _) in enumerate(lines)
        if ind == indent and _LIST_ITEM_RE.match(content)
    ]
    if not starts or starts[0] != 0:
        raise ValueError(f"unrecognised leanproxy config line: {lines[0][2]!r}")
    bounds = starts + [len(lines)]
    out = []
    for a, b in zip(bounds, bounds[1:]):
        head_indent, head_content, head_raw = lines[a]
        rest = _LIST_ITEM_RE.match(head_content).group("rest")
        km = _KEY_RE.match(rest)
        body = lines[a + 1:b]
        if not km:
            if body:
                raise ValueError(f"unrecognised leanproxy config line: {head_raw!r}")
            out.append(_unquote(rest))
            continue
        item_key_indent = head_indent + (len(head_content) - len(rest))
        seeded = [(item_key_indent, rest, head_raw)] + body
        out.append(_parse_mapping(seeded, item_key_indent))
    return out


def _parse_block(lines: list):
    """Parse a nested block as either a list or a mapping, by its first line."""
    indent = lines[0][0]
    if _LIST_ITEM_RE.match(lines[0][1]):
        return _parse_list(lines, indent)
    return _parse_mapping(lines, indent)


def lp_servers(lp_cfg_text: str) -> list:
    """Every entry of a leanproxy_servers.yaml `servers:` block, as dicts.

    Deliberately a small indent-aware block parser rather than a full YAML
    parse: this module is stdlib-only. Strict parsing is scoped to the
    `servers:` block only — real configs have unrelated top-level sections
    afterward (e.g. `bouncer:`, with its own nested `enabled:`) that this has
    no business interpreting — but inside that block anything unrecognised
    raises rather than being silently dropped.

    Nested blocks (`stdio:`, `http:`, `tools:`) are parsed rather than
    skipped, because the native arm now has to reproduce a proxied server's
    command line faithfully enough to attach it directly to the agent
    (review C-4).
    """
    lines = _significant_lines(lp_cfg_text)
    servers = []
    i = 0
    while i < len(lines):
        indent, content, raw = lines[i]
        km = _KEY_RE.match(content)
        if indent == 0 and km and km.group("key") == "servers":
            if km.group("value").strip():
                raise ValueError(
                    f"unsupported inline value for the servers: block: {raw!r} "
                    f"— expected a block list, not a flow-style/scalar value"
                )
            j = i + 1
            while j < len(lines) and lines[j][0] > 0:
                j += 1
            block = lines[i + 1:j]
            if block:
                parsed = _parse_list(block, block[0][0])
                for item in parsed:
                    if not isinstance(item, dict):
                        raise ValueError(f"unrecognised leanproxy server entry: {item!r}")
                    servers.append(item)
            i = j
            continue
        i += 1
    return servers


def _server_enabled(srv: dict) -> bool:
    value = srv.get("enabled")
    return isinstance(value, str) and value.strip().lower() == "true"


def _lp_enabled_servers(lp_cfg_text: str) -> list:
    """Names of servers marked enabled in a leanproxy_servers.yaml."""
    return [
        _unquote(s["name"]) for s in lp_servers(lp_cfg_text)
        if s.get("name") and _server_enabled(s)
    ]


def _flow_list(value, what: str) -> list:
    """An inline flow-style list (`[]`, `["--a", "--b"]`) as a Python list.

    A block-style list has already been parsed into a real list by
    `_parse_list`, so it passes through. Anything else raises: a silently
    mis-parsed args list would launch the native arm's copy of a server with
    a different command line than the proxy arms', which is precisely the
    class of cross-arm divergence this whole module is trying to eliminate.
    """
    if isinstance(value, list):
        return [_unquote(v) if isinstance(v, str) else v for v in value]
    if value is None:
        return []
    v = str(value).strip()
    if not v:
        return []
    if not (v.startswith("[") and v.endswith("]")):
        raise ValueError(f"{what}: expected a list, got {value!r}")
    inner = v[1:-1].strip()
    if not inner:
        return []
    # Split on commas that are not inside quotes, keeping each item's raw
    # text. Whitespace AROUND an item is separator noise and goes; whitespace
    # INSIDE a quoted item is part of the value and stays, so an argument that
    # legitimately ends in a space survives the round trip.
    parts, buf, quote = [], "", None
    for ch in inner:
        if quote:
            buf += ch
            if ch == quote:
                quote = None
            continue
        if ch in "\"'":
            quote = ch
            buf += ch
            continue
        if ch == ",":
            parts.append(buf)
            buf = ""
            continue
        buf += ch
    if quote:
        raise ValueError(f"{what}: unterminated quote in {value!r}")
    parts.append(buf)
    return [_unquote(p) for p in parts]


def _server_identity(srv: dict) -> tuple:
    """(kind, value) uniquely identifying the upstream a config entry points at.

    Names are not identity: on this operator's own machine the same upstream
    binary is registered as `codebase-memory-mcp` in Bob and `codebase-memory`
    in the leanproxy config, and a name-only confound guard waved the
    resulting double-load straight through (review I-2). stdio servers are
    identified by their resolved command path, http servers by URL.
    """
    transport = _unquote(srv.get("transport", "")).lower()
    stdio = srv.get("stdio")
    http = srv.get("http")
    if isinstance(stdio, dict) and stdio.get("command"):
        return ("command", os.path.realpath(_unquote(stdio["command"])))
    if isinstance(http, dict) and http.get("url"):
        return ("url", _unquote(http["url"]))
    if transport:
        return ("transport-only", transport)
    return ("unknown", "")


def _bob_entry_identity(entry: dict) -> tuple:
    if entry.get("url"):
        return ("url", str(entry["url"]))
    if entry.get("command"):
        return ("command", os.path.realpath(str(entry["command"])))
    return ("unknown", "")


def detect_confound(bob_cfg: dict, lp_cfg_text: str) -> list:
    """Servers reachable both directly from the agent and through the proxy.

    A server loaded twice inflates baseline schema weight and confounds every
    arm, so the harness refuses to run until this is resolved. Matching is on
    NAME *and* on resolved identity (command path / URL), because the same
    upstream registered under two different names is the same double-load and
    the name-only version of this guard missed it (review I-2).
    """
    proxied = [s for s in lp_servers(lp_cfg_text) if _server_enabled(s)]
    by_name = {_unquote(s["name"]): s for s in proxied if s.get("name")}
    by_identity = {}
    for s in proxied:
        ident = _server_identity(s)
        if ident[0] in ("command", "url"):
            by_identity.setdefault(ident, []).append(_unquote(s.get("name", "?")))

    problems = []
    for name, entry in (bob_cfg.get("mcpServers") or {}).items():
        if name == "leanproxy":
            continue
        if entry.get("disabled") is True or entry.get("enabled") is False:
            continue
        if name in by_name:
            problems.append(
                f"{name!r} is enabled directly in Bob and also proxied by leanproxy "
                f"— it would be loaded twice"
            )
            continue
        ident = _bob_entry_identity(entry)
        aliases = by_identity.get(ident)
        if aliases:
            problems.append(
                f"{name!r} is enabled directly in Bob and the SAME upstream "
                f"({ident[0]}={ident[1]!r}) is proxied by leanproxy under the name(s) "
                f"{aliases} — a different name is still a double-load"
            )
    return problems


# ---------------------------------------------------------------------------
# converting proxied servers into agent-attached servers (native arm)
# ---------------------------------------------------------------------------


class UnmirrorableServer(Exception):
    """A proxied server the native arm cannot faithfully attach directly.

    Raised rather than approximated. An approximated native arm is a
    comparison against a different tool inventory wearing the same label,
    which is the failure this harness exists to catch, not commit.
    """


def lp_server_to_bob_entry(srv: dict) -> dict:
    """One leanproxy server config as an agent (Bob) mcpServers entry.

    The native arm exists to answer "same tools, no proxy", so this has to
    reproduce the upstream exactly. Where it cannot — env vars and HTTP auth
    headers, whose representation in Bob's config this harness has no
    authority to guess — it raises instead of shipping a native arm that
    quietly talks to a differently-configured server (or to none at all).
    """
    name = _unquote(srv.get("name", ""))
    transport = _unquote(srv.get("transport", "")).lower()
    stdio = srv.get("stdio")
    http = srv.get("http")

    if transport in ("", "stdio") and isinstance(stdio, dict):
        command = _unquote(stdio.get("command", ""))
        if not command:
            raise UnmirrorableServer(f"{name!r}: stdio server has no command")
        env = _flow_list(stdio.get("env", ""), f"{name}.stdio.env")
        if env:
            raise UnmirrorableServer(
                f"{name!r}: sets env {env} in the leanproxy config, and this harness "
                f"will not guess how to express that in the agent's own config — the "
                f"native arm would then run a differently-configured server than the "
                f"proxy arms. Move the variables into the process environment, or "
                f"disable this server for the duration of the sweep."
            )
        entry = {
            "command": command,
            "args": _flow_list(stdio.get("args", ""), f"{name}.stdio.args"),
            "disabled": False,
            "enabled": True,
        }
        cwd = _unquote(stdio.get("cwd", "")) if isinstance(stdio.get("cwd"), str) else ""
        if cwd:
            entry["cwd"] = cwd
        return entry

    if transport in ("http", "sse", "streamable-http") and isinstance(http, dict):
        url = _unquote(http.get("url", ""))
        if not url:
            raise UnmirrorableServer(f"{name!r}: http server has no url")
        headers = http.get("headers")
        if headers:
            raise UnmirrorableServer(
                f"{name!r}: is an HTTP server with auth headers configured in "
                f"leanproxy. This harness will not copy credentials into the agent's "
                f"config, so the native arm cannot reach it and would silently "
                f"measure a smaller tool inventory than the proxy arms. Disable this "
                f"server for the duration of the sweep."
            )
        return {"type": "http", "url": url, "disabled": False, "enabled": True}

    raise UnmirrorableServer(
        f"{name!r}: transport {transport!r} has no stdio/http block this harness "
        f"knows how to attach directly to the agent"
    )


def direct_entries_for_proxied_servers(lp_cfg_text: str) -> dict:
    """Bob mcpServers entries for every ENABLED proxied server.

    This is what makes the native arm a real baseline: the arms differ only
    in whether leanproxy sits in between, not in which servers exist.
    """
    entries = {}
    problems = []
    for srv in lp_servers(lp_cfg_text):
        if not _server_enabled(srv):
            continue
        name = _unquote(srv.get("name", ""))
        if not name:
            problems.append("a server entry has no name")
            continue
        try:
            entries[name] = lp_server_to_bob_entry(srv)
        except UnmirrorableServer as exc:
            problems.append(str(exc))
    if problems:
        raise UnmirrorableServer(
            "the native arm cannot be built from the leanproxy config:\n  - "
            + "\n  - ".join(problems)
        )
    return entries


def tool_filtered_servers(lp_cfg_text: str) -> list:
    """Enabled proxied servers whose tool list the proxy filters.

    A `tools.include`/`tools.exclude` filter means the proxy arms see fewer
    tools than a directly-attached copy of the same server, so "same tools,
    no proxy" is not literally true for that server. That is a real
    asymmetry in the native arm's favour on residency and possibly against it
    on discovery, and it is the operator's call to accept — but it must be an
    explicit call, not a silent one.
    """
    out = []
    for srv in lp_servers(lp_cfg_text):
        if not _server_enabled(srv):
            continue
        tools = srv.get("tools")
        if isinstance(tools, dict) and (tools.get("include") or tools.get("exclude")):
            out.append(_unquote(srv.get("name", "?")))
    return out


# ---------------------------------------------------------------------------
# arm configuration
# ---------------------------------------------------------------------------


def ballast_servers(mock_bin: str, servers: int, tools_per: int, description: str) -> dict:
    """Agent mcpServers entries for synthetic ballast (native arm only).

    `description` is REQUIRED and has no default. mockmcp falls back to a
    46-character built-in description when `--description` is absent, which is
    4.3x lighter per tool than the 568-character one Layer 1 uses — and
    nothing downstream can detect the difference, because abreport.py joins
    the layers on the tool COUNT (review C-1). Callers pass
    `load_ballast_fixture()["description"]`, the same bytes ballast.go embeds.
    """
    if not description:
        raise ValueError(
            "ballast_servers requires the shared ballast description "
            "(load_ballast_fixture()['description']); without it mockmcp falls back "
            "to its 46-char default and Layer 2's ballast weighs 4.3x less per tool "
            "than Layer 1's, silently mislabelling the joined x-axis"
        )
    return {
        f"ballast{i}": {
            "command": mock_bin,
            "args": [f"--tools={tools_per}", f"--description={description}"],
            "disabled": False,
            "enabled": True,
        }
        for i in range(servers)
    }


def ballast_lp_entries(mock_bin: str, servers: int, tools_per: int, description: str) -> list:
    """The same ballast, as leanproxy server config dicts (proxy arms).

    Layer 1 writes exactly this shape into a leanproxy config for the router
    and lazy arms (tests/bench/e2e/arms.go:Capture). Same binary, same args,
    same description — only the side of the proxy differs.
    """
    if not description:
        raise ValueError("ballast_lp_entries requires the shared ballast description")
    return [
        {
            "name": f"ballast{i}",
            "command": mock_bin,
            "args": [f"--tools={tools_per}", f"--description={description}"],
        }
        for i in range(servers)
    ]


_ADAPTIVE_STUB_RE = re.compile(r"^\s*adaptive_stub_after:\s*(?P<value>.*)$")


def build_arm_lp_config(lp_cfg_text: str, ballast: list) -> str:
    """The leanproxy config the proxy arms run: the operator's own, plus ballast.

    Splicing into a copy of the real config rather than regenerating one keeps
    every proxied server's real settings (tool filters, response caps, result
    caches) byte-for-byte, so the proxy arms front exactly what they front in
    production — with the ballast added BEHIND the proxy, matching Layer 1
    (review C-2).

    `adaptive_stub_after` is stripped. Under adaptive stubs the proxy's
    tools/list depends on usage recorded in ~/.config/leanproxy/toolusage.json
    and every live run mutates it, so the lazy arm's residency would drift
    during the sweep and would no longer describe the fixed figure Layer 1
    measured against a config with no such setting (review I-4).
    """
    lines = lp_cfg_text.splitlines()

    kept = []
    for raw in lines:
        m = _ADAPTIVE_STUB_RE.match(_strip_comment(raw).rstrip())
        if m:
            if not m.group("value").strip():
                raise ValueError(
                    f"cannot neutralise adaptive stubs: {raw!r} opens a block rather "
                    f"than carrying a duration — remove it by hand before sweeping"
                )
            continue
        kept.append(raw)

    servers_at = []
    for i, raw in enumerate(kept):
        line = _strip_comment(raw).rstrip()
        if line != line.lstrip():
            continue  # not a top-level key
        km = _KEY_RE.match(line)
        if km and km.group("key") == "servers":
            servers_at.append(i)
    if len(servers_at) != 1:
        raise ValueError(
            f"expected exactly one top-level `servers:` block in the leanproxy "
            f"config, found {len(servers_at)} — refusing to guess where ballast goes"
        )

    if not ballast:
        return "\n".join(kept) + "\n"

    block = []
    for spec in ballast:
        args = ", ".join(json.dumps(a) for a in spec["args"])
        block.extend([
            f"  - name: {spec['name']}",
            "    transport: stdio",
            "    enabled: true",
            "    stdio:",
            f"      command: {json.dumps(spec['command'])}",
            f"      args: [{args}]",
            "      env: []",
        ])

    at = servers_at[0]
    return "\n".join(kept[:at + 1] + block + kept[at + 1:]) + "\n"


def arm_config(arm, leanproxy_bin, base, ballast=None, direct_servers=None,
               lp_config_path=None, log_file=None) -> dict:
    """Return an agent mcp.json for the given arm, leaving `base` untouched.

    The topology mirrors Layer 1's `Capture` exactly:

      native  — no proxy; the proxied servers AND the ballast are attached
                directly to the agent (Layer 1's captureNative dials both
                kinds of upstream directly).
      router  — leanproxy without --lazy-tools, pointed at `lp_config_path`,
                which already contains the proxied servers AND the ballast.
                Nothing extra is attached to the agent.
      lazy    — same, with --lazy-tools.

    Ballast is therefore attached directly in exactly one arm, not in all
    three. Attaching it in all three (the previous behaviour) added an
    identical constant to every arm's live session while Layer 1's residency
    said the proxy had hidden all of it, so the swept variable moved the
    reported answer without moving the measured behaviour (review C-2).
    """
    if arm not in ARMS:
        raise ValueError(f"unknown arm: {arm}")

    cfg = copy.deepcopy(base)
    servers = cfg.setdefault("mcpServers", {})
    servers.pop("leanproxy", None)

    def _attach(new, kind):
        if not new:
            return
        collisions = sorted(set(new) & set(servers))
        if collisions:
            raise ValueError(
                f"{kind} server name(s) {collisions} already present in the "
                f"operator's mcpServers — refusing to silently overwrite a "
                f"real server for the duration of this arm's live run"
            )
        servers.update(copy.deepcopy(new))

    if arm == "native":
        _attach(direct_servers, "proxied")
        _attach(ballast, "ballast")
        return cfg

    if not lp_config_path:
        raise ValueError(
            f"arm {arm!r} needs lp_config_path: the proxy arms carry their ballast "
            f"behind the proxy, which requires an explicit leanproxy config"
        )

    args = ["server", "run", "--stdio", "--config", lp_config_path]
    if arm == "lazy":
        args.append("--lazy-tools")
    if log_file:
        args.extend(["--log-file", log_file])

    servers["leanproxy"] = {
        "command": leanproxy_bin,
        "args": args,
        "disabled": False,
        "enabled": True,
    }
    return cfg


# ---------------------------------------------------------------------------
# a minimal MCP stdio client, for the pre-spend preflight
# ---------------------------------------------------------------------------


class McpStdio:
    """Just enough MCP to ask a server what tools it has.

    Mirrors tests/bench/e2e/client.go: initialize, notifications/initialized,
    tools/list, tools/call. Reads run on a background thread so a server that
    starts but never answers times out instead of hanging the preflight
    forever — a preflight that can hang is a preflight operators skip.

    HOME (and USERPROFILE, for Windows) are pointed at a private, per-instance
    temp directory instead of the operator's real one, mirroring
    tests/bench/e2e/client.go's Dial. Without this, every preflight spawn of
    leanproxy-mcp — including on a path where the preflight goes on to REFUSE
    the configuration — writes ballast tool caches into the operator's real
    ~/.config/leanproxy/toolcache and rewrites the live daemon's
    ~/.config/leanproxy/status/current.json (internal/cachefile.Dir and
    pkg/statusfile resolve both via os.UserHomeDir, i.e. $HOME). See
    TestPreflightIsolatesHomeDirectory.
    """

    def __init__(self, command: str, args: list, cwd: str = None, timeout: float = 60.0):
        self.command = command
        self.args = list(args or [])
        self.cwd = cwd
        self.timeout = timeout
        self.proc = None
        self._lines = queue.Queue()
        self._stderr = []
        self._next = 1
        self._home_dir = None

    def __enter__(self):
        self._home_dir = tempfile.mkdtemp(prefix="leanproxy-ab-home-")
        try:
            env = dict(os.environ)
            env["HOME"] = self._home_dir
            env["USERPROFILE"] = self._home_dir
            self.proc = subprocess.Popen(
                [self.command] + self.args,
                cwd=self.cwd,
                env=env,
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
                bufsize=1,
            )
        except Exception:
            shutil.rmtree(self._home_dir, ignore_errors=True)
            self._home_dir = None
            raise
        threading.Thread(target=self._pump, args=(self.proc.stdout, self._lines), daemon=True).start()
        threading.Thread(target=self._drain_stderr, daemon=True).start()
        return self

    def __exit__(self, exc_type, exc, tb):
        self.close()
        return False

    @staticmethod
    def _pump(stream, sink):
        try:
            for line in stream:
                sink.put(line)
        except Exception:  # noqa: BLE001 - the process died; _rpc reports it
            pass
        finally:
            sink.put(None)

    def _drain_stderr(self):
        try:
            for line in self.proc.stderr:
                self._stderr.append(line.rstrip())
                del self._stderr[:-40]
        except Exception:  # noqa: BLE001
            pass

    def _tail(self) -> str:
        return "\n".join(self._stderr[-10:]) or "(no stderr)"

    def _send(self, obj: dict) -> None:
        try:
            self.proc.stdin.write(json.dumps(obj) + "\n")
            self.proc.stdin.flush()
        except (BrokenPipeError, ValueError) as exc:
            raise RuntimeError(
                f"{self.command}: died before it could be asked for its tools "
                f"({exc}); stderr tail:\n{self._tail()}"
            ) from None

    def _rpc(self, method: str, params: dict):
        self._next += 1
        request_id = self._next
        req = {"jsonrpc": "2.0", "id": request_id, "method": method}
        if params is not None:
            req["params"] = params
        self._send(req)

        deadline = time.time() + self.timeout
        while True:
            remaining = deadline - time.time()
            if remaining <= 0:
                raise RuntimeError(
                    f"{self.command}: no response to {method} within {self.timeout:.0f}s; "
                    f"stderr tail:\n{self._tail()}"
                )
            try:
                line = self._lines.get(timeout=remaining)
            except queue.Empty:
                continue
            if line is None:
                raise RuntimeError(
                    f"{self.command}: exited during {method}; stderr tail:\n{self._tail()}"
                )
            line = line.strip()
            if not line:
                continue
            try:
                msg = json.loads(line)
            except ValueError:
                continue  # servers legitimately emit non-JSON banners on stdout
            # Only this request's own response counts: a notification has no
            # id, and a server->client request carries both an id and a
            # `method`, so neither may be mistaken for the answer.
            if "method" in msg or msg.get("id") != request_id:
                continue
            if msg.get("error"):
                raise RuntimeError(f"{self.command}: {method} failed: {msg['error']}")
            return msg.get("result")

    def initialize(self):
        self._rpc("initialize", {
            "protocolVersion": "2024-11-05",
            "capabilities": {},
            "clientInfo": {"name": "abbench-preflight", "version": "1"},
        })
        self._send({"jsonrpc": "2.0", "method": "notifications/initialized"})

    def tool_names(self) -> list:
        result = self._rpc("tools/list", {}) or {}
        return [t.get("name", "") for t in result.get("tools", [])]

    def call_tool_text(self, name: str, arguments: dict) -> str:
        result = self._rpc("tools/call", {"name": name, "arguments": arguments}) or {}
        return "".join(
            block.get("text", "") for block in result.get("content", [])
            if isinstance(block, dict)
        )

    def close(self):
        try:
            if self.proc:
                try:
                    if self.proc.stdin:
                        self.proc.stdin.close()
                except Exception:  # noqa: BLE001
                    pass
                try:
                    self.proc.terminate()
                    self.proc.wait(timeout=5)
                except Exception:  # noqa: BLE001
                    try:
                        self.proc.kill()
                    except Exception:  # noqa: BLE001
                        pass
                self.proc = None
        finally:
            if self._home_dir:
                shutil.rmtree(self._home_dir, ignore_errors=True)
                self._home_dir = None


# ---------------------------------------------------------------------------
# preflight: refuse an unusable configuration BEFORE spending anything
# ---------------------------------------------------------------------------


def _native_inventory(entries: dict) -> tuple:
    """(tool names, unreachable notes) for a native-arm mcpServers dict."""
    names, notes = [], []
    for server, entry in sorted(entries.items()):
        if entry.get("type") or entry.get("url"):
            notes.append(f"{server}: HTTP transport — not inventoried by the preflight")
            continue
        try:
            with McpStdio(entry["command"], entry.get("args", [])) as c:
                c.initialize()
                names.extend(f"{server}_{n}" for n in c.tool_names())
        except Exception as exc:  # noqa: BLE001 - a dead server is a finding
            notes.append(f"{server}: could not be inventoried: {exc}")
    return names, notes


def _proxy_inventory(arm: str, leanproxy_bin: str, lp_config_path: str, server_names: list) -> tuple:
    """(searchable text, notes) describing what a proxy arm can reach.

    For the lazy arm that is the tools/list names directly. For the router arm
    tools/list is only the three wrappers, so this asks `list_tools` for each
    configured server — the same reachability probe Layer 1's
    verifyRouterReachable performs — and searches the text it returns.
    """
    args = ["server", "run", "--stdio", "--config", lp_config_path]
    if arm == "lazy":
        args.append("--lazy-tools")
    notes = []
    try:
        with McpStdio(leanproxy_bin, args) as c:
            c.initialize()
            names = c.tool_names()
            if arm == "lazy":
                return "\n".join(names), notes
            missing = [w for w in ("list_tools", "invoke_tool", "search_tools") if w not in names]
            if missing:
                notes.append(f"router arm is missing its wrapper tool(s) {missing}")
            chunks = []
            for server in server_names:
                try:
                    chunks.append(c.call_tool_text("list_tools", {"server_name": server}))
                except Exception as exc:  # noqa: BLE001
                    notes.append(f"router arm could not list tools for {server!r}: {exc}")
            return "\n".join(chunks), notes
    except Exception as exc:  # noqa: BLE001
        notes.append(f"{arm} arm could not be started: {exc}")
        return "", notes


def preflight(tasks, direct_servers, ballast_bob, leanproxy_bin,
              lp_config_path, lp_server_names, allow_tool_filters, filtered_servers) -> list:
    """Everything that must hold before the first coin is spent.

    The failure this exists to prevent is specific and was reproduced: with
    the proxied servers absent from the native arm and the router arm unable
    to score a success, a full sweep ran to completion, spent the whole
    budget, and produced zero verdicts — every one of those conditions being
    knowable in advance, for free. Returns a list of problems; empty means go.
    """
    problems = []
    expect = sorted({t["expect_tool"] for t in tasks})

    if not direct_servers:
        problems.append(
            "no enabled servers in the leanproxy config — there is nothing for the "
            "proxy arms to proxy and nothing for the native arm to attach"
        )
        return problems

    if filtered_servers and not allow_tool_filters:
        problems.append(
            f"server(s) {filtered_servers} have a tools.include/exclude filter in the "
            f"leanproxy config, so the proxy arms see fewer tools than the native arm's "
            f"directly-attached copy of the same server — the arms would not be "
            f"comparing the same inventory. Re-run with --allow-tool-filter-asymmetry "
            f"to accept that (it is recorded in every output record), or drop the "
            f"filter for the sweep."
        )

    native_entries = dict(direct_servers)
    native_entries.update(ballast_bob or {})
    names, notes = _native_inventory(native_entries)
    for note in notes:
        problems.append(f"native arm: {note}")
    for tool in expect:
        if not any(tool in n for n in names):
            problems.append(
                f"native arm: no tool matching {tool!r} — every task expecting it would "
                f"score succeeded=False and the arm would be dropped from the report "
                f"after being paid for"
            )

    for arm in PROXY_ARMS:
        text, notes = _proxy_inventory(arm, leanproxy_bin, lp_config_path, lp_server_names)
        for note in notes:
            problems.append(f"{arm} arm: {note}")
        for tool in expect:
            if tool not in text:
                problems.append(f"{arm} arm: cannot reach a tool matching {tool!r}")

    return problems


# ---------------------------------------------------------------------------
# working-tree protection
# ---------------------------------------------------------------------------


def git_status(cwd: str):
    """`git status --porcelain` for cwd, or None if this isn't a git repo.

    The sweep runs an unsupervised, write-capable agent in this directory
    (3 arms x 5 tasks x 2 ballast points = 30 autonomous runs by default). The
    fixture prompts are read-only questions, but nothing constrains the agent
    to them (review I-3), so the harness refuses to start on a dirty tree —
    there would be no way to tell afterwards what the sweep changed — and
    reports anything that changed once it finishes.
    """
    try:
        proc = subprocess.run(
            ["git", "status", "--porcelain"],
            cwd=cwd, capture_output=True, text=True, timeout=60,
        )
    except (OSError, subprocess.SubprocessError):
        return None
    if proc.returncode != 0:
        return None
    return proc.stdout


# ---------------------------------------------------------------------------
# config swap
# ---------------------------------------------------------------------------


def _swap_signals() -> tuple:
    """Signals whose default action would kill the process mid-swap.

    SIGHUP matters specifically: a closed terminal or dropped SSH session
    delivers it, its default action terminates the process, and the config
    left behind is an arm's — ballast attached and, in the native arm, no
    leanproxy at all. The operator's agent is then broken with nothing to say
    so (review I-6).
    """
    names = ("SIGINT", "SIGTERM", "SIGHUP", "SIGQUIT")
    return tuple(getattr(signal, n) for n in names if hasattr(signal, n))


class ConfigSwap:
    """Swap a JSON config into place, restoring it on every exit path.

    Restores on normal exit, on exception (including one raised inside
    __enter__ itself, which Python's `with` statement does NOT route to
    __exit__), and on SIGINT/SIGTERM/SIGHUP/SIGQUIT. A half-swapped config
    leaves the agent broken, so restoration is unconditional and fails closed:
    a pre-existing backup file means a prior run crashed without restoring (or
    another instance is already running), and this refuses to proceed rather
    than risk clobbering the one copy of the real original.
    """

    def __init__(self, path: str, new_cfg: dict):
        self.path = path
        self.new_cfg = new_cfg
        self.backup = path + ".abbench-backup"
        self._tmp = path + ".abbench-tmp"
        self._prev_handlers = {}
        self._backed_up = False

    def __enter__(self):
        self._claim_backup()
        try:
            for sig in _swap_signals():
                prev = signal.signal(sig, self._on_signal)
                self._prev_handlers[sig] = prev if prev is not None else signal.SIG_DFL
            text = json.dumps(self.new_cfg, indent=2)  # raises before any write happens
            with open(self._tmp, "w") as fh:
                fh.write(text)
            os.replace(self._tmp, self.path)  # atomic swap; path is never half-written
        except BaseException:
            self._restore()
            raise
        return self

    def _claim_backup(self):
        """Atomically claim the backup path AND fill it, refusing if one exists.

        The copy is written straight into the exclusively-created descriptor
        rather than opening it, closing it, and copying afterwards: death by
        signal in that window used to leave a 0-byte backup beside a
        perfectly good config, and the refusal message's first suggestion
        ("restore from the backup") would then destroy it (review I-6).
        """
        try:
            fd = os.open(self.backup, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        except FileExistsError:
            raise RuntimeError(
                f"backup file already exists at {self.backup!r} — a previous abbench "
                f"run may have crashed without restoring, or another instance is "
                f"running right now. CHECK THE BACKUP FIRST: if it is non-empty and "
                f"looks like your real config, move it back over {self.path!r}; if it "
                f"is empty or truncated, your real config is untouched and you should "
                f"just delete the backup. Do not copy an empty backup over a working "
                f"config."
            ) from None
        try:
            with open(self.path, "rb") as src, os.fdopen(fd, "wb") as dst:
                shutil.copyfileobj(src, dst)
                dst.flush()
                os.fsync(dst.fileno())
        except BaseException:
            try:
                os.close(fd)
            except OSError:
                pass  # fdopen already owns (and closed) it
            if os.path.exists(self.backup):
                os.remove(self.backup)
            raise
        self._backed_up = True

    def _on_signal(self, signum, frame):
        self._restore()
        sys.exit(128 + signum)

    def _restore(self):
        if self._backed_up and os.path.exists(self.backup):
            shutil.move(self.backup, self.path)
            self._backed_up = False
        elif os.path.exists(self.backup):
            # the backup claim was made but the copy never completed — it's an
            # empty stub, not a real backup; remove it without touching path
            os.remove(self.backup)
        if os.path.exists(self._tmp):
            os.remove(self._tmp)
        for sig, handler in self._prev_handlers.items():
            signal.signal(sig, handler)
        self._prev_handlers.clear()

    def __exit__(self, exc_type, exc, tb):
        self._restore()
        return False


# ---------------------------------------------------------------------------
# task fixture and Bob driver
# ---------------------------------------------------------------------------


def load_tasks(path: str) -> list:
    """Load and validate the frozen task fixture."""
    with open(path) as fh:
        doc = json.load(fh)
    tasks = doc.get("tasks") or []
    seen = set()
    for t in tasks:
        for key in ("id", "prompt", "expect_tool"):
            if key not in t:
                raise ValueError(f"task missing {key!r}: {t}")
        if t["id"] in seen:
            raise ValueError(f"duplicate task id: {t['id']}")
        seen.add(t["id"])
    return tasks


def _latest_task_row(db_path: str):
    """(id, first_message, workspace) for the newest task row, or None.

    Ordered by (created_at desc, id desc) to match this table's own index
    (idx_tasks_created) and give a deterministic tiebreak when two rows share
    a millisecond.
    """
    conn = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    try:
        return conn.execute(
            "select id, first_message, json_extract(env, '$.workspace') "
            "from tasks order by created_at desc, id desc limit 1"
        ).fetchone()
    finally:
        conn.close()


def run_task(prompt: str, cwd: str, db_path: str, timeout: int = 900) -> str:
    """Run one prompt through Bob headlessly and return the new task id.

    The id is recovered by diffing the newest task row before and after,
    since `bob run` does not print it in a machine-readable form. Bob's db is
    shared with any other session touching this repo — the operator running
    Bob interactively, or another arm of this same sweep — so a bare "newest
    row differs from `before`" check can silently attribute an unrelated
    session's task, and its costs, to this run. The candidate is therefore
    verified against both `workspace` and `first_message` before being
    trusted; any mismatch raises rather than guessing.
    """
    before = _latest_task_row(db_path)
    before_id = before[0] if before else None

    proc = subprocess.run(
        ["bob", "run", prompt],
        cwd=cwd,
        capture_output=True,
        text=True,
        timeout=timeout,
    )

    # Poll briefly: Bob writes the task row asynchronously on completion.
    deadline = time.time() + 30
    while time.time() < deadline:
        after = _latest_task_row(db_path)
        if after and after[0] != before_id:
            task_id, first_message, workspace = after
            if workspace != cwd or (first_message or "") != prompt:
                raise RuntimeError(
                    f"newest task row {task_id!r} does not match this run — "
                    f"got workspace={workspace!r} first_message={first_message!r}, "
                    f"expected workspace={cwd!r} first_message={prompt!r}; "
                    f"likely a concurrent Bob session in this repo raced the "
                    f"poll — refusing to attribute its costs to this run"
                )
            return task_id
        time.sleep(1)

    raise RuntimeError(
        f"no new task row appeared after `bob run` (exit {proc.returncode})\n"
        f"stdout: {proc.stdout[-500:]}\nstderr: {proc.stderr[-500:]}"
    )


def _tool_call_signature(data: str) -> tuple:
    """(called tool name, its arguments dict) from a role='tool' message.

    The real identifier lives at `toolUsage.signature.name` — verified
    against actual recorded history
    (`json_extract(data,'$.toolUsage.signature.name')`). Matching the whole
    `data` blob instead (as an earlier version of this function did) is
    wrong: it can also hit the tool's result `content`, so `succeeded` can go
    true because the expected tool's name merely appeared in some *other*
    tool's output text.

    Arguments come back too, because in the router arm the tool the model
    reached for is an ARGUMENT and never a called tool name (see
    `_reached_expected_tool`).

    A row that isn't valid JSON, or is JSON but missing the name, raises
    rather than being silently treated as "no match": a shape this doesn't
    recognise could just as easily be the exact tool being looked for, and
    reporting failure for a task that actually succeeded would be a worse
    outcome than a loud error naming the row.
    """
    try:
        obj = json.loads(data)
    except (TypeError, ValueError) as exc:
        raise ValueError(f"tool message is not valid JSON: {data[:200]!r}") from exc
    try:
        signature = obj["toolUsage"]["signature"]
        name = signature["name"]
    except (KeyError, TypeError) as exc:
        raise ValueError(
            f"tool message missing toolUsage.signature.name: {data[:200]!r}"
        ) from exc

    arguments = signature.get("arguments")
    if isinstance(arguments, str):
        try:
            arguments = json.loads(arguments)
        except ValueError:
            arguments = None
    if not isinstance(arguments, dict):
        arguments = {}
    return name, arguments


# leanproxy's router wrapper. In the router arm the client sees only
# list_tools / invoke_tool / search_tools (pkg/mcp/tool_index.go); the real
# tool is invoke_tool's `tool` argument.
ROUTER_INVOKE_TOOL = "invoke_tool"


def _reached_expected_tool(name: str, arguments: dict, expect_tool: str) -> bool:
    """Did this one recorded tool call reach `expect_tool`?

    Two ways, and only two:

    - The called tool's own name contains it. Substring, not equality,
      because the same tool is `codebase-memory_get_architecture` behind the
      proxy and `get_architecture` natively; fixtures name the unprefixed
      form so both arms compare.

    - The called tool is leanproxy's `invoke_tool` wrapper AND its `tool`
      argument names it. In the router arm this is the ONLY way the model can
      reach a real tool, so without this clause every router run scored
      False, the whole arm was correctly dropped by abreport's suppression
      logic, and the operator had already paid for every run of it
      (review C-3).

    This does not weaken the rule anywhere. The second clause fires only for
    a tool literally named `invoke_tool`, and only on its `tool` argument —
    not on `search_tools`' query, not on any tool's result text. A router run
    where the model searched and listed but never invoked the right tool
    still scores False, which is the genuine discovery failure the success
    rule exists to catch.
    """
    if expect_tool in name:
        return True
    if name.split("__")[-1] == ROUTER_INVOKE_TOOL or name == ROUTER_INVOKE_TOOL:
        invoked = arguments.get("tool")
        return isinstance(invoked, str) and expect_tool in invoked
    return False


# Bob writes `tasks.costs` asynchronously, after the row itself exists, so
# a read taken the moment `run_task` returns can legitimately find it empty.
COSTS_TIMEOUT_SECONDS = 60.0
_COSTS_POLL_INTERVAL = 0.25

# (record key, Bob's costs key). A key absent from a populated costs object
# is OMITTED from the record rather than defaulted to 0 — `paired_deltas`
# already drops a pair whose record lacks the field, and abreport's
# `_is_scored` already treats a record without `output_tokens` as unscored.
# A 0 would instead be averaged in as a real measurement.
_COSTS_FIELDS = (
    ("input_tokens", "input"),
    ("output_tokens", "output"),
    ("cache_read", "cacheRead"),
    ("cache_write", "cacheWrite"),
    ("cost_usd", "cost"),
    ("context_tokens", "contextTokens"),
)


def _read_costs(db_path: str, task_id: str, timeout: float) -> dict:
    """Bob's `costs` object for one task, waiting for it to be written.

    Raises rather than returning `{}`. An unpopulated costs column means "no
    measurement exists", and the caller's `.get(field, 0)` would turn that
    into a measured $0 run with 0 input tokens — which then enters
    `paired_deltas` at face value and drags abreport's observed-token means.
    The spec's own audit found costs populated on only 270 of 351 real
    tasks, so this is the common case, not an exotic one. Raising routes it
    to `main`'s existing handler, which records the run as a failure with an
    error and moves on.
    """
    deadline = time.time() + timeout
    while True:
        conn = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
        try:
            row = conn.execute(
                "select costs from tasks where id = ?", (task_id,)).fetchone()
        finally:
            conn.close()

        raw = row[0] if row else None
        if raw:
            try:
                costs = json.loads(raw)
            except ValueError as exc:
                raise RuntimeError(
                    f"task {task_id!r} costs is not valid JSON: {raw[:200]!r}"
                ) from exc
            if isinstance(costs, dict) and costs:
                return costs

        remaining = deadline - time.time()
        if remaining <= 0:
            raise RuntimeError(
                f"task {task_id!r} has no costs after {timeout:g}s — Bob never "
                f"populated tasks.costs for this run, so it has no measured "
                f"tokens or cost. Recording it as a failure rather than as a "
                f"$0 run with zero tokens."
            )
        time.sleep(min(_COSTS_POLL_INTERVAL, remaining))


def read_task_result(db_path: str, task_id: str, expect_tool: str,
                     costs_timeout: float = None) -> dict:
    """Read ground-truth token counts, turn count, and success for one task.

    Turn count is the number of assistant messages: each one is a model call
    that re-sends the whole conversation, which is exactly the cost lazy
    loading trades residency for.

    `succeeded` means "the model reached for the right tool" (see
    `_reached_expected_tool`), not "the task completed correctly": a tool
    call's own `toolUsage.signature.isError` is not consulted.

    Raises if Bob never wrote this task's costs (see `_read_costs`).
    """
    if costs_timeout is None:
        costs_timeout = COSTS_TIMEOUT_SECONDS
    costs = _read_costs(db_path, task_id, costs_timeout)

    conn = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    try:
        turns = conn.execute(
            "select count(*) from messages where task_id = ? and role = 'assistant'",
            (task_id,),
        ).fetchone()[0]

        tool_rows = conn.execute(
            "select data from messages where task_id = ? and role = 'tool'",
            (task_id,),
        ).fetchall()
    finally:
        conn.close()

    calls = [_tool_call_signature(r[0]) for r in tool_rows if r[0]]
    succeeded = any(_reached_expected_tool(n, a, expect_tool) for n, a in calls)

    res = {"task_id": task_id, "turns": turns, "succeeded": succeeded}
    for key, costs_key in _COSTS_FIELDS:
        if costs_key in costs:
            res[key] = costs[costs_key]
    return res


# ---------------------------------------------------------------------------
# paired analysis
# ---------------------------------------------------------------------------


def paired_deltas(records: list, baseline: str, arm: str, field: str) -> dict:
    """Per-task deltas between two arms, paired on task id.

    Pairing is the primary statistic. Observed per-task cost spans an order of
    magnitude, so an unpaired mean over five tasks would report task difficulty
    rather than arm effect. Tasks present in only one arm are dropped: an
    unpaired point cannot contribute a delta. The same reasoning applies when
    a task IS present in both arms but one side's record has no `field` at
    all — a `run_task` failure records `succeeded: False` and an `error`
    instead of token/cost metrics (see `main`), and a bare `KeyError` there
    would crash the whole sweep's reporting instead of just excluding that
    one unpaired point.

    Task keys carry an `@<ballast_level>` suffix, so pairing is automatically
    within-level. Callers that summarise across a multi-level sweep must
    still summarise PER LEVEL — a total summed over every level mixes
    conditions and a `consistent` flag demanding one sign across all of them
    asks the wrong question (review M-4); `main` does this per level.

    `consistent` reports whether every paired delta shares a sign AND there
    are at least `MIN_PAIRS_FOR_CONSISTENCY` of them — a single pair cannot
    disagree with itself, so without that floor one surviving pair (e.g.
    four of five tasks failed to pair) would print as a confident finding
    from what is really just noise. When `consistent` is False the caller
    should report "no detectable effect" rather than a point estimate.

    `verdict` spells out which of the three states produced `consistent`,
    so a downstream consumer doesn't have to re-derive "not enough pairs"
    vs. "enough pairs, but they disagree" by cross-referencing `pairs`
    itself: one of `"no_pairs"`, `"insufficient_pairs"`, `"consistent"`,
    `"inconsistent"`.
    """
    by_arm = {}
    for r in records:
        by_arm.setdefault(r["arm"], {})[r["task"]] = r

    base = by_arm.get(baseline, {})
    other = by_arm.get(arm, {})
    shared = sorted(
        t for t in (set(base) & set(other))
        if field in base[t] and field in other[t]
    )

    deltas = {t: other[t][field] - base[t][field] for t in shared}
    signs = {d > 0 for d in deltas.values() if d != 0}
    pairs = len(shared)
    same_sign = len(signs) <= 1
    consistent = pairs >= MIN_PAIRS_FOR_CONSISTENCY and same_sign

    if pairs == 0:
        verdict = "no_pairs"
    elif pairs < MIN_PAIRS_FOR_CONSISTENCY:
        verdict = "insufficient_pairs"
    elif same_sign:
        verdict = "consistent"
    else:
        verdict = "inconsistent"

    return {
        "baseline": baseline,
        "arm": arm,
        "field": field,
        "pairs": pairs,
        "deltas": deltas,
        "total_delta": sum(deltas.values()),
        "consistent": consistent,
        "verdict": verdict,
    }


def _is_scored(r: dict) -> bool:
    """Did this run get far enough to produce turn/output metrics?

    False for a run that crashed before scoring: `main`'s run_task and
    read_task_result handlers persist `succeeded: False` and an `error`, but
    no `turns`/`output_tokens`.
    """
    return "turns" in r and "output_tokens" in r


def _is_successful(r: dict) -> bool:
    """Did this run produce metrics AND reach the expected tool?

    Both halves matter, and conflating them is what this function exists to
    stop. A run that CRASHED carries no metrics, so it drops out of any
    average by itself. A run that COMPLETED but never found the expected
    tool carries full cost and turn fields, and averaging it in lets a
    badly-failing arm look cheapest: a model that gives up early posts a low
    turn count and a small bill. abreport excludes exactly this class
    (bc3025f); abbench's own summary now agrees with it rather than printing
    a note claiming an exclusion it never performed.

    An ABSENT `succeeded` key counts as success, matching abreport's
    `_is_successful`: real `read_task_result` output always carries the key
    explicitly, so absence only arises in minimal fixtures.
    """
    return _is_scored(r) and r.get("succeeded", True) is True


def _success_rate(records: list, arm: str, level) -> float:
    at = [r for r in records if r["arm"] == arm and r["ballast_tools"] == level]
    if not at:
        return None
    return sum(1 for r in at if _is_successful(r)) / len(at)


def paired_summary_lines(records: list) -> list:
    """The per-level paired-delta summary, as lines.

    Only successful, scored runs reach `paired_deltas` (see
    `_is_successful`), and every verdict carries the success rate that
    exclusion implies — a `+0.0031` computed from two of five runs is a
    different claim from the same number computed from five of five, and the
    reader should not have to cross-reference the failure list below to tell
    them apart.
    """
    lines = ["Paired per-task deltas vs native (primary statistic), per ballast level:"]
    levels = sorted({r["ballast_tools"] for r in records})
    for level in levels:
        at_level = [r for r in records if r["ballast_tools"] == level]
        usable = [r for r in at_level if _is_successful(r)]
        for arm in PROXY_ARMS:
            for field in ("cost_usd", "turns"):
                d = paired_deltas(usable, "native", arm, field)
                if d["verdict"] == "no_pairs":
                    continue
                if d["verdict"] == "insufficient_pairs":
                    verdict = f"insufficient pairs (n={d['pairs']}) — no finding"
                elif d["verdict"] == "consistent":
                    verdict = f"{d['total_delta']:+.4f}"
                else:
                    verdict = "no detectable effect (signs disagree)"

                rates = []
                for name in ("native", arm):
                    rate = _success_rate(at_level, name, level)
                    if rate is not None:
                        rates.append(f"{name} {rate:.0%} ok")
                suffix = f"  [{', '.join(rates)}]" if rates else ""
                lines.append(f"  ballast={level:<4} {arm:<7} {field:<9} "
                             f"pairs={d['pairs']} {verdict}{suffix}")
    return lines


def failure_note_lines(records: list) -> list:
    """The excluded-runs note.

    The wording is load-bearing. The previous version justified the
    exclusion with "a failed run has no cost/turn metrics" — true only of a
    run that crashed. A run that completed without finding the expected tool
    has every metric, and the old summary averaged it straight in while this
    note told the reader it had not.
    """
    failures = [r for r in records if not _is_successful(r)]
    if not failures:
        return []
    lines = [
        f"\n{len(failures)} run(s) excluded from the comparison above — discovery "
        f"failures are a real cost of lazy loading and are reported, not "
        f"discarded. Two kinds are excluded: a run that crashed before scoring "
        f"(no metrics to average), and a run that completed without reaching the "
        f"expected tool (full metrics, but a model that gives up early posts a "
        f"low turn count and a small bill, which would make a failing arm look "
        f"cheapest). Each verdict above carries the success rate this implies:",
    ]
    for f in failures:
        reason = "crashed" if not _is_scored(f) else "never reached the expected tool"
        lines.append(f"  {f['arm']}/{f['task']} — {reason}")
    return lines


def binary_provenance(path: str) -> dict:
    """Identify the leanproxy binary a live run was measured against.

    Layer 3 joins Layer 1 (residency, built from source) to Layer 2 (live,
    whatever `--leanproxy-bin` points at) on `ballast_tools` alone, then
    multiplies one layer's turn count by the other's residency figure.
    Without this, a working-tree residency figure could be silently
    multiplied by turn counts from a different proxy build — the same
    cross-layer divergence class as reviews C-1/C-2. The hash goes into
    every live record so the join is auditable after the fact.
    """
    p = os.path.abspath(os.path.expanduser(path))
    if not (os.path.isfile(p) and os.access(p, os.X_OK)):
        raise ValueError(f"{p!r} is not an executable file")
    h = hashlib.sha256()
    with open(p, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return {"leanproxy_bin": p, "leanproxy_sha256": h.hexdigest()}


def _persist_records(path: str, records: list) -> None:
    """Write `records` to `path` as a JSON array, atomically.

    Called after every task in the sweep, not just once at the end. Each
    call fully replaces `path` with the array-so-far, so a crash on run 14
    of 15 (a raise from `read_task_result` on a malformed row is plausible,
    not exotic, across that many live model runs) leaves every
    already-paid-for measurement on disk as valid, parseable JSON — the
    same shape Task 9's `combine()` reads with a plain `json.load`, whether
    the sweep finished or not. Writes go through a temp file and
    `os.replace` (same pattern as `ConfigSwap`) so a crash mid-write can't
    leave `path` holding a truncated, unparseable fragment.
    """
    tmp = path + ".tmp"
    with open(tmp, "w") as fh:
        json.dump(records, fh, indent=2)
    os.replace(tmp, path)


BALLAST_SERVERS_PER_POINT = 2  # fixed server count for any tools>0 ballast point


def _ballast_shape(tools: int) -> tuple:
    """(servers, per, actual) for one ballast sweep point.

    `tools == 0` is the true baseline: zero ballast servers, no `--mock-bin`
    required. Any `tools > 0` is split across a fixed server count; if it
    doesn't divide evenly this raises rather than silently flooring `actual`
    to a value that could collide with a different point's tag — e.g.
    `tools=1` flooring to `actual=0` would tag identically to the true
    zero-ballast point while actually attaching two live (if toolless) mock
    servers, and `paired_deltas` would silently let one point's records
    overwrite the other's under the shared key. Nominal and actual counts
    must never diverge, not merely "usually agree" (Layer 1 already learned
    this the hard way for residency).

    A negative `tools` is rejected here too, not only by `main`'s upfront
    check: Python's floor division can make a negative point look internally
    consistent (`tools=-2` gives `servers=2, per=-1, actual=-2 == tools`,
    so the divisibility check alone would wave it through), and this
    function's own contract — "a live tool count" — has no legitimate
    negative value regardless of who calls it.
    """
    if tools < 0:
        raise ValueError(f"ballast point {tools} is negative — not a valid tool count")
    if tools == 0:
        return 0, 0, 0
    servers = BALLAST_SERVERS_PER_POINT
    per = tools // servers
    actual = servers * per
    if actual != tools:
        raise ValueError(
            f"ballast point {tools} does not divide evenly across "
            f"{servers} servers (would floor to actual={actual}, per-server="
            f"{per}) — choose a multiple of {servers}, e.g. {actual} or "
            f"{actual + servers}"
        )
    return servers, per, actual


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--out", default="bench-results")
    ap.add_argument("--bob-config", default=DEFAULT_BOB_CFG)
    ap.add_argument("--lp-config", default=DEFAULT_LP_CFG)
    ap.add_argument("--db", default=DEFAULT_DB)
    ap.add_argument("--cwd", default=os.getcwd())
    ap.add_argument("--leanproxy-bin", default=os.path.join(HOME, ".local", "bin", "leanproxy-mcp"))
    ap.add_argument("--tasks", default=DEFAULT_TASKS)
    ap.add_argument("--ballast-fixture", default=DEFAULT_BALLAST_FIXTURE,
                    help="shared ballast definition; must be the file Layer 1 embeds")
    ap.add_argument("--allow-ballast-fixture-override", action="store_true",
                    help="allow --ballast-fixture to point somewhere other than the "
                         "file Layer 1's ballast.go embeds. Without this, a different "
                         "fixture reopens review C-1 (two layers weighing ballast "
                         "differently under the same ballast_tools x-axis label) with "
                         "no guard to catch it — TestBallastWeightIsIdenticalAcrossLayers "
                         "only ever compares Layer 2 against DEFAULT_BALLAST_FIXTURE")
    ap.add_argument("--ballast-points", default="0,100",
                     help="comma-separated total ballast tool counts to run live")
    ap.add_argument("--mock-bin", default="", help="path to the built mockmcp binary")
    ap.add_argument("--allow-dirty-repo", action="store_true",
                    help="sweep even though --cwd has uncommitted changes (the sweep "
                         "runs an unsupervised write-capable agent there)")
    ap.add_argument("--allow-tool-filter-asymmetry", action="store_true",
                    help="sweep even though a proxied server's tools are filtered, so "
                         "the native arm sees a larger inventory than the proxy arms")
    ap.add_argument("--skip-preflight", action="store_true",
                    help="skip the pre-spend reachability checks — this spends real "
                         "money on a configuration nothing has verified")
    args = ap.parse_args(argv)

    if os.environ.get("LEANPROXY_AB_LIVE") != "1":
        print("abbench spends coins; set LEANPROXY_AB_LIVE=1 to run it.")
        return 0

    with open(args.bob_config) as fh:
        bob_cfg = json.load(fh)
    with open(args.lp_config) as fh:
        lp_text = fh.read()

    problems = detect_confound(bob_cfg, lp_text)
    if problems:
        print("Refusing to run — config confounds detected:", file=sys.stderr)
        for p in problems:
            print(f"  - {p}", file=sys.stderr)
        return 2

    if (os.path.abspath(args.ballast_fixture) != os.path.abspath(DEFAULT_BALLAST_FIXTURE)
            and not args.allow_ballast_fixture_override):
        print(
            f"Refusing to run — --ballast-fixture={args.ballast_fixture!r} differs from "
            f"the default ({DEFAULT_BALLAST_FIXTURE!r}), the file "
            f"tests/bench/e2e/ballast.go embeds. Running Layer 2 against a different "
            f"ballast definition than Layer 1 is exactly review C-1 (the two layers' "
            f"ballast tools weighing different amounts under the same ballast_tools "
            f"label), and nothing downstream would notice. Pass "
            f"--allow-ballast-fixture-override to do this deliberately.",
            file=sys.stderr)
        return 2

    ballast_fixture = load_ballast_fixture(args.ballast_fixture)
    ballast_description = ballast_fixture["description"]

    try:
        direct_servers = direct_entries_for_proxied_servers(lp_text)
    except UnmirrorableServer as exc:
        print(f"Refusing to run — {exc}", file=sys.stderr)
        return 2
    filtered = tool_filtered_servers(lp_text)
    lp_names = sorted(direct_servers)

    tasks = load_tasks(args.tasks)
    leanproxy_bin = args.leanproxy_bin

    # Before anything is spent: a binary that isn't there can't be measured,
    # and a binary that IS there gets hashed into every record so Layer 3's
    # join can be audited for cross-layer build divergence later.
    try:
        provenance = binary_provenance(leanproxy_bin)
    except ValueError as exc:
        print(f"Refusing to run — --leanproxy-bin {exc}", file=sys.stderr)
        return 2

    records = []

    points = [int(x) for x in args.ballast_points.split(",") if x.strip()]

    # Reject negatives immediately, before any other validation: a list of
    # live tool counts has no legitimate negative value. Without this,
    # `_ballast_shape` would accept a negative point whose floor division
    # happens to come out even (e.g. tools=-2: servers=2, per=-1,
    # actual=-2 == tools, so the divisibility check never fires), and the
    # mock-bin gate below (`t != 0`, not `t > 0`) would otherwise be the only
    # thing standing between a negative point and a live run with a
    # synthetic ballast server launched via `command: ""`.
    negatives = [t for t in points if t < 0]
    if negatives:
        print(f"--ballast-points must not contain negative values: {negatives}", file=sys.stderr)
        return 2

    # Validate the WHOLE sweep before running any of it: a gate that only
    # fires when a later point is reached still lets every earlier point run
    # for real first, spending money on live model calls before the sweep is
    # known to be well-formed. `t != 0`, not `t > 0`, so this can't silently
    # disagree with `_ballast_shape` about what counts as a non-zero point.
    if any(t != 0 for t in points) and not args.mock_bin:
        print("--mock-bin is required for non-zero ballast points", file=sys.stderr)
        return 2
    if args.mock_bin and not (os.path.isfile(args.mock_bin) and os.access(args.mock_bin, os.X_OK)):
        print(f"--mock-bin {args.mock_bin!r} is not an executable file", file=sys.stderr)
        return 2
    shapes = [(tools, _ballast_shape(tools)) for tools in points]  # raises before any run

    # The sweep drives an unsupervised, write-capable agent in --cwd. On a
    # dirty tree there is no way to tell afterwards what it changed.
    dirty_before = git_status(args.cwd)
    if dirty_before and not args.allow_dirty_repo:
        print(
            f"Refusing to run — {args.cwd} has uncommitted changes and this sweep runs "
            f"{len(ARMS) * len(tasks) * len(points)} unsupervised agent sessions there "
            f"with write-capable tools. Commit or stash first, or pass "
            f"--allow-dirty-repo:\n{dirty_before}", file=sys.stderr)
        return 2

    os.makedirs(args.out, exist_ok=True)
    stamp = time.strftime("%Y%m%d-%H%M%S")
    out_path = os.path.join(args.out, f"e2e-live-{stamp}.json")

    # The generated leanproxy configs are verbatim copies of the operator's,
    # which may carry credentials, so they live in a 0700 temp directory that
    # is removed on the way out rather than beside the results.
    workdir = tempfile.mkdtemp(prefix="abbench-")
    try:
        note = dict(provenance)
        if filtered:
            note["tool_filter_asymmetry"] = filtered

        for tools, (servers, per, actual) in shapes:
            ballast_bob = (
                ballast_servers(args.mock_bin, servers, per, ballast_description)
                if servers else {}
            )
            ballast_lp = (
                ballast_lp_entries(args.mock_bin, servers, per, ballast_description)
                if servers else []
            )
            lp_arm_cfg = os.path.join(workdir, f"leanproxy-servers-{actual}.yaml")
            with open(os.open(lp_arm_cfg, os.O_CREAT | os.O_WRONLY | os.O_TRUNC, 0o600), "w") as fh:
                fh.write(build_arm_lp_config(lp_text, ballast_lp))

            if not args.skip_preflight:
                found = preflight(
                    tasks, direct_servers, ballast_bob, leanproxy_bin,
                    lp_arm_cfg, lp_names, args.allow_tool_filter_asymmetry, filtered,
                )
                if found:
                    print(f"Refusing to run — preflight failed at ballast_tools={actual} "
                          f"(nothing has been spent):", file=sys.stderr)
                    for p in found:
                        print(f"  - {p}", file=sys.stderr)
                    return 2

            for arm in ARMS:
                # arm_config raises ValueError on a name collision (e.g. a
                # *disabled* Bob entry whose name matches a proxied server —
                # detect_confound deliberately skips disabled entries, so
                # such a config reaches here). native runs first and this is
                # a static property of the config, so it fails here before
                # any task in the sweep has spent anything; catching it turns
                # what was a raw traceback into the same kind of clean,
                # exit-2 refusal as every other check on this path (N-3).
                try:
                    cfg = arm_config(
                        arm, leanproxy_bin, bob_cfg,
                        ballast=ballast_bob if arm == "native" else None,
                        direct_servers=direct_servers if arm == "native" else None,
                        lp_config_path=lp_arm_cfg,
                        log_file=os.path.join(workdir, f"leanproxy-{arm}-{actual}.log"),
                    )
                except ValueError as exc:
                    print(f"Refusing to run — {arm} arm: {exc}", file=sys.stderr)
                    return 2
                with ConfigSwap(args.bob_config, cfg):
                    for t in tasks:
                        task_key = f"{t['id']}@{actual}"
                        print(f"[{arm}] {task_key} ...", flush=True)
                        try:
                            task_id = run_task(t["prompt"], args.cwd, args.db)
                        except Exception as exc:  # noqa: BLE001 - a failed run is data
                            print(f"  run failed: {exc}", file=sys.stderr)
                            records.append({"layer": "live", "origin": "measured", "arm": arm,
                                            "task": task_key, "ballast_tools": actual,
                                            "succeeded": False, "error": str(exc), **note})
                            _persist_records(out_path, records)
                            continue

                        try:
                            res = read_task_result(args.db, task_id, t["expect_tool"])
                        except Exception as exc:  # noqa: BLE001 - fail-closed means never
                            # recording a WRONG measurement, not discarding a correct
                            # one already paid for; a malformed row degrades to a
                            # recorded failure for this one task, same as run_task's
                            # own except above, rather than crashing the whole sweep.
                            print(f"  read_task_result failed: {exc}", file=sys.stderr)
                            records.append({"layer": "live", "origin": "measured", "arm": arm,
                                            "task": task_key, "ballast_tools": actual,
                                            "task_id": task_id,
                                            "succeeded": False, "error": str(exc), **note})
                            _persist_records(out_path, records)
                            continue

                        res.update({"layer": "live", "origin": "measured", "arm": arm,
                                    "task": task_key, "ballast_tools": actual, **note})
                        records.append(res)
                        _persist_records(out_path, records)
                        cost = res.get("cost_usd")
                        cost_str = f"{cost:.4f}" if cost is not None else "n/a"
                        print(f"  cost={cost_str} turns={res['turns']} "
                              f"ok={res['succeeded']}")
    finally:
        shutil.rmtree(workdir, ignore_errors=True)

    print(f"\nwrote {out_path}")
    print(f"measured against {provenance['leanproxy_bin']} "
          f"(sha256 {provenance['leanproxy_sha256'][:12]})\n")
    for line in paired_summary_lines(records):
        print(line)
    for line in failure_note_lines(records):
        print(line)

    dirty_after = git_status(args.cwd)
    if dirty_after and dirty_after != (dirty_before or ""):
        print(f"\nWARNING: {args.cwd} changed during the sweep — the agent sessions "
              f"were unsupervised and write-capable. Review before trusting the tree:\n"
              f"{dirty_after}", file=sys.stderr)

    return 0


if __name__ == "__main__":
    sys.exit(main())
