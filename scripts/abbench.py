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
DEFAULT_DB = os.path.join(HOME, ".bob", "db", "bob.db")

ARMS = ("native", "router", "lazy")


# ---------------------------------------------------------------------------
# confound detection
# ---------------------------------------------------------------------------


def _lp_enabled_servers(lp_cfg_text: str) -> list:
    """Names of servers marked enabled in a leanproxy_servers.yaml.

    Deliberately a regex rather than a YAML parse: this module is stdlib-only
    and the shape being matched is a flat `- name:` / `enabled:` pair list.
    """
    names = []
    current = None
    for line in lp_cfg_text.splitlines():
        m = re.match(r"\s*-\s+name:\s*(\S+)", line)
        if m:
            current = m.group(1).strip("\"'")
            continue
        if current and re.match(r"\s*enabled:\s*true\b", line):
            names.append(current)
            current = None
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

    Restores on normal exit, on exception, and on SIGINT/SIGTERM. A
    half-swapped config leaves Bob broken, so restoration is unconditional.
    """

    def __init__(self, path: str, new_cfg: dict):
        self.path = path
        self.new_cfg = new_cfg
        self.backup = path + ".abbench-backup"
        self._prev_handlers = {}

    def __enter__(self):
        shutil.copy2(self.path, self.backup)
        for sig in (signal.SIGINT, signal.SIGTERM):
            self._prev_handlers[sig] = signal.signal(sig, self._on_signal)
        with open(self.path, "w") as fh:
            json.dump(self.new_cfg, fh, indent=2)
        return self

    def _on_signal(self, signum, frame):
        self._restore()
        sys.exit(128 + signum)

    def _restore(self):
        if os.path.exists(self.backup):
            shutil.move(self.backup, self.path)
        for sig, handler in self._prev_handlers.items():
            if handler is not None:
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
