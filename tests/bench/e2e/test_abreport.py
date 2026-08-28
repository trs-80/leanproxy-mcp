"""Tests for scripts/abreport.py."""

import contextlib
import glob
import importlib.util
import io
import json
import os
import pathlib
import tempfile
import unittest

_ROOT = pathlib.Path(__file__).resolve().parents[3]
_FIXTURES = pathlib.Path(__file__).resolve().parent / "fixtures"
_SPEC = importlib.util.spec_from_file_location(
    "abreport",
    _ROOT / "scripts" / "abreport.py",
)
abreport = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(abreport)


class TestCombine(unittest.TestCase):
    RESIDENCY = [
        {"layer": "residency", "arm": "lazy", "ballast_tools": 0, "residency_tokens": 500},
        {"layer": "residency", "arm": "lazy", "ballast_tools": 100, "residency_tokens": 3000},
        {"layer": "residency", "arm": "native", "ballast_tools": 0, "residency_tokens": 900},
        {"layer": "residency", "arm": "native", "ballast_tools": 100, "residency_tokens": 12000},
    ]
    LIVE = [
        {"layer": "live", "arm": "lazy", "ballast_tools": 0, "turns": 4, "output_tokens": 200},
        {"layer": "live", "arm": "native", "ballast_tools": 0, "turns": 3, "output_tokens": 150},
    ]

    # -- the brief's four prescribed tests, verbatim --

    def test_measured_points_use_real_turn_counts(self):
        rows = abreport.combine(self.RESIDENCY, self.LIVE)
        lazy0 = [r for r in rows if r["arm"] == "lazy" and r["ballast_tools"] == 0][0]
        self.assertEqual(lazy0["origin"], "measured")
        self.assertEqual(lazy0["net_tokens"], 500 * 4 + 200)

    def test_points_without_live_data_are_marked_derived(self):
        rows = abreport.combine(self.RESIDENCY, self.LIVE)
        lazy100 = [r for r in rows if r["arm"] == "lazy" and r["ballast_tools"] == 100][0]
        self.assertEqual(lazy100["origin"], "derived")

    def test_derived_points_reuse_the_arms_measured_turn_count(self):
        rows = abreport.combine(self.RESIDENCY, self.LIVE)
        lazy100 = [r for r in rows if r["arm"] == "lazy" and r["ballast_tools"] == 100][0]
        self.assertEqual(lazy100["net_tokens"], 3000 * 4 + 200)

    def test_arm_with_no_live_data_at_all_is_skipped(self):
        rows = abreport.combine(self.RESIDENCY, [])
        self.assertEqual(rows, [])

    # -- additional coverage --

    def test_derived_point_picks_nearest_live_level_not_first(self):
        residency = self.RESIDENCY + [
            {"layer": "residency", "arm": "lazy", "ballast_tools": 90, "residency_tokens": 2700},
        ]
        live = self.LIVE + [
            {"layer": "live", "arm": "lazy", "ballast_tools": 100, "turns": 10, "output_tokens": 50},
        ]
        rows = abreport.combine(residency, live)
        lazy90 = [r for r in rows if r["arm"] == "lazy" and r["ballast_tools"] == 90][0]
        # 90 is nearer to the level=100 sample (turns=10) than level=0 (turns=4).
        self.assertEqual(lazy90["origin"], "derived")
        self.assertEqual(lazy90["turns"], 10)
        self.assertFalse(lazy90["tie_broken"])

    def test_multiple_live_samples_at_one_level_are_averaged(self):
        live = self.LIVE + [
            {"layer": "live", "arm": "lazy", "ballast_tools": 0, "turns": 6, "output_tokens": 400},
        ]
        rows = abreport.combine(self.RESIDENCY, live)
        lazy0 = [r for r in rows if r["arm"] == "lazy" and r["ballast_tools"] == 0][0]
        self.assertEqual(lazy0["turns"], 5)  # mean(4, 6)
        self.assertEqual(lazy0["output_tokens"], 300)  # mean(200, 400)
        self.assertEqual(lazy0["n"], 2)
        self.assertEqual(lazy0["turns_min"], 4)
        self.assertEqual(lazy0["turns_max"], 6)

    def test_incomplete_live_record_is_excluded_from_the_join(self):
        """A run that crashed before scoring (abbench's run_task/read_task_result
        except-branches) has succeeded=False and an error, but no turns or
        output_tokens — it must not poison the average or crash combine()."""
        live = self.LIVE + [
            {"layer": "live", "arm": "lazy", "ballast_tools": 0, "task": "x@0",
             "succeeded": False, "error": "timeout"},
        ]
        rows = abreport.combine(self.RESIDENCY, live)
        lazy0 = [r for r in rows if r["arm"] == "lazy" and r["ballast_tools"] == 0][0]
        self.assertEqual(lazy0["turns"], 4)  # unaffected by the crashed record
        self.assertEqual(lazy0["n"], 1)
        self.assertEqual(lazy0["attempted"], 2)  # the crash still counts as an attempt
        self.assertAlmostEqual(lazy0["success_rate"], 0.5)

    def test_arm_with_only_incomplete_live_records_is_skipped(self):
        live = [
            {"layer": "live", "arm": "lazy", "ballast_tools": 0, "task": "x@0",
             "succeeded": False, "error": "timeout"},
        ]
        rows = abreport.combine(self.RESIDENCY, live)
        self.assertEqual(rows, [])

    def test_residency_record_missing_required_key_raises(self):
        bad = [{"layer": "residency", "arm": "lazy", "residency_tokens": 500}]  # no ballast_tools
        with self.assertRaises(ValueError):
            abreport.combine(bad, self.LIVE)

    def test_live_record_missing_required_key_raises(self):
        bad = [{"layer": "live", "arm": "lazy", "turns": 4, "output_tokens": 200}]  # no ballast_tools
        with self.assertRaises(ValueError):
            abreport.combine(self.RESIDENCY, bad)

    def test_non_numeric_ballast_tools_raises_a_named_valueerror(self):
        """Regression (review M3): this used to be a bare TypeError from
        inside the nearest-level lambda, not a named, fail-closed ValueError."""
        bad = [{"layer": "live", "arm": "lazy", "ballast_tools": "many",
                "turns": 4, "output_tokens": 200}]
        with self.assertRaises(ValueError):
            abreport.combine(self.RESIDENCY, bad)

    # -- C1: succeeded=False must not be averaged in --

    def test_succeeded_false_record_is_excluded_even_though_scored(self):
        """Regression (review C1): a run that completed but never found the
        expected tool carries real turns/output_tokens alongside
        succeeded=False, and used to be averaged in — making a failing arm
        look cheapest, since giving up is fast."""
        live = [
            {"layer": "live", "arm": "lazy", "ballast_tools": 0, "turns": 4,
             "output_tokens": 200, "succeeded": True},
            {"layer": "live", "arm": "lazy", "ballast_tools": 0, "turns": 1,
             "output_tokens": 20, "succeeded": False},
            {"layer": "live", "arm": "native", "ballast_tools": 0, "turns": 3,
             "output_tokens": 150, "succeeded": True},
        ]
        rows = abreport.combine(self.RESIDENCY, live)
        lazy0 = [r for r in rows if r["arm"] == "lazy" and r["ballast_tools"] == 0][0]
        self.assertEqual(lazy0["turns"], 4)  # the failed run's turns=1 must not pull this down
        self.assertEqual(lazy0["n"], 1)
        self.assertEqual(lazy0["attempted"], 2)
        self.assertAlmostEqual(lazy0["success_rate"], 0.5)

    def test_arm_all_succeeded_false_at_a_level_falls_back_or_is_skipped(self):
        live = [
            {"layer": "live", "arm": "lazy", "ballast_tools": 0, "turns": 1,
             "output_tokens": 20, "succeeded": False},
        ]
        rows = abreport.combine(self.RESIDENCY, live)
        self.assertEqual(rows, [])  # no successful sample anywhere for lazy

    def test_absent_succeeded_key_defaults_to_success(self):
        """Backward compatibility: fixtures (and the brief's own prescribed
        ones) that never set `succeeded` at all must still be usable."""
        live = [{"layer": "live", "arm": "lazy", "ballast_tools": 0, "turns": 4, "output_tokens": 200}]
        rows = abreport.combine(self.RESIDENCY, live)
        lazy0 = [r for r in rows if r["arm"] == "lazy" and r["ballast_tools"] == 0][0]
        self.assertEqual(lazy0["n"], 1)
        self.assertEqual(lazy0["success_rate"], 1.0)

    # -- I1: tie-break policy --

    def test_tie_break_picks_worse_case_for_a_proxy_arm(self):
        residency = [{"layer": "residency", "arm": "lazy", "ballast_tools": 50, "residency_tokens": 1000}]
        live = [
            {"layer": "live", "arm": "lazy", "ballast_tools": 0, "turns": 2, "output_tokens": 50},
            {"layer": "live", "arm": "lazy", "ballast_tools": 100, "turns": 6, "output_tokens": 50},
        ]
        rows = abreport.combine(residency, live)
        self.assertEqual(rows[0]["turns"], 6)  # the more expensive tied candidate
        self.assertTrue(rows[0]["tie_broken"])

    def test_tie_break_picks_best_case_for_native(self):
        residency = [{"layer": "residency", "arm": "native", "ballast_tools": 50, "residency_tokens": 1000}]
        live = [
            {"layer": "live", "arm": "native", "ballast_tools": 0, "turns": 2, "output_tokens": 50},
            {"layer": "live", "arm": "native", "ballast_tools": 100, "turns": 6, "output_tokens": 50},
        ]
        rows = abreport.combine(residency, live)
        self.assertEqual(rows[0]["turns"], 2)  # native's cheapest tied candidate
        self.assertTrue(rows[0]["tie_broken"])


