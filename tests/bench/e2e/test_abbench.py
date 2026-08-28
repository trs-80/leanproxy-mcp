"""Tests for the pure functions in scripts/abbench.py."""

import importlib.util
import json
import os
import pathlib
import tempfile
import unittest

_SPEC = importlib.util.spec_from_file_location(
    "abbench",
    pathlib.Path(__file__).resolve().parents[3] / "scripts" / "abbench.py",
)
abbench = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(abbench)


class TestDetectConfound(unittest.TestCase):
    def test_flags_server_enabled_in_both_places(self):
        bob = {"mcpServers": {"context7": {"disabled": False}, "leanproxy": {"disabled": False}}}
        lp = 'servers:\n  - name: context7\n    enabled: true\n'
        problems = abbench.detect_confound(bob, lp)
        self.assertTrue(any("context7" in p for p in problems), problems)

    def test_clean_config_has_no_problems(self):
        bob = {"mcpServers": {"leanproxy": {"disabled": False}}}
        lp = 'servers:\n  - name: context7\n    enabled: true\n'
        self.assertEqual(abbench.detect_confound(bob, lp), [])

    def test_disabled_in_bob_is_not_a_confound(self):
        bob = {"mcpServers": {"context7": {"disabled": True}, "leanproxy": {"disabled": False}}}
        lp = 'servers:\n  - name: context7\n    enabled: true\n'
        self.assertEqual(abbench.detect_confound(bob, lp), [])


