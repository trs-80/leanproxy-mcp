"""Tests for the pure functions in scripts/abbench.py."""

import importlib.util
import json
import os
import pathlib
import sys
import tempfile
import time
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


DESC = "a" * 568  # stands in for the shared fixture description in unit tests

LP_STDIO = (
    'version: "1"\n'
    "servers:\n"
    "  - name: codebase-memory\n"
    "    transport: stdio\n"
    "    enabled: true\n"
    "    stdio:\n"
    "      command: /usr/bin/true\n"
    "      args: []\n"
)


class TestArmConfig(unittest.TestCase):
    BASE = {"mcpServers": {"leanproxy": {"command": "/old/bin", "args": ["server", "run", "--stdio"]}}}

    def test_router_arm_omits_lazy_tools(self):
        cfg = abbench.arm_config("router", "/new/bin", self.BASE, lp_config_path="/tmp/lp.yaml")
        args = cfg["mcpServers"]["leanproxy"]["args"]
        self.assertIn("--stdio", args)
        self.assertNotIn("--lazy-tools", args)

    def test_lazy_arm_includes_lazy_tools(self):
        cfg = abbench.arm_config("lazy", "/new/bin", self.BASE, lp_config_path="/tmp/lp.yaml")
        self.assertIn("--lazy-tools", cfg["mcpServers"]["leanproxy"]["args"])

    def test_proxy_arms_point_at_the_generated_config(self):
        """I-4/C-2: the proxy arms must run against the config this harness
        generated (operator's servers + ballast, adaptive stubs stripped),
        not whatever ~/.config/leanproxy_servers.yaml happens to hold."""
        for arm in ("router", "lazy"):
            cfg = abbench.arm_config(arm, "/new/bin", self.BASE, lp_config_path="/tmp/lp.yaml")
            args = cfg["mcpServers"]["leanproxy"]["args"]
            self.assertIn("--config", args)
            self.assertEqual(args[args.index("--config") + 1], "/tmp/lp.yaml")

    def test_proxy_arm_without_a_config_path_refuses(self):
        with self.assertRaises(ValueError):
            abbench.arm_config("router", "/new/bin", self.BASE)

    def test_native_arm_removes_the_proxy_entirely(self):
        cfg = abbench.arm_config("native", "/new/bin", self.BASE)
        self.assertNotIn("leanproxy", cfg["mcpServers"])

    def test_native_arm_attaches_the_proxied_servers_directly(self):
        """C-4: the native arm used to only REMOVE leanproxy, leaving its tool
        inventory to whatever the operator happened to have enabled outside
        the proxy — on this machine, nothing. It must attach the proxied
        servers directly so it genuinely tests 'same tools, no proxy'."""
        direct = abbench.direct_entries_for_proxied_servers(LP_STDIO)
        cfg = abbench.arm_config("native", "/new/bin", self.BASE, direct_servers=direct)
        self.assertIn("codebase-memory", cfg["mcpServers"])
        self.assertEqual(cfg["mcpServers"]["codebase-memory"]["command"], "/usr/bin/true")
        self.assertNotIn("leanproxy", cfg["mcpServers"])

    def test_proxy_arms_do_not_attach_the_proxied_servers_directly(self):
        """The mirror of the above: attaching them in a proxy arm would be
        exactly the double-load `detect_confound` refuses to run with."""
        direct = abbench.direct_entries_for_proxied_servers(LP_STDIO)
        for arm in ("router", "lazy"):
            cfg = abbench.arm_config(arm, "/new/bin", self.BASE,
                                     direct_servers=direct, lp_config_path="/tmp/lp.yaml")
            self.assertNotIn("codebase-memory", cfg["mcpServers"])

    def test_does_not_mutate_base(self):
        abbench.arm_config("lazy", "/new/bin", self.BASE, lp_config_path="/tmp/lp.yaml")
        self.assertNotIn("--lazy-tools", self.BASE["mcpServers"]["leanproxy"]["args"])


class TestBallast(unittest.TestCase):
    def test_generates_named_ballast_entries(self):
        b = abbench.ballast_servers("/tmp/mockmcp", 2, 50, DESC)
        self.assertEqual(len(b), 2)
        self.assertIn("ballast0", b)
        self.assertIn("--tools=50", b["ballast0"]["args"])

    def test_zero_servers_yields_nothing(self):
        self.assertEqual(abbench.ballast_servers("/tmp/mockmcp", 0, 50, DESC), {})

    def test_ballast_without_a_description_refuses(self):
        """C-1: mockmcp silently falls back to a 46-character default
        description when --description is absent, making Layer 2's ballast
        4.3x lighter per tool than Layer 1's while both are labelled with the
        same tool count. There must be no way to build ballast without one."""
        with self.assertRaises(ValueError):
            abbench.ballast_servers("/tmp/mockmcp", 2, 50, "")
        with self.assertRaises(ValueError):
            abbench.ballast_lp_entries("/tmp/mockmcp", 2, 50, "")

    def test_ballast_always_carries_the_description_flag(self):
        for entry in abbench.ballast_servers("/tmp/mockmcp", 2, 50, DESC).values():
            self.assertIn(f"--description={DESC}", entry["args"])
        for spec in abbench.ballast_lp_entries("/tmp/mockmcp", 2, 50, DESC):
            self.assertIn(f"--description={DESC}", spec["args"])

    def test_bob_and_lp_ballast_carry_identical_args(self):
        """The two sides of the same ballast point must be the same server
        with the same command line — only which side of the proxy it sits on
        may differ."""
        bob = abbench.ballast_servers("/tmp/mockmcp", 2, 50, DESC)
        lp = abbench.ballast_lp_entries("/tmp/mockmcp", 2, 50, DESC)
        self.assertEqual(
            [bob[f"ballast{i}"]["args"] for i in range(2)],
            [spec["args"] for spec in lp],
        )

    def test_arm_config_merges_ballast_into_native(self):
        base = {"mcpServers": {"leanproxy": {"command": "/old", "args": []}}}
        ballast = abbench.ballast_servers("/tmp/mockmcp", 2, 50, DESC)
        cfg = abbench.arm_config("native", "/new/bin", base, ballast)
        self.assertIn("ballast0", cfg["mcpServers"])
        self.assertNotIn("leanproxy", cfg["mcpServers"])

    def test_arm_config_without_ballast_is_unchanged(self):
        base = {"mcpServers": {"leanproxy": {"command": "/old", "args": []}}}
        cfg = abbench.arm_config("lazy", "/new/bin", base, lp_config_path="/tmp/lp.yaml")
        self.assertEqual([k for k in cfg["mcpServers"] if k.startswith("ballast")], [])

    def test_ballast_is_never_attached_directly_in_a_proxy_arm(self):
        """C-2 regression. Layer 1's Capture() puts ballast BEHIND the proxy
        for the router and lazy arms; this layer used to attach it directly
        to the agent in all three. `net_tokens = residency x turns + output`
        then multiplied a turn count from a world where the proxy never
        touched the ballast by a residency figure from a world where it hid
        all of it — an error that ran one way, in the proxy's favour."""
        base = {"mcpServers": {"leanproxy": {"command": "/old", "args": []}}}
        ballast = abbench.ballast_servers("/tmp/mockmcp", 2, 50, DESC)
        for arm in ("router", "lazy"):
            cfg = abbench.arm_config(arm, "/new/bin", base, ballast,
                                     lp_config_path="/tmp/lp.yaml")
            self.assertEqual(
                [k for k in cfg["mcpServers"] if k.startswith("ballast")], [],
                f"{arm} arm must carry ballast behind the proxy, not on the agent")
            self.assertIn("leanproxy", cfg["mcpServers"])

    def test_arm_config_ballast_does_not_mutate_the_ballast_dict(self):
        base = {"mcpServers": {"leanproxy": {"command": "/old", "args": []}}}
        ballast = abbench.ballast_servers("/tmp/mockmcp", 1, 50, DESC)
        cfg = abbench.arm_config("native", "/new/bin", base, ballast)
        cfg["mcpServers"]["ballast0"]["args"].append("--mutated")
        self.assertNotIn("--mutated", ballast["ballast0"]["args"])


class TestBallastFixture(unittest.TestCase):
    """C-1: the shared fixture is the single source of ballast tool weight for
    BOTH layers. Layer 1 embeds it with //go:embed; this layer reads it here.
    The byte-for-byte cross-layer guard lives in
    TestBallastWeightIsIdenticalAcrossLayers (tests/bench/e2e/ballast_test.go),
    which measures both layers' real tools/list payloads."""

    def test_default_fixture_loads_and_self_checks(self):
        doc = abbench.load_ballast_fixture()
        self.assertEqual(len(doc["description"]), doc["description_chars"])
        self.assertEqual(doc["description_chars"], 568)

    def test_fixture_whose_recorded_length_disagrees_is_rejected(self):
        with tempfile.TemporaryDirectory() as d:
            p = os.path.join(d, "ballast.json")
            with open(p, "w") as fh:
                json.dump({"description": "short", "description_chars": 568}, fh)
            with self.assertRaises(ValueError):
                abbench.load_ballast_fixture(p)

    def test_fixture_without_a_description_is_rejected(self):
        with tempfile.TemporaryDirectory() as d:
            p = os.path.join(d, "ballast.json")
            with open(p, "w") as fh:
                json.dump({"description": "", "description_chars": 0}, fh)
            with self.assertRaises(ValueError):
                abbench.load_ballast_fixture(p)

    def test_go_source_reads_the_fixture_rather_than_its_own_literal(self):
        """The one thing that would silently undo C-1 is someone pasting the
        prose back into ballast.go as a literal. Assert Layer 1 still embeds
        the shared file."""
        go = pathlib.Path(__file__).resolve().parent / "ballast.go"
        src = go.read_text()
        self.assertIn("//go:embed fixtures/ballast.json", src)
        self.assertIn("BallastToolDescription = Ballast.Description", src)


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

            n = len(abbench._swap_signals())
            self.assertEqual(len(calls), 2 * n)  # n installs + n restores
            for sig, handler in calls[n:]:
                self.assertEqual(handler, abbench.signal.SIG_DFL)

    def test_sighup_is_handled(self):
        """I-6: a closed terminal or dropped SSH session delivers SIGHUP,
        whose default action terminates the process and leaves an arm's
        config in place — ballast attached and, in the native arm, the proxy
        removed. It must be handled like SIGINT/SIGTERM."""
        if not hasattr(abbench.signal, "SIGHUP"):
            self.skipTest("no SIGHUP on this platform")
        self.assertIn(abbench.signal.SIGHUP, abbench._swap_signals())

    def test_backup_is_complete_before_the_claim_returns(self):
        """I-6: the backup used to be created empty via O_CREAT|O_EXCL and
        filled by a separate copy afterwards. Death by signal in that window
        left a 0-byte backup beside an untouched, perfectly good config —
        and the refusal message's first suggestion would then destroy it.
        The copy now goes straight into the exclusively-created descriptor,
        so the backup is either absent or complete."""
        with tempfile.TemporaryDirectory() as d:
            p = os.path.join(d, "mcp.json")
            self._write(p, {"original": True})
            swap = abbench.ConfigSwap(p, {"swapped": True})
            swap._claim_backup()
            try:
                with open(swap.backup) as fh:
                    self.assertEqual(json.load(fh), {"original": True})
            finally:
                os.remove(swap.backup)

    def test_refusal_message_leads_with_checking_the_backup(self):
        """I-6: 'restore <path> from the backup' as the first instruction
        destroys a working config when the backup is the 0-byte stub of a
        signal-killed run. The message must tell the operator to check the
        backup is real first."""
        with tempfile.TemporaryDirectory() as d:
            p = os.path.join(d, "mcp.json")
            self._write(p, {"swapped": "from_prior_crashed_run"})
            open(p + ".abbench-backup", "w").close()  # the 0-byte stub
            with self.assertRaises(RuntimeError) as ctx:
                abbench.ConfigSwap(p, {"x": 1})._claim_backup()
            msg = str(ctx.exception)
            self.assertIn("CHECK THE BACKUP FIRST", msg)
            self.assertIn("empty", msg)


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


