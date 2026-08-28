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
import sys

HOME = os.path.expanduser("~")
DEFAULT_BOB_CFG = os.path.join(HOME, ".bob", "settings", "mcp.json")
DEFAULT_LP_CFG = os.path.join(HOME, ".config", "leanproxy_servers.yaml")
DEFAULT_DB = os.path.join(HOME, ".bob", "db", "bob.db")  # unused until Task 6's runner reads turn logs

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
    worse than one that refuses to run.
    """
    names = []
    item = None          # top-level keys collected for the item being scanned
    item_dash_indent = None  # column of the '-' that started this item
    item_key_indent = None   # column at which this item's own keys sit
    in_servers = False

    def _flush():
        if item is None:
            return
        name = item.get("name")
        enabled = item.get("enabled")
        if name and enabled is not None and enabled.strip().lower() == "true":
            names.append(name.strip().strip("\"'"))

    for raw in lp_cfg_text.splitlines():
        line = _strip_comment(raw).rstrip()
        if not line.strip():
            continue
        indent = len(line) - len(line.lstrip())
        content = line[indent:]

        if not in_servers:
            km = _KEY_RE.match(content)
            if indent == 0 and km and km.group("key") == "servers":
                in_servers = True
            continue  # nothing outside `servers:` is ours to interpret

        if indent == 0:
            # dedent to a sibling top-level key: the servers block is over
            _flush()
            item = None
            item_dash_indent = None
            item_key_indent = None
            in_servers = False
            continue

        if item is not None and indent <= item_dash_indent:
            _flush()
            item = None
            item_dash_indent = None
            item_key_indent = None

        m_item = _LIST_ITEM_RE.match(content)
        if m_item:
            _flush()
            rest = m_item.group("rest")
            km = _KEY_RE.match(rest)
            if not km:
                raise ValueError(f"unrecognised leanproxy config line: {raw!r}")
            item = {km.group("key"): km.group("value").strip()}
            item_dash_indent = indent
            item_key_indent = indent + (len(content) - len(rest))
            continue

        if item is not None and indent == item_key_indent:
            km = _KEY_RE.match(content)
            if not km:
                raise ValueError(f"unrecognised leanproxy config line: {raw!r}")
            item[km.group("key")] = km.group("value").strip()
            continue

        if item is not None and indent > item_key_indent:
            continue  # nested under one of this item's keys — not top-level

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


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--out", default="bench-results")
    ap.add_argument("--bob-config", default=DEFAULT_BOB_CFG)
    ap.add_argument("--lp-config", default=DEFAULT_LP_CFG)
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

    print("config clean; runner lands in the next task")
    return 0


if __name__ == "__main__":
    sys.exit(main())
