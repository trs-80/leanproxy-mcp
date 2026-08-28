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

Honesty rules this script enforces, none of them optional:

- A point whose ballast level has its own live measurement is `measured`; a
  point that borrows another level's turn count is `derived`. That
  distinction is a column in the table and a tag on every breakeven line.
- A live run that completed but never found the expected tool
  (`succeeded: False`) is a REAL result, not noise — but it is cheap (the
  model gives up early, so it posts a low turn count), and averaging it in
  alongside successful runs makes a failing arm look like the cheapest one.
  It is excluded from every turn/output average, and the success rate that
  exclusion implies is printed next to every verdict. Below
  `MIN_SUCCESS_RATE_FOR_VERDICT`, no saves/COSTS MORE number is printed at
  all — a "saving" manufactured mostly out of early bail-outs is not a
  finding.
- A `derived` verdict is sensitivity-tested against a ±1 turn error in the
  borrowed turn count and tagged FLIPS or stable accordingly — this matters
  most for the router arm, whose residency is flat across every swept level,
  so a derived router verdict is a pure function of exactly the number being
  perturbed.
- Cross-arm comparisons are paired by task id where the two arms' samples
  share one, per the same reasoning `scripts/abbench.py`'s own
  `paired_deltas` documents: an unpaired mean over a handful of tasks whose
  per-task cost spans an order of magnitude reports task difficulty, not arm
  effect.
- An arm with no usable live data anywhere is omitted from the table — and
  the omission is announced by name, with the reason, rather than left for
  the reader to notice on their own.

`residency_tokens` (and therefore `net_tokens`, which is derived from it) is
an ESTIMATE: ceil(payload_bytes/4) from reporter.NewEstimator(), not real
tokenizer output — see the footnote the table prints. `net_tokens` is also a
structural FLOOR for the proxy arms specifically: it only counts the
schema payload as it exists before any tool is called, but a tool's full
schema (native) or the router's discovery response (router) enters the
conversation as soon as one is actually used, and is re-sent every
subsequent turn. Compare the printed `input_tokens(observed)` — real token
accounting from the live run — against `residency* x turns` and treat a wide
gap as evidence this model is optimistic for that row.

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
BASELINE_ARM = "native"

# Below this fraction of scored live runs actually finding the expected
# tool, a verdict is not a finding — it is mostly measuring how cheap it is
# to give up. 0.5 means "the model failed more often than it succeeded";
# there is no principled tighter number, but *some* floor is required so a
# 20%-success arm cannot print as the biggest saver (see review C1).
MIN_SUCCESS_RATE_FOR_VERDICT = 0.5

# How many turns of error to test a derived (borrowed) turn count against
# when checking whether a saves/COSTS MORE verdict is robust (see review
# C2). 1 is the smallest error that means anything — "is this verdict even
# stable under the single smallest plausible miss" — not a claim that a
# borrowed turn count is only ever off by exactly one.
TURN_SENSITIVITY = 1


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


def _require_keys(record: dict, keys: tuple, kind: str) -> None:
    missing = [k for k in keys if k not in record]
    if missing:
        raise ValueError(f"{kind} record missing {missing}: {record!r}")


