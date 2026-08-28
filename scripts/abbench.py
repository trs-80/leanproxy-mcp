#!/usr/bin/env python3
"""abbench — live A/B benchmark for LeanProxy across three arms.

Measures what the residency sweep cannot: how many turns a real model takes,
and whether it finds the tools it needs at all. Layer 1
(tests/bench/e2e/residency_test.go) covers residency for free; this script
covers behaviour, and it spends coins to do it.

    LEANPROXY_AB_LIVE=1 python3 scripts/abbench.py --out bench-results

Python 3.9+, standard library only.
"""

from __future__ import annotations

import argparse
import copy
import json
import os
import re
import shutil
import signal
import sqlite3
import subprocess
import sys
import time

HOME = os.path.expanduser("~")
DEFAULT_BOB_CFG = os.path.join(HOME, ".bob", "settings", "mcp.json")
DEFAULT_LP_CFG = os.path.join(HOME, ".config", "leanproxy_servers.yaml")
DEFAULT_DB = os.path.join(HOME, ".bob", "db", "bob.db")  # default db_path for run_task/read_task_result; main() doesn't wire the sweep loop yet

ARMS = ("native", "router", "lazy")


# ---------------------------------------------------------------------------
# confound detection
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


def _lp_enabled_servers(lp_cfg_text: str) -> list:
    """Names of servers marked enabled in a leanproxy_servers.yaml.

    Deliberately a small indent-aware block parser rather than a full YAML
    parse: this module is stdlib-only. Each list item (`- name: ...`) is
    treated as a flat block of top-level `key: value` pairs living at the
    item's own indent column; a key nested under a deeper mapping inside the
    item (e.g. a `retry:` sub-block) is not mistaken for the item's own
    `enabled:` — and key order within the item doesn't matter.

    Strict parsing is scoped to the `servers:` block only: real configs have
    unrelated top-level sections afterward (e.g. `bouncer:`, with its own
    nested `enabled:`) that this has no business interpreting. Content that
    doesn't fit the expected shape *inside* `servers:` raises rather than
    silently under-reporting — a confound guard that can under-report is
    worse than one that refuses to run. This includes content indented
    deeper than an item's own key column when the key that precedes it
    already carried a scalar value: real YAML rejects that shape outright
    ("mapping values are not allowed here"), so treating it as harmless
    nesting would silently drop a server that really is enabled. Only a key
    with an *empty* inline value (a genuine block opener, e.g. `retry:`)
    may be followed by deeper-indented content, and that content is then
    skipped wholesale — this parser does not care what it says.
    """
    names = []
    item = None          # top-level keys collected for the item being scanned
    item_dash_indent = None  # column of the '-' that started this item
    item_key_indent = None   # column at which this item's own keys sit
    last_key_empty = False   # did the most recent item_key_indent key have no inline value?
    in_nested = False        # are we inside a block that key legitimately opened?
    in_servers = False

    def _flush():
        if item is None:
            return
        name = item.get("name")
        enabled = item.get("enabled")
        if name and enabled is not None and enabled.strip().lower() == "true":
            names.append(name.strip().strip("\"'"))

    def _reset_item():
        nonlocal item, item_dash_indent, item_key_indent, last_key_empty, in_nested
        item = None
        item_dash_indent = None
        item_key_indent = None
        last_key_empty = False
        in_nested = False

    for raw in lp_cfg_text.splitlines():
        line = _strip_comment(raw).rstrip()
        if not line.strip():
            continue
        indent = len(line) - len(line.lstrip())
        content = line[indent:]

        if not in_servers:
            km = _KEY_RE.match(content)
            if indent == 0 and km and km.group("key") == "servers":
                if km.group("value").strip():
                    raise ValueError(
                        f"unsupported inline value for the servers: block: {raw!r} "
                        f"— expected a block list, not a flow-style/scalar value"
                    )
                in_servers = True
            continue  # nothing outside `servers:` is ours to interpret

        if indent == 0:
            # dedent to a sibling top-level key: the servers block is over
            _flush()
            _reset_item()
            in_servers = False
            continue

        if item is not None and indent <= item_dash_indent:
            _flush()
            _reset_item()

        m_item = _LIST_ITEM_RE.match(content)
        if m_item:
            _flush()
            rest = m_item.group("rest")
            km = _KEY_RE.match(rest)
            if not km:
                raise ValueError(f"unrecognised leanproxy config line: {raw!r}")
            value = km.group("value").strip()
            item = {km.group("key"): value}
            item_dash_indent = indent
            item_key_indent = indent + (len(content) - len(rest))
            last_key_empty = not value
            in_nested = False
            continue

        if item is not None and indent == item_key_indent:
            km = _KEY_RE.match(content)
            if not km:
                raise ValueError(f"unrecognised leanproxy config line: {raw!r}")
            value = km.group("value").strip()
            item[km.group("key")] = value
            last_key_empty = not value
            in_nested = False
            continue

        if item is not None and indent > item_key_indent:
            if in_nested:
                continue  # already inside a legitimately-opened nested block
            if last_key_empty:
                in_nested = True
                continue
            # the preceding key already had a scalar value — real YAML
            # rejects a deeper-indented continuation after it outright
            raise ValueError(
                f"unrecognised leanproxy config line: {raw!r} — indented "
                f"past a key that already had a value"
            )

        # inside `servers:`, indented, but not attributable to any open item
        raise ValueError(f"unrecognised leanproxy config line: {raw!r}")

    _flush()
    return names