class TestPairedDelta(unittest.TestCase):
    def test_pairs_on_shared_task_id_ignoring_the_ballast_suffix(self):
        arm_row = {"residency_tokens": 100, "samples": [
            {"task": "t1@2", "turns": 2, "output_tokens": 10},
            {"task": "t2@2", "turns": 4, "output_tokens": 10},
        ]}
        native_row = {"residency_tokens": 50, "samples": [
            {"task": "t1@2", "turns": 1, "output_tokens": 5},
            {"task": "t2@2", "turns": 1, "output_tokens": 5},
        ]}
        delta, pairs = abreport._paired_delta(arm_row, native_row)
        self.assertEqual(pairs, 2)
        # t1: (100*2+10) - (50*1+5) = 210-55 = 155; t2: (100*4+10)-(50*1+5)=410-55=355
        self.assertAlmostEqual(delta, (155 + 355) / 2)

    def test_turn_delta_only_perturbs_the_derived_side(self):
        """Regression: _verdict_flips must perturb the SAME paired quantity
        _paired_delta prints, not a coarser row-level average that could
        disagree with it. A `measured` side must never move, even when a
        non-zero delta is passed for it."""
        derived_arm = {"origin": "derived", "residency_tokens": 100, "samples": [
            {"task": "t1@2", "turns": 2, "output_tokens": 10},
        ]}
        measured_native = {"origin": "measured", "residency_tokens": 50, "samples": [
            {"task": "t1@2", "turns": 1, "output_tokens": 5},
        ]}
        base_delta, _ = abreport._paired_delta(derived_arm, measured_native)
        # Perturbing the derived arm's side moves the delta...
        perturbed_arm, _ = abreport._paired_delta(derived_arm, measured_native, arm_turn_delta=1)
        self.assertNotEqual(perturbed_arm, base_delta)
        # ...but perturbing the measured native's side must not, since a
        # measured turn count is real, not a guess.
        perturbed_native, _ = abreport._paired_delta(derived_arm, measured_native, native_turn_delta=1)
        self.assertEqual(perturbed_native, base_delta)

    def test_verdict_flips_uses_the_paired_path_when_tasks_are_shared(self):
        """A case that is stable at the row-aggregate level (a naive
        net_tokens-vs-net_tokens perturbation would never cross zero) but
        where individual paired tasks are close enough to the boundary that
        perturbing per-task turn counts does flip some of them — proving
        _verdict_flips is really exercising the paired path, not silently
        falling back to the row-level one."""
        native = {"origin": "measured", "residency_tokens": 100, "net_tokens": 210, "samples": [
            {"task": "t1@2", "turns": 1, "output_tokens": 10},
            {"task": "t2@2", "turns": 1, "output_tokens": 100},
        ]}
        router = {"origin": "derived", "residency_tokens": 100, "turns": 1, "net_tokens": 200,
                  "samples": [
                      {"task": "t1@2", "turns": 1, "output_tokens": 0},
                      {"task": "t2@2", "turns": 1, "output_tokens": 0},
                  ]}
        self.assertTrue(abreport._verdict_flips(router, native))

    def test_falls_back_to_unpaired_delta_when_no_shared_task_ids(self):
        arm_row = {"residency_tokens": 100, "net_tokens": 500,
                   "samples": [{"task": "onlyA@2", "turns": 2, "output_tokens": 10}]}
        native_row = {"residency_tokens": 50, "net_tokens": 200,
                      "samples": [{"task": "onlyB@2", "turns": 1, "output_tokens": 5}]}
        delta, pairs = abreport._paired_delta(arm_row, native_row)
        self.assertEqual(pairs, 0)
        self.assertEqual(delta, 500 - 200)

    def test_samples_without_a_task_field_are_ignored_for_pairing(self):
        arm_row = {"residency_tokens": 100, "net_tokens": 500,
                   "samples": [{"turns": 2, "output_tokens": 10}]}
        native_row = {"residency_tokens": 50, "net_tokens": 200,
                      "samples": [{"turns": 1, "output_tokens": 5}]}
        delta, pairs = abreport._paired_delta(arm_row, native_row)
        self.assertEqual(pairs, 0)


