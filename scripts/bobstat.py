#!/usr/bin/env python3
"""bobstat — visualize Bob (IBM agent) usage, cost and tool behaviour.

Reads Bob's local SQLite store plus leanproxy's config/usage/log files and
reports where the coins actually go. Python 3.9+, standard library only.

    scripts/bobstat.py                          # terminal report, all history
    scripts/bobstat.py --since 7d               # last week
    scripts/bobstat.py --project leanproxy      # one repo
    scripts/bobstat.py --html /tmp/bob.html --open
    scripts/bobstat.py --json                   # machine-readable

Under Bob's flat per-token pricing there is no cache discount, so the dominant
cost is *residency*: every token in context is re-billed on every subsequent
turn. The report is built around that fact.
"""

from __future__ import annotations

import argparse
import collections
import html
import json
import math
import os
import re
import sqlite3
import statistics
import sys
import time
import webbrowser
from datetime import datetime

# ---------------------------------------------------------------------------
# defaults
# ---------------------------------------------------------------------------

HOME = os.path.expanduser("~")
DEFAULT_DB = os.path.join(HOME, ".bob", "db", "bob.db")
DEFAULT_CONFIG = os.path.join(HOME, ".config", "leanproxy_servers.yaml")
DEFAULT_USAGE = os.path.join(HOME, ".config", "leanproxy", "toolusage.json")
DEFAULT_LOG = "/tmp/leanproxy-bob.log"

DEFAULT_RATE = 2.0          # $ per 1M tokens, flat across all token classes
DEFAULT_CHARS_PER_TOKEN = 4.0
DEFAULT_STUB_TOKENS = 120   # measured: ~129 tok per tool stub in tools/list
CACHE_FAST_MS = 5           # a repeat served this fast never touched the backend

BLOCKS = " ▏▎▍▌▋▊▉█"


# ---------------------------------------------------------------------------
# tiny helpers
# ---------------------------------------------------------------------------


def pct(vals, p):
    """Nearest-rank percentile. vals need not be sorted."""
    if not vals:
        return 0
    s = sorted(vals)
    k = max(0, min(len(s) - 1, int(math.ceil(p / 100.0 * len(s))) - 1))
    return s[k]


def human_int(n):
    n = float(n)
    for unit, div in (("B", 1e9), ("M", 1e6), ("K", 1e3)):
        if abs(n) >= div:
            return f"{n / div:.1f}{unit}"
    return f"{n:.0f}"


def money(v):
    if v == 0:
        return "$0"
    if abs(v) < 0.01:
        return f"${v:.4f}"
    return f"${v:,.2f}"


def parse_duration(text):
    """'168h' / '60s' / '7d' / '30m' -> seconds. None if unparseable."""
    if text is None:
        return None
    m = re.fullmatch(r"\s*(\d+(?:\.\d+)?)\s*([smhdw])\s*", str(text))
    if not m:
        return None
    mult = {"s": 1, "m": 60, "h": 3600, "d": 86400, "w": 604800}[m.group(2)]
    return float(m.group(1)) * mult


def parse_since(text):
    """--since value -> epoch ms cutoff, or None."""
    if not text:
        return None
    secs = parse_duration(text)
    if secs is not None:
        return (time.time() - secs) * 1000.0
    for fmt in ("%Y-%m-%d", "%Y/%m/%d", "%Y-%m-%dT%H:%M"):
        try:
            dt = datetime.strptime(text, fmt)
            return dt.timestamp() * 1000.0
        except ValueError:
            continue
    raise SystemExit(f"bobstat: cannot parse --since {text!r} (try 7d, 24h, or 2026-08-20)")


def to_ms(ts):
    """Normalize an epoch that may be in seconds or milliseconds to ms."""
    if ts is None:
        return None
    ts = float(ts)
    return ts * 1000.0 if ts < 1e12 else ts


def day_of(ms):
    return datetime.fromtimestamp(ms / 1000.0).strftime("%Y-%m-%d")


# ---------------------------------------------------------------------------
# minimal YAML reader (leanproxy config only — nested maps, dash lists, inline
# lists, scalars, comments). Falls back to {} rather than pulling in PyYAML.
# ---------------------------------------------------------------------------


def _scalar(text):
    text = text.strip()
    if text.startswith("[") and text.endswith("]"):
        inner = text[1:-1].strip()
        return [_scalar(x) for x in inner.split(",")] if inner else []
    if len(text) >= 2 and text[0] == text[-1] and text[0] in "\"'":
        return text[1:-1]
    low = text.lower()
    if low in ("true", "yes"):
        return True
    if low in ("false", "no"):
        return False
    if low in ("null", "~", ""):
        return None
    try:
        return int(text)
    except ValueError:
        pass
    try:
        return float(text)
    except ValueError:
        return text


def _strip_comment(line):
    out, quote = [], None
    for ch in line:
        if quote:
            out.append(ch)
            if ch == quote:
                quote = None
        elif ch in "\"'":
            quote = ch
            out.append(ch)
        elif ch == "#":
            break
        else:
            out.append(ch)
    return "".join(out).rstrip()


def parse_mini_yaml(path):
    try:
        with open(path, "r", encoding="utf-8") as fh:
            raw = fh.read()
    except OSError:
        return {}
    lines = []
    for ln in raw.splitlines():
        body = _strip_comment(ln.replace("\t", "    "))
        if body.strip():
            lines.append((len(body) - len(body.lstrip()), body.strip()))

    def block(i, indent):
        """Parse from line i at the given indent. Returns (value, next_i)."""
        if i < len(lines) and lines[i][1].startswith("- "):
            items = []
            while i < len(lines) and lines[i][0] == indent and lines[i][1].startswith("- "):
                rest = lines[i][1][2:].strip()
                if ":" in rest and not rest.startswith("["):
                    # inline first key of a mapping item; re-parse as a map at
                    # a virtual indent just past the dash.
                    virt = indent + 2
                    lines[i] = (virt, rest)
                    val, i = block(i, virt)
                else:
                    val, i = _scalar(rest), i + 1
                items.append(val)
            return items, i
        out = {}
        while i < len(lines):
            ind, body = lines[i]
            if ind < indent or body.startswith("- "):
                break
            if ind > indent:  # stray deeper line; skip defensively
                i += 1
                continue
            if ":" not in body:
                i += 1
                continue
            key, _, rest = body.partition(":")
            key, rest = key.strip(), rest.strip()
            if rest:
                out[key] = _scalar(rest)
                i += 1
            else:
                i += 1
                if i < len(lines) and lines[i][0] > indent:
                    out[key], i = block(i, lines[i][0])
                else:
                    out[key] = None
        return out, i

    try:
        value, _ = block(0, lines[0][0] if lines else 0)
        return value if isinstance(value, dict) else {}
    except Exception:
        return {}


def load_config(path):
    """-> {servers: {name: {enabled, include, caps, cache_ttl, stub_after}}}"""
    doc = parse_mini_yaml(path)
    servers = {}
    for entry in doc.get("servers") or []:
        if not isinstance(entry, dict):
            continue
        name = entry.get("name")
        if not name:
            continue
        tools = entry.get("tools") or {}
        caps = tools.get("max_response_chars") or {}
        ttls = tools.get("cache_ttl") or {}
        include = tools.get("include") or []
        servers[name] = {
            "enabled": bool(entry.get("enabled", True)),
            "transport": entry.get("transport"),
            "include": [str(t) for t in include] if isinstance(include, list) else [],
            "caps": {k: int(v) for k, v in caps.items() if isinstance(v, (int, float))},
            "cache_ttl": {k: parse_duration(v) for k, v in ttls.items()},
            "stub_after": parse_duration(tools.get("adaptive_stub_after")),
        }
    return {"servers": servers, "path": path, "found": bool(servers)}


