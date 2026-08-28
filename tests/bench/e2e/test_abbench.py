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


if __name__ == "__main__":
    unittest.main()
