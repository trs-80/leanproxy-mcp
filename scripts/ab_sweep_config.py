#!/usr/bin/env python3
"""Prepare (and undo) the operator's MCP configuration for a live A/B sweep.

`abbench.py` refuses to spend anything against a configuration it cannot
measure. Two of its refusals are environmental rather than code, and both are
resolved the same way — by disabling an entry for the duration of the sweep:

  1. A server reachable BOTH directly from the agent and through the proxy.
     It is loaded twice, so its schema weight lands in every arm's baseline
     and confounds the comparison. Matching is on resolved command path or
     URL, not name, so the same upstream under two different names still
     counts (`codebase-memory-mcp` in the agent vs `codebase-memory` behind
     the proxy).

  2. A proxied HTTP server with auth headers. This harness will not copy
     credentials into the agent's config, so the native arm cannot reach it
     and would silently measure a smaller tool inventory than the proxy arms.

What gets disabled is computed from abbench's own checks, never hardcoded, so
this stays correct as the operator's configuration changes.

    python3 scripts/ab_sweep_config.py check     # what would refuse, and why
    python3 scripts/ab_sweep_config.py apply     # disable those entries
    LEANPROXY_AB_LIVE=1 make bench-e2e-live
    python3 scripts/ab_sweep_config.py restore   # put production back

`apply` copies each file it touches to `<path>.absweep-bak` first and refuses
if that backup already exists, so a second `apply` can never promote an
already-edited file to "the operator's original". `restore` puts the backups
back byte for byte and removes them.
"""

import argparse
import importlib.util
import json
import os
import pathlib
import re
import shutil
import sys

_REPO_ROOT = pathlib.Path(__file__).resolve().parents[1]

_spec = importlib.util.spec_from_file_location(
    "abbench", _REPO_ROOT / "scripts" / "abbench.py")
abbench = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(abbench)

BACKUP_SUFFIX = ".absweep-bak"


class AlreadyApplied(Exception):
    """A backup already exists, so the live file is not the operator's own."""


class NotApplied(Exception):
    """Nothing to restore."""


def backup_path(path: str) -> str:
    return path + BACKUP_SUFFIX


# ---------------------------------------------------------------------------
# what would refuse
# ---------------------------------------------------------------------------


def _unmirrorable_proxied_servers(lp_text: str) -> list:
    """Enabled proxied servers the native arm cannot attach directly.

    Asks `lp_server_to_bob_entry` per server rather than parsing the combined
    message `direct_entries_for_proxied_servers` raises, so this cannot drift
    from what abbench actually rejects.
    """
    out = []
    for srv in abbench.lp_servers(lp_text):
        if not abbench._server_enabled(srv):
            continue
        name = abbench._unquote(srv.get("name", ""))
        try:
            abbench.lp_server_to_bob_entry(srv)
        except abbench.UnmirrorableServer as exc:
            out.append((name, str(exc)))
    return out


def _double_loaded_agent_entries(bob_cfg: dict, lp_text: str) -> list:
    """Agent-side server names that abbench's confound check objects to."""
    names = []
    for problem in abbench.detect_confound(bob_cfg, lp_text):
        m = re.match(r"^'([^']+)'", problem)
        if m:
            names.append(m.group(1))
    return names


def check(bob_path: str, lp_path: str) -> tuple:
    """(ready, problems) for the configuration as it stands right now."""
    lp_text = open(lp_path).read()
    bob_cfg = json.load(open(bob_path))

    problems = list(abbench.detect_confound(bob_cfg, lp_text))
    problems += [msg for _, msg in _unmirrorable_proxied_servers(lp_text)]
    return (not problems), problems


# ---------------------------------------------------------------------------
# editing
# ---------------------------------------------------------------------------


def _claim_backup(path: str) -> None:
    """Copy path to its backup, refusing if one is already there.

    O_EXCL, like abbench's ConfigSwap: the failure this prevents is a second
    apply overwriting the pristine backup with the already-edited file, after
    which restore would reinstate the sweep configuration as production.
    """
    dst = backup_path(path)
    mode = os.stat(path).st_mode & 0o777
    try:
        fd = os.open(dst, os.O_CREAT | os.O_EXCL | os.O_WRONLY, mode)
    except FileExistsError as exc:
        raise AlreadyApplied(
            f"{dst} already exists — {path} is not the operator's original. "
            f"Run `restore` before applying again."
        ) from exc
    with os.fdopen(fd, "wb") as out, open(path, "rb") as src:
        shutil.copyfileobj(src, out)
        out.flush()
        os.fsync(out.fileno())


def _write_preserving_mode(path: str, data) -> None:
    mode = os.stat(path).st_mode & 0o777
    tmp = path + ".absweep-tmp"
    flags = os.O_CREAT | os.O_WRONLY | os.O_TRUNC
    with os.fdopen(os.open(tmp, flags, mode), "wb" if isinstance(data, bytes) else "w") as fh:
        fh.write(data)
        fh.flush()
        os.fsync(fh.fileno())
    os.replace(tmp, path)