# ---------------------------------------------------------------------------
# tool-name classification
# ---------------------------------------------------------------------------

INSTANCE_SUFFIX = re.compile(r"_[0-9a-f]{4}$")


def display_name(kind, server, tool, raw):
    """Short readable name: '⇢ server/tool' proxied, '· server/tool' direct."""
    if kind == "proxied":
        return f"⇢ {server}/{tool}"
    if kind == "direct":
        return f"· {server}/{tool}"
    return raw


def classify_tool(raw, known_servers):
    """-> (kind, server, tool) where kind is proxied | direct | native."""
    if raw.startswith("mcp__leanproxy__"):
        rest = raw[len("mcp__leanproxy__"):]
        for srv in sorted(known_servers, key=len, reverse=True):
            if rest.startswith(srv + "_"):
                return "proxied", srv, rest[len(srv) + 1:]
        server, _, tool = rest.partition("_")
        return "proxied", server, tool or rest
    if raw.startswith("mcp__"):
        rest = raw[len("mcp__"):]
        server, sep, tool = rest.partition("__")
        if sep:
            return "direct", INSTANCE_SUFFIX.sub("", server), tool
    return "native", "", raw


# ---------------------------------------------------------------------------
# data loading
# ---------------------------------------------------------------------------


class Turn:
    __slots__ = ("ts", "index", "spend", "task_id")

    def __init__(self, ts, index, spend, task_id):
        self.ts, self.index, self.spend, self.task_id = ts, index, spend, task_id


class ToolCall:
    __slots__ = ("ts", "task_id", "raw", "kind", "server", "tool", "args_key",
                 "chars", "is_error", "duration_ms", "turns_after")

    def __init__(self, **kw):
        for k in self.__slots__:
            setattr(self, k, kw.get(k))


def load(args, config):
    known_servers = set(config["servers"]) | {"codebase-memory", "context7"}
    cutoff = parse_since(args.since)

    con = sqlite3.connect(f"file:{args.db}?mode=ro", uri=True)
    con.row_factory = sqlite3.Row

    tasks = {}
    for row in con.execute(
        "SELECT id, project_id, title, first_message, status, task_type, costs,"
        "       created_at, updated_at FROM tasks"
    ):
        if cutoff and (row["updated_at"] or 0) < cutoff:
            continue
        if args.project and args.project not in (row["project_id"] or ""):
            continue
        if args.task and not row["id"].startswith(args.task):
            continue
        try:
            costs = json.loads(row["costs"]) if row["costs"] else {}
        except (ValueError, TypeError):
            costs = {}
        tasks[row["id"]] = {
            "id": row["id"],
            "project": (row["project_id"] or "").replace("file:", ""),
            "title": (row["title"] or row["first_message"] or "").strip().replace("\n", " "),
            "status": row["status"],
            "type": row["task_type"],
            "costs": costs,
            "created_at": row["created_at"],
            "updated_at": row["updated_at"],
            "turns": [],
            "tool_calls": [],
        }
    if not tasks:
        return {"tasks": {}, "turns": [], "tool_calls": [], "skipped": 0}

    turns, tool_calls, skipped = [], [], 0
    for row in con.execute(
        "SELECT task_id, role, data, created_at FROM messages "
        "WHERE role IN ('assistant','tool') ORDER BY task_id, created_at, rowid"
    ):
        task = tasks.get(row["task_id"])
        if task is None:
            continue
        try:
            data = json.loads(row["data"])
        except ValueError:
            skipped += 1
            continue
        meta = data.get("_meta") or {}
        ts = to_ms(meta.get("timestamp")) or row["created_at"]

        if row["role"] == "assistant":
            spend = meta.get("spend")
            if not spend:
                continue
            turn = Turn(ts, len(task["turns"]), spend, row["task_id"])
            task["turns"].append(turn)
            turns.append(turn)
            continue

        usage = data.get("toolUsage") or {}
        sig = usage.get("signature") or {}
        raw = sig.get("name")
        if not raw:
            continue
        arguments = sig.get("arguments")
        if isinstance(arguments, str):
            try:
                arguments = json.loads(arguments)
            except ValueError:
                pass
        try:
            args_key = json.dumps(arguments, sort_keys=True, default=str)
        except (TypeError, ValueError):
            args_key = repr(arguments)
        kind, server, tool = classify_tool(raw, known_servers)
        call = ToolCall(
            ts=ts, task_id=row["task_id"], raw=raw, kind=kind, server=server, tool=tool,
            args_key=args_key, chars=len(data.get("content") or ""),
            is_error=bool(sig.get("isError")), duration_ms=meta.get("durationMs"),
            turns_after=0,
        )
        task["tool_calls"].append(call)
        tool_calls.append(call)
    con.close()

    # residency: how many billed turns each tool result survives into.
    for task in tasks.values():
        turn_times = [t.ts for t in task["turns"]]
        for call in task["tool_calls"]:
            call.turns_after = sum(1 for t in turn_times if t > call.ts)

    return {"tasks": tasks, "turns": turns, "tool_calls": tool_calls, "skipped": skipped}


def load_toolusage(path):
    try:
        with open(path, "r", encoding="utf-8") as fh:
            return json.load(fh)
    except (OSError, ValueError):
        return {}


LOGFMT = re.compile(r'(\w+)=("(?:[^"\\]|\\.)*"|\S+)')


def load_log(path):
    try:
        with open(path, "r", encoding="utf-8", errors="replace") as fh:
            lines = fh.readlines()
    except OSError:
        return None
    counts = collections.Counter()
    levels = collections.Counter()
    stub_counts = []
    first = last = None
    for line in lines:
        fields = dict(LOGFMT.findall(line))
        msg = (fields.get("msg") or "").strip('"')
        if not msg:
            continue
        counts[msg] += 1
        levels[(fields.get("level") or "").strip('"')] += 1
        if msg == "lazy loading: sent tool stubs to client":
            try:
                stub_counts.append(int(fields.get("count", "0").strip('"')))
            except ValueError:
                pass
        stamp = (fields.get("time") or "").strip('"')
        if stamp:
            first = first or stamp
            last = stamp
    return {
        "path": path,
        "sessions": counts.get("leanproxy-mcp stdio mode started", 0),
        "crashes": counts.get("server process crashed", 0),
        "restarts": counts.get("scheduled restart", 0),
        "max_restarts": counts.get("max restarts exceeded, leaving server in error state until next use", 0),
        "init_failed": counts.get("initialize request failed", 0),
        "stubs_last": stub_counts[-1] if stub_counts else None,
        "errors": levels.get("ERROR", 0),
        "warns": levels.get("WARN", 0),
        "first": first,
        "last": last,
        "top": counts.most_common(8),
    }


# ---------------------------------------------------------------------------
# analysis
# ---------------------------------------------------------------------------