class TestLpEnabledServers(unittest.TestCase):
    """Covers C3 (key-order false negative) and I1 (nested-key leak), plus the
    adjacent shapes a real leanproxy_servers.yaml can contain."""

    def test_reversed_key_order_is_still_detected(self):
        lp = "servers:\n  - enabled: true\n    name: context7\n"
        self.assertEqual(abbench._lp_enabled_servers(lp), ["context7"])

    def test_nested_enabled_key_does_not_leak_into_the_item(self):
        lp = (
            "servers:\n"
            "  - name: context7\n"
            "    enabled: false\n"
            "    retry:\n"
            "      enabled: true\n"
        )
        self.assertEqual(abbench._lp_enabled_servers(lp), [])

    def test_quoted_name_is_unquoted(self):
        lp = 'servers:\n  - name: "context7"\n    enabled: true\n'
        self.assertEqual(abbench._lp_enabled_servers(lp), ["context7"])

    def test_comments_are_ignored(self):
        lp = (
            "servers:\n"
            "  # a leading comment\n"
            "  - name: context7  # inline comment\n"
            "    enabled: true  # also enabled\n"
        )
        self.assertEqual(abbench._lp_enabled_servers(lp), ["context7"])

    def test_dedent_ends_the_previous_item_block(self):
        lp = (
            "servers:\n"
            "  - name: context7\n"
            "    enabled: true\n"
            "other_top_level_key: 1\n"
        )
        self.assertEqual(abbench._lp_enabled_servers(lp), ["context7"])

    def test_second_item_does_not_leak_into_the_first(self):
        lp = (
            "servers:\n"
            "  - name: context7\n"
            "    enabled: true\n"
            "  - name: other\n"
            "    enabled: false\n"
        )
        self.assertEqual(abbench._lp_enabled_servers(lp), ["context7"])

    def test_unrecognised_shape_fails_closed(self):
        lp = "servers:\n  ???not valid yaml-ish???\n"
        with self.assertRaises(ValueError):
            abbench._lp_enabled_servers(lp)

    def test_over_indented_continuation_after_a_scalar_raises(self):
        """N1: `enabled: true` indented deeper than `name:` in the same item,
        with no key at name:'s own indent opening a block, is not legal YAML
        (real YAML rejects it as 'mapping values are not allowed here') and
        must not be silently dropped — it must raise, not return []."""
        lp = "servers:\n  - name: context7\n      enabled: true\n"
        with self.assertRaises(ValueError):
            abbench._lp_enabled_servers(lp)

    def test_deeply_nested_content_under_an_empty_valued_key_is_skipped(self):
        """The legitimate counterpart to N1: a key with an EMPTY inline value
        (`http:`) genuinely opens a nested block, and content several levels
        deeper inside it must still be skipped, not attributed to the item."""
        lp = (
            "servers:\n"
            "  - name: context7\n"
            "    enabled: true\n"
            "    http:\n"
            "      headers:\n"
            "        enabled: true\n"
            "        nested:\n"
            "          enabled: true\n"
        )
        self.assertEqual(abbench._lp_enabled_servers(lp), ["context7"])

    def test_flow_style_list_item_raises(self):
        lp = "servers:\n  - {name: context7, enabled: true}\n"
        with self.assertRaises(ValueError):
            abbench._lp_enabled_servers(lp)

    def test_flow_style_servers_value_raises_rather_than_silently_finding_nothing(self):
        """`servers: [...]` all on one line would otherwise be silently
        treated as an empty block list — a real server enabled that way
        would vanish from the confound check without a trace."""
        lp = "servers: [{name: context7, enabled: true}]\n"
        with self.assertRaises(ValueError):
            abbench._lp_enabled_servers(lp)

    def test_empty_servers_block_returns_no_names(self):
        lp = "servers:\nbouncer:\n  enabled: true\n"
        self.assertEqual(abbench._lp_enabled_servers(lp), [])

    def test_crlf_line_endings_parse_correctly(self):
        lp = "servers:\r\n  - name: context7\r\n    enabled: true\r\n"
        self.assertEqual(abbench._lp_enabled_servers(lp), ["context7"])

    def test_unquoted_hash_in_name_is_preserved_not_treated_as_comment(self):
        lp = "servers:\n  - name: conte#xt7\n    enabled: true\n"
        self.assertEqual(abbench._lp_enabled_servers(lp), ["conte#xt7"])

    def test_multi_document_yaml_scans_each_document(self):
        lp = (
            "---\n"
            "servers:\n"
            "  - name: context7\n"
            "    enabled: true\n"
            "---\n"
            "servers:\n"
            "  - name: other\n"
            "    enabled: true\n"
        )
        self.assertEqual(abbench._lp_enabled_servers(lp), ["context7", "other"])

    def test_sections_after_the_servers_block_are_not_validated(self):
        """A real leanproxy_servers.yaml has unrelated top-level sections
        (e.g. `bouncer:`) after the servers list, with their own nested
        `enabled:` keys that must not be mistaken for a server's. Only
        content inside the `servers:` block is subject to strict parsing."""
        lp = (
            'version: "1"\n'
            "servers:\n"
            "  - name: context7\n"
            "    enabled: true\n"
            "bouncer:\n"
            "  enabled: true\n"
            "  patterns:\n"
            "    - name: local-canary\n"
            '      pattern: "CANARY_[A-Z0-9]{12}"\n'
        )
        self.assertEqual(abbench._lp_enabled_servers(lp), ["context7"])


class TestArmConfig(unittest.TestCase):
    BASE = {"mcpServers": {"leanproxy": {"command": "/old/bin", "args": ["server", "run", "--stdio"]}}}

    def test_router_arm_omits_lazy_tools(self):
        cfg = abbench.arm_config("router", "/new/bin", self.BASE)
        args = cfg["mcpServers"]["leanproxy"]["args"]
        self.assertIn("--stdio", args)
        self.assertNotIn("--lazy-tools", args)

    def test_lazy_arm_includes_lazy_tools(self):
        cfg = abbench.arm_config("lazy", "/new/bin", self.BASE)
        self.assertIn("--lazy-tools", cfg["mcpServers"]["leanproxy"]["args"])

    def test_native_arm_removes_the_proxy_entirely(self):
        cfg = abbench.arm_config("native", "/new/bin", self.BASE)
        self.assertNotIn("leanproxy", cfg["mcpServers"])

    def test_does_not_mutate_base(self):
        abbench.arm_config("lazy", "/new/bin", self.BASE)
        self.assertNotIn("--lazy-tools", self.BASE["mcpServers"]["leanproxy"]["args"])


