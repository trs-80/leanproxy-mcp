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


if __name__ == "__main__":
    unittest.main()
