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

    def test_multiple_live_samples_at_one_level_are_averaged(self):
        live = self.LIVE + [
            {"layer": "live", "arm": "lazy", "ballast_tools": 0, "turns": 6, "output_tokens": 400},
        ]
        rows = abreport.combine(self.RESIDENCY, live)
        lazy0 = [r for r in rows if r["arm"] == "lazy" and r["ballast_tools"] == 0][0]
        self.assertEqual(lazy0["turns"], 5)  # mean(4, 6)
        self.assertEqual(lazy0["output_tokens"], 300)  # mean(200, 400)

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


class TestAgainstRealResidencyFixture(unittest.TestCase):
    """Sanity checks against the actual bench-results/e2e-residency-*.json
    emitted by Layer 1, joined with a hand-built (free) live file — no coins
    spent to run this test suite."""

    def setUp(self):
        paths = sorted(glob.glob(str(_ROOT / "bench-results" / "e2e-residency-*.json")))
        if not paths:
            self.skipTest("no bench-results/e2e-residency-*.json present")
        self.residency, live = abreport._load_records(paths)
        self.residency = abreport._dedupe_residency(self.residency)
        self.assertEqual(live, [])  # real file is residency-only, by construction

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


if __name__ == "__main__":
    unittest.main()