class TestConfigSwap(unittest.TestCase):
    def _write(self, path, obj):
        with open(path, "w") as fh:
            json.dump(obj, fh)

    def test_restores_on_clean_exit(self):
        with tempfile.TemporaryDirectory() as d:
            p = os.path.join(d, "mcp.json")
            self._write(p, {"original": True})
            with abbench.ConfigSwap(p, {"swapped": True}):
                with open(p) as fh:
                    self.assertEqual(json.load(fh), {"swapped": True})
            with open(p) as fh:
                self.assertEqual(json.load(fh), {"original": True})

    def test_restores_on_exception(self):
        with tempfile.TemporaryDirectory() as d:
            p = os.path.join(d, "mcp.json")
            self._write(p, {"original": True})
            with self.assertRaises(RuntimeError):
                with abbench.ConfigSwap(p, {"swapped": True}):
                    raise RuntimeError("boom")
            with open(p) as fh:
                self.assertEqual(json.load(fh), {"original": True})

    def test_enter_failure_restores_and_does_not_corrupt(self):
        """C1: a failure inside __enter__ itself (after the backup and signal
        handlers are set up, during serialisation) must not leave the real
        config truncated — __exit__ is never called when __enter__ raises, so
        __enter__ has to restore on its own way out."""

        class Unserialisable:
            pass

        with tempfile.TemporaryDirectory() as d:
            p = os.path.join(d, "mcp.json")
            self._write(p, {"original": True})

            with self.assertRaises(TypeError):
                with abbench.ConfigSwap(p, {"bad": Unserialisable()}):
                    pass

            with open(p) as fh:
                self.assertEqual(json.load(fh), {"original": True})
            self.assertFalse(os.path.exists(p + ".abbench-backup"))

    def test_refuses_when_backup_already_exists(self):
        """C2/I3: a pre-existing backup means either a prior run crashed
        without restoring, or another instance is already running. Either
        way the true original must not be clobbered — refuse loudly instead."""
        with tempfile.TemporaryDirectory() as d:
            p = os.path.join(d, "mcp.json")
            self._write(p, {"swapped": "from_prior_crashed_run"})
            backup = p + ".abbench-backup"
            self._write(backup, {"original": "true_original"})

            with self.assertRaises(RuntimeError):
                with abbench.ConfigSwap(p, {"swapped": "new_run"}):
                    pass

            with open(backup) as fh:
                self.assertEqual(json.load(fh), {"original": "true_original"})
            with open(p) as fh:
                self.assertEqual(json.load(fh), {"swapped": "from_prior_crashed_run"})

    def test_signal_handler_falls_back_to_default_when_previous_was_none(self):
        """I2: signal.signal() can return None for the previous handler. That
        must not be treated as 'nothing to restore' — fall back to SIG_DFL so
        the abbench signal handler is never left installed after the with
        block exits."""
        import unittest.mock as mock

        with tempfile.TemporaryDirectory() as d:
            p = os.path.join(d, "mcp.json")
            self._write(p, {"original": True})

            calls = []

            def fake_signal(sig, handler):
                calls.append((sig, handler))
                return None

            with mock.patch.object(abbench.signal, "signal", side_effect=fake_signal):
                with abbench.ConfigSwap(p, {"swapped": True}):
                    pass

            # 2 installs (SIGINT, SIGTERM) + 2 restores == 4 total calls.
            self.assertEqual(len(calls), 4)
            for sig, handler in calls[2:]:
                self.assertEqual(handler, abbench.signal.SIG_DFL)


class TestLoadTasks(unittest.TestCase):
    def test_loads_frozen_fixture(self):
        path = pathlib.Path(__file__).resolve().parent / "fixtures" / "tasks.json"
        tasks = abbench.load_tasks(str(path))
        self.assertEqual(len(tasks), 5)
        for t in tasks:
            self.assertIn("id", t)
            self.assertIn("prompt", t)
            self.assertIn("expect_tool", t)

    def test_rejects_duplicate_ids(self):
        with tempfile.TemporaryDirectory() as d:
            p = os.path.join(d, "tasks.json")
            with open(p, "w") as fh:
                json.dump({"tasks": [
                    {"id": "a", "prompt": "x", "expect_tool": "t"},
                    {"id": "a", "prompt": "y", "expect_tool": "t"},
                ]}, fh)
            with self.assertRaises(ValueError):
                abbench.load_tasks(p)