def _tool_message(name, content="irrelevant content", arguments=None):
    """A role='tool' message's `data`, shaped like real recorded history:
    `json_extract(data,'$.toolUsage.signature.name')` on real rows, with
    `$.toolUsage.signature.arguments` alongside it (verified against the
    operator's own message store)."""
    return json.dumps({
        "role": "tool",
        "content": content,
        "toolUsage": {"signature": {"id": "tooluse_x", "name": name,
                                    "arguments": arguments or {}}},
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

    # ---- C-3: the router arm -------------------------------------------

    def test_router_arm_success_is_detected_via_invoke_tools_arguments(self):
        """C-3 regression. In the router arm leanproxy exposes only
        list_tools/invoke_tool/search_tools, so the real tool is an ARGUMENT
        and never a called tool name. Every router run therefore scored
        succeeded=False, abreport correctly dropped the whole arm, and the
        operator had already paid for every run of it."""
        with tempfile.TemporaryDirectory() as d:
            messages = [
                _tool_message("mcp__leanproxy__search_tools",
                              arguments={"query": "architecture"}),
                _tool_message("mcp__leanproxy__invoke_tool",
                              arguments={"server": "codebase-memory",
                                         "tool": "get_architecture",
                                         "arguments": {"project": "x"}}),
            ]
            res = abbench.read_task_result(self._db(d, messages), "t1", "get_architecture")
            self.assertTrue(res["succeeded"])

    def test_router_genuine_discovery_failure_still_scores_false(self):
        """The other half of C-3, and the more important half: the success
        rule is the one thing that would catch a real router discovery
        failure. A run that searched and listed but never invoked the right
        tool must still fail."""
        with tempfile.TemporaryDirectory() as d:
            messages = [
                _tool_message("mcp__leanproxy__search_tools",
                              arguments={"query": "get_architecture"},
                              content="codebase-memory_get_architecture: summarise..."),
                _tool_message("mcp__leanproxy__list_tools",
                              arguments={"server_name": "codebase-memory"},
                              content="codebase-memory tools (8): get_architecture, ..."),
            ]
            res = abbench.read_task_result(self._db(d, messages), "t1", "get_architecture")
            self.assertFalse(res["succeeded"])

    def test_router_invoking_a_different_tool_scores_false(self):
        with tempfile.TemporaryDirectory() as d:
            messages = [_tool_message(
                "mcp__leanproxy__invoke_tool",
                arguments={"server": "codebase-memory", "tool": "search_code"},
            )]
            res = abbench.read_task_result(self._db(d, messages), "t1", "get_architecture")
            self.assertFalse(res["succeeded"])

    def test_router_invoke_tool_without_a_tool_argument_scores_false(self):
        with tempfile.TemporaryDirectory() as d:
            messages = [_tool_message("mcp__leanproxy__invoke_tool",
                                      arguments={"server": "codebase-memory"})]
            res = abbench.read_task_result(self._db(d, messages), "t1", "get_architecture")
            self.assertFalse(res["succeeded"])

    def test_arguments_recorded_as_a_json_string_still_parse(self):
        with tempfile.TemporaryDirectory() as d:
            messages = [_tool_message(
                "mcp__leanproxy__invoke_tool",
                arguments=json.dumps({"server": "codebase-memory",
                                      "tool": "get_architecture"}),
            )]
            res = abbench.read_task_result(self._db(d, messages), "t1", "get_architecture")
            self.assertTrue(res["succeeded"])

    def test_expected_tool_named_only_in_a_search_query_does_not_count(self):
        """The router clause must read invoke_tool's `tool` argument and
        nothing else — not search_tools' query, which names the target
        precisely when the model has NOT yet reached it."""
        self.assertFalse(abbench._reached_expected_tool(
            "mcp__leanproxy__search_tools",
            {"query": "get_architecture"},
            "get_architecture",
        ))

    def test_a_non_router_tool_named_tool_argument_is_ignored(self):
        """Only leanproxy's invoke_tool wrapper gets the argument treatment;
        an unrelated tool that happens to take a `tool` argument does not."""
        self.assertFalse(abbench._reached_expected_tool(
            "some_other_tool", {"tool": "get_architecture"}, "get_architecture"))


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


class TestPairedDeltas(unittest.TestCase):
    RECORDS = [
        {"arm": "native", "task": "a", "cost_usd": 1.00, "turns": 3},
        {"arm": "native", "task": "b", "cost_usd": 0.10, "turns": 2},
        {"arm": "lazy",   "task": "a", "cost_usd": 0.80, "turns": 3},
        {"arm": "lazy",   "task": "b", "cost_usd": 0.08, "turns": 2},
    ]

    def test_pairs_by_task_and_reports_consistent_sign(self):
        out = abbench.paired_deltas(self.RECORDS, "native", "lazy", "cost_usd")
        self.assertAlmostEqual(out["deltas"]["a"], -0.20)
        self.assertAlmostEqual(out["deltas"]["b"], -0.02)
        self.assertTrue(out["consistent"])
        self.assertAlmostEqual(out["total_delta"], -0.22)

    def test_reports_inconsistent_when_signs_disagree(self):
        recs = self.RECORDS + [
            {"arm": "native", "task": "c", "cost_usd": 0.10, "turns": 1},
            {"arm": "lazy",   "task": "c", "cost_usd": 0.30, "turns": 3},
        ]
        out = abbench.paired_deltas(recs, "native", "lazy", "cost_usd")
        self.assertFalse(out["consistent"])

    def test_ignores_tasks_missing_from_one_arm(self):
        recs = self.RECORDS + [{"arm": "native", "task": "z", "cost_usd": 5.0, "turns": 9}]
        out = abbench.paired_deltas(recs, "native", "lazy", "cost_usd")
        self.assertNotIn("z", out["deltas"])
        self.assertEqual(out["pairs"], 2)

    def test_ignores_a_failed_run_missing_the_field_instead_of_raising(self):
        """A run_task failure records succeeded=False and an error instead of
        token/cost metrics (see main). That task is present in both arms but
        has no `cost_usd` on the failing side — treat it like an unpaired
        point rather than raising KeyError and losing every other pair."""
        recs = self.RECORDS + [
            {"arm": "native", "task": "w", "cost_usd": 0.5, "turns": 4},
            {"arm": "lazy", "task": "w", "succeeded": False, "error": "timed out"},
        ]
        out = abbench.paired_deltas(recs, "native", "lazy", "cost_usd")
        self.assertNotIn("w", out["deltas"])
        self.assertEqual(out["pairs"], 2)

    def test_single_pair_is_not_consistent_even_without_disagreement(self):
        """A lone pair can't disagree with itself, so treating it as
        'consistent' would print a confident-looking signed number from a
        single, possibly noisy, data point — exactly the point-estimate-
        from-noise outcome pairing exists to prevent. Reachable in practice:
        if four of five tasks fail to pair, the one survivor must not look
        like a finding."""
        recs = [
            {"arm": "native", "task": "solo", "cost_usd": 1.00},
            {"arm": "lazy",   "task": "solo", "cost_usd": 0.50},
        ]
        out = abbench.paired_deltas(recs, "native", "lazy", "cost_usd")
        self.assertEqual(out["pairs"], 1)
        self.assertFalse(out["consistent"])
        self.assertEqual(out["verdict"], "insufficient_pairs")

    def test_verdict_distinguishes_no_pairs_from_consistent_from_inconsistent(self):
        """`verdict` must let a downstream consumer tell "no disagreement
        among enough pairs" from "only one pair existed" without having to
        cross-reference `pairs` itself."""
        out_none = abbench.paired_deltas([], "native", "lazy", "cost_usd")
        self.assertEqual(out_none["verdict"], "no_pairs")

        out_consistent = abbench.paired_deltas(self.RECORDS, "native", "lazy", "cost_usd")
        self.assertEqual(out_consistent["verdict"], "consistent")

        recs = self.RECORDS + [
            {"arm": "native", "task": "c", "cost_usd": 0.10},
            {"arm": "lazy",   "task": "c", "cost_usd": 0.30},
        ]
        out_inconsistent = abbench.paired_deltas(recs, "native", "lazy", "cost_usd")
        self.assertEqual(out_inconsistent["verdict"], "inconsistent")

    def test_records_from_different_ballast_levels_do_not_pair(self):
        """main() tags `task` as f"{id}@{actual_ballast}" so a sweep across
        multiple ballast points never pairs a k=0 measurement against a
        k=100 measurement for the "same" underlying task id — they are
        different points on the sweep, not repeats of one measurement."""
        recs = [
            {"arm": "native", "task": "a@0", "ballast_tools": 0, "cost_usd": 1.00},
            {"arm": "lazy", "task": "a@0", "ballast_tools": 0, "cost_usd": 0.80},
            {"arm": "native", "task": "a@100", "ballast_tools": 100, "cost_usd": 1.00},
            {"arm": "lazy", "task": "a@100", "ballast_tools": 100, "cost_usd": 0.95},
        ]
        out = abbench.paired_deltas(recs, "native", "lazy", "cost_usd")
        self.assertEqual(out["pairs"], 2)
        self.assertIn("a@0", out["deltas"])
        self.assertIn("a@100", out["deltas"])
        self.assertAlmostEqual(out["deltas"]["a@0"], -0.20)
        self.assertAlmostEqual(out["deltas"]["a@100"], -0.05)


class TestMainIncrementalPersistence(unittest.TestCase):
    """main() must persist each record to disk as it's produced, not only
    after the full sweep completes — a crash on run N of M must not lose
    the N-1 already-paid-for measurements that preceded it."""

    def test_survives_a_mid_sweep_crash(self):
        import unittest.mock as mock

        with tempfile.TemporaryDirectory() as d:
            bob_cfg_path = os.path.join(d, "mcp.json")
            lp_cfg_path = os.path.join(d, "leanproxy_servers.yaml")
            out_dir = os.path.join(d, "out")
            tasks_path = os.path.join(d, "tasks.json")
            db_path = os.path.join(d, "bob.db")

            with open(bob_cfg_path, "w") as fh:
                json.dump({"mcpServers": {}}, fh)
            with open(lp_cfg_path, "w") as fh:
                fh.write(LP_STDIO)
            with open(tasks_path, "w") as fh:
                json.dump({"tasks": [
                    {"id": "t1", "prompt": "one", "expect_tool": "get_architecture"},
                    {"id": "t2", "prompt": "two", "expect_tool": "get_architecture"},
                ]}, fh)

            calls = {"n": 0}

            def fake_run_task(prompt, cwd, db, timeout=900):
                calls["n"] += 1
                if calls["n"] == 2:
                    # An operator Ctrl-C (or any BaseException that isn't an
                    # Exception subclass) partway through the sweep: main()'s
                    # per-task guard catches Exception only, so this must
                    # propagate rather than being swallowed as task data.
                    raise KeyboardInterrupt()
                return f"task-{calls['n']}"

            def fake_read_task_result(db, task_id, expect_tool):
                return {"task_id": task_id, "input_tokens": 1, "output_tokens": 1,
                        "cache_read": 0, "cache_write": 0, "cost_usd": 0.1,
                        "context_tokens": 10, "turns": 2, "succeeded": True}

            with mock.patch.object(abbench, "run_task", side_effect=fake_run_task), \
                 mock.patch.object(abbench, "read_task_result", side_effect=fake_read_task_result), \
                 mock.patch.dict(os.environ, {"LEANPROXY_AB_LIVE": "1"}):
                # Scoped to the k=0 ballast point: this test is about the
                # incremental-persistence mechanism, not the ballast sweep
                # dimension. The default "0,100" sweep would now (correctly,
                # see I2) refuse upfront without --mock-bin before run_task
                # is ever called, which would prevent this scenario from
                # ever reaching the interrupt.
                with self.assertRaises(KeyboardInterrupt):
                    abbench.main(["--out", out_dir, "--bob-config", bob_cfg_path,
                                  "--lp-config", lp_cfg_path, "--db", db_path,
                                  "--tasks", tasks_path, "--leanproxy-bin", "/usr/bin/true", "--cwd", d, "--skip-preflight",
                                  "--ballast-points", "0"])

            # the first task's record must already be on disk even though
            # the sweep never finished and no post-loop write ever ran.
            out_files = os.listdir(out_dir)
            self.assertEqual(len(out_files), 1)
            with open(os.path.join(out_dir, out_files[0])) as fh:
                persisted = json.load(fh)
            self.assertEqual(len(persisted), 1)
            # task is tagged with the (actual) ballast level; the interrupt
            # fires during the k=0 sweep point, before k=100 is ever reached.
            self.assertEqual(persisted[0]["task"], "t1@0")
            self.assertEqual(persisted[0]["ballast_tools"], 0)
            self.assertEqual(persisted[0]["arm"], "native")

            # ConfigSwap must still have restored the original config.
            with open(bob_cfg_path) as fh:
                self.assertEqual(json.load(fh), {"mcpServers": {}})

    def test_a_malformed_row_degrades_to_a_recorded_failure_not_a_crash(self):
        """read_task_result is documented to raise on an unparseable or
        unexpected tool-message row. Fail-closed means never recording a
        WRONG measurement, not discarding measurements already correctly
        taken elsewhere in the sweep — so this must become that one task's
        recorded failure and the sweep must continue, not crash."""
        import unittest.mock as mock

        with tempfile.TemporaryDirectory() as d:
            bob_cfg_path = os.path.join(d, "mcp.json")
            lp_cfg_path = os.path.join(d, "leanproxy_servers.yaml")
            out_dir = os.path.join(d, "out")
            tasks_path = os.path.join(d, "tasks.json")
            db_path = os.path.join(d, "bob.db")

            with open(bob_cfg_path, "w") as fh:
                json.dump({"mcpServers": {}}, fh)
            with open(lp_cfg_path, "w") as fh:
                fh.write(LP_STDIO)
            with open(tasks_path, "w") as fh:
                json.dump({"tasks": [
                    {"id": "t1", "prompt": "one", "expect_tool": "get_architecture"},
                ]}, fh)

            def fake_run_task(prompt, cwd, db, timeout=900):
                return "task-1"

            def fake_read_task_result(db, task_id, expect_tool):
                raise ValueError("tool message missing toolUsage.signature.name: '...'")

            with mock.patch.object(abbench, "run_task", side_effect=fake_run_task), \
                 mock.patch.object(abbench, "read_task_result", side_effect=fake_read_task_result), \
                 mock.patch.dict(os.environ, {"LEANPROXY_AB_LIVE": "1"}):
                # Scoped to the k=0 ballast point only: this test is about the
                # incremental-persistence mechanism, not the ballast sweep
                # dimension (covered separately in TestBallastSweep), and a
                # non-zero point would require a real --mock-bin.
                rc = abbench.main(["--out", out_dir, "--bob-config", bob_cfg_path,
                                    "--lp-config", lp_cfg_path, "--db", db_path,
                                    "--tasks", tasks_path, "--leanproxy-bin", "/usr/bin/true", "--cwd", d, "--skip-preflight",
                                    "--ballast-points", "0"])

            self.assertEqual(rc, 0)
            # one output file for the whole sweep — the same path is
            # re-persisted after every task across all three arms.
            out_files = os.listdir(out_dir)
            self.assertEqual(len(out_files), 1)
            with open(os.path.join(out_dir, out_files[0])) as fh:
                persisted = json.load(fh)
            self.assertEqual(len(persisted), 3)  # one task x three arms, all "failed"
            for rec in persisted:
                self.assertFalse(rec["succeeded"])
                self.assertIn("error", rec)
                self.assertEqual(rec["task"], "t1@0")
                self.assertEqual(rec["ballast_tools"], 0)


class TestBallastShape(unittest.TestCase):
    """Direct coverage of `_ballast_shape`, the C1 fix: nominal and actual
    ballast counts must never diverge — a point that can't divide evenly
    across the fixed server count raises instead of silently flooring to a
    value that could collide with a different point's pairing tag."""

    def test_zero_is_the_true_baseline(self):
        self.assertEqual(abbench._ballast_shape(0), (0, 0, 0))

    def test_divisible_point_matches_nominal_exactly(self):
        self.assertEqual(abbench._ballast_shape(100), (2, 50, 100))

    def test_the_original_collision_point_now_raises_instead_of_flooring_to_zero(self):
        """C1 regression: `tools=1` used to floor to `actual=0`, tagging
        identically to the true zero-ballast point while actually attaching
        two live mock servers. It must now raise, not collide."""
        with self.assertRaises(ValueError):
            abbench._ballast_shape(1)

    def test_odd_point_raises(self):
        with self.assertRaises(ValueError):
            abbench._ballast_shape(101)

    def test_negative_point_raises_even_when_internally_divisible(self):
        """Round-2 regression: `tools=-2` floor-divides "evenly" (servers=2,
        per=-1, actual=-2 == tools), so the divisibility check alone would
        wave it through. Negative tool counts are rejected outright,
        independent of divisibility."""
        with self.assertRaises(ValueError):
            abbench._ballast_shape(-2)

    def test_odd_negative_point_also_raises(self):
        with self.assertRaises(ValueError):
            abbench._ballast_shape(-1)


class TestBallastSweep(unittest.TestCase):
    """main()'s ballast dimension: the mock-bin gate (validated once, before
    any point runs), the mock-bin executable check, the C1 divisibility
    guard applied to the whole sweep before any point runs, and per-point
    tagging with the actual ballast tool count."""

    def _setup(self, d, task_ids):
        bob_cfg_path = os.path.join(d, "mcp.json")
        lp_cfg_path = os.path.join(d, "leanproxy_servers.yaml")
        out_dir = os.path.join(d, "out")
        tasks_path = os.path.join(d, "tasks.json")
        db_path = os.path.join(d, "bob.db")

        with open(bob_cfg_path, "w") as fh:
            json.dump({"mcpServers": {}}, fh)
        with open(lp_cfg_path, "w") as fh:
            fh.write(LP_STDIO)
        with open(tasks_path, "w") as fh:
            json.dump({"tasks": [
                {"id": tid, "prompt": tid, "expect_tool": "get_architecture"}
                for tid in task_ids
            ]}, fh)

        # A real, executable file — I3 checks isfile + X_OK, and this is
        # never actually invoked in these tests (run_task is mocked), so its
        # content doesn't matter.
        mock_bin_path = os.path.join(d, "mockmcp")
        with open(mock_bin_path, "w") as fh:
            fh.write("#!/bin/sh\nexit 0\n")
        os.chmod(mock_bin_path, 0o755)

        return bob_cfg_path, lp_cfg_path, out_dir, tasks_path, db_path, mock_bin_path

    def test_nonzero_ballast_point_without_mock_bin_refuses(self):
        import unittest.mock as mock

        with tempfile.TemporaryDirectory() as d:
            bob_cfg_path, lp_cfg_path, out_dir, tasks_path, db_path, _ = self._setup(d, ["t1"])

            with mock.patch.object(abbench, "run_task") as run_task_mock, \
                 mock.patch.dict(os.environ, {"LEANPROXY_AB_LIVE": "1"}):
                rc = abbench.main(["--out", out_dir, "--bob-config", bob_cfg_path,
                                    "--lp-config", lp_cfg_path, "--db", db_path,
                                    "--tasks", tasks_path, "--leanproxy-bin", "/usr/bin/true", "--cwd", d, "--skip-preflight",
                                    "--ballast-points", "100"])

            self.assertEqual(rc, 2)
            run_task_mock.assert_not_called()
            # never entered ConfigSwap for this point — the real config
            # must be untouched, not left mid-swap.
            with open(bob_cfg_path) as fh:
                self.assertEqual(json.load(fh), {"mcpServers": {}})

    def test_mock_bin_gate_fires_before_any_point_runs_not_just_the_nonzero_one(self):
        """I2 regression: with the DEFAULT `--ballast-points "0,100"` and no
        `--mock-bin`, the old code let the whole k=0 point run for real
        (spending money) before the k=100 point's guard fired. The gate must
        now look at the whole points list before anything runs, so k=0 must
        not run either."""
        import unittest.mock as mock

        with tempfile.TemporaryDirectory() as d:
            bob_cfg_path, lp_cfg_path, out_dir, tasks_path, db_path, _ = self._setup(d, ["t1"])

            with mock.patch.object(abbench, "run_task") as run_task_mock, \
                 mock.patch.dict(os.environ, {"LEANPROXY_AB_LIVE": "1"}):
                # no --ballast-points override: exercises the real default,
                # "0,100"
                rc = abbench.main(["--out", out_dir, "--bob-config", bob_cfg_path,
                                    "--lp-config", lp_cfg_path, "--db", db_path,
                                    "--tasks", tasks_path, "--leanproxy-bin", "/usr/bin/true"])

            self.assertEqual(rc, 2)
            run_task_mock.assert_not_called()
            with open(bob_cfg_path) as fh:
                self.assertEqual(json.load(fh), {"mcpServers": {}})

    def test_mock_bin_that_is_not_an_executable_file_refuses(self):
        """I3 regression: a truthy but bogus --mock-bin (typo'd path, or a
        non-executable file) must be caught before any point runs, not
        discovered mid-sweep when Bob fails to spawn it."""
        import unittest.mock as mock

        with tempfile.TemporaryDirectory() as d:
            bob_cfg_path, lp_cfg_path, out_dir, tasks_path, db_path, _ = self._setup(d, ["t1"])
            bogus = os.path.join(d, "does-not-exist")

            with mock.patch.object(abbench, "run_task") as run_task_mock, \
                 mock.patch.dict(os.environ, {"LEANPROXY_AB_LIVE": "1"}):
                rc = abbench.main(["--out", out_dir, "--bob-config", bob_cfg_path,
                                    "--lp-config", lp_cfg_path, "--db", db_path,
                                    "--tasks", tasks_path, "--leanproxy-bin", "/usr/bin/true", "--cwd", d, "--skip-preflight",
                                    "--ballast-points", "0,100", "--mock-bin", bogus])

            self.assertEqual(rc, 2)
            run_task_mock.assert_not_called()

    def test_non_divisible_point_in_a_multi_point_sweep_raises_before_any_point_runs(self):
        """C1 regression at the main() level: `--ballast-points "0,1"` used
        to run k=0 for real, then run k=1 for real and silently overwrite
        k=0's records under the same "@0" tag. It must now raise before
        either point runs."""
        import unittest.mock as mock

        with tempfile.TemporaryDirectory() as d:
            bob_cfg_path, lp_cfg_path, out_dir, tasks_path, db_path, mock_bin = self._setup(d, ["t1"])

            with mock.patch.object(abbench, "run_task") as run_task_mock, \
                 mock.patch.dict(os.environ, {"LEANPROXY_AB_LIVE": "1"}):
                with self.assertRaises(ValueError):
                    abbench.main(["--out", out_dir, "--bob-config", bob_cfg_path,
                                  "--lp-config", lp_cfg_path, "--db", db_path,
                                  "--tasks", tasks_path, "--leanproxy-bin", "/usr/bin/true", "--cwd", d, "--skip-preflight",
                                  "--ballast-points", "0,1", "--mock-bin", mock_bin])

            run_task_mock.assert_not_called()
            with open(bob_cfg_path) as fh:
                self.assertEqual(json.load(fh), {"mcpServers": {}})

    def test_negative_ballast_point_refuses_before_any_run(self):
        """Round-2 regression: `--ballast-points "-2"` used to evade both
        gates — `_ballast_shape`'s divisibility check passes (floor division
        of an even negative is internally consistent) and the old `t > 0`
        mock-bin gate doesn't see a negative as "non-zero" either — and ran
        all three arms for real with a ballast server launched via
        `command: ""`. A negative point must refuse immediately, without
        even needing --mock-bin to be given."""
        import unittest.mock as mock

        with tempfile.TemporaryDirectory() as d:
            bob_cfg_path, lp_cfg_path, out_dir, tasks_path, db_path, _ = self._setup(d, ["t1"])

            with mock.patch.object(abbench, "run_task") as run_task_mock, \
                 mock.patch.dict(os.environ, {"LEANPROXY_AB_LIVE": "1"}):
                rc = abbench.main(["--out", out_dir, "--bob-config", bob_cfg_path,
                                    "--lp-config", lp_cfg_path, "--db", db_path,
                                    "--tasks", tasks_path, "--leanproxy-bin", "/usr/bin/true", "--cwd", d, "--skip-preflight",
                                    "--ballast-points", "-2"])

            self.assertEqual(rc, 2)
            run_task_mock.assert_not_called()
            with open(bob_cfg_path) as fh:
                self.assertEqual(json.load(fh), {"mcpServers": {}})

    def test_mixed_list_with_a_negative_point_refuses_the_whole_sweep(self):
        """A valid point earlier in the list must not run before a later
        negative point is discovered — the whole list is validated upfront,
        same as C1's non-divisible-point case."""
        import unittest.mock as mock

        with tempfile.TemporaryDirectory() as d:
            bob_cfg_path, lp_cfg_path, out_dir, tasks_path, db_path, mock_bin = self._setup(d, ["t1"])

            with mock.patch.object(abbench, "run_task") as run_task_mock, \
                 mock.patch.dict(os.environ, {"LEANPROXY_AB_LIVE": "1"}):
                rc = abbench.main(["--out", out_dir, "--bob-config", bob_cfg_path,
                                    "--lp-config", lp_cfg_path, "--db", db_path,
                                    "--tasks", tasks_path, "--leanproxy-bin", "/usr/bin/true", "--cwd", d, "--skip-preflight",
                                    "--ballast-points", "100,-2", "--mock-bin", mock_bin])

            self.assertEqual(rc, 2)
            run_task_mock.assert_not_called()
            with open(bob_cfg_path) as fh:
                self.assertEqual(json.load(fh), {"mcpServers": {}})

    def test_sweep_tags_records_with_the_actual_ballast_count(self):
        import unittest.mock as mock

        with tempfile.TemporaryDirectory() as d:
            bob_cfg_path, lp_cfg_path, out_dir, tasks_path, db_path, mock_bin = self._setup(d, ["t1"])
            calls = {"n": 0}

            def fake_run_task(prompt, cwd, db, timeout=900):
                calls["n"] += 1
                return f"task-{calls['n']}"

            def fake_read_task_result(db, task_id, expect_tool):
                return {"task_id": task_id, "input_tokens": 1, "output_tokens": 1,
                        "cache_read": 0, "cache_write": 0, "cost_usd": 0.1,
                        "context_tokens": 10, "turns": 2, "succeeded": True}

            with mock.patch.object(abbench, "run_task", side_effect=fake_run_task), \
                 mock.patch.object(abbench, "read_task_result", side_effect=fake_read_task_result), \
                 mock.patch.dict(os.environ, {"LEANPROXY_AB_LIVE": "1"}):
                rc = abbench.main(["--out", out_dir, "--bob-config", bob_cfg_path,
                                    "--lp-config", lp_cfg_path, "--db", db_path,
                                    "--tasks", tasks_path, "--leanproxy-bin", "/usr/bin/true", "--cwd", d, "--skip-preflight",
                                    "--ballast-points", "0,100", "--mock-bin", mock_bin])

            self.assertEqual(rc, 0)
            out_files = os.listdir(out_dir)
            with open(os.path.join(out_dir, out_files[0])) as fh:
                persisted = json.load(fh)

            # one task x three arms x two ballast points == 6 records
            self.assertEqual(len(persisted), 6)
            tasks_seen = {r["task"] for r in persisted}
            self.assertEqual(tasks_seen, {"t1@0", "t1@100"})
            ballast_seen = {r["ballast_tools"] for r in persisted}
            self.assertEqual(ballast_seen, {0, 100})

    def test_records_at_different_ballast_points_do_not_pair(self):
        """End-to-end version of the paired_deltas unit test: a live sweep
        across k=0 and k=100 must not let paired_deltas compare a k=0
        measurement against a k=100 one for the "same" task id."""
        import unittest.mock as mock

        with tempfile.TemporaryDirectory() as d:
            bob_cfg_path, lp_cfg_path, out_dir, tasks_path, db_path, mock_bin = self._setup(d, ["t1"])

            costs = iter([0.10, 0.08, 0.06, 0.50, 0.48, 0.46])  # native,router,lazy x k=0,k=100

            def fake_run_task(prompt, cwd, db, timeout=900):
                return "task-x"

            def fake_read_task_result(db, task_id, expect_tool):
                return {"task_id": task_id, "input_tokens": 1, "output_tokens": 1,
                        "cache_read": 0, "cache_write": 0, "cost_usd": next(costs),
                        "context_tokens": 10, "turns": 2, "succeeded": True}

            with mock.patch.object(abbench, "run_task", side_effect=fake_run_task), \
                 mock.patch.object(abbench, "read_task_result", side_effect=fake_read_task_result), \
                 mock.patch.dict(os.environ, {"LEANPROXY_AB_LIVE": "1"}):
                rc = abbench.main(["--out", out_dir, "--bob-config", bob_cfg_path,
                                    "--lp-config", lp_cfg_path, "--db", db_path,
                                    "--tasks", tasks_path, "--leanproxy-bin", "/usr/bin/true", "--cwd", d, "--skip-preflight",
                                    "--ballast-points", "0,100", "--mock-bin", mock_bin])
            self.assertEqual(rc, 0)

            out_files = os.listdir(out_dir)
            with open(os.path.join(out_dir, out_files[0])) as fh:
                persisted = json.load(fh)

            d_lazy = abbench.paired_deltas(persisted, "native", "lazy", "cost_usd")
            self.assertEqual(d_lazy["pairs"], 2)
            self.assertIn("t1@0", d_lazy["deltas"])
            self.assertIn("t1@100", d_lazy["deltas"])


class TestArmConfigBallastCollision(unittest.TestCase):
    """I4 regression: ballast key names must never silently overwrite a
    real server already present in the operator's config."""

    def test_ballast_name_colliding_with_a_real_server_raises(self):
        base = {"mcpServers": {"ballast0": {"command": "/real/server", "args": []}}}
        ballast = abbench.ballast_servers("/tmp/mockmcp", 1, 50, DESC)
        with self.assertRaises(ValueError):
            abbench.arm_config("native", "/new/bin", base, ballast)

    def test_non_colliding_ballast_names_are_unaffected(self):
        base = {"mcpServers": {"some-other-server": {"command": "/real/server", "args": []}}}
        ballast = abbench.ballast_servers("/tmp/mockmcp", 1, 50, DESC)
        cfg = abbench.arm_config("native", "/new/bin", base, ballast)
        self.assertIn("ballast0", cfg["mcpServers"])
        self.assertIn("some-other-server", cfg["mcpServers"])

    def test_proxied_server_name_colliding_with_a_real_one_raises(self):
        """The same guard, for the servers the native arm now attaches
        directly (C-4): silently overwriting a real entry would swap the
        operator's server for a differently-configured one mid-sweep."""
        base = {"mcpServers": {"codebase-memory": {"command": "/something/else", "args": []}}}
        direct = abbench.direct_entries_for_proxied_servers(LP_STDIO)
        with self.assertRaises(ValueError):
            abbench.arm_config("native", "/new/bin", base, direct_servers=direct)


# ---------------------------------------------------------------------------
# the fuller leanproxy config parse the native arm depends on (C-4)
# ---------------------------------------------------------------------------


LP_REALISTIC = (
    'version: "1"\n'
    "servers:\n"
    "  - name: codebase-memory\n"
    "    transport: stdio\n"
    "    enabled: true\n"
    "    stdio:\n"
    "      command: /usr/local/bin/codebase-memory-mcp\n"
    '      args: ["--tool-profile=analysis", "--quiet"]\n'
    "    tools:\n"
    "      include: [search_graph, get_architecture]\n"
    "      max_response_chars:\n"
    "        get_architecture: 6000\n"
    "      adaptive_stub_after: 168h\n"
    "  - name: context7\n"
    "    transport: http\n"
    "    enabled: true\n"
    "    http:\n"
    "      url: https://mcp.context7.com/mcp\n"
    "  - name: disabled-one\n"
    "    transport: stdio\n"
    "    enabled: false\n"
    "    stdio:\n"
    "      command: /bin/false\n"
    "bouncer:\n"
    "  enabled: true\n"
)


class TestLpServersFullParse(unittest.TestCase):
    def test_parses_nested_stdio_block(self):
        servers = abbench.lp_servers(LP_REALISTIC)
        self.assertEqual([s["name"] for s in servers],
                         ["codebase-memory", "context7", "disabled-one"])
        self.assertEqual(servers[0]["stdio"]["command"], "/usr/local/bin/codebase-memory-mcp")

    def test_parses_flow_style_args(self):
        servers = abbench.lp_servers(LP_REALISTIC)
        args = abbench._flow_list(servers[0]["stdio"]["args"], "args")
        self.assertEqual(args, ["--tool-profile=analysis", "--quiet"])

    def test_parses_deeply_nested_maps(self):
        servers = abbench.lp_servers(LP_REALISTIC)
        self.assertEqual(servers[0]["tools"]["max_response_chars"]["get_architecture"], "6000")

    def test_block_style_list_under_a_nested_key(self):
        text = (
            "servers:\n"
            "  - name: s\n"
            "    enabled: true\n"
            "    tools:\n"
            "      include:\n"
            "        - search_graph\n"
            "        - trace_path\n"
        )
        servers = abbench.lp_servers(text)
        self.assertEqual(servers[0]["tools"]["include"], ["search_graph", "trace_path"])

    def test_direct_entries_only_cover_enabled_servers(self):
        entries = abbench.direct_entries_for_proxied_servers(LP_REALISTIC)
        self.assertEqual(sorted(entries), ["codebase-memory", "context7"])
        self.assertEqual(entries["codebase-memory"]["args"],
                         ["--tool-profile=analysis", "--quiet"])
        self.assertEqual(entries["context7"], {"type": "http",
                                               "url": "https://mcp.context7.com/mcp",
                                               "disabled": False, "enabled": True})

    def test_http_server_with_auth_headers_refuses_rather_than_dropping_them(self):
        """Fail closed: copying credentials into the agent's config is not
        this harness's call, and silently omitting them would give the native
        arm a smaller inventory than the proxy arms while both are labelled
        'same tools'."""
        text = (
            "servers:\n"
            "  - name: context7\n"
            "    transport: http\n"
            "    enabled: true\n"
            "    http:\n"
            "      url: https://mcp.context7.com/mcp\n"
            "      headers:\n"
            "        CONTEXT7_API_KEY: secret\n"
        )
        with self.assertRaises(abbench.UnmirrorableServer):
            abbench.direct_entries_for_proxied_servers(text)

    def test_stdio_server_with_env_refuses(self):
        text = (
            "servers:\n"
            "  - name: s\n"
            "    transport: stdio\n"
            "    enabled: true\n"
            "    stdio:\n"
            "      command: /bin/true\n"
            '      env: ["TOKEN=abc"]\n'
        )
        with self.assertRaises(abbench.UnmirrorableServer):
            abbench.direct_entries_for_proxied_servers(text)

    def test_tool_filtered_servers_are_reported(self):
        self.assertEqual(abbench.tool_filtered_servers(LP_REALISTIC), ["codebase-memory"])


class TestConfoundAliasing(unittest.TestCase):
    """I-2: on the operator's own machine the same upstream binary is
    registered as `codebase-memory-mcp` in Bob and `codebase-memory` in the
    leanproxy config, and the name-only guard waved the double-load through."""

    def test_same_command_under_a_different_name_is_still_a_confound(self):
        bob = {"mcpServers": {
            "codebase-memory-mcp": {"command": "/usr/local/bin/codebase-memory-mcp",
                                    "args": [], "disabled": False},
            "leanproxy": {"disabled": False},
        }}
        problems = abbench.detect_confound(bob, LP_REALISTIC)
        self.assertTrue(any("codebase-memory" in p for p in problems), problems)

    def test_same_url_under_a_different_name_is_still_a_confound(self):
        bob = {"mcpServers": {
            "ctx7": {"type": "http", "url": "https://mcp.context7.com/mcp", "disabled": False},
            "leanproxy": {"disabled": False},
        }}
        problems = abbench.detect_confound(bob, LP_REALISTIC)
        self.assertTrue(any("context7" in p for p in problems), problems)

    def test_an_unrelated_server_is_not_flagged(self):
        bob = {"mcpServers": {
            "something-else": {"command": "/usr/bin/env", "args": [], "disabled": False},
            "leanproxy": {"disabled": False},
        }}
        self.assertEqual(abbench.detect_confound(bob, LP_REALISTIC), [])

    def test_a_disabled_alias_is_not_a_confound(self):
        bob = {"mcpServers": {
            "codebase-memory-mcp": {"command": "/usr/local/bin/codebase-memory-mcp",
                                    "args": [], "disabled": True},
            "leanproxy": {"disabled": False},
        }}
        self.assertEqual(abbench.detect_confound(bob, LP_REALISTIC), [])


class TestBuildArmLpConfig(unittest.TestCase):
    """C-2 and I-4: the proxy arms run against a generated config that keeps
    every real server's settings, adds the ballast BEHIND the proxy, and has
    no adaptive stubs to make residency history-dependent."""

    def test_ballast_is_spliced_into_the_servers_block(self):
        ballast = abbench.ballast_lp_entries("/tmp/mockmcp", 2, 50, DESC)
        out = abbench.build_arm_lp_config(LP_REALISTIC, ballast)
        parsed = abbench.lp_servers(out)
        names = [abbench._unquote(s["name"]) for s in parsed]
        self.assertIn("ballast0", names)
        self.assertIn("ballast1", names)
        self.assertIn("codebase-memory", names)

    def test_spliced_ballast_carries_the_shared_description(self):
        ballast = abbench.ballast_lp_entries("/tmp/mockmcp", 1, 50, DESC)
        out = abbench.build_arm_lp_config(LP_REALISTIC, ballast)
        spliced = [s for s in abbench.lp_servers(out)
                   if abbench._unquote(s["name"]) == "ballast0"][0]
        args = abbench._flow_list(spliced["stdio"]["args"], "args")
        self.assertEqual(args, [f"--tools=50", f"--description={DESC}"])

    def test_real_server_settings_survive_verbatim(self):
        out = abbench.build_arm_lp_config(LP_REALISTIC, [])
        self.assertIn("include: [search_graph, get_architecture]", out)
        self.assertIn("get_architecture: 6000", out)

    def test_adaptive_stubs_are_stripped(self):
        """I-4: under adaptive stubs the proxy's tools/list depends on usage
        recorded in ~/.config/leanproxy/toolusage.json, which every live run
        mutates — so the lazy arm's residency would drift during the sweep and
        would not describe the fixed figure Layer 1 measured."""
        out = abbench.build_arm_lp_config(LP_REALISTIC, [])
        self.assertNotIn("adaptive_stub_after", out)

    def test_a_config_with_no_servers_block_refuses(self):
        with self.assertRaises(ValueError):
            abbench.build_arm_lp_config('version: "1"\nbouncer:\n  enabled: true\n', [])


class TestPreflight(unittest.TestCase):
    """C-4: an unusable configuration must refuse BEFORE anything is spent.

    The reproduced failure was a full sweep that ran to completion, spent the
    whole budget, and produced zero verdicts — every condition being knowable
    in advance, for free.
    """

    TASKS = [{"id": "t1", "prompt": "p", "expect_tool": "get_architecture"}]

    def test_no_enabled_proxied_servers_is_refused(self):
        problems = abbench.preflight(self.TASKS, {}, {}, "/usr/bin/true",
                                     "/tmp/lp.yaml", [], False, [])
        self.assertTrue(any("nothing for the proxy arms" in p for p in problems), problems)

    def test_tool_filter_asymmetry_is_refused_unless_allowed(self):
        import unittest.mock as mock
        direct = {"codebase-memory": {"command": "/bin/true", "args": []}}
        with mock.patch.object(abbench, "_native_inventory", return_value=(["x_get_architecture"], [])), \
             mock.patch.object(abbench, "_proxy_inventory", return_value=("get_architecture", [])):
            blocked = abbench.preflight(self.TASKS, direct, {}, "/usr/bin/true",
                                        "/tmp/lp.yaml", ["codebase-memory"], False,
                                        ["codebase-memory"])
            allowed = abbench.preflight(self.TASKS, direct, {}, "/usr/bin/true",
                                        "/tmp/lp.yaml", ["codebase-memory"], True,
                                        ["codebase-memory"])
        self.assertTrue(any("tools.include/exclude" in p for p in blocked), blocked)
        self.assertEqual(allowed, [])

    def test_native_arm_without_the_expected_tool_is_refused(self):
        """The exact C-4 case-1 state on the operator's machine: the proxied
        server is not attached to the agent outside the proxy, so every task
        scores succeeded=False and the sweep yields no verdicts at all."""
        import unittest.mock as mock
        direct = {"codebase-memory": {"command": "/bin/true", "args": []}}
        with mock.patch.object(abbench, "_native_inventory", return_value=([], [])), \
             mock.patch.object(abbench, "_proxy_inventory", return_value=("get_architecture", [])):
            problems = abbench.preflight(self.TASKS, direct, {}, "/usr/bin/true",
                                         "/tmp/lp.yaml", ["codebase-memory"], False, [])
        self.assertTrue(any("native arm" in p and "get_architecture" in p for p in problems),
                        problems)

    def test_proxy_arm_that_cannot_reach_the_tool_is_refused(self):
        import unittest.mock as mock
        direct = {"codebase-memory": {"command": "/bin/true", "args": []}}
        with mock.patch.object(abbench, "_native_inventory",
                               return_value=(["codebase-memory_get_architecture"], [])), \
             mock.patch.object(abbench, "_proxy_inventory", return_value=("", [])):
            problems = abbench.preflight(self.TASKS, direct, {}, "/usr/bin/true",
                                         "/tmp/lp.yaml", ["codebase-memory"], False, [])
        self.assertTrue(any("router arm" in p for p in problems), problems)
        self.assertTrue(any("lazy arm" in p for p in problems), problems)

    def test_a_fully_reachable_configuration_passes(self):
        import unittest.mock as mock
        direct = {"codebase-memory": {"command": "/bin/true", "args": []}}
        with mock.patch.object(abbench, "_native_inventory",
                               return_value=(["codebase-memory_get_architecture"], [])), \
             mock.patch.object(abbench, "_proxy_inventory",
                               return_value=("codebase-memory tools (8): get_architecture", [])):
            self.assertEqual(
                abbench.preflight(self.TASKS, direct, {}, "/usr/bin/true",
                                  "/tmp/lp.yaml", ["codebase-memory"], False, []),
                [])

    def test_main_refuses_before_spending_when_preflight_fails(self):
        import unittest.mock as mock

        with tempfile.TemporaryDirectory() as d:
            bob_cfg_path = os.path.join(d, "mcp.json")
            lp_cfg_path = os.path.join(d, "leanproxy_servers.yaml")
            tasks_path = os.path.join(d, "tasks.json")
            with open(bob_cfg_path, "w") as fh:
                json.dump({"mcpServers": {}}, fh)
            with open(lp_cfg_path, "w") as fh:
                fh.write(LP_STDIO)
            with open(tasks_path, "w") as fh:
                json.dump({"tasks": [{"id": "t1", "prompt": "p",
                                      "expect_tool": "get_architecture"}]}, fh)

            with mock.patch.object(abbench, "preflight",
                                   return_value=["native arm: no tool matching 'x'"]), \
                 mock.patch.object(abbench, "run_task") as run_task_mock, \
                 mock.patch.dict(os.environ, {"LEANPROXY_AB_LIVE": "1"}):
                rc = abbench.main(["--out", os.path.join(d, "out"),
                                   "--bob-config", bob_cfg_path,
                                   "--lp-config", lp_cfg_path,
                                   "--db", os.path.join(d, "bob.db"),
                                   "--tasks", tasks_path, "--cwd", d,
                                   "--leanproxy-bin", "/usr/bin/true",
                                   "--ballast-points", "0"])

            self.assertEqual(rc, 2)
            run_task_mock.assert_not_called()
            with open(bob_cfg_path) as fh:
                self.assertEqual(json.load(fh), {"mcpServers": {}})


FAKE_MCP_SERVER = r'''
import json, sys
TOOLS = json.loads(sys.argv[1]) if len(sys.argv) > 1 else []
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    msg = json.loads(line)
    if "id" not in msg:
        continue  # a notification; nothing to answer
    if msg["method"] == "initialize":
        result = {"protocolVersion": "2024-11-05", "capabilities": {}}
    elif msg["method"] == "tools/list":
        result = {"tools": [{"name": t, "description": "d"} for t in TOOLS]}
    elif msg["method"] == "tools/call":
        result = {"content": [{"type": "text",
                               "text": "called %s" % msg["params"]["name"]}]}
    else:
        print(json.dumps({"jsonrpc": "2.0", "id": msg["id"],
                          "error": {"code": -32601, "message": "no such method"}}),
              flush=True)
        continue
    print(json.dumps({"jsonrpc": "2.0", "id": msg["id"], "result": result}), flush=True)
'''

# A fake MCP server whose only tool reports the HOME environment variable it
# was started with, for N-1: proving the preflight's subprocesses see an
# isolated HOME rather than the operator's real one.
HOME_REPORTING_MCP_SERVER = r'''
import json, os, sys
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    msg = json.loads(line)
    if "id" not in msg:
        continue  # a notification; nothing to answer
    if msg["method"] == "initialize":
        result = {"protocolVersion": "2024-11-05", "capabilities": {}}
    elif msg["method"] == "tools/list":
        result = {"tools": [{"name": "whoami", "description": "d"}]}
    elif msg["method"] == "tools/call":
        result = {"content": [{"type": "text", "text": os.environ.get("HOME", "")}]}
    else:
        print(json.dumps({"jsonrpc": "2.0", "id": msg["id"],
                          "error": {"code": -32601, "message": "no such method"}}),
              flush=True)
        continue
    print(json.dumps({"jsonrpc": "2.0", "id": msg["id"], "result": result}), flush=True)
'''


class TestMcpStdio(unittest.TestCase):
    """The preflight's own MCP client. Tested against a stdlib fake server so
    this runs anywhere `go test ./...` does, without building Go binaries."""

    def _server(self, tools):
        return abbench.McpStdio(sys.executable,
                                ["-c", FAKE_MCP_SERVER, json.dumps(tools)],
                                timeout=30)

    def test_handshake_and_tools_list(self):
        with self._server(["get_architecture", "trace_path"]) as c:
            c.initialize()
            self.assertEqual(c.tool_names(), ["get_architecture", "trace_path"])

    def test_tool_call_text(self):
        with self._server(["x"]) as c:
            c.initialize()
            self.assertEqual(c.call_tool_text("list_tools", {"server_name": "s"}),
                             "called list_tools")

    def test_a_server_that_never_answers_times_out_rather_than_hanging(self):
        c = abbench.McpStdio(sys.executable, ["-c", "import time; time.sleep(30)"],
                             timeout=1.0)
        with c:
            with self.assertRaises(RuntimeError):
                c.initialize()

    def test_a_server_that_exits_immediately_is_reported_not_hung(self):
        c = abbench.McpStdio(sys.executable, ["-c", "raise SystemExit(3)"], timeout=10)
        with c:
            with self.assertRaises(RuntimeError):
                c.initialize()

    def test_home_is_isolated_to_a_private_temp_dir_and_removed_on_close(self):
        """N-1: without this, every preflight spawn of leanproxy-mcp writes
        ballast tool caches into the operator's real ~/.config/leanproxy and
        rewrites the live daemon's status file — on the refusal path too.
        Mirrors tests/bench/e2e/client.go's TestDialIsolatesHomeDirectory."""
        real_home = os.path.expanduser("~")
        c = abbench.McpStdio(sys.executable, ["-c", HOME_REPORTING_MCP_SERVER], timeout=10)
        with c:
            c.initialize()
            seen_home = c.call_tool_text("whoami", {})
            isolated_dir = c._home_dir
            self.assertTrue(isolated_dir, "McpStdio did not record an isolated HOME dir")
            self.assertEqual(seen_home, isolated_dir,
                              "subprocess did not see the isolated HOME")
            self.assertNotEqual(os.path.realpath(isolated_dir), os.path.realpath(real_home))
            self.assertTrue(os.path.isdir(isolated_dir))
        self.assertFalse(os.path.exists(isolated_dir),
                          "isolated HOME dir was not cleaned up on close")

    def test_home_is_isolated_even_when_the_subprocess_fails_to_start(self):
        """The refusal path: a server that cannot even be spawned must still
        leave no isolated-home directory behind."""
        c = abbench.McpStdio("/nonexistent/leanproxy-does-not-exist", [], timeout=10)
        with self.assertRaises(Exception):
            with c:
                pass
        self.assertIsNone(c._home_dir)

    def test_each_instance_gets_its_own_isolated_home(self):
        with abbench.McpStdio(sys.executable, ["-c", HOME_REPORTING_MCP_SERVER], timeout=10) as c1:
            c1.initialize()
            home1 = c1.call_tool_text("whoami", {})
        with abbench.McpStdio(sys.executable, ["-c", HOME_REPORTING_MCP_SERVER], timeout=10) as c2:
            c2.initialize()
            home2 = c2.call_tool_text("whoami", {})
        self.assertNotEqual(home1, home2)


class TestInventories(unittest.TestCase):
    """The pre-spend reachability probes, against the same fake server."""

    def _entry(self, tools):
        return {"command": sys.executable,
                "args": ["-c", FAKE_MCP_SERVER, json.dumps(tools)]}

    def test_native_inventory_prefixes_names_by_server(self):
        names, notes = abbench._native_inventory(
            {"codebase-memory": self._entry(["get_architecture"])})
        self.assertEqual(names, ["codebase-memory_get_architecture"])
        self.assertEqual(notes, [])

    def test_native_inventory_reports_an_unreachable_server_rather_than_passing(self):
        names, notes = abbench._native_inventory(
            {"broken": {"command": sys.executable, "args": ["-c", "raise SystemExit(1)"]}})
        self.assertEqual(names, [])
        self.assertTrue(any("could not be inventoried" in n for n in notes), notes)

    def test_http_servers_are_named_as_not_inventoried_not_silently_skipped(self):
        names, notes = abbench._native_inventory(
            {"context7": {"type": "http", "url": "https://example.invalid"}})
        self.assertEqual(names, [])
        self.assertTrue(any("not inventoried" in n for n in notes), notes)


class TestBallastFixtureOverrideGuard(unittest.TestCase):
    """N-5: --ballast-fixture is an unguarded escape hatch straight back to
    C-1 — it lets Layer 2 use a different ballast definition than the one
    Layer 1 embeds, and TestBallastWeightIsIdenticalAcrossLayers only ever
    checks Layer 2 against DEFAULT_BALLAST_FIXTURE, so a different path is
    invisible to that guard. main() must refuse it unless the operator also
    passes --allow-ballast-fixture-override."""

    def _setup(self, d):
        bob_cfg_path = os.path.join(d, "mcp.json")
        lp_cfg_path = os.path.join(d, "leanproxy_servers.yaml")
        tasks_path = os.path.join(d, "tasks.json")
        alt_fixture_path = os.path.join(d, "alt-ballast.json")
        with open(bob_cfg_path, "w") as fh:
            json.dump({"mcpServers": {}}, fh)
        with open(lp_cfg_path, "w") as fh:
            fh.write(LP_STDIO)
        with open(tasks_path, "w") as fh:
            json.dump({"tasks": [{"id": "t1", "prompt": "p",
                                  "expect_tool": "get_architecture"}]}, fh)
        with open(alt_fixture_path, "w") as fh:
            json.dump({"description": "x" * 10, "description_chars": 10}, fh)
        return bob_cfg_path, lp_cfg_path, tasks_path, alt_fixture_path

    def _run(self, d, extra=()):
        import unittest.mock as mock
        bob_cfg_path, lp_cfg_path, tasks_path, alt_fixture_path = self._setup(d)
        with mock.patch.object(abbench, "run_task") as run_task_mock, \
             mock.patch.object(abbench, "read_task_result",
                               return_value={"turns": 1, "output_tokens": 1,
                                             "input_tokens": 1, "cost_usd": 0.0,
                                             "succeeded": True}), \
             mock.patch.dict(os.environ, {"LEANPROXY_AB_LIVE": "1"}):
            rc = abbench.main(["--out", os.path.join(d, "out"),
                               "--bob-config", bob_cfg_path,
                               "--lp-config", lp_cfg_path,
                               "--db", os.path.join(d, "bob.db"),
                               "--tasks", tasks_path, "--cwd", d,
                               "--leanproxy-bin", "/usr/bin/true",
                               "--skip-preflight", "--ballast-points", "0",
                               "--ballast-fixture", alt_fixture_path, *extra])
        return rc, run_task_mock

    def test_a_non_default_fixture_refuses_without_the_override_flag(self):
        with tempfile.TemporaryDirectory() as d:
            rc, run_task_mock = self._run(d)
            self.assertEqual(rc, 2)
            run_task_mock.assert_not_called()

    def test_the_override_flag_permits_a_non_default_fixture(self):
        with tempfile.TemporaryDirectory() as d:
            rc, run_task_mock = self._run(d, extra=("--allow-ballast-fixture-override",))
            self.assertEqual(rc, 0)
            self.assertTrue(run_task_mock.called)

    def test_the_default_fixture_needs_no_override_flag(self):
        with tempfile.TemporaryDirectory() as d:
            import unittest.mock as mock
            bob_cfg_path, lp_cfg_path, tasks_path, _ = self._setup(d)
            with mock.patch.object(abbench, "run_task") as run_task_mock, \
                 mock.patch.object(abbench, "read_task_result",
                                   return_value={"turns": 1, "output_tokens": 1,
                                                 "input_tokens": 1, "cost_usd": 0.0,
                                                 "succeeded": True}), \
                 mock.patch.dict(os.environ, {"LEANPROXY_AB_LIVE": "1"}):
                rc = abbench.main(["--out", os.path.join(d, "out"),
                                   "--bob-config", bob_cfg_path,
                                   "--lp-config", lp_cfg_path,
                                   "--db", os.path.join(d, "bob.db"),
                                   "--tasks", tasks_path, "--cwd", d,
                                   "--leanproxy-bin", "/usr/bin/true",
                                   "--skip-preflight", "--ballast-points", "0"])
            self.assertEqual(rc, 0)
            self.assertTrue(run_task_mock.called)


class TestDirtyRepoGuard(unittest.TestCase):
    """I-3: the sweep drives 30 unsupervised, write-capable agent sessions in
    --cwd. On a dirty tree there is no way to tell afterwards what it did."""

    def _setup(self, d):
        bob_cfg_path = os.path.join(d, "mcp.json")
        lp_cfg_path = os.path.join(d, "leanproxy_servers.yaml")
        tasks_path = os.path.join(d, "tasks.json")
        with open(bob_cfg_path, "w") as fh:
            json.dump({"mcpServers": {}}, fh)
        with open(lp_cfg_path, "w") as fh:
            fh.write(LP_STDIO)
        with open(tasks_path, "w") as fh:
            json.dump({"tasks": [{"id": "t1", "prompt": "p",
                                  "expect_tool": "get_architecture"}]}, fh)
        return bob_cfg_path, lp_cfg_path, tasks_path

    def _run(self, d, extra=()):
        import unittest.mock as mock
        bob_cfg_path, lp_cfg_path, tasks_path = self._setup(d)
        with mock.patch.object(abbench, "run_task") as run_task_mock, \
             mock.patch.object(abbench, "read_task_result",
                               return_value={"turns": 1, "output_tokens": 1,
                                             "input_tokens": 1, "cost_usd": 0.0,
                                             "succeeded": True}), \
             mock.patch.dict(os.environ, {"LEANPROXY_AB_LIVE": "1"}):
            rc = abbench.main(["--out", os.path.join(d, "out"),
                               "--bob-config", bob_cfg_path,
                               "--lp-config", lp_cfg_path,
                               "--db", os.path.join(d, "bob.db"),
                               "--tasks", tasks_path, "--cwd", d,
                               "--leanproxy-bin", "/usr/bin/true",
                               "--skip-preflight", "--ballast-points", "0", *extra])
        return rc, run_task_mock

    def test_dirty_tree_refuses_before_spending(self):
        import unittest.mock as mock
        with tempfile.TemporaryDirectory() as d:
            with mock.patch.object(abbench, "git_status", return_value=" M scripts/abbench.py\n"):
                rc, run_task_mock = self._run(d)
            self.assertEqual(rc, 2)
            run_task_mock.assert_not_called()

    def test_dirty_tree_can_be_overridden_explicitly(self):
        import unittest.mock as mock
        with tempfile.TemporaryDirectory() as d:
            with mock.patch.object(abbench, "git_status", return_value=" M x\n"):
                rc, run_task_mock = self._run(d, extra=("--allow-dirty-repo",))
            self.assertEqual(rc, 0)
            self.assertTrue(run_task_mock.called)

    def test_a_non_repo_cwd_is_not_treated_as_dirty(self):
        with tempfile.TemporaryDirectory() as d:
            rc, run_task_mock = self._run(d)
            self.assertEqual(rc, 0)
            self.assertTrue(run_task_mock.called)


class TestDisabledEntryNameCollision(unittest.TestCase):
    """N-3: detect_confound deliberately skips disabled Bob entries (they are
    exactly what the native arm's directly-attached copy is meant to
    replace), so a disabled entry whose name collides with a proxied server
    passes the confound check and the whole preflight, then used to blow up
    with a raw traceback in arm_config's native-arm _attach — main() must
    turn that into the same clean, exit-2 refusal as every other check on
    this path, not a traceback."""

    def _run(self, d):
        import unittest.mock as mock
        bob_cfg_path = os.path.join(d, "mcp.json")
        lp_cfg_path = os.path.join(d, "leanproxy_servers.yaml")
        tasks_path = os.path.join(d, "tasks.json")
        with open(bob_cfg_path, "w") as fh:
            # "codebase-memory" collides with LP_STDIO's proxied server name,
            # but is disabled — detect_confound and preflight both pass it.
            json.dump({"mcpServers": {"codebase-memory": {"command": "/bin/false",
                                                           "disabled": True}}}, fh)
        with open(lp_cfg_path, "w") as fh:
            fh.write(LP_STDIO)
        with open(tasks_path, "w") as fh:
            json.dump({"tasks": [{"id": "t1", "prompt": "p",
                                  "expect_tool": "get_architecture"}]}, fh)
        with mock.patch.object(abbench, "run_task") as run_task_mock, \
             mock.patch.dict(os.environ, {"LEANPROXY_AB_LIVE": "1"}):
            rc = abbench.main(["--out", os.path.join(d, "out"),
                               "--bob-config", bob_cfg_path,
                               "--lp-config", lp_cfg_path,
                               "--db", os.path.join(d, "bob.db"),
                               "--tasks", tasks_path, "--cwd", d,
                               "--leanproxy-bin", "/usr/bin/true",
                               "--skip-preflight", "--ballast-points", "0"])
        return rc, run_task_mock

    def test_collision_with_a_disabled_entry_refuses_cleanly_instead_of_crashing(self):
        with tempfile.TemporaryDirectory() as d:
            rc, run_task_mock = self._run(d)
            self.assertEqual(rc, 2)
            run_task_mock.assert_not_called()


class TestCostsAreNeverInventedAsZero(unittest.TestCase):
    """Review Important #1: Bob writes `tasks.costs` asynchronously, and the
    spec's own audit found it populated on only 270 of 351 real tasks. The
    old `costs = json.loads(row[0]) if row and row[0] else {}` followed by
    `.get(field, 0)` turned "no measurement exists" into "a measured $0 run
    with 0 input tokens", which then entered paired_deltas at face value and
    dragged abreport's observed-token means. This is the same silent-zero
    class the Go side fixed with pointer-typed Succeeded/CostUSD."""

    def _db(self, d, costs="__default__"):
        import sqlite3
        p = os.path.join(d, "bob.db")
        conn = sqlite3.connect(p)
        conn.execute("create table tasks (id text, costs text)")
        conn.execute(
            "create table messages (task_id text, role text, data text, created_at int)")
        if costs == "__default__":
            costs = json.dumps({"input": 1000, "output": 50, "cacheRead": 800,
                                "cacheWrite": 200, "cost": 0.21,
                                "contextTokens": 900})
        if costs is not None:
            conn.execute("insert into tasks values (?,?)", ("t1", costs))
        rows = [("tool", _tool_message("codebase-memory_get_architecture")),
                ("assistant", json.dumps({"role": "assistant"})),
                ("assistant", json.dumps({"role": "assistant"}))]
        for i, (role, data) in enumerate(rows):
            conn.execute("insert into messages values (?,?,?,?)", ("t1", role, data, i))
        conn.commit()
        conn.close()
        return p

    def test_raises_when_the_task_row_never_carries_costs(self):
        """A row whose costs column is NULL must raise, so main's existing
        handler records it as a failure with an error, rather than returning
        a full record of zeros that reads as a real $0 measurement."""
        with tempfile.TemporaryDirectory() as d:
            db = self._db(d, costs=None)
            with self.assertRaises(RuntimeError) as ctx:
                abbench.read_task_result(db, "t1", "get_architecture",
                                         costs_timeout=0.3)
            self.assertIn("costs", str(ctx.exception))

    def test_raises_when_costs_is_an_empty_object(self):
        with tempfile.TemporaryDirectory() as d:
            db = self._db(d, costs="{}")
            with self.assertRaises(RuntimeError):
                abbench.read_task_result(db, "t1", "get_architecture",
                                         costs_timeout=0.3)

    def test_polls_until_bob_finishes_writing_costs(self):
        """The write is asynchronous: the row can exist with empty costs at
        the moment run_task returns. Waiting is the correct response to a
        race, not recording zeros."""
        import sqlite3
        import threading

        with tempfile.TemporaryDirectory() as d:
            db = self._db(d, costs="{}")

            def finish_write():
                time.sleep(0.4)
                conn = sqlite3.connect(db)
                conn.execute(
                    "update tasks set costs = ? where id = ?",
                    (json.dumps({"input": 1234, "output": 56, "cost": 0.42}), "t1"),
                )
                conn.commit()
                conn.close()

            t = threading.Thread(target=finish_write)
            t.start()
            try:
                res = abbench.read_task_result(db, "t1", "get_architecture",
                                               costs_timeout=10.0)
            finally:
                t.join()

            self.assertEqual(res["input_tokens"], 1234)
            self.assertEqual(res["output_tokens"], 56)
            self.assertEqual(res["cost_usd"], 0.42)

    def test_omits_fields_absent_from_a_populated_costs_object(self):
        """A costs object that exists but lacks a field must OMIT that field,
        not default it to 0: paired_deltas already drops a pair whose record
        has no such key, and abreport's _is_scored already treats a record
        with no output_tokens as unscored. A zero would instead be averaged
        in as a real datum."""
        with tempfile.TemporaryDirectory() as d:
            db = self._db(d, costs=json.dumps({"input": 10, "output": 5}))
            res = abbench.read_task_result(db, "t1", "get_architecture",
                                           costs_timeout=0.3)
            self.assertEqual(res["input_tokens"], 10)
            self.assertEqual(res["output_tokens"], 5)
            self.assertNotIn("cost_usd", res)
            self.assertNotIn("context_tokens", res)

    def test_a_run_with_unwritten_costs_is_recorded_as_a_failure_not_a_zero(self):
        """End-to-end at the main() level: the sweep must persist a failure
        record with an error, and must NOT persist cost_usd/input_tokens of
        zero for a run whose costs Bob never wrote."""
        import unittest.mock as mock

        with tempfile.TemporaryDirectory() as d:
            bob_cfg_path = os.path.join(d, "mcp.json")
            lp_cfg_path = os.path.join(d, "leanproxy_servers.yaml")
            out_dir = os.path.join(d, "out")
            tasks_path = os.path.join(d, "tasks.json")
            db_path = self._db(d, costs=None)

            with open(bob_cfg_path, "w") as fh:
                json.dump({"mcpServers": {}}, fh)
            with open(lp_cfg_path, "w") as fh:
                fh.write(LP_STDIO)
            with open(tasks_path, "w") as fh:
                json.dump({"tasks": [{"id": "t1", "prompt": "one",
                                      "expect_tool": "get_architecture"}]}, fh)

            with mock.patch.object(abbench, "run_task", return_value="t1"), \
                 mock.patch.object(abbench, "COSTS_TIMEOUT_SECONDS", 0.3), \
                 mock.patch.dict(os.environ, {"LEANPROXY_AB_LIVE": "1"}):
                rc = abbench.main(["--out", out_dir, "--bob-config", bob_cfg_path,
                                   "--lp-config", lp_cfg_path, "--db", db_path,
                                   "--tasks", tasks_path, "--cwd", d,
                                   "--leanproxy-bin", "/usr/bin/true",
                                   "--skip-preflight", "--ballast-points", "0"])

            self.assertEqual(rc, 0)
            with open(os.path.join(out_dir, os.listdir(out_dir)[0])) as fh:
                persisted = json.load(fh)
            self.assertEqual(len(persisted), 3)
            for rec in persisted:
                self.assertFalse(rec["succeeded"])
                self.assertIn("error", rec)
                self.assertNotIn("cost_usd", rec)
                self.assertNotIn("input_tokens", rec)


class TestPairedSummaryExcludesFailedRuns(unittest.TestCase):
    """Review Important #2: abbench's own failure note claimed a failed run
    "has no cost/turn metrics, so it is EXCLUDED from the paired-delta
    comparison above". That is true only for a run that CRASHED. A run that
    completed but never found the expected tool carries full cost/turn
    fields and was averaged straight into the summary — the exact class
    abreport excludes (bc3025f), and the one that lets a badly-failing arm
    look cheapest because a model that gives up early posts a low turn
    count."""

    def _rec(self, arm, task, **kw):
        r = {"layer": "live", "origin": "measured", "arm": arm, "task": task,
             "ballast_tools": 0, "turns": 5, "output_tokens": 100,
             "cost_usd": 1.0, "succeeded": True}
        r.update(kw)
        return r

    def test_a_completed_but_unsuccessful_run_is_not_paired(self):
        """native/t1 and router/t1 both carry metrics, but router gave up
        early (succeeded False, turns 1). Pairing them would report the
        give-up as a 4-turn saving."""
        records = [
            self._rec("native", "t1@0", turns=5, cost_usd=1.0),
            self._rec("router", "t1@0", turns=1, cost_usd=0.1, succeeded=False),
            self._rec("native", "t2@0", turns=5, cost_usd=1.0),
            self._rec("router", "t2@0", turns=6, cost_usd=1.2),
        ]
        lines = abbench.paired_summary_lines(records)
        turns_lines = [l for l in lines if "router" in l and "turns" in l]
        self.assertEqual(len(turns_lines), 1)
        self.assertIn("pairs=1", turns_lines[0])
        self.assertNotIn("-4", turns_lines[0])

    def test_summary_lines_report_the_success_rate_behind_each_verdict(self):
        records = [
            self._rec("native", "t1@0"),
            self._rec("router", "t1@0", succeeded=False),
            self._rec("native", "t2@0"),
            self._rec("router", "t2@0"),
        ]
        lines = abbench.paired_summary_lines(records)
        router_lines = [l for l in lines if "router" in l]
        self.assertTrue(router_lines)
        for line in router_lines:
            self.assertIn("50%", line)

    def test_a_crashed_run_without_metrics_is_still_excluded(self):
        records = [
            self._rec("native", "t1@0"),
            {"layer": "live", "arm": "router", "task": "t1@0", "ballast_tools": 0,
             "succeeded": False, "error": "no new task row appeared"},
            self._rec("native", "t2@0"),
            self._rec("router", "t2@0"),
        ]
        lines = abbench.paired_summary_lines(records)
        turns_lines = [l for l in lines if "router" in l and "turns" in l]
        self.assertEqual(len(turns_lines), 1)
        self.assertIn("pairs=1", turns_lines[0])

    def test_failure_note_does_not_claim_failed_runs_lack_metrics(self):
        """The note must describe what actually happens now — both kinds of
        failure are excluded — instead of asserting a reason ("has no
        cost/turn metrics") that is false for the completed-but-failed
        kind."""
        records = [
            self._rec("native", "t1@0"),
            self._rec("router", "t1@0", succeeded=False),
        ]
        note = "\n".join(abbench.failure_note_lines(records))
        self.assertNotIn("has no cost/turn metrics", note)
        self.assertIn("router/t1@0", note)


class TestLeanproxyBinaryProvenance(unittest.TestCase):
    """Review Important #3: Layer 2 defaults to ~/.local/bin/leanproxy-mcp
    while Layer 1 builds from source, and Layer 3 joins the two on
    ballast_tools alone. Nothing recorded which binary produced a live run,
    so the join could silently multiply a working-tree residency figure by
    turn counts from a different proxy build — the same cross-layer
    divergence class as reviews C-1/C-2, with no guard."""

    def test_provenance_identifies_the_binary_by_content(self):
        with tempfile.TemporaryDirectory() as d:
            p = os.path.join(d, "leanproxy-mcp")
            with open(p, "w") as fh:
                fh.write("#!/bin/sh\nexit 0\n")
            os.chmod(p, 0o755)

            prov = abbench.binary_provenance(p)
            self.assertEqual(prov["leanproxy_bin"], os.path.abspath(p))
            self.assertEqual(len(prov["leanproxy_sha256"]), 64)

            with open(p, "w") as fh:
                fh.write("#!/bin/sh\nexit 1\n")
            self.assertNotEqual(prov["leanproxy_sha256"],
                                abbench.binary_provenance(p)["leanproxy_sha256"])

    def test_missing_binary_refuses_before_any_run(self):
        import unittest.mock as mock

        with tempfile.TemporaryDirectory() as d:
            bob_cfg_path = os.path.join(d, "mcp.json")
            lp_cfg_path = os.path.join(d, "leanproxy_servers.yaml")
            tasks_path = os.path.join(d, "tasks.json")
            with open(bob_cfg_path, "w") as fh:
                json.dump({"mcpServers": {}}, fh)
            with open(lp_cfg_path, "w") as fh:
                fh.write(LP_STDIO)
            with open(tasks_path, "w") as fh:
                json.dump({"tasks": [{"id": "t1", "prompt": "p",
                                      "expect_tool": "get_architecture"}]}, fh)

            with mock.patch.object(abbench, "run_task") as run_task_mock, \
                 mock.patch.dict(os.environ, {"LEANPROXY_AB_LIVE": "1"}):
                rc = abbench.main(["--out", os.path.join(d, "out"),
                                   "--bob-config", bob_cfg_path,
                                   "--lp-config", lp_cfg_path,
                                   "--db", os.path.join(d, "bob.db"),
                                   "--tasks", tasks_path, "--cwd", d,
                                   "--leanproxy-bin", os.path.join(d, "nope"),
                                   "--skip-preflight", "--ballast-points", "0"])

            self.assertEqual(rc, 2)
            run_task_mock.assert_not_called()

    def test_every_live_record_carries_the_binary_hash(self):
        import unittest.mock as mock

        with tempfile.TemporaryDirectory() as d:
            bob_cfg_path = os.path.join(d, "mcp.json")
            lp_cfg_path = os.path.join(d, "leanproxy_servers.yaml")
            out_dir = os.path.join(d, "out")
            tasks_path = os.path.join(d, "tasks.json")
            db_path = os.path.join(d, "bob.db")

            with open(bob_cfg_path, "w") as fh:
                json.dump({"mcpServers": {}}, fh)
            with open(lp_cfg_path, "w") as fh:
                fh.write(LP_STDIO)
            with open(tasks_path, "w") as fh:
                json.dump({"tasks": [{"id": "t1", "prompt": "one",
                                      "expect_tool": "get_architecture"}]}, fh)

            def fake_read_task_result(db, task_id, expect_tool, costs_timeout=None):
                return {"task_id": task_id, "input_tokens": 1, "output_tokens": 1,
                        "cache_read": 0, "cache_write": 0, "cost_usd": 0.1,
                        "context_tokens": 10, "turns": 2, "succeeded": True}

            with mock.patch.object(abbench, "run_task", return_value="task-1"), \
                 mock.patch.object(abbench, "read_task_result",
                                   side_effect=fake_read_task_result), \
                 mock.patch.dict(os.environ, {"LEANPROXY_AB_LIVE": "1"}):
                rc = abbench.main(["--out", out_dir, "--bob-config", bob_cfg_path,
                                   "--lp-config", lp_cfg_path, "--db", db_path,
                                   "--tasks", tasks_path, "--cwd", d,
                                   "--leanproxy-bin", "/usr/bin/true",
                                   "--skip-preflight", "--ballast-points", "0"])

            self.assertEqual(rc, 0)
            with open(os.path.join(out_dir, os.listdir(out_dir)[0])) as fh:
                persisted = json.load(fh)
            self.assertEqual(len(persisted), 3)
            expected = abbench.binary_provenance("/usr/bin/true")
            for rec in persisted:
                self.assertEqual(rec["leanproxy_sha256"], expected["leanproxy_sha256"])
                self.assertEqual(rec["leanproxy_bin"], expected["leanproxy_bin"])


if __name__ == "__main__":
    unittest.main()