class TestVerdictFlips(unittest.TestCase):
    """These fixtures carry no `samples` (or samples with no shared `task`
    ids), so _paired_delta takes its unpaired row-level fallback — which is
    exactly the path that perturbs the row's own mean turn count via
    _row_net. Paired-path sensitivity is covered separately below via
    TestAgainstRealResidencyFixture.test_derived_router_verdict_is_flagged_for_flip_risk."""

    def test_measured_vs_measured_never_flips(self):
        arm = {"origin": "measured", "net_tokens": 100, "residency_tokens": 10,
               "turns": 2, "output_tokens": 5, "samples": []}
        native = {"origin": "measured", "net_tokens": 200, "residency_tokens": 20,
                  "turns": 2, "output_tokens": 5, "samples": []}
        self.assertFalse(abreport._verdict_flips(arm, native))

    def test_flat_residency_derived_row_can_flip(self):
        # router-shaped: residency flat, net_tokens is purely turns*residency+output.
        native = {"origin": "measured", "net_tokens": 900, "residency_tokens": 300,
                  "turns": 3, "output_tokens": 0, "samples": []}
        router_turns2 = {"origin": "derived", "residency_tokens": 544, "turns": 2,
                         "output_tokens": 0, "net_tokens": 544 * 2, "samples": []}
        router_turns3 = {"origin": "derived", "residency_tokens": 544, "turns": 3,
                         "output_tokens": 0, "net_tokens": 544 * 3, "samples": []}
        # turns=2 -> net=1088 (COSTS MORE than native's 900); turns=3 -> net=1632
        # (also COSTS MORE) — but a +-1 perturbation around either base crosses
        # native's 900 for the turns=2 case, so that one must report a flip risk.
        self.assertTrue(abreport._verdict_flips(router_turns2, native) or
                        abreport._verdict_flips(router_turns3, native))

    def test_stable_when_perturbation_does_not_cross_native(self):
        native = {"origin": "measured", "net_tokens": 100000, "residency_tokens": 1000,
                  "turns": 100, "output_tokens": 0, "samples": []}
        arm = {"origin": "derived", "residency_tokens": 10, "turns": 2,
               "output_tokens": 0, "net_tokens": 20, "samples": []}
        self.assertFalse(abreport._verdict_flips(arm, native))