def disable_lp_servers(lp_text: str, names: list) -> str:
    """Set `enabled: false` on the named servers of a leanproxy config.

    Textual and block-scoped: abbench's parser is read-only and this module is
    stdlib-only, so rewriting the file through a YAML round-trip is not an
    option — and would reflow the operator's comments even if it were. The
    scan tracks which `- name:` block each line belongs to and stops at the
    next top-level key, so `bouncer:`'s own `enabled:` is never touched.
    """
    if not names:
        return lp_text
    wanted = set(names)
    out, current = [], None
    for line in lp_text.splitlines(True):
        if re.match(r"^[A-Za-z0-9_-]+:", line):
            current = None
        m = re.match(r"^\s*-\s*name:\s*(\S+)", line)
        if m:
            current = abbench._unquote(m.group(1))
        if current in wanted and re.match(r"^\s*enabled:\s*true\s*$", line):
            line = line.replace("true", "false")
        out.append(line)
    return "".join(out)


def disable_bob_servers(bob_cfg: dict, names: list) -> dict:
    """Disable the named agent-side servers, writing both flags.

    Both `disabled: true` and `enabled: false`: the two spellings coexist in
    real agent configs, and abbench treats either as off.
    """
    for name in names:
        entry = bob_cfg.get("mcpServers", {}).get(name)
        if entry is None:
            continue
        entry["disabled"] = True
        entry["enabled"] = False
    return bob_cfg


def apply(bob_path: str, lp_path: str) -> list:
    """Disable whatever would make abbench refuse. Returns what changed.

    Refuses outright if any backup is already present. "A backup exists"
    means the live files are this script's output rather than the operator's
    own, and that is worth saying plainly even when a second apply would find
    nothing left to change — otherwise `apply` reports "nothing to change" on
    an already-applied config, which reads identically to "your production
    config was already fine".
    """
    existing = [p for p in (lp_path, bob_path) if os.path.exists(backup_path(p))]
    if existing:
        raise AlreadyApplied(
            f"{', '.join(backup_path(p) for p in existing)} already present — the "
            f"sweep configuration is applied. Run `restore` first."
        )

    lp_text = open(lp_path).read()
    bob_cfg = json.load(open(bob_path))

    lp_names = [name for name, _ in _unmirrorable_proxied_servers(lp_text)]
    bob_names = _double_loaded_agent_entries(bob_cfg, lp_text)

    changed = []
    if lp_names:
        _claim_backup(lp_path)
        _write_preserving_mode(lp_path, disable_lp_servers(lp_text, lp_names))
        changed += [f"{lp_path}: disabled proxied server {n!r}" for n in lp_names]
    if bob_names:
        _claim_backup(bob_path)
        _write_preserving_mode(
            bob_path,
            json.dumps(disable_bob_servers(bob_cfg, bob_names), indent=2) + "\n")
        changed += [f"{bob_path}: disabled agent-side server {n!r}" for n in bob_names]
    return changed


def restore(bob_path: str, lp_path: str) -> list:
    restored = []
    for path in (lp_path, bob_path):
        src = backup_path(path)
        if not os.path.exists(src):
            continue
        mode = os.stat(path).st_mode & 0o777
        os.replace(src, path)
        os.chmod(path, mode)
        restored.append(path)
    if not restored:
        raise NotApplied("no .absweep-bak files found — nothing to restore")
    return restored


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("action", choices=("check", "apply", "restore"))
    ap.add_argument("--bob-config", default=abbench.DEFAULT_BOB_CFG)
    ap.add_argument("--lp-config", default=abbench.DEFAULT_LP_CFG)
    args = ap.parse_args(argv)

    if args.action == "check":
        ready, problems = check(args.bob_config, args.lp_config)
        if ready:
            print("ready — abbench's confound and native-mirror checks both pass")
            return 0
        print("not ready — abbench would refuse:")
        for p in problems:
            print(f"  - {p}")
        print("\nRun `python3 scripts/ab_sweep_config.py apply` to disable these "
              "for the sweep.")
        return 1

    if args.action == "apply":
        try:
            changed = apply(args.bob_config, args.lp_config)
        except AlreadyApplied as exc:
            print(f"Refusing — {exc}", file=sys.stderr)
            return 2
        if not changed:
            print("nothing to change — the configuration already passes both checks")
            return 0
        for c in changed:
            print(f"  {c}")
        ready, problems = check(args.bob_config, args.lp_config)
        if not ready:
            print("\nSTILL not ready:", file=sys.stderr)
            for p in problems:
                print(f"  - {p}", file=sys.stderr)
            return 1
        print("\nready — run `python3 scripts/ab_sweep_config.py restore` afterwards")
        return 0

    try:
        restored = restore(args.bob_config, args.lp_config)
    except NotApplied as exc:
        print(f"{exc}", file=sys.stderr)
        return 2
    for p in restored:
        print(f"  restored {p}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