def analyze(data, config, usage, args):
    tasks, turns, calls = data["tasks"], data["turns"], data["tool_calls"]
    rate = args.rate / 1e6
    cpt = args.chars_per_token

    billed = [t for t in tasks.values() if t["costs"].get("cost")]
    task_costs = [t["costs"]["cost"] for t in billed]
    total_task_cost = sum(task_costs)
    total_turn_cost = sum(t.spend.get("cost", 0) for t in turns)

    tok = collections.Counter()
    for t in billed:
        c = t["costs"]
        tok["input"] += c.get("input", 0)
        tok["output"] += c.get("output", 0)
        tok["cacheRead"] += c.get("cacheRead", 0)
        tok["cacheWrite"] += c.get("cacheWrite", 0)
    tok["fresh"] = max(0, tok["input"] - tok["cacheRead"] - tok["cacheWrite"])
    billed_tokens = tok["input"] + tok["output"]

    # --- billing model: is cost == (input + output) * flat rate? ------------
    implied, residuals = [], []
    for t in billed:
        c = t["costs"]
        denom = c.get("input", 0) + c.get("output", 0)
        if denom > 0:
            implied.append(c["cost"] / denom * 1e6)
            residuals.append(abs(c["cost"] - denom * rate))
    billing = {
        "implied_rate": statistics.median(implied) if implied else None,
        "max_residual": max(residuals) if residuals else 0.0,
        "samples": len(implied),
        "cache_discount_value": tok["cacheRead"] * rate * 0.9,
    }

    # --- daily timeline -----------------------------------------------------
    daily = collections.defaultdict(lambda: {"cost": 0.0, "turns": 0, "tokens": 0})
    for t in turns:
        d = daily[day_of(t.ts)]
        d["cost"] += t.spend.get("cost", 0)
        d["turns"] += 1
        d["tokens"] += t.spend.get("input", 0) + t.spend.get("output", 0)
    daily = dict(sorted(daily.items()))

    # --- context creep ------------------------------------------------------
    by_index = collections.defaultdict(list)
    cost_by_index = collections.defaultdict(list)
    for t in turns:
        ctx = t.spend.get("contextTokens") or 0
        if ctx:
            by_index[t.index].append(ctx)
        cost_by_index[t.index].append(t.spend.get("cost", 0))
    creep = [
        {
            "index": i,
            "median_context": statistics.median(by_index[i]) if by_index.get(i) else 0,
            "median_cost": statistics.median(cost_by_index[i]) if cost_by_index.get(i) else 0,
            "n": len(cost_by_index.get(i, [])),
        }
        for i in sorted(cost_by_index)
    ]
    growth = []
    for t in tasks.values():
        ctxs = [x.spend.get("contextTokens") or 0 for x in t["turns"]]
        ctxs = [c for c in ctxs if c]
        if len(ctxs) >= 3:
            growth.append((ctxs[-1] - ctxs[0]) / (len(ctxs) - 1))
    creep_summary = {
        "median_growth_per_turn": statistics.median(growth) if growth else 0,
        "cost_per_turn_at_start": statistics.median(
            [c for i, cs in cost_by_index.items() if i < 3 for c in cs]
        ) if cost_by_index else 0,
        "cost_per_turn_late": statistics.median(
            [c for i, cs in cost_by_index.items() if i >= 20 for c in cs]
        ) if any(i >= 20 for i in cost_by_index) else 0,
        "tasks_sampled": len(growth),
    }

    # --- per-tool stats -----------------------------------------------------
    stats = {}
    for call in calls:
        s = stats.setdefault(call.raw, {
            "raw": call.raw, "kind": call.kind, "server": call.server, "tool": call.tool,
            "calls": 0, "errors": 0, "chars": [], "durations": [],
            "total_chars": 0, "residency_tokens": 0,
        })
        s["calls"] += 1
        s["errors"] += int(call.is_error)
        s["chars"].append(call.chars)
        s["total_chars"] += call.chars
        if isinstance(call.duration_ms, (int, float)):
            s["durations"].append(call.duration_ms)
        s["residency_tokens"] += (call.chars / cpt) * (call.turns_after + 1)
    for s in stats.values():
        s["name"] = display_name(s["kind"], s["server"], s["tool"], s["raw"])
        s["p50"] = pct(s["chars"], 50)
        s["p90"] = pct(s["chars"], 90)
        s["max"] = max(s["chars"]) if s["chars"] else 0
        s["tokens"] = s["total_chars"] / cpt
        s["residency_cost"] = s["residency_tokens"] * rate
        s["mean_ms"] = statistics.mean(s["durations"]) if s["durations"] else None
        s["chars"] = None  # drop the raw list; percentiles are computed
    tools = sorted(stats.values(), key=lambda s: s["residency_cost"], reverse=True)

    by_kind = collections.Counter()
    cost_by_kind = collections.Counter()
    for s in stats.values():
        by_kind[s["kind"]] += s["calls"]
        cost_by_kind[s["kind"]] += s["residency_cost"]

    # --- waste ---------------------------------------------------------------
    waste = {}
    servers = config["servers"]
    called = {(s["server"], s["tool"]) for s in stats.values() if s["kind"] == "proxied"}
    unused = []
    for name, srv in servers.items():
        if not srv["enabled"]:
            continue
        for tool in srv["include"]:
            if (name, tool) not in called:
                info = usage.get(f"{name}/{tool}") or {}
                last = to_ms(info.get("last_used"))
                unused.append({
                    "server": name, "tool": tool,
                    "last_used": day_of(last) if last else "never",
                    "stubbed": srv["stub_after"] is not None and (
                        last is None or (time.time() * 1000 - last) / 1000.0 > srv["stub_after"]
                    ),
                })
    n_turns = len(turns)
    waste["unused"] = unused
    waste["unused_cost"] = len([u for u in unused if not u["stubbed"]]) * args.stub_tokens * n_turns * rate

    caps = {}
    for name, srv in servers.items():
        for tool, cap in srv["caps"].items():
            caps[(name, tool)] = cap
    truncated = collections.Counter()
    cap_total = collections.Counter()
    for call in calls:
        cap = caps.get((call.server, call.tool))
        if cap:
            cap_total[(call.server, call.tool)] += 1
            if call.chars >= cap * 0.98:
                truncated[(call.server, call.tool)] += 1
    waste["truncation"] = [
        {"server": k[0], "tool": k[1], "cap": caps[k], "hits": v, "calls": cap_total[k]}
        for k, v in sorted(truncated.items(), key=lambda kv: -kv[1])
    ]

    ttls = {}
    for name, srv in servers.items():
        for tool, ttl in srv["cache_ttl"].items():
            if ttl:
                ttls[(name, tool)] = ttl
    repeats = collections.defaultdict(lambda: {
        "repeats": 0, "served_fast": 0, "in_ttl": 0, "wasted_tokens": 0.0, "ttl": None,
        "name": None})
    for task in tasks.values():
        seen = {}
        for call in sorted(task["tool_calls"], key=lambda c: c.ts):
            key = (call.raw, call.args_key)
            prev = seen.get(key)
            seen[key] = call.ts
            if prev is None:
                continue
            r = repeats[call.raw]
            r["name"] = display_name(call.kind, call.server, call.tool, call.raw)
            r["repeats"] += 1
            r["wasted_tokens"] += call.chars / cpt
            ttl = ttls.get((call.server, call.tool))
            r["ttl"] = ttl
            if isinstance(call.duration_ms, (int, float)) and call.duration_ms <= CACHE_FAST_MS:
                r["served_fast"] += 1
            if ttl and (call.ts - prev) / 1000.0 <= ttl:
                r["in_ttl"] += 1
    waste["repeats"] = sorted(
        ({"raw": k, **v} for k, v in repeats.items()),
        key=lambda r: -r["repeats"],
    )
    waste["repeat_cost"] = sum(r["wasted_tokens"] for r in waste["repeats"]) * rate

    loops = []
    for task in tasks.values():
        run_name, run_len, run_head = None, 0, None
        ordered = sorted(task["tool_calls"], key=lambda c: c.ts)
        for call in ordered + [None]:
            name = call.raw if call else None
            if name == run_name:
                run_len += 1
                continue
            if run_len >= 4 and run_head is not None:
                loops.append({
                    "task": task["id"][:8], "title": task["title"][:60],
                    "tool": display_name(run_head.kind, run_head.server, run_head.tool, run_name),
                    "n": run_len, "at": day_of(run_head.ts),
                })
            run_name, run_len, run_head = name, 1, call
    waste["loops"] = sorted(loops, key=lambda l: -l["n"])

    errors = sorted(
        ({"raw": s["raw"], "name": s["name"], "errors": s["errors"], "calls": s["calls"]}
         for s in stats.values() if s["errors"]),
        key=lambda e: -e["errors"],
    )
    waste["errors"] = errors

    top_tasks = sorted(billed, key=lambda t: -t["costs"]["cost"])[:args.top]

    span = None
    if turns:
        span = (min(t.ts for t in turns), max(t.ts for t in turns))

    return {
        "range": span,
        "tasks_total": len(tasks),
        "tasks_billed": len(billed),
        "turns": n_turns,
        "total_task_cost": total_task_cost,
        "total_turn_cost": total_turn_cost,
        "median_task_cost": statistics.median(task_costs) if task_costs else 0,
        "p90_task_cost": pct(task_costs, 90),
        "max_task_cost": max(task_costs) if task_costs else 0,
        "tokens": dict(tok),
        "billed_tokens": billed_tokens,
        "billing": billing,
        "daily": daily,
        "creep": creep,
        "creep_summary": creep_summary,
        "tools": tools,
        "by_kind": dict(by_kind),
        "cost_by_kind": dict(cost_by_kind),
        "waste": waste,
        "top_tasks": top_tasks,
        "projects": collections.Counter(t["project"] for t in tasks.values()),
        "skipped": data["skipped"],
        "rate": args.rate,
    }