class TestLoadRecords(unittest.TestCase):
    def test_splits_by_layer_field_not_filename(self):
        with tempfile.TemporaryDirectory() as d:
            # Deliberately misleading filename: content, not name, decides.
            path = os.path.join(d, "mystery.json")
            with open(path, "w") as fh:
                json.dump([
                    {"layer": "residency", "arm": "native", "ballast_tools": 2, "residency_tokens": 500},
                    {"layer": "live", "arm": "native", "ballast_tools": 2, "turns": 3, "output_tokens": 100},
                ], fh)
            residency, live = abreport._load_records([path])
            self.assertEqual(len(residency), 1)
            self.assertEqual(len(live), 1)

    def test_merges_records_across_multiple_files(self):
        with tempfile.TemporaryDirectory() as d:
            p1 = os.path.join(d, "a.json")
            p2 = os.path.join(d, "b.json")
            with open(p1, "w") as fh:
                json.dump([{"layer": "residency", "arm": "native", "ballast_tools": 2, "residency_tokens": 500}], fh)
            with open(p2, "w") as fh:
                json.dump([{"layer": "residency", "arm": "native", "ballast_tools": 4, "residency_tokens": 900}], fh)
            residency, live = abreport._load_records([p1, p2])
            self.assertEqual(len(residency), 2)

    def test_unrecognised_layer_raises(self):
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "bad.json")
            with open(path, "w") as fh:
                json.dump([{"layer": "bogus", "arm": "native"}], fh)
            with self.assertRaises(ValueError):
                abreport._load_records([path])

    def test_non_list_json_raises(self):
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "bad.json")
            with open(path, "w") as fh:
                json.dump({"not": "a list"}, fh)
            with self.assertRaises(ValueError):
                abreport._load_records([path])


