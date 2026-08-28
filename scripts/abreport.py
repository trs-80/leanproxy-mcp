#!/usr/bin/env python3
"""abreport — combine the residency sweep and the live A/B run into one curve.

    python3 scripts/abreport.py bench-results/e2e-residency-*.json \
                                bench-results/e2e-live-*.json

Layer 1 (tests/bench/e2e/residency_test.go, via `make bench-e2e`) measures
tool-schema residency exactly at every sweep point, for free. Layer 2
(scripts/abbench.py, via `make bench-e2e-live`) measures how many turns a
real model takes and whether it finds the tool it needs, but only at the
handful of ballast levels someone actually paid coins to run live. This
script is Layer 3: it joins the two into net tokens per task, and prints the
breakeven point — the ballast level at which each proxy arm starts costing
fewer tokens per task than native — which is the answer this whole harness
was built to produce.

A point whose ballast level has its own live measurement is `measured`; a
point that borrows another level's turn count is `derived`. That distinction
is never hidden — it is a column in the table and a tag on every breakeven
line. An arm with no usable live data anywhere is omitted rather than
guessed at.

`residency_tokens` (and therefore `net_tokens`, which is derived from it) is
an ESTIMATE: ceil(payload_bytes/4) from reporter.NewEstimator(), not real
tokenizer output. Punctuation-dense JSON tokenises worse than 4 chars/token.
Treat both columns as directional, not exact — see the footnote the table
prints.

Reads result files only. Never writes to ~/.bob/ or ~/.config/, and spends
no money — there is nothing here to gate behind LEANPROXY_AB_LIVE.

Python 3.9+, standard library only.
"""

from __future__ import annotations

import argparse
import json
import statistics
import sys

PROXY_ARMS = ("router", "lazy")


# ---------------------------------------------------------------------------
# loading and classification
# ---------------------------------------------------------------------------


def _load_records(paths: list) -> tuple:
    """Load every given JSON result file and split records by their own `layer`.

    Files arrive as one flat list of paths: the shell expands
    `e2e-residency-*.json` and `e2e-live-*.json` into their own matching
    filenames and hands this script the concatenation, with no marker
    showing where one glob's matches end and the other's begin — and
    residency accumulates a new timestamped file on every `make bench-e2e`
    run, so more than one file per glob is the normal case, not an edge
    case. Classifying by each record's own `layer` field (not by filename
    or by argv position) is the only classification that survives that.
    """
    residency, live = [], []
    for path in paths:
        with open(path) as fh:
            records = json.load(fh)
        if not isinstance(records, list):
            raise ValueError(f"{path}: expected a JSON array of records, got {type(records).__name__}")
        for r in records:
            layer = r.get("layer")
            if layer == "residency":
                residency.append(r)
            elif layer == "live":
                live.append(r)
            else:
                raise ValueError(f"{path}: record has unrecognised layer {layer!r}: {r!r}")
    return residency, live


def _dedupe_residency(records: list) -> list:
    """Collapse repeated residency measurements from multiple sweep-run files.

    Layer 1 is exact and deterministic at every point, and nothing deletes
    old `e2e-residency-*.json` files, so re-running `make bench-e2e` a few
    times over a session (as happened during this harness's own
    development — three identical files currently sit in bench-results/)
    produces exact duplicates of the same (arm, ballast_tools) point.
    Silently keeping every duplicate would multiply that point's weight
    wherever it's later aggregated; picking one file arbitrarily could hide
    a real regression between two runs. So: identical duplicates collapse
    to one row, but a *conflicting* duplicate (same key, different
    residency_tokens) raises rather than silently picking a winner.
    """
    by_key = {}
    for r in records:
        for key in ("arm", "ballast_tools", "residency_tokens"):
            if key not in r:
                raise ValueError(f"residency record missing {key!r}: {r!r}")
        dkey = (r["arm"], r["ballast_tools"])
        prev = by_key.get(dkey)
        if prev is None:
            by_key[dkey] = r
        elif prev["residency_tokens"] != r["residency_tokens"]:
            raise ValueError(
                f"conflicting residency measurements for arm={dkey[0]!r} "
                f"ballast_tools={dkey[1]!r}: {prev['residency_tokens']} vs "
                f"{r['residency_tokens']} residency_tokens — two sweep runs "
                f"disagree; investigate before trusting either"
            )
    return list(by_key.values())


# ---------------------------------------------------------------------------
# the join
# ---------------------------------------------------------------------------