def detect_confound(bob_cfg: dict, lp_cfg_text: str) -> list:
    """Servers reachable both directly from Bob and through the proxy.

    A server loaded twice inflates baseline schema weight and confounds every
    arm, so the harness refuses to run until this is resolved.
    """
    proxied = set(_lp_enabled_servers(lp_cfg_text))
    problems = []
    for name, entry in (bob_cfg.get("mcpServers") or {}).items():
        if name == "leanproxy":
            continue
        if entry.get("disabled") is True or entry.get("enabled") is False:
            continue
        if name in proxied:
            problems.append(
                f"{name!r} is enabled directly in Bob and also proxied by leanproxy "
                f"— it would be loaded twice"
            )
    return problems


# ---------------------------------------------------------------------------
# arm configuration
# ---------------------------------------------------------------------------


def arm_config(arm: str, leanproxy_bin: str, base: dict) -> dict:
    """Return a Bob mcp.json for the given arm, leaving `base` untouched."""
    if arm not in ARMS:
        raise ValueError(f"unknown arm: {arm}")

    cfg = copy.deepcopy(base)
    servers = cfg.setdefault("mcpServers", {})

    if arm == "native":
        servers.pop("leanproxy", None)
        return cfg

    args = ["server", "run", "--stdio", "--log-file", "/tmp/leanproxy-abbench.log"]
    if arm == "lazy":
        args.insert(3, "--lazy-tools")

    servers["leanproxy"] = {
        "command": leanproxy_bin,
        "args": args,
        "disabled": False,
        "enabled": True,
    }
    return cfg


# ---------------------------------------------------------------------------
# config swap
# ---------------------------------------------------------------------------


class ConfigSwap:
    """Swap a JSON config into place, restoring it on every exit path.

    Restores on normal exit, on exception (including one raised inside
    __enter__ itself, which Python's `with` statement does NOT route to
    __exit__), and on SIGINT/SIGTERM. A half-swapped config leaves Bob
    broken, so restoration is unconditional and fails closed: a pre-existing
    backup file means a prior run crashed without restoring (or another
    instance is already running), and this refuses to proceed rather than
    risk clobbering the one copy of the real original.
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
            shutil.copy2(self.path, self.backup)
            self._backed_up = True
            for sig in (signal.SIGINT, signal.SIGTERM):
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
        """Atomically claim the backup path, refusing if one already exists.

        A pre-existing backup is either a crashed prior run (the real
        original is sitting in it, unrestored) or a concurrent instance —
        either way, proceeding would risk clobbering the only copy of the
        true original, so this raises loudly instead.
        """
        try:
            fd = os.open(self.backup, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
            os.close(fd)
        except FileExistsError:
            raise RuntimeError(
                f"backup file already exists at {self.backup!r} — a previous "
                f"abbench run may have crashed without restoring, or another "
                f"instance is already running; restore {self.path!r} from the "
                f"backup (or remove the backup if it's stale) before running again"
            ) from None

    def _on_signal(self, signum, frame):
        self._restore()
        sys.exit(128 + signum)

    def _restore(self):
        if self._backed_up and os.path.exists(self.backup):
            shutil.move(self.backup, self.path)
            self._backed_up = False
        elif os.path.exists(self.backup):
            # the backup claim was made but copy2 never completed — it's an
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


def _tool_call_name(data: str) -> str:
    """The called tool's name from a role='tool' message's `data` JSON.

    The real identifier lives at `toolUsage.signature.name` — verified
    against actual recorded history
    (`json_extract(data,'$.toolUsage.signature.name')`). Matching the whole
    `data` blob instead (as an earlier version of this function did) is
    wrong: it can also hit the tool's result `content` or call `arguments`,
    so `succeeded` can go true because the expected tool's name merely
    appeared in some *other* tool's output text.

    A row that isn't valid JSON, or is JSON but missing this path, raises
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
        return obj["toolUsage"]["signature"]["name"]
    except (KeyError, TypeError) as exc:
        raise ValueError(
            f"tool message missing toolUsage.signature.name: {data[:200]!r}"
        ) from exc