class TestDedupeResidency(unittest.TestCase):
    def test_identical_duplicates_collapse_to_one(self):
        rec = {"layer": "residency", "arm": "router", "ballast_tools": 2, "residency_tokens": 544}
        out = abreport._dedupe_residency([rec, dict(rec)])
        self.assertEqual(len(out), 1)

    def test_conflicting_duplicates_raise(self):
        rec1 = {"layer": "residency", "arm": "router", "ballast_tools": 2, "residency_tokens": 544}
        rec2 = {"layer": "residency", "arm": "router", "ballast_tools": 2, "residency_tokens": 999}
        with self.assertRaises(ValueError):
            abreport._dedupe_residency([rec1, rec2])

    def test_distinct_keys_both_kept(self):
        rec1 = {"layer": "residency", "arm": "router", "ballast_tools": 2, "residency_tokens": 544}
        rec2 = {"layer": "residency", "arm": "lazy", "ballast_tools": 2, "residency_tokens": 141}
        out = abreport._dedupe_residency([rec1, rec2])
        self.assertEqual(len(out), 2)

    def test_non_numeric_ballast_tools_raises(self):
        rec = {"layer": "residency", "arm": "router", "ballast_tools": "two", "residency_tokens": 544}
        with self.assertRaises(ValueError):
            abreport._dedupe_residency([rec])