def _numeric_ballast(record: dict, kind: str):
    """`ballast_tools` as a number, raising a named ValueError otherwise.

    Both "the key is absent" and "the key holds something that isn't a
    number" are checked here, in one place, so every caller that needs a
    ballast level to do arithmetic or distance comparisons on (the nearest-
    level search in particular — a bare `abs(k - level)` on a non-numeric
    key would raise an unlabelled TypeError deep inside a lambda) gets a
    consistent, named failure instead.
    """
    if "ballast_tools" not in record:
        raise ValueError(f"{kind} record missing 'ballast_tools': {record!r}")
    value = record["ballast_tools"]
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise ValueError(f"{kind} record has non-numeric ballast_tools={value!r}: {record!r}")
    return value


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

    Live records are deliberately NOT deduped this way (see combine()): two
    live sweep runs of the same task/arm/level are two independent samples
    of a real model, not two measurements of one deterministic quantity,
    and averaging them in is exactly the added evidence the harness wants —
    dropping one would throw away paid-for data for no honesty benefit.
    """
    by_key = {}
    for r in records:
        _require_keys(r, ("arm", "residency_tokens"), "residency")
        level = _numeric_ballast(r, "residency")
        key = (r["arm"], level)
        prev = by_key.get(key)
        if prev is None:
            by_key[key] = r
        elif prev["residency_tokens"] != r["residency_tokens"]:
            raise ValueError(
                f"conflicting residency measurements for arm={key[0]!r} "
                f"ballast_tools={key[1]!r}: {prev['residency_tokens']} vs "
                f"{r['residency_tokens']} residency_tokens — two sweep runs "
                f"disagree; investigate before trusting either"
            )
    return list(by_key.values())


# ---------------------------------------------------------------------------
# the join
# ---------------------------------------------------------------------------


def _is_scored(r: dict) -> bool:
    """Did this live run get far enough to produce turn/output metrics?

    False for a run that crashed before scoring (abbench.py's
    run_task/read_task_result exception handlers persist `succeeded: False`
    and an `error`, but no `turns`/`output_tokens` — there is nothing to
    average in).
    """
    return "turns" in r and "output_tokens" in r


def _is_successful(r: dict) -> bool:
    """Did this live run find the expected tool?

    A scored run always carries an explicit `succeeded` in real abbench.py
    output (read_task_result always sets it). `.get("succeeded", True)`
    treats an ABSENT key as success rather than failure, deliberately: the
    brief's four prescribed tests (and several of this module's own) build
    minimal live fixtures with no `succeeded` key at all, and there is no
    reason a hypothetical future live-record shape without that field
    should default to "assume everything failed". An explicit
    `succeeded: False` — the model ran to completion but never called the
    expected tool, the actual failure mode this harness exists to catch —
    is excluded regardless.
    """
    return _is_scored(r) and r.get("succeeded", True) is True


def _pick_source_level(levels: dict, target, arm: str):
    """Choose which live ballast level backs a residency point with no live
    data of its own. Returns (source_level, was_a_tie).

    Nearest-by-distance is the primary rule. On an exact tie — the swept
    residency level sits exactly between two live levels, which is not a
    contrived case: abbench.py's own documented default
    `--ballast-points "0,100"` against the real residency sweep's level 50
    hits it exactly — the pick is not arbitrary. It goes to whichever tied
    level makes the reported number HARDEST on the claim being made: the
    highest mean turn count for a proxy arm (its own worst case among the
    tied candidates), the lowest mean turn count for native (native's own
    best case — the hardest bar for a proxy arm to clear). That is a
    stated, conservative policy, not silent first-in-file-wins.
    """
    by_distance = {}
    for level in levels:
        by_distance.setdefault(abs(level - target), []).append(level)
    nearest = min(by_distance)
    candidates = by_distance[nearest]
    if len(candidates) == 1:
        return candidates[0], False

    means = {lvl: statistics.mean(s["turns"] for s in levels[lvl]) for lvl in candidates}
    if arm == BASELINE_ARM:
        chosen = min(candidates, key=lambda lvl: (means[lvl], lvl))
    else:
        chosen = max(candidates, key=lambda lvl: (means[lvl], -lvl))
    return chosen, True


def combine(residency: list, live: list) -> list:
    """Join residency and live records into net tokens per task.

        net_tokens = residency_tokens x turns + output_tokens

    Residency is measured exactly at every sweep point. Turns and output
    come from the live layer, which runs at only some points, and only from
    SUCCESSFUL, scored samples at that point (see _is_successful) — a run
    that crashed contributes nothing, and a run that completed but never
    found the expected tool is excluded from the average rather than
    quietly dragging it down, because that failure is cheap and would
    otherwise make a badly-failing arm look like the cheapest one.

    A point with usable live data at its own ballast level is `measured`; a
    point that borrows another level's turn count is `derived`
    (see _pick_source_level for how the borrowed level is chosen, including
    tie-breaking). Arms with no usable live data anywhere are omitted
    rather than guessed at.

    Each row also carries the sample count, attempted count, success rate,
    turn-count spread, an `input_tokens` cross-check where the live records
    have it, and the raw successful `samples` themselves (carrying each
    sample's `task` id) — the last so a caller can compute a same-task-
    paired delta between two arms' rows rather than subtracting two
    independently-averaged means.
    """
    attempted = {}  # arm -> level -> ALL live records at that level (scored or crashed)
    for r in live:
        _require_keys(r, ("arm",), "live")
        level = _numeric_ballast(r, "live")
        attempted.setdefault(r["arm"], {}).setdefault(level, []).append(r)

    usable = {}  # arm -> level -> successful, scored records only
    for arm, levels in attempted.items():
        for level, records in levels.items():
            good = [r for r in records if _is_successful(r)]
            if good:
                usable.setdefault(arm, {})[level] = good

    rows = []
    for res in residency:
        _require_keys(res, ("arm", "residency_tokens"), "residency")
        arm = res["arm"]
        level = _numeric_ballast(res, "residency")
        if arm not in usable:
            continue

        levels = usable[arm]
        if level in levels:
            source_level, origin, tie = level, "measured", False
        else:
            source_level, tie = _pick_source_level(levels, level, arm)
            origin = "derived"

        samples = levels[source_level]
        attempted_n = len(attempted[arm][source_level])
        turns_vals = [s["turns"] for s in samples]
        output_vals = [s["output_tokens"] for s in samples]
        input_vals = [s["input_tokens"] for s in samples if "input_tokens" in s]
        turns = statistics.mean(turns_vals)
        output = statistics.mean(output_vals)

        rows.append({
            "arm": arm,
            "ballast_tools": level,
            "origin": origin,
            "tie_broken": tie,
            "source_level": source_level,
            "residency_tokens": res["residency_tokens"],
            "turns": turns,
            "turns_min": min(turns_vals),
            "turns_max": max(turns_vals),
            "output_tokens": output,
            "n": len(samples),
            "attempted": attempted_n,
            "success_rate": (len(samples) / attempted_n) if attempted_n else None,
            "input_tokens_observed": statistics.mean(input_vals) if input_vals else None,
            "net_tokens": res["residency_tokens"] * turns + output,
            "samples": samples,
        })
    return rows


# ---------------------------------------------------------------------------
# cross-arm comparison
# ---------------------------------------------------------------------------


def _task_base(task_field):
    """Strip the "@<ballast_tools>" suffix `abbench.py` tags task keys with,
    so the same fixture task run at two different ballast levels still
    pairs on its underlying id."""
    if not task_field:
        return task_field
    return task_field.rsplit("@", 1)[0] if "@" in task_field else task_field


def _sample_net(residency_tokens, sample: dict, turn_delta: int = 0):
    """One sample's net_tokens, optionally with its turn count perturbed.

    `turn_delta` is meant to be applied only to a sample drawn from a
    `derived` row — see _paired_delta's `arm_turn_delta`/`native_turn_delta`
    parameters, which apply it conditionally on `origin`. This helper
    itself does not know or care which; it just adds the delta if asked.
    """
    turns = max(sample["turns"] + turn_delta, 0) if turn_delta else sample["turns"]
    return residency_tokens * turns + sample["output_tokens"]


def _row_net(row: dict, turn_delta: int = 0):
    """`row`'s own aggregate net_tokens, optionally with its (mean) turn
    count perturbed — used only by _paired_delta's unpaired fallback, where
    there is no per-task sample to perturb individually. A no-op for a
    `measured` row: its turn count is real, not a guess.
    """
    if row.get("origin") != "derived" or not turn_delta:
        return row["net_tokens"]
    turns = max(row["turns"] + turn_delta, 0)
    return row["residency_tokens"] * turns + row["output_tokens"]


def _paired_delta(arm_row: dict, native_row: dict, arm_turn_delta: int = 0, native_turn_delta: int = 0):
    """Mean per-task net-token delta (arm - native), paired by task id, and
    how many tasks paired.

    abbench.py's own `paired_deltas` documents why pairing matters here:
    per-task cost spans an order of magnitude, so an unpaired mean over a
    handful of tasks reports task difficulty, not arm effect — and it gets
    worse once combine() excludes some samples (crashed, or
    succeeded=False): if that drops one task from one arm but not the
    other, an unpaired mean compares different task sets entirely.

    Falls back to the unpaired row-level delta — communicated to the
    caller via a returned pair count of 0, not hidden — when the two rows'
    samples share no task id at all, e.g. each arm's derived turn count was
    borrowed from a different live ballast level with a disjoint task set.

    `arm_turn_delta`/`native_turn_delta` perturb every paired sample's turn
    count on that side by the given amount, but ONLY if that side's row is
    `derived` — a `measured` turn count is real, not a guess, so there is
    nothing to perturb it against. This is how _verdict_flips tests the
    robustness of exactly the number this function prints, rather than a
    coarser row-level average that could disagree with it.
    """
    arm_by_task = {_task_base(s["task"]): s for s in arm_row["samples"] if "task" in s}
    native_by_task = {_task_base(s["task"]): s for s in native_row["samples"] if "task" in s}
    shared = sorted(set(arm_by_task) & set(native_by_task))
    if not shared:
        return _row_net(arm_row, arm_turn_delta) - _row_net(native_row, native_turn_delta), 0

    a_delta = arm_turn_delta if arm_row.get("origin") == "derived" else 0
    n_delta = native_turn_delta if native_row.get("origin") == "derived" else 0
    deltas = []
    for t in shared:
        a, n = arm_by_task[t], native_by_task[t]
        a_net = _sample_net(arm_row["residency_tokens"], a, a_delta)
        n_net = _sample_net(native_row["residency_tokens"], n, n_delta)
        deltas.append(a_net - n_net)
    return statistics.mean(deltas), len(shared)


def _verdict_flips(arm_row: dict, native_row: dict, spread: int = TURN_SENSITIVITY) -> bool:
    """Would a +-`spread`-turn error in either row's borrowed turn count flip
    saves vs. COSTS MORE?

    Perturbs the SAME paired, per-task quantity _paired_delta computes for
    the printed verdict (not a coarser row-level net_tokens average) — a
    sensitivity check that describes a different number than the one on
    screen would be worse than no check at all. `_paired_delta` itself only
    perturbs a side when that side's row is `derived`, so a measured/
    measured comparison always reports stable: there is no turn count to be
    wrong about. This matters most for the router arm: its residency is
    flat across every swept ballast level, so a derived router row's
    net_tokens is a pure function of exactly the number this perturbs
    (review C2).
    """
    base_delta, _ = _paired_delta(arm_row, native_row)
    base_costs_more = base_delta > 0
    for da in range(-spread, spread + 1):
        for dn in range(-spread, spread + 1):
            if da == 0 and dn == 0:
                continue
            delta, _ = _paired_delta(arm_row, native_row, da, dn)
            if (delta > 0) != base_costs_more:
                return True
    return False


# ---------------------------------------------------------------------------
# reporting
# ---------------------------------------------------------------------------


ESTIMATE_FOOTNOTE = (
    "* residency_tokens is an ESTIMATE — ceil(payload_bytes/4) from "
    "reporter.NewEstimator(), not real tokenizer output. Punctuation-dense "
    "JSON tokenises worse than 4 chars/token. net_tokens is derived from "
    "this estimate, so treat both columns as directional, not exact."
)

UNDERSTATEMENT_FOOTNOTE = (
    "* net_tokens is a FLOOR for router and lazy, not a ceiling: "
    "residency_tokens is the tool-schema payload before any tool is called. "
    "Once a tool is actually used, its full schema (native) or the router's "
    "discovery response (router) enters the conversation as tool-result "
    "content and is re-sent every subsequent turn — this model does not "
    "count that growth for the proxy arms, while native already carries "
    "every schema from turn one. Compare each row's 'input_tokens observed' "
    "(real token accounting from the live run) against 'residency* x turns' "
    "and treat a wide gap as evidence this model is optimistic for that row."
)


def _print_table(rows: list) -> None:
    print(f"{'ballast':>8} {'arm':<8} {'origin':<10} {'n':>3} {'succ%':>6} "
          f"{'residency*':>10} {'turns':>6} {'net_tokens*':>12}")
    for r in rows:
        origin_label = r["origin"] + ("~tie" if r["tie_broken"] else "")
        succ = "n/a" if r["success_rate"] is None else f"{r['success_rate'] * 100:.0f}%"
        print(f"{r['ballast_tools']:>8} {r['arm']:<8} {origin_label:<10} "
              f"{r['n']:>3} {succ:>6} {r['residency_tokens']:>10} "
              f"{r['turns']:>6.1f} {r['net_tokens']:>12.0f}")

        detail = f"           turns {r['turns_min']:.0f}-{r['turns_max']:.0f} (n={r['n']}"
        if r["success_rate"] is not None:
            detail += f", {r['success_rate'] * 100:.0f}% of {r['attempted']} attempted"
        detail += ")"
        if r["input_tokens_observed"] is not None:
            modelled = r["residency_tokens"] * r["turns"]
            ratio = (r["input_tokens_observed"] / modelled) if modelled else float("inf")
            detail += (f"; input_tokens observed={r['input_tokens_observed']:.0f} vs "
                       f"residency-modelled={modelled:.0f} ({ratio:.2f}x)")
        print(detail)
    print()
    print(ESTIMATE_FOOTNOTE)
    print(UNDERSTATEMENT_FOOTNOTE)


def _print_incomplete(live: list, visible_arms: set) -> None:
    """Crashed runs for an arm that DOES appear in the table above.

    Crashes for an arm that does NOT appear (i.e. it was omitted entirely)
    are reported by _print_omitted_arms instead, as part of explaining the
    omission — listing them here too would attribute a crash to a row the
    reader cannot see (review M5).
    """
    incomplete = [r for r in live if not _is_scored(r) and r.get("arm") in visible_arms]
    if not incomplete:
        return
    print(f"\n{len(incomplete)} live run(s) for a reported arm had no usable turn/output "
          f"data (crashed before scoring) and were excluded from its averages above:")
    for r in incomplete:
        print(f"  {r.get('arm', '?')}/{r.get('task', '?')}: {r.get('error', 'no error recorded')}")


def _print_omitted_arms(residency: list, live: list, rows: list) -> None:
    """State by name every arm that had residency data but produced no row.

    Required by the brief ("omit rather than guess"), but omission with no
    explanation is indistinguishable from a bug to a reader who did not run
    the tool (review I3) — this says which of "never run live", "every run
    crashed", or "every run completed but never found the tool" happened.
    """
    all_arms = sorted({r["arm"] for r in residency if "arm" in r})
    present = {r["arm"] for r in rows}
    missing = [a for a in all_arms if a not in present]
    if not missing:
        return

    print("\nArms with residency data but no live-backed row (omitted, not guessed at):")
    for arm in missing:
        arm_live = [r for r in live if r.get("arm") == arm]
        if not arm_live:
            print(f"  {arm}: no live data was collected for this arm at any ballast level")
            continue
        scored = [r for r in arm_live if _is_scored(r)]
        crashed = len(arm_live) - len(scored)
        successful = [r for r in scored if _is_successful(r)]
        if successful:
            # Shouldn't happen if combine() truly omitted this arm; defensive only.
            print(f"  {arm}: had successful live data that did not join any residency level")
        elif scored:
            print(f"  {arm}: {len(scored)} live run(s) completed but never found the expected "
                  f"tool (0% success), plus {crashed} that crashed — no usable turn count")
        else:
            print(f"  {arm}: all {crashed} live run(s) crashed before scoring — no usable data")


def _print_breakeven(rows: list) -> None:
    """The line this harness exists to produce: does each proxy arm save net
    tokens per task versus native, at which ballast level, stated plainly
    enough — including where the claim is fragile — for a reader who never
    ran the sweep to trust it or distrust it correctly.
    """
    by_level = {}
    for r in rows:
        by_level.setdefault(r["ballast_tools"], {})[r["arm"]] = r

    print("\nBreakeven (net tokens per task, proxy arm vs native; paired by task id where possible):")
    printed = False
    first_save = {arm: None for arm in PROXY_ARMS}

    for level in sorted(by_level):
        native = by_level[level].get(BASELINE_ARM)
        if native is None:
            continue
        for arm in PROXY_ARMS:
            got = by_level[level].get(arm)
            if got is None:
                continue
            printed = True

            unreliable = []
            if got["success_rate"] is not None and got["success_rate"] < MIN_SUCCESS_RATE_FOR_VERDICT:
                unreliable.append(f"{arm} succeeded {got['success_rate'] * 100:.0f}%")
            if native["success_rate"] is not None and native["success_rate"] < MIN_SUCCESS_RATE_FOR_VERDICT:
                unreliable.append(f"native succeeded {native['success_rate'] * 100:.0f}%")
            if unreliable:
                print(f"  ballast={level:<4} {arm:<7} UNRELIABLE — {'; '.join(unreliable)} "
                      f"(below the {MIN_SUCCESS_RATE_FOR_VERDICT * 100:.0f}% success threshold — "
                      f"mostly gave up rather than completed the task; no verdict printed)")
                continue

            delta, pairs = _paired_delta(got, native)
            verdict = "saves" if delta < 0 else "COSTS MORE"
            origin = "measured" if got["origin"] == native["origin"] == "measured" else "derived"
            flip = _verdict_flips(got, native) if origin == "derived" else False

            tags = [origin]
            tags.append(f"paired n={pairs}" if pairs else "unpaired")
            if origin == "derived":
                tags.append("FLIPS under +-1 turn error" if flip else "stable under +-1 turn error")
            if arm == "router" and origin == "derived":
                tags.append("router residency is flat: this verdict IS the borrowed turn count")

            print(f"  ballast={level:<4} {arm:<7} {verdict} {abs(delta):>9.0f} "
                  f"tok/task vs native  [{', '.join(tags)}]")

            if verdict == "saves" and first_save[arm] is None:
                first_save[arm] = (level, flip)

    if not printed:
        print("  (no ballast level has both a native and a proxy-arm data point)")
        return

    print("\nFirst breakeven per arm:")
    for arm in PROXY_ARMS:
        if first_save[arm] is None:
            print(f"  {arm}: never saves net tokens vs native across the swept range")
            continue
        level, flip = first_save[arm]
        caveat = " (fragile: this verdict flips under a +-1 turn error)" if flip else ""
        print(f"  {arm}: first saves net tokens vs native at ballast_tools={level}{caveat}")


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
    visible_arms = {r["arm"] for r in rows}
    _print_table(rows)
    _print_incomplete(live, visible_arms)
    _print_omitted_arms(residency, live, rows)
    _print_breakeven(rows)
    return 0


if __name__ == "__main__":
    sys.exit(main())