# ---------------------------------------------------------------------------
# terminal rendering
# ---------------------------------------------------------------------------


class Ink:
    def __init__(self, enabled):
        self.on = enabled

    def _w(self, code, text):
        return f"\033[{code}m{text}\033[0m" if self.on else str(text)

    def bold(self, t):
        return self._w("1", t)

    def dim(self, t):
        return self._w("2", t)

    def cyan(self, t):
        return self._w("36", t)

    def yellow(self, t):
        return self._w("33", t)

    def red(self, t):
        return self._w("31", t)

    def green(self, t):
        return self._w("32", t)


def bar(value, peak, width):
    if peak <= 0:
        return ""
    units = value / peak * width
    full = int(units)
    frac = units - full
    out = "█" * full
    if frac > 0.05 and full < width:
        out += BLOCKS[max(1, min(8, int(frac * 8)))]
    return out


def render_terminal(r, log, ink, args):
    W = args.width
    out = []
    add = out.append

    def rule(title):
        add("")
        add(ink.bold(title) + " " + ink.dim("─" * max(0, W - len(title) - 1)))

    when = "all history"
    if r["range"]:
        when = f"{day_of(r['range'][0])} → {day_of(r['range'][1])}"
    add(ink.bold("BOBSTAT") + ink.dim(f"  ·  {when}  ·  {r['tasks_total']} tasks, {r['turns']} turns"))

    # -- overview ------------------------------------------------------------
    rule("OVERVIEW")
    add(f"  {ink.cyan(ink.bold(money(r['total_task_cost'])))} total across {r['tasks_billed']} billed tasks"
        f"   {ink.dim('·')}  {human_int(r['billed_tokens'])} tokens @ ${r['rate']:.2f}/M")
    add(f"  per task   median {money(r['median_task_cost'])}   p90 {money(r['p90_task_cost'])}"
        f"   max {money(r['max_task_cost'])}")
    drift = r["total_task_cost"] - r["total_turn_cost"]
    if abs(drift) > 0.01:
        add(ink.dim(f"  note: turn-level spend sums to {money(r['total_turn_cost'])}"
                    f" ({money(drift)} less) — older turns pruned from message history."))

    # -- daily ---------------------------------------------------------------
    if r["daily"]:
        rule("SPEND BY DAY")
        peak = max(d["cost"] for d in r["daily"].values()) or 1
        for day, d in list(r["daily"].items())[-21:]:
            add(f"  {day}  {money(d['cost']):>9}  {ink.cyan(bar(d['cost'], peak, W - 42))}"
                f"  {ink.dim(str(d['turns']) + ' turns')}")

    # -- token mix -----------------------------------------------------------
    rule("TOKEN MIX")
    tk = r["tokens"]
    total = r["billed_tokens"] or 1
    for label, key in (("cache read", "cacheRead"), ("cache write", "cacheWrite"),
                       ("fresh input", "fresh"), ("output", "output")):
        v = tk.get(key, 0)
        add(f"  {label:<12} {human_int(v):>7}  {v / total * 100:5.1f}%  "
            f"{ink.cyan(bar(v, total, W - 40))}  {ink.dim(money(v * r['rate'] / 1e6))}")
    b = r["billing"]
    if b["implied_rate"]:
        ok = b["max_residual"] < 0.01
        verdict = ink.green("holds") if ok else ink.yellow("DRIFTED")
        add("")
        add(f"  billing model: cost = (input + output) × ${b['implied_rate']:.2f}/M — {verdict}"
            + ink.dim(f"  (max residual {money(b['max_residual'])} over {b['samples']} tasks)"))
        add(ink.dim(f"  a 90%-off cache-read discount would have been worth "
                    f"{money(b['cache_discount_value'])}; Bob gives none."))

    # -- context creep -------------------------------------------------------
    cs = r["creep_summary"]
    if r["creep"]:
        rule("CONTEXT CREEP")
        add(f"  context grows {ink.bold(human_int(cs['median_growth_per_turn']))} tokens/turn (median"
            f" over {cs['tasks_sampled']} tasks)")
        if cs["cost_per_turn_late"]:
            mult = cs["cost_per_turn_late"] / cs["cost_per_turn_at_start"] if cs["cost_per_turn_at_start"] else 0
            add(f"  turn cost: {money(cs['cost_per_turn_at_start'])} early → "
                f"{money(cs['cost_per_turn_late'])} after turn 20 "
                + (ink.yellow(f"({mult:.1f}× — start a fresh task sooner)") if mult > 2 else ""))
        peak = max((c["median_context"] for c in r["creep"]), default=1) or 1
        buckets = [c for c in r["creep"] if c["n"] >= 2][:30]
        for c in buckets:
            add(f"  turn {c['index']:>3}  {human_int(c['median_context']):>7} ctx  "
                f"{ink.cyan(bar(c['median_context'], peak, W - 45))}  "
                f"{ink.dim(money(c['median_cost']) + '/turn')}")

    # -- tools ---------------------------------------------------------------
    rule("TOOLS  (by residency cost — result tokens × turns they stay in context)")
    add(ink.dim(f"  {'tool':<44}{'calls':>6}{'err':>5}{'p50':>8}{'p90':>8}{'max':>8}"
                f"{'ms':>7}{'resid $':>10}"))
    for s in r["tools"][:args.top]:
        ms = f"{s['mean_ms']:.0f}" if s["mean_ms"] is not None else "-"
        # pad before colouring — ANSI codes count toward str width otherwise
        err_cell = f"{s['errors']:>5}"
        err_cell = ink.red(err_cell) if s["errors"] else ink.dim(err_cell)
        add(f"  {s['name'][:43]:<44}{s['calls']:>6}{err_cell}"
            f"{human_int(s['p50']):>8}{human_int(s['p90']):>8}{human_int(s['max']):>8}"
            f"{ms:>7}{money(s['residency_cost']):>10}")
    kinds = r["by_kind"]
    if kinds:
        parts = [f"{k} {v} calls / {money(r['cost_by_kind'].get(k, 0))}" for k, v in
                 sorted(kinds.items(), key=lambda kv: -kv[1])]
        add(ink.dim("  " + "   ·   ".join(parts)))

    # -- waste ---------------------------------------------------------------
    w = r["waste"]
    rule("WASTE & OPPORTUNITIES")
    any_finding = False

    if w["unused"]:
        any_finding = True
        live = [u for u in w["unused"] if not u["stubbed"]]
        add(f"  {ink.yellow('▸')} {len(w['unused'])} configured tools never called"
            f" ({len(live)} still full-size in tools/list)")
        for u in w["unused"][:8]:
            tag = ink.dim("stubbed") if u["stubbed"] else ink.yellow("full stub")
            add(f"      {u['server']}/{u['tool']:<28} last used {u['last_used']:<12} {tag}")
        if w["unused_cost"] > 0.005:
            add(ink.dim(f"      ≈{money(w['unused_cost'])} of schema re-billed over"
                        f" {r['turns']} turns (at {args.stub_tokens} tok/stub)"))

    if w["truncation"]:
        any_finding = True
        add(f"  {ink.yellow('▸')} results hitting their max_response_chars cap:")
        for t in w["truncation"][:8]:
            add(f"      {t['server']}/{t['tool']:<28} {t['hits']}/{t['calls']} calls at cap {t['cap']}"
                + (ink.red("  ← check for refetch loops") if t["hits"] > t["calls"] * 0.5 else ""))

    repeats = [x for x in w["repeats"] if x["repeats"] >= 2][:8]
    if repeats:
        any_finding = True
        add(f"  {ink.yellow('▸')} identical repeat calls (candidates for cache_ttl):")
        for x in repeats:
            ttl = f"ttl {int(x['ttl'])}s" if x["ttl"] else ink.yellow("no ttl")
            add(f"      {(x['name'] or x['raw'])[:44]:<46} {x['repeats']:>3} repeats  "
                f"{x['served_fast']:>3} served <{CACHE_FAST_MS}ms  {x['in_ttl']:>3} within ttl  {ttl}")
        add(ink.dim(f"      ≈{money(w['repeat_cost'])} of duplicated result tokens"))

    if w["loops"]:
        any_finding = True
        add(f"  {ink.yellow('▸')} suspected pagination/retry loops (≥4 consecutive identical tool):")
        for l in w["loops"][:6]:
            add(f"      {l['at']}  {l['tool'][:40]:<42} ×{l['n']:<4} "
                f"{ink.dim(l['task'] + '  ' + l['title'])}")

    if w["errors"]:
        any_finding = True
        add(f"  {ink.yellow('▸')} tool errors:")
        for e in w["errors"][:6]:
            add(f"      {e['name'][:44]:<46} {e['errors']}/{e['calls']} failed")

    if not any_finding:
        add(ink.green("  nothing flagged — config matches observed usage"))

    # -- top tasks -----------------------------------------------------------
    if r["top_tasks"]:
        rule("COSTLIEST TASKS")
        for t in r["top_tasks"][:8]:
            c = t["costs"]
            title = t["title"][:52] or ink.dim("(untitled)")
            add(f"  {money(c['cost']):>8}  {len(t['turns']):>3} turns  "
                f"{human_int(c.get('contextTokens', 0)):>7} ctx  {title}")

    # -- proxy health --------------------------------------------------------
    if log:
        rule("LEANPROXY HEALTH")
        add(f"  {log['sessions']} sessions  ·  {log['stubs_last'] or '?'} tool stubs sent last start"
            f"  ·  {log['errors']} errors, {log['warns']} warnings")
        if log["crashes"] or log["max_restarts"]:
            add(f"  {ink.red('▸')} {log['crashes']} backend crashes, {log['restarts']} restarts"
                + (ink.red(f", {log['max_restarts']} gave up") if log["max_restarts"] else ""))
        if log["init_failed"]:
            add(ink.dim(f"  {log['init_failed']} initialize requests failed (client re-init noise)"))

    if r["skipped"]:
        add("")
        add(ink.dim(f"  {r['skipped']} unparseable messages skipped"))
    add("")
    return "\n".join(out)