class TestMain(unittest.TestCase):
    RESIDENCY = [
        {"layer": "residency", "origin": "measured", "arm": "router",
         "ballast_servers": 2, "ballast_tools": 2, "payload_bytes": 2174, "residency_tokens": 544},
        {"layer": "residency", "origin": "measured", "arm": "lazy",
         "ballast_servers": 2, "ballast_tools": 2, "payload_bytes": 563, "residency_tokens": 141},
        {"layer": "residency", "origin": "measured", "arm": "native",
         "ballast_servers": 2, "ballast_tools": 2, "payload_bytes": 1377, "residency_tokens": 345},
    ]
    LIVE = [
        {"layer": "live", "origin": "measured", "arm": "router", "task": "t1@2",
         "ballast_tools": 2, "turns": 2, "input_tokens": 1000, "output_tokens": 120,
         "cache_read": 0, "cache_write": 0, "cost_usd": 0.01, "succeeded": True},
        {"layer": "live", "origin": "measured", "arm": "lazy", "task": "t1@2",
         "ballast_tools": 2, "turns": 1, "input_tokens": 500, "output_tokens": 90,
         "cache_read": 0, "cache_write": 0, "cost_usd": 0.005, "succeeded": True},
        {"layer": "live", "origin": "measured", "arm": "native", "task": "t1@2",
         "ballast_tools": 2, "turns": 1, "input_tokens": 800, "output_tokens": 100,
         "cache_read": 0, "cache_write": 0, "cost_usd": 0.008, "succeeded": True},
    ]

    def _write(self, d, name, records):
        path = os.path.join(d, name)
        with open(path, "w") as fh:
            json.dump(records, fh)
        return path

    def test_exit_zero_and_prints_table_and_breakeven(self):
        with tempfile.TemporaryDirectory() as d:
            res = self._write(d, "e2e-residency-x.json", self.RESIDENCY)
            live = self._write(d, "e2e-live-x.json", self.LIVE)
            buf = io.StringIO()
            with contextlib.redirect_stdout(buf):
                rc = abreport.main([res, live])
            self.assertEqual(rc, 0)
            out = buf.getvalue()
            self.assertIn("Breakeven", out)
            self.assertIn("ESTIMATE", out)
            self.assertIn("measured", out)
            self.assertIn("First breakeven per arm", out)  # I6

    def test_router_costs_more_than_native_at_low_ballast(self):
        """Sanity check against the measured Layer 1 result: router's fixed
        544-token floor exceeds native's 345 tokens at ballast_tools=2, and
        turns are equal here, so router must show COSTS MORE, not saves."""
        with tempfile.TemporaryDirectory() as d:
            res = self._write(d, "e2e-residency-x.json", self.RESIDENCY)
            live = self._write(d, "e2e-live-x.json", self.LIVE)
            buf = io.StringIO()
            with contextlib.redirect_stdout(buf):
                abreport.main([res, live])
            out = buf.getvalue()
            self.assertIn("COSTS MORE", out)

    def test_multiple_residency_files_are_merged_by_content_not_argv_position(self):
        with tempfile.TemporaryDirectory() as d:
            res1 = self._write(d, "e2e-residency-1.json", self.RESIDENCY[:1])
            res2 = self._write(d, "e2e-residency-2.json", self.RESIDENCY[1:])
            live = self._write(d, "e2e-live-x.json", self.LIVE)
            buf = io.StringIO()
            with contextlib.redirect_stdout(buf):
                rc = abreport.main([res1, res2, live])
            self.assertEqual(rc, 0)
            self.assertIn("router", buf.getvalue())
            self.assertIn("lazy", buf.getvalue())

    def test_no_live_files_given_exits_nonzero(self):
        with tempfile.TemporaryDirectory() as d:
            res = self._write(d, "e2e-residency-x.json", self.RESIDENCY)
            buf, err = io.StringIO(), io.StringIO()
            with contextlib.redirect_stdout(buf), contextlib.redirect_stderr(err):
                rc = abreport.main([res])
            self.assertEqual(rc, 1)

    def test_no_residency_files_given_exits_nonzero(self):
        with tempfile.TemporaryDirectory() as d:
            live = self._write(d, "e2e-live-x.json", self.LIVE)
            buf, err = io.StringIO(), io.StringIO()
            with contextlib.redirect_stdout(buf), contextlib.redirect_stderr(err):
                rc = abreport.main([live])
            self.assertEqual(rc, 1)

    def test_incomplete_live_run_is_reported_not_silently_dropped(self):
        live = self.LIVE + [
            {"layer": "live", "origin": "measured", "arm": "router", "task": "crashed@2",
             "ballast_tools": 2, "succeeded": False, "error": "no new task row appeared"},
        ]
        with tempfile.TemporaryDirectory() as d:
            res = self._write(d, "e2e-residency-x.json", self.RESIDENCY)
            live_path = self._write(d, "e2e-live-x.json", live)
            buf = io.StringIO()
            with contextlib.redirect_stdout(buf):
                abreport.main([res, live_path])
            out = buf.getvalue()
            self.assertIn("crashed before scoring", out)
            self.assertIn("no new task row appeared", out)

    # -- C1: low success rate suppresses the numeric verdict --

    def test_low_success_arm_prints_unreliable_not_a_number(self):
        live = [
            {"layer": "live", "arm": "lazy", "task": "a@2", "ballast_tools": 2,
             "turns": 1, "output_tokens": 10, "succeeded": False},
            {"layer": "live", "arm": "lazy", "task": "b@2", "ballast_tools": 2,
             "turns": 1, "output_tokens": 10, "succeeded": False},
            {"layer": "live", "arm": "lazy", "task": "c@2", "ballast_tools": 2,
             "turns": 5, "output_tokens": 90, "succeeded": True},
            {"layer": "live", "arm": "native", "task": "a@2", "ballast_tools": 2,
             "turns": 1, "output_tokens": 90, "succeeded": True},
        ]
        with tempfile.TemporaryDirectory() as d:
            res = self._write(d, "e2e-residency-x.json", self.RESIDENCY)
            live_path = self._write(d, "e2e-live-x.json", live)
            buf = io.StringIO()
            with contextlib.redirect_stdout(buf):
                abreport.main([res, live_path])
            out = buf.getvalue()
            self.assertIn("UNRELIABLE", out)
            self.assertIn("lazy succeeded 33%", out)
            # And the fragile arm must not ALSO print a numeric saves/COSTS
            # MORE line for the same (level, arm) pair.
            lazy_lines = [ln for ln in out.splitlines() if "ballast=2" in ln and " lazy " in ln]
            self.assertTrue(all("UNRELIABLE" in ln for ln in lazy_lines if "lazy" in ln))

    # -- I3: omitted arms are announced --

    def test_omitted_arm_is_announced_with_a_reason(self):
        live = [r for r in self.LIVE if r["arm"] != "router"]  # router never run live
        with tempfile.TemporaryDirectory() as d:
            res = self._write(d, "e2e-residency-x.json", self.RESIDENCY)
            live_path = self._write(d, "e2e-live-x.json", live)
            buf = io.StringIO()
            with contextlib.redirect_stdout(buf):
                abreport.main([res, live_path])
            out = buf.getvalue()
            self.assertIn("router: no live data was collected", out)
            # router must not silently appear in the breakeven table either
            self.assertNotIn("router  saves", out)
            self.assertNotIn("router  COSTS MORE", out)

    def test_omitted_arm_crash_is_not_double_counted_in_incomplete_section(self):
        """Review M5: a crash for an arm that never produces a row must be
        explained via the omission note, not also listed generically."""
        live = [
            {"layer": "live", "arm": "router", "task": "a@2", "ballast_tools": 2,
             "succeeded": False, "error": "boom"},
            {"layer": "live", "arm": "native", "task": "a@2", "ballast_tools": 2,
             "turns": 1, "output_tokens": 90, "succeeded": True},
        ]
        with tempfile.TemporaryDirectory() as d:
            res = self._write(d, "e2e-residency-x.json", self.RESIDENCY)
            live_path = self._write(d, "e2e-live-x.json", live)
            buf = io.StringIO()
            with contextlib.redirect_stdout(buf):
                abreport.main([res, live_path])
            out = buf.getvalue()
            self.assertIn("router: all 1 live run(s) crashed", out)
            self.assertNotIn("run(s) for a reported arm had no usable", out)

    # -- I4: input_tokens cross-check is printed --

    def test_input_tokens_observed_cross_check_is_printed(self):
        with tempfile.TemporaryDirectory() as d:
            res = self._write(d, "e2e-residency-x.json", self.RESIDENCY)
            live = self._write(d, "e2e-live-x.json", self.LIVE)
            buf = io.StringIO()
            with contextlib.redirect_stdout(buf):
                abreport.main([res, live])
            out = buf.getvalue()
            self.assertIn("input_tokens observed=", out)
            self.assertIn("FLOOR for router and lazy", out)