def _tool_message(name, content="irrelevant content"):
    """A role='tool' message's `data`, shaped like real recorded history:
    `json_extract(data,'$.toolUsage.signature.name')` on real rows."""
    return json.dumps({
        "role": "tool",
        "content": content,
        "toolUsage": {"signature": {"id": "tooluse_x", "name": name, "arguments": {}}},
    })


class TestReadTaskResult(unittest.TestCase):
    def _db(self, d, tool_messages=None):
        import sqlite3
        p = os.path.join(d, "bob.db")
        conn = sqlite3.connect(p)
        conn.execute("create table tasks (id text, costs text)")
        conn.execute("create table messages (task_id text, role text, data text, created_at int)")
        conn.execute(
            "insert into tasks values (?,?)",
            ("t1", json.dumps({"input": 1000, "output": 50, "cacheRead": 800,
                               "cacheWrite": 200, "cost": 0.21, "contextTokens": 900})),
        )
        if tool_messages is None:
            tool_messages = [_tool_message("codebase-memory_get_architecture")]
        rows = (
            [("user", json.dumps({"role": "user"}))]
            + [("tool", m) for m in tool_messages]
            + [("assistant", json.dumps({"role": "assistant"})),
               ("assistant", json.dumps({"role": "assistant"}))]
        )
        for i, (role, data) in enumerate(rows):
            conn.execute("insert into messages values (?,?,?,?)", ("t1", role, data, i))
        conn.commit()
        conn.close()
        return p

    def test_extracts_tokens_and_turns(self):
        with tempfile.TemporaryDirectory() as d:
            res = abbench.read_task_result(self._db(d), "t1", "codebase-memory_get_architecture")
            self.assertEqual(res["input_tokens"], 1000)
            self.assertEqual(res["output_tokens"], 50)
            self.assertEqual(res["cost_usd"], 0.21)
            self.assertEqual(res["turns"], 2)
            self.assertTrue(res["succeeded"])

    def test_marks_failure_when_expected_tool_absent(self):
        with tempfile.TemporaryDirectory() as d:
            res = abbench.read_task_result(self._db(d), "t1", "codebase-memory_trace_path")
            self.assertFalse(res["succeeded"])

    def test_matches_on_suffix_so_arms_with_different_prefixes_compare(self):
        with tempfile.TemporaryDirectory() as d:
            res = abbench.read_task_result(self._db(d), "t1", "get_architecture")
            self.assertTrue(res["succeeded"])

    def test_does_not_match_tool_name_appearing_only_in_another_calls_content(self):
        """C2 regression: the expected tool's name can legitimately appear in
        some OTHER tool call's result content or arguments (e.g. a
        search_code hit on the literal string 'get_architecture'). That must
        not count as success — only the CALLED tool's own name may match."""
        with tempfile.TemporaryDirectory() as d:
            messages = [_tool_message(
                "codebase-memory_search_code",
                content="found a reference to get_architecture in docs/index.md",
            )]
            db = self._db(d, tool_messages=messages)
            res = abbench.read_task_result(db, "t1", "get_architecture")
            self.assertFalse(res["succeeded"])

    def test_raises_on_unparseable_tool_message(self):
        with tempfile.TemporaryDirectory() as d:
            db = self._db(d, tool_messages=["not json"])
            with self.assertRaises(ValueError):
                abbench.read_task_result(db, "t1", "get_architecture")

    def test_raises_on_tool_message_missing_toolusage_path(self):
        with tempfile.TemporaryDirectory() as d:
            db = self._db(d, tool_messages=[json.dumps({"role": "tool", "content": "x"})])
            with self.assertRaises(ValueError):
                abbench.read_task_result(db, "t1", "get_architecture")