# ---------------------------------------------------------------------------
# HTML rendering
# ---------------------------------------------------------------------------

CSS = """
:root{color-scheme:light;--surface-1:#fcfcfb;--plane:#f9f9f7;--text-primary:#0b0b0b;
--text-secondary:#52514e;--muted:#898781;--grid:#e1e0d9;--baseline:#c3c2b7;
--border:rgba(11,11,11,.10);--s1:#2a78d6;--s2:#eb6834;--s3:#1baf7a;--s4:#eda100;
--good:#0ca30c;--warning:#fab219;--critical:#d03b3b;--seq-4:#86b6ef;--seq-7:#256abf}
@media (prefers-color-scheme:dark){:root:not([data-theme=light]){color-scheme:dark;
--surface-1:#1a1a19;--plane:#0d0d0d;--text-primary:#fff;--text-secondary:#c3c2b7;
--muted:#898781;--grid:#2c2c2a;--baseline:#383835;--border:rgba(255,255,255,.10);
--s1:#3987e5;--s2:#d95926;--s3:#199e70;--s4:#c98500;--seq-4:#184f95;--seq-7:#3987e5}}
[data-theme=dark]{color-scheme:dark;--surface-1:#1a1a19;--plane:#0d0d0d;--text-primary:#fff;
--text-secondary:#c3c2b7;--grid:#2c2c2a;--baseline:#383835;--border:rgba(255,255,255,.10);
--s1:#3987e5;--s2:#d95926;--s3:#199e70;--s4:#c98500;--seq-4:#184f95;--seq-7:#3987e5}
*{box-sizing:border-box}
body{margin:0;background:var(--plane);color:var(--text-primary);
font:15px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif}
.wrap{max-width:1120px;margin:0 auto;padding:40px 24px 80px}
h1{font-size:26px;margin:0 0 4px;letter-spacing:-.01em}
h2{font-size:13px;text-transform:uppercase;letter-spacing:.08em;color:var(--text-secondary);
margin:44px 0 14px;font-weight:600}
.sub{color:var(--muted);font-size:13px;margin-bottom:8px}
.card{background:var(--surface-1);border:1px solid var(--border);border-radius:10px;
padding:20px 22px;margin-bottom:14px}
.tiles{display:grid;grid-template-columns:repeat(auto-fit,minmax(160px,1fr));gap:12px}
.tile .k{font-size:12px;color:var(--muted);text-transform:uppercase;letter-spacing:.06em}
.tile .v{font-size:30px;font-weight:600;letter-spacing:-.02em;margin-top:4px}
.tile .n{font-size:12px;color:var(--text-secondary);margin-top:2px}
table{width:100%;border-collapse:collapse;font-size:13px;font-variant-numeric:tabular-nums}
th{text-align:right;color:var(--muted);font-weight:500;padding:6px 8px;
border-bottom:1px solid var(--grid);white-space:nowrap}
th:first-child,td:first-child{text-align:left}
td{text-align:right;padding:6px 8px;border-bottom:1px solid var(--grid);white-space:nowrap}
tr:last-child td{border-bottom:none}
.scroll{overflow-x:auto}
.mono{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12px}
.legend{display:flex;flex-wrap:wrap;gap:14px;font-size:12px;color:var(--text-secondary);margin-top:10px}
.legend i{width:10px;height:10px;border-radius:2px;display:inline-block;margin-right:6px}
.note{font-size:13px;color:var(--text-secondary);margin-top:10px}
.flag{display:flex;gap:10px;padding:10px 0;border-bottom:1px solid var(--grid);font-size:13px}
.flag:last-child{border-bottom:none}
.flag b{font-weight:600}
.dot{width:8px;height:8px;border-radius:50%;flex:0 0 8px;margin-top:6px}
details{margin-top:12px}summary{cursor:pointer;font-size:12px;color:var(--muted)}
#tip{position:fixed;pointer-events:none;opacity:0;transition:opacity .1s;
background:var(--surface-1);border:1px solid var(--border);border-radius:6px;padding:6px 9px;
font-size:12px;box-shadow:0 4px 14px rgba(0,0,0,.16);z-index:9;white-space:nowrap}
svg{display:block;width:100%;height:auto;overflow:visible}
.gl{stroke:var(--grid);stroke-width:1}
.ax{fill:var(--muted);font-size:11px}
.lbl{fill:var(--text-secondary);font-size:11px}
"""