class TestAgainstRealResidencyFixture(unittest.TestCase):
    """Sanity checks against a committed copy of a real
    bench-results/e2e-residency-*.json emitted by Layer 1 (bench-results/
    itself is gitignored, so this uses a checked-in fixture — see review
    M4 — and falls back to the live directory if present, to also cover a
    fresh sweep run on the author's machine), joined with a hand-built
    (free) live file — no coins spent to run this test suite."""

    def setUp(self):
        real = sorted(glob.glob(str(_ROOT / "bench-results" / "e2e-residency-*.json")))
        paths = real or [str(_FIXTURES / "residency_sample.json")]
        self.residency, live = abreport._load_records(paths)
        self.residency = abreport._dedupe_residency(self.residency)
        self.assertEqual(live, [])  # these files are residency-only, by construction

    def test_router_is_flat_across_every_ballast_level(self):
        router_tokens = {r["residency_tokens"] for r in self.residency if r["arm"] == "router"}
        self.assertEqual(router_tokens, {544})

    def test_router_floor_exceeds_native_at_the_smallest_swept_level(self):
        by_arm_at_2 = {r["arm"]: r["residency_tokens"] for r in self.residency if r["ballast_tools"] == 2}
        self.assertGreater(by_arm_at_2["router"], by_arm_at_2["native"])

    def test_combine_with_synthetic_live_labels_matching_levels_measured(self):
        live = [
            {"layer": "live", "arm": "router", "ballast_tools": 2, "turns": 2, "output_tokens": 100},
            {"layer": "live", "arm": "native", "ballast_tools": 2, "turns": 1, "output_tokens": 90},
        ]
        rows = abreport.combine(self.residency, live)
        router2 = [r for r in rows if r["arm"] == "router" and r["ballast_tools"] == 2][0]
        self.assertEqual(router2["origin"], "measured")
        # every other swept level for 'router' has no live data at its own
        # level, so it must borrow — and be labelled as having done so.
        router_other = [r for r in rows if r["arm"] == "router" and r["ballast_tools"] != 2]
        self.assertTrue(router_other)
        self.assertTrue(all(r["origin"] == "derived" for r in router_other))

    def test_derived_router_verdict_is_flagged_for_flip_risk(self):
        """Regression (review C2): router's residency is flat, so a derived
        router row's net_tokens is entirely a function of the borrowed turn
        count — the tool must at least be ABLE to say whether that borrowed
        count is fragile."""
        live = [
            {"layer": "live", "arm": "router", "task": "a@2", "ballast_tools": 2,
             "turns": 2, "output_tokens": 100},
            {"layer": "live", "arm": "native", "task": "a@2", "ballast_tools": 2,
             "turns": 1, "output_tokens": 90},
        ]
        rows = abreport.combine(self.residency, live)
        by_level = {}
        for r in rows:
            by_level.setdefault(r["ballast_tools"], {})[r["arm"]] = r
        native8, router8 = by_level[8]["native"], by_level[8]["router"]
        self.assertEqual(router8["origin"], "derived")
        # Should not raise, and must return a definite bool either way.
        self.assertIn(abreport._verdict_flips(router8, native8), (True, False))


if __name__ == "__main__":
    unittest.main()