class TestRunTask(unittest.TestCase):
    def _db(self, d):
        import sqlite3
        p = os.path.join(d, "bob.db")
        conn = sqlite3.connect(p)
        conn.execute(
            "create table tasks (id text, first_message text, env text, created_at int)"
        )
        conn.commit()
        conn.close()
        return p

    def _insert_task(self, db_path, task_id, first_message, workspace, created_at):
        import sqlite3
        conn = sqlite3.connect(db_path)
        conn.execute(
            "insert into tasks values (?,?,?,?)",
            (task_id, first_message, json.dumps({"workspace": workspace}), created_at),
        )
        conn.commit()
        conn.close()

    def test_returns_the_correlated_task_id(self):
        """Normal case: bob run's subprocess call is faked; the row that
        appears afterward matches this run's cwd and prompt, so it's trusted."""
        import unittest.mock as mock

        with tempfile.TemporaryDirectory() as d:
            db = self._db(d)

            def fake_run(*args, **kwargs):
                self._insert_task(db, "new-id", "do the thing", "/repo", 100)
                return mock.Mock(returncode=0, stdout="", stderr="")

            with mock.patch.object(abbench.subprocess, "run", side_effect=fake_run):
                with mock.patch.object(abbench.time, "sleep"):
                    task_id = abbench.run_task("do the thing", "/repo", db, timeout=5)
            self.assertEqual(task_id, "new-id")

    def test_raises_when_a_racing_unrelated_row_appears(self):
        """C1 regression: an ambient Bob session (interactive, or another arm
        of the same sweep) inserts a newer row in the same repo during the
        poll window. It must not be attributed to this run — raise instead
        of silently returning the wrong id."""
        import unittest.mock as mock

        with tempfile.TemporaryDirectory() as d:
            db = self._db(d)

            def fake_run(*args, **kwargs):
                self._insert_task(db, "someone-elses-id", "an unrelated prompt", "/repo", 100)
                return mock.Mock(returncode=0, stdout="", stderr="")

            with mock.patch.object(abbench.subprocess, "run", side_effect=fake_run):
                with mock.patch.object(abbench.time, "sleep"):
                    with self.assertRaises(RuntimeError):
                        abbench.run_task("do the thing", "/repo", db, timeout=5)

    def test_raises_when_racing_row_has_a_different_workspace(self):
        import unittest.mock as mock

        with tempfile.TemporaryDirectory() as d:
            db = self._db(d)

            def fake_run(*args, **kwargs):
                self._insert_task(db, "other-repo-id", "do the thing", "/other-repo", 100)
                return mock.Mock(returncode=0, stdout="", stderr="")

            with mock.patch.object(abbench.subprocess, "run", side_effect=fake_run):
                with mock.patch.object(abbench.time, "sleep"):
                    with self.assertRaises(RuntimeError):
                        abbench.run_task("do the thing", "/repo", db, timeout=5)

    def test_raises_on_timeout_when_no_row_appears(self):
        import unittest.mock as mock

        with tempfile.TemporaryDirectory() as d:
            db = self._db(d)

            with mock.patch.object(
                abbench.subprocess, "run",
                return_value=mock.Mock(returncode=0, stdout="", stderr=""),
            ):
                fast_clock = iter([0, 40])  # first check inside deadline, second past it
                with mock.patch.object(abbench.time, "time", side_effect=lambda: next(fast_clock, 40)):
                    with mock.patch.object(abbench.time, "sleep"):
                        with self.assertRaises(RuntimeError):
                            abbench.run_task("do the thing", "/repo", db, timeout=5)

    def test_still_correlates_a_matching_row_even_after_a_nonzero_exit(self):
        """The db row, not the process exit code, is ground truth: a `bob
        run` that itself exits non-zero may still have recorded a real task
        (e.g. the model errored out after making some tool calls). If a
        correctly-correlated row shows up, return it rather than raising
        merely because the exit code was non-zero."""
        import unittest.mock as mock

        with tempfile.TemporaryDirectory() as d:
            db = self._db(d)

            def fake_run(*args, **kwargs):
                self._insert_task(db, "new-id", "do the thing", "/repo", 100)
                return mock.Mock(returncode=1, stdout="", stderr="boom")

            with mock.patch.object(abbench.subprocess, "run", side_effect=fake_run):
                with mock.patch.object(abbench.time, "sleep"):
                    task_id = abbench.run_task("do the thing", "/repo", db, timeout=5)
            self.assertEqual(task_id, "new-id")


if __name__ == "__main__":
    unittest.main()