def read_task_result(db_path: str, task_id: str, expect_tool: str) -> dict:
    """Read ground-truth token counts, turn count, and success for one task.

    Turn count is the number of assistant messages: each one is a model call
    that re-sends the whole conversation, which is exactly the cost lazy
    loading trades residency for.

    `succeeded` means "the model reached for the right tool" — it matches
    `expect_tool` as a substring of the CALLED TOOL'S NAME ONLY (see
    `_tool_call_name`), not "the task completed correctly": a tool call's
    own `toolUsage.signature.isError` is not consulted. Substring, not
    equality, because the same tool is named `codebase-memory_get_architecture`
    behind the proxy and `get_architecture` natively; fixtures name the
    unprefixed form so both arms compare.
    """
    conn = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    try:
        row = conn.execute("select costs from tasks where id = ?", (task_id,)).fetchone()
        costs = json.loads(row[0]) if row and row[0] else {}

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

    tool_names = [_tool_call_name(r[0]) for r in tool_rows if r[0]]
    succeeded = any(expect_tool in name for name in tool_names)

    return {
        "task_id": task_id,
        "input_tokens": costs.get("input", 0),
        "output_tokens": costs.get("output", 0),
        "cache_read": costs.get("cacheRead", 0),
        "cache_write": costs.get("cacheWrite", 0),
        "cost_usd": costs.get("cost", 0.0),
        "context_tokens": costs.get("contextTokens", 0),
        "turns": turns,
        "succeeded": succeeded,
    }


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

    `consistent` reports whether every paired delta shares a sign. When it is
    False the caller should report "no detectable effect" rather than a point
    estimate.
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

    return {
        "baseline": baseline,
        "arm": arm,
        "field": field,
        "pairs": len(shared),
        "deltas": deltas,
        "total_delta": sum(deltas.values()),
        "consistent": len(signs) <= 1 and len(deltas) > 0,
    }


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--out", default="bench-results")
    ap.add_argument("--bob-config", default=DEFAULT_BOB_CFG)
    ap.add_argument("--lp-config", default=DEFAULT_LP_CFG)
    ap.add_argument("--db", default=DEFAULT_DB)
    ap.add_argument("--cwd", default=os.getcwd())
    ap.add_argument("--leanproxy-bin", default=os.path.join(HOME, ".local", "bin", "leanproxy-mcp"))
    ap.add_argument("--tasks", default=os.path.join(
        os.path.dirname(os.path.abspath(__file__)), "..",
        "tests", "bench", "e2e", "fixtures", "tasks.json"))
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

    tasks = load_tasks(args.tasks)
    leanproxy_bin = args.leanproxy_bin
    records = []

    for arm in ARMS:
        cfg = arm_config(arm, leanproxy_bin, bob_cfg)
        with ConfigSwap(args.bob_config, cfg):
            for t in tasks:
                print(f"[{arm}] {t['id']} ...", flush=True)
                try:
                    task_id = run_task(t["prompt"], args.cwd, args.db)
                except Exception as exc:  # noqa: BLE001 - a failed run is data
                    print(f"  run failed: {exc}", file=sys.stderr)
                    records.append({"layer": "live", "origin": "measured", "arm": arm,
                                    "task": t["id"], "succeeded": False, "error": str(exc)})
                    continue
                res = read_task_result(args.db, task_id, t["expect_tool"])
                res.update({"layer": "live", "origin": "measured", "arm": arm, "task": t["id"]})
                records.append(res)
                print(f"  cost={res['cost_usd']:.4f} turns={res['turns']} ok={res['succeeded']}")

    os.makedirs(args.out, exist_ok=True)
    stamp = time.strftime("%Y%m%d-%H%M%S")
    out_path = os.path.join(args.out, f"e2e-live-{stamp}.json")
    with open(out_path, "w") as fh:
        json.dump(records, fh, indent=2)

    print(f"\nwrote {out_path}\n")
    print("Paired per-task deltas vs native (primary statistic):")
    for arm in ("router", "lazy"):
        for field in ("cost_usd", "turns"):
            d = paired_deltas(records, "native", arm, field)
            if d["pairs"] == 0:
                continue
            verdict = f"{d['total_delta']:+.4f}" if d["consistent"] else "no detectable effect (signs disagree)"
            print(f"  {arm:<7} {field:<9} pairs={d['pairs']} {verdict}")

    failures = [r for r in records if not r.get("succeeded")]
    if failures:
        print(f"\n{len(failures)} task run(s) failed — discovery failures are a real "
              f"cost of lazy loading and are reported, not discarded:")
        for f in failures:
            print(f"  {f['arm']}/{f['task']}")

    return 0


if __name__ == "__main__":
    sys.exit(main())