JS = """
var tip=document.getElementById('tip');
document.addEventListener('mouseover',function(e){
  var t=e.target.closest('[data-tip]'); if(!t)return;
  tip.textContent=t.getAttribute('data-tip'); tip.style.opacity=1;});
document.addEventListener('mousemove',function(e){
  if(tip.style.opacity!=1)return;
  var x=e.clientX+14,y=e.clientY+14;
  if(x+tip.offsetWidth>innerWidth)x=e.clientX-tip.offsetWidth-14;
  tip.style.left=x+'px';tip.style.top=y+'px';});
document.addEventListener('mouseout',function(e){
  if(e.target.closest('[data-tip]'))tip.style.opacity=0;});
"""

E = html.escape


def svg_vbars(rows, value_key, label_key, tip_fn, height=180, color="var(--s1)"):
    """Vertical bar chart. rows: list of dicts. Returns SVG string."""
    if not rows:
        return ""
    n = len(rows)
    w, pad_l, pad_b, pad_t = 1000, 58, 26, 12
    peak = max(r[value_key] for r in rows) or 1
    plot_w = w - pad_l - 8
    plot_h = height - pad_b - pad_t
    slot = plot_w / n
    bw = max(2.0, min(38.0, slot - 2))  # 2px surface gap between adjacent bars
    parts = [f'<svg viewBox="0 0 {w} {height}" role="img">']
    for frac in (0, 0.5, 1):
        y = pad_t + plot_h * (1 - frac)
        parts.append(f'<line class="gl" x1="{pad_l}" x2="{w - 8}" y1="{y:.1f}" y2="{y:.1f}"/>')
        parts.append(f'<text class="ax" x="{pad_l - 6}" y="{y + 3.5:.1f}" text-anchor="end">'
                     f'{E(money(peak * frac))}</text>')
    step = max(1, n // 12)
    for i, r in enumerate(rows):
        h = plot_h * (r[value_key] / peak)
        x = pad_l + slot * i + (slot - bw) / 2
        y = pad_t + plot_h - h
        parts.append(f'<rect x="{x:.1f}" y="{y:.1f}" width="{bw:.1f}" height="{max(h, 1):.1f}" '
                     f'rx="4" fill="{color}" data-tip="{E(tip_fn(r))}"/>')
        if i % step == 0 or i == n - 1:
            parts.append(f'<text class="ax" x="{x + bw / 2:.1f}" y="{height - 8}" '
                         f'text-anchor="middle">{E(str(r[label_key])[5:])}</text>')
    parts.append(f'<line class="gl" x1="{pad_l}" x2="{w - 8}" y1="{pad_t + plot_h}" '
                 f'y2="{pad_t + plot_h}" stroke="var(--baseline)"/>')
    parts.append("</svg>")
    return "".join(parts)


def svg_hbars(rows, value_key, label_key, tip_fn, color="var(--s1)"):
    """Horizontal bars, one row per item, direct-labelled values."""
    if not rows:
        return ""
    row_h, pad_l, pad_r = 24, 300, 90
    w = 1000
    height = row_h * len(rows) + 8
    peak = max(r[value_key] for r in rows) or 1
    plot_w = w - pad_l - pad_r
    parts = [f'<svg viewBox="0 0 {w} {height}" role="img">']
    for i, r in enumerate(rows):
        y = i * row_h + 4
        bw = plot_w * (r[value_key] / peak)
        parts.append(f'<text class="lbl" x="{pad_l - 10}" y="{y + 12}" text-anchor="end">'
                     f'{E(str(r[label_key])[:46])}</text>')
        parts.append(f'<rect x="{pad_l}" y="{y + 3}" width="{max(bw, 1):.1f}" height="{row_h - 10}" '
                     f'rx="4" fill="{color}" data-tip="{E(tip_fn(r))}"/>')
        parts.append(f'<text class="lbl" x="{pad_l + bw + 8:.1f}" y="{y + 12}">'
                     f'{E(r.get("_label", ""))}</text>')
    parts.append("</svg>")
    return "".join(parts)


def svg_stack(segments, height=54):
    """One horizontal stacked bar. segments: [(label, value, color)]"""
    total = sum(s[1] for s in segments) or 1
    w = 1000
    parts = [f'<svg viewBox="0 0 {w} {height}" role="img">']
    x = 0.0
    for label, value, color in segments:
        seg = w * (value / total)
        if seg <= 0:
            continue
        parts.append(f'<rect x="{x:.1f}" y="6" width="{max(seg - 2, 1):.1f}" height="28" rx="4" '
                     f'fill="{color}" data-tip="{E(label)}: {E(human_int(value))} tokens '
                     f'({value / total * 100:.1f}%)"/>')
        if seg > 90:
            parts.append(f'<text class="lbl" x="{x + 6:.1f}" y="{50}">'
                         f'{E(label)} {value / total * 100:.0f}%</text>')
        x += seg
    parts.append("</svg>")
    return "".join(parts)


def svg_line(rows, x_key, y_key, tip_fn, height=200, color="var(--s1)", y_fmt=human_int):
    if len(rows) < 2:
        return ""
    w, pad_l, pad_b, pad_t = 1000, 54, 26, 12
    plot_w, plot_h = w - pad_l - 10, height - pad_b - pad_t
    xs = [r[x_key] for r in rows]
    ys = [r[y_key] for r in rows]
    x0, x1 = min(xs), max(xs) or 1
    peak = max(ys) or 1
    def px(v):
        return pad_l + plot_w * ((v - x0) / (x1 - x0) if x1 > x0 else 0)
    def py(v):
        return pad_t + plot_h * (1 - v / peak)
    parts = [f'<svg viewBox="0 0 {w} {height}" role="img">']
    for frac in (0, 0.5, 1):
        y = pad_t + plot_h * (1 - frac)
        parts.append(f'<line class="gl" x1="{pad_l}" x2="{w - 10}" y1="{y:.1f}" y2="{y:.1f}"/>')
        parts.append(f'<text class="ax" x="{pad_l - 6}" y="{y + 3.5:.1f}" text-anchor="end">'
                     f'{E(y_fmt(peak * frac))}</text>')
    d = " ".join(f"{'M' if i == 0 else 'L'}{px(r[x_key]):.1f},{py(r[y_key]):.1f}"
                 for i, r in enumerate(rows))
    parts.append(f'<path d="{d}" fill="none" stroke="{color}" stroke-width="2" '
                 f'stroke-linejoin="round"/>')
    for r in rows:
        parts.append(f'<circle cx="{px(r[x_key]):.1f}" cy="{py(r[y_key]):.1f}" r="4.5" '
                     f'fill="{color}" stroke="var(--surface-1)" stroke-width="2" '
                     f'data-tip="{E(tip_fn(r))}"/>')
    for v in (x0, (x0 + x1) // 2, x1):
        parts.append(f'<text class="ax" x="{px(v):.1f}" y="{height - 8}" text-anchor="middle">'
                     f'turn {int(v)}</text>')
    parts.append(f'<line x1="{pad_l}" x2="{w - 10}" y1="{pad_t + plot_h}" y2="{pad_t + plot_h}" '
                 f'stroke="var(--baseline)"/>')
    parts.append("</svg>")
    return "".join(parts)


def table(headers, rows):
    head = "".join(f"<th>{E(h)}</th>" for h in headers)
    body = "".join("<tr>" + "".join(f"<td>{c}</td>" for c in row) + "</tr>" for row in rows)
    return f'<div class="scroll"><table><thead><tr>{head}</tr></thead><tbody>{body}</tbody></table></div>'


def render_html(r, log, args):
    when = "all history"
    if r["range"]:
        when = f"{day_of(r['range'][0])} → {day_of(r['range'][1])}"
    P = []
    add = P.append

    add(f'<div class="wrap"><h1>Bob usage</h1>'
        f'<div class="sub">{E(when)} · {r["tasks_total"]} tasks · {r["turns"]} turns · '
        f'flat ${r["rate"]:.2f}/M</div>')

    # tiles
    tiles = [
        ("total spend", money(r["total_task_cost"]), f'{r["tasks_billed"]} billed tasks'),
        ("median task", money(r["median_task_cost"]), f'p90 {money(r["p90_task_cost"])}'),
        ("tokens billed", human_int(r["billed_tokens"]), "input + output"),
        ("context growth", human_int(r["creep_summary"]["median_growth_per_turn"]) + "/turn",
         "median across tasks"),
        ("tool calls", human_int(sum(r["by_kind"].values())),
         " · ".join(f'{k} {v}' for k, v in sorted(r["by_kind"].items(), key=lambda kv: -kv[1]))),
    ]
    add('<div class="tiles">' + "".join(
        f'<div class="card tile"><div class="k">{E(k)}</div><div class="v">{E(v)}</div>'
        f'<div class="n">{E(n)}</div></div>' for k, v, n in tiles) + "</div>")

    # daily
    if r["daily"]:
        rows = [{"day": d, **v} for d, v in r["daily"].items()]
        add("<h2>Spend by day</h2><div class='card'>")
        add(svg_vbars(rows, "cost", "day",
                      lambda x: f'{x["day"]}: {money(x["cost"])} · {x["turns"]} turns · '
                                f'{human_int(x["tokens"])} tokens'))
        add("<details><summary>data table</summary>"
            + table(["day", "spend", "turns", "tokens"],
                    [[E(x["day"]), money(x["cost"]), x["turns"], human_int(x["tokens"])]
                     for x in rows]) + "</details></div>")

    # token mix
    tk = r["tokens"]
    segs = [("cache read", tk.get("cacheRead", 0), "var(--s1)"),
            ("cache write", tk.get("cacheWrite", 0), "var(--s2)"),
            ("fresh input", tk.get("fresh", 0), "var(--s3)"),
            ("output", tk.get("output", 0), "var(--s4)")]
    b = r["billing"]
    add("<h2>Token mix</h2><div class='card'>")
    add(svg_stack(segs))
    add('<div class="legend">' + "".join(
        f'<span><i style="background:{c}"></i>{E(l)} — {human_int(v)} '
        f'({money(v * r["rate"] / 1e6)})</span>' for l, v, c in segs) + "</div>")
    if b["implied_rate"]:
        ok = b["max_residual"] < 0.01
        add(f'<div class="note"><b>Billing model:</b> cost = (input + output) × '
            f'${b["implied_rate"]:.2f}/M — <span style="color:'
            f'{"var(--good)" if ok else "var(--warning)"}">{"holds" if ok else "drifted"}</span> '
            f'across {b["samples"]} tasks (max residual {money(b["max_residual"])}). '
            f'Cache reads are billed at full price: a 90% cache discount would have been worth '
            f'<b>{money(b["cache_discount_value"])}</b>.</div>')
    add("</div>")

    # context creep
    creep = [c for c in r["creep"] if c["n"] >= 2]
    if len(creep) >= 2:
        cs = r["creep_summary"]
        add("<h2>Context creep</h2><div class='card'>")
        add(svg_line(creep, "index", "median_context",
                     lambda x: f'turn {x["index"]}: {human_int(x["median_context"])} ctx · '
                               f'{money(x["median_cost"])}/turn · n={x["n"]}'))
        add('<div class="note">Median context by turn index. Under flat pricing every token here '
            f'is re-billed each turn — cost per turn runs {money(cs["cost_per_turn_at_start"])} '
            f'early vs {money(cs["cost_per_turn_late"])} after turn 20.</div>')
        add("<details><summary>cost per turn</summary>"
            + table(["turn", "median context", "median cost", "samples"],
                    [[x["index"], human_int(x["median_context"]), money(x["median_cost"]), x["n"]]
                     for x in creep]) + "</details></div>")

    # tools
    tools = r["tools"][:args.top]
    if tools:
        add("<h2>Tools by residency cost</h2><div class='card'>")
        bars = [{"name": t["name"], "v": t["residency_cost"],
                 "_label": money(t["residency_cost"]), "t": t} for t in tools]
        add(svg_hbars(bars, "v", "name",
                      lambda x: f'{x["name"]}: {money(x["v"])} residency · {x["t"]["calls"]} calls · '
                                f'p50 {human_int(x["t"]["p50"])} chars'))
        add('<div class="note">Residency cost = result tokens × the number of turns they stay in '
            'context. A big result early in a long task is billed over and over.</div>')
        add(table(["tool", "kind", "calls", "err", "p50 chars", "p90", "max", "mean ms", "residency $"],
                  [[E(t["name"]), E(t["kind"]), t["calls"], t["errors"], human_int(t["p50"]),
                    human_int(t["p90"]), human_int(t["max"]),
                    f'{t["mean_ms"]:.0f}' if t["mean_ms"] is not None else "–",
                    money(t["residency_cost"])] for t in tools]))
        add("</div>")

    # waste
    w = r["waste"]
    flags = []
    if w["unused"]:
        live = [u for u in w["unused"] if not u["stubbed"]]
        flags.append(("var(--warning)", f'{len(w["unused"])} configured tools never called',
                      ", ".join(f'{u["server"]}/{u["tool"]}' for u in w["unused"][:10])
                      + (f' — {len(live)} still full-size, ≈{money(w["unused_cost"])} of schema '
                         f'across {r["turns"]} turns' if live else " — all adaptively stubbed")))
    for t in w["truncation"][:6]:
        sev = "var(--critical)" if t["hits"] > t["calls"] * 0.5 else "var(--warning)"
        flags.append((sev, f'{t["server"]}/{t["tool"]} hit its {t["cap"]}-char cap',
                      f'{t["hits"]} of {t["calls"]} calls truncated — check whether the model '
                      f'refetched to get the tail'))
    for x in [x for x in w["repeats"] if x["repeats"] >= 2][:6]:
        ttl = f'ttl {int(x["ttl"])}s' if x["ttl"] else "no cache_ttl configured"
        flags.append(("var(--s1)", f'{x["name"] or x["raw"]} repeated {x["repeats"]}×',
                      f'{x["served_fast"]} served in <{CACHE_FAST_MS}ms (cache), '
                      f'{x["in_ttl"]} landed inside the TTL window — {ttl}'))
    for l in w["loops"][:5]:
        flags.append(("var(--warning)", f'{l["tool"]} ×{l["n"]} back to back',
                      f'{l["at"]} · task {l["task"]} · {l["title"]}'))
    for e in w["errors"][:5]:
        flags.append(("var(--critical)", f'{e["name"]} failed {e["errors"]}/{e["calls"]} calls', ""))
    add("<h2>Waste &amp; opportunities</h2><div class='card'>")
    if flags:
        for color, title, detail in flags:
            add(f'<div class="flag"><span class="dot" style="background:{color}"></span>'
                f'<span><b>{E(title)}</b>'
                + (f'<br><span style="color:var(--text-secondary)">{E(detail)}</span>' if detail else "")
                + "</span></div>")
    else:
        add('<div class="note">Nothing flagged — config matches observed usage.</div>')
    add("</div>")

    # top tasks
    if r["top_tasks"]:
        add("<h2>Costliest tasks</h2><div class='card'>")
        add(table(["task", "spend", "turns", "context", "project"],
                  [[E(t["title"][:70] or "(untitled)"), money(t["costs"]["cost"]), len(t["turns"]),
                    human_int(t["costs"].get("contextTokens", 0)),
                    E(os.path.basename(t["project"]))] for t in r["top_tasks"][:12]]))
        add("</div>")

    if log:
        add("<h2>Leanproxy health</h2><div class='card'>")
        add(table(["metric", "value"], [
            ["sessions", log["sessions"]],
            ["tool stubs sent (last start)", log["stubs_last"] if log["stubs_last"] is not None else "–"],
            ["backend crashes", log["crashes"]],
            ["restarts scheduled", log["restarts"]],
            ["restart budgets exhausted", log["max_restarts"]],
            ["log errors / warnings", f'{log["errors"]} / {log["warns"]}'],
        ]))
        add("</div>")

    add(f'<div class="sub" style="margin-top:40px">Generated by scripts/bobstat.py from '
        f'{E(args.db)}{" · " + E(log["path"]) if log else ""}</div>')
    add("</div>")

    return (f'<!doctype html><html><head><meta charset="utf-8">'
            f'<meta name="viewport" content="width=device-width,initial-scale=1">'
            f'<title>Bob usage — {E(when)}</title><style>{CSS}</style></head><body>'
            f'{"".join(P)}<div id="tip"></div><script>{JS}</script></body></html>')


# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------


def to_jsonable(r):
    out = dict(r)
    out["daily"] = r["daily"]
    out["projects"] = dict(r["projects"])
    out["top_tasks"] = [
        {"id": t["id"], "title": t["title"], "project": t["project"],
         "cost": t["costs"].get("cost", 0), "turns": len(t["turns"]),
         "context": t["costs"].get("contextTokens", 0)}
        for t in r["top_tasks"]
    ]
    if r["range"]:
        out["range"] = {"from": day_of(r["range"][0]), "to": day_of(r["range"][1])}
    return out


def main(argv=None):
    p = argparse.ArgumentParser(
        prog="bobstat", description="Visualize Bob usage, cost and tool behaviour.")
    p.add_argument("--db", default=DEFAULT_DB, help=f"Bob sqlite store (default {DEFAULT_DB})")
    p.add_argument("--config", default=DEFAULT_CONFIG, help="leanproxy servers yaml")
    p.add_argument("--usage", default=DEFAULT_USAGE, help="leanproxy toolusage.json")
    p.add_argument("--log", default=DEFAULT_LOG, help="leanproxy log file")
    p.add_argument("--since", help="7d / 24h / 2026-08-20 — filter by task activity")
    p.add_argument("--project", help="substring match against the task's project path")
    p.add_argument("--task", help="task id prefix")
    p.add_argument("--rate", type=float, default=DEFAULT_RATE, help="$ per 1M tokens (default 2.0)")
    p.add_argument("--chars-per-token", type=float, default=DEFAULT_CHARS_PER_TOKEN,
                   dest="chars_per_token")
    p.add_argument("--stub-tokens", type=int, default=DEFAULT_STUB_TOKENS, dest="stub_tokens",
                   help="estimated tokens per tool stub in tools/list")
    p.add_argument("--top", type=int, default=15, help="rows in the leaderboards")
    p.add_argument("--width", type=int, default=0, help="terminal width")
    p.add_argument("--html", metavar="PATH", help="also write a self-contained HTML report")
    p.add_argument("--open", action="store_true", help="open the HTML report in a browser")
    p.add_argument("--json", action="store_true", help="emit JSON instead of the terminal report")
    p.add_argument("--no-color", action="store_true")
    args = p.parse_args(argv)

    if not args.width:
        args.width = min(110, max(72, os.get_terminal_size().columns if sys.stdout.isatty() else 100))
    if not os.path.exists(args.db):
        raise SystemExit(f"bobstat: no Bob database at {args.db} (use --db)")

    config = load_config(args.config)
    usage = load_toolusage(args.usage)
    log = load_log(args.log)
    data = load(args, config)
    if not data["tasks"]:
        raise SystemExit("bobstat: no tasks matched the filters")
    report = analyze(data, config, usage, args)

    if args.json:
        json.dump(to_jsonable(report), sys.stdout, indent=2, default=str)
        sys.stdout.write("\n")
    else:
        ink = Ink(sys.stdout.isatty() and not args.no_color and not os.environ.get("NO_COLOR"))
        print(render_terminal(report, log, ink, args))

    if args.html:
        path = os.path.abspath(args.html)
        with open(path, "w", encoding="utf-8") as fh:
            fh.write(render_html(report, log, args))
        if not args.json:
            print(f"  html report → {path}\n")
        if args.open:
            webbrowser.open(f"file://{path}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