def combine(residency: list, live: list) -> list:
    """Join residency and live records into net tokens per task.

        net_tokens = residency_tokens x turns + output_tokens

    Residency is measured exactly at every sweep point. Turns and output
    come from the live layer, which runs at only some points. A point with
    live data at its own ballast level is `measured`; a point that borrows
    another level's turn count is `derived`. Arms with no usable live data
    anywhere are omitted rather than guessed at.

    A live record with no `turns`/`output_tokens` (a run that crashed
    before it could be scored — see abbench.py's `run_task`/
    `read_task_result` exception handlers, which record `succeeded: False`
    and an `error` but no token/turn metrics) contributes nothing to the
    join: it cannot be averaged into a turn count that doesn't exist. If
    that leaves an (arm, ballast_tools) point with no usable samples at
    all, it is exactly as unusable as a point with no live data.
    """
    by_arm = {}
    for r in live:
        for key in ("arm", "ballast_tools"):
            if key not in r:
                raise ValueError(f"live record missing {key!r}: {r!r}")
        if "turns" not in r or "output_tokens" not in r:
            continue  # crashed before scoring — no metrics to contribute
        by_arm.setdefault(r["arm"], {}).setdefault(r["ballast_tools"], []).append(r)

    rows = []
    for res in residency:
        for key in ("arm", "ballast_tools", "residency_tokens"):
            if key not in res:
                raise ValueError(f"residency record missing {key!r}: {res!r}")

        arm = res["arm"]
        if arm not in by_arm:
            continue

        level = res["ballast_tools"]
        levels = by_arm[arm]
        if level in levels:
            samples, origin = levels[level], "measured"
        else:
            # Borrow the arm's nearest measured level. Turn count is a
            # property of how the model works, not of ballast size, so
            # this is a defensible carry-over — but it is labelled, never
            # silent. Ties resolve to whichever level `live` listed first.
            nearest = min(levels, key=lambda k: abs(k - level))
            samples, origin = levels[nearest], "derived"

        turns = statistics.mean(s["turns"] for s in samples)
        output = statistics.mean(s["output_tokens"] for s in samples)

        rows.append({
            "arm": arm,
            "ballast_tools": level,
            "origin": origin,
            "residency_tokens": res["residency_tokens"],
            "turns": turns,
            "output_tokens": output,
            "net_tokens": res["residency_tokens"] * turns + output,
        })
    return rows


# ---------------------------------------------------------------------------
# reporting
# ---------------------------------------------------------------------------


ESTIMATE_FOOTNOTE = (
    "* residency_tokens is an ESTIMATE — ceil(payload_bytes/4) from "
    "reporter.NewEstimator(), not real tokenizer output. Punctuation-dense "
    "JSON tokenises worse than 4 chars/token. net_tokens is derived from "
    "this estimate, so treat both columns as directional, not exact."
)


def _print_table(rows: list) -> None:
    print(f"{'ballast':>8} {'arm':<8} {'origin':<9} {'residency*':>10} {'turns':>6} {'net_tokens*':>12}")
    for r in rows:
        print(f"{r['ballast_tools']:>8} {r['arm']:<8} {r['origin']:<9} "
              f"{r['residency_tokens']:>10} {r['turns']:>6.1f} {r['net_tokens']:>12.0f}")
    print()
    print(ESTIMATE_FOOTNOTE)


def _print_incomplete(live: list) -> None:
    incomplete = [r for r in live if "turns" not in r or "output_tokens" not in r]
    if not incomplete:
        return
    print(f"\n{len(incomplete)} live run(s) had no usable turn/output data "
          f"(crashed before scoring) and were excluded from the averages above:")
    for r in incomplete:
        print(f"  {r.get('arm', '?')}/{r.get('task', '?')}: {r.get('error', 'no error recorded')}")


def _print_breakeven(rows: list) -> None:
    """The line this harness exists to produce: does each proxy arm save
    net tokens per task versus native, and at which ballast level, stated
    plainly enough that a reader who never ran the sweep can trust it."""
    by_level = {}
    for r in rows:
        by_level.setdefault(r["ballast_tools"], {})[r["arm"]] = r

    print("\nBreakeven (net tokens per task, proxy arm vs native):")
    printed = False
    for level in sorted(by_level):
        native = by_level[level].get("native")
        if native is None:
            continue
        for arm in PROXY_ARMS:
            got = by_level[level].get(arm)
            if got is None:
                continue
            printed = True
            delta = got["net_tokens"] - native["net_tokens"]
            verdict = "saves" if delta < 0 else "COSTS MORE"
            # If either side of the comparison borrowed its turn count from
            # another ballast level, the comparison itself is derived, not
            # measured — that has to travel with the verdict, not just live
            # in the table above it.
            origin = "measured" if got["origin"] == native["origin"] == "measured" else "derived"
            print(f"  ballast={level:<4} {arm:<7} {verdict} {abs(delta):>9.0f} "
                  f"tok/task vs native  [{origin}]")
    if not printed:
        print("  (no ballast level has both a native and a proxy-arm data point)")


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument(
        "files", nargs="+",
        help="e2e-residency-*.json and/or e2e-live-*.json result files")
    args = ap.parse_args(argv)

    residency, live = _load_records(args.files)
    residency = _dedupe_residency(residency)

    if not residency:
        print("no residency records found in the given files", file=sys.stderr)
        return 1
    if not live:
        print("no live records found in the given files — nothing to combine "
              "(pass an e2e-live-*.json from `make bench-e2e-live`)", file=sys.stderr)
        return 1

    rows = combine(residency, live)
    if not rows:
        print("no arm has both residency and usable live data — nothing to report", file=sys.stderr)
        return 1

    rows.sort(key=lambda r: (r["ballast_tools"], r["arm"]))
    _print_table(rows)
    _print_incomplete(live)
    _print_breakeven(rows)
    return 0


if __name__ == "__main__":
    sys.exit(main())
