"""Tests for scripts/ab_sweep_config.py — the pre-sweep config preparation.

The script exists because abbench refuses to spend anything against a
configuration it cannot measure, and the two refusals an operator actually
hits are environmental, not code: a server loaded both directly and through
the proxy (double-counted schema weight in every arm), and a proxied HTTP
server whose credentials the harness will not copy into the agent's config
(the native arm could not reach it, so it would measure a smaller inventory
than the proxy arms). Both are fixed by disabling entries for the duration of
the sweep, and both must be put back afterwards.
"""

import importlib.util
import json
import os
import pathlib
import stat
import tempfile
import unittest

_ROOT = pathlib.Path(__file__).resolve().parents[3]


def _load(name, relpath):
    spec = importlib.util.spec_from_file_location(name, _ROOT / relpath)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


abbench = _load("abbench", "scripts/abbench.py")
sweepcfg = _load("ab_sweep_config", "scripts/ab_sweep_config.py")


LP_CONFOUNDED = """version: "1"
servers:
  - name: codebase-memory
    transport: stdio
    enabled: true
    stdio:
      command: /usr/bin/true
      args: []
  - name: context7
    transport: http
    enabled: true
    http:
      url: https://mcp.context7.com/mcp
      headers:
        CONTEXT7_API_KEY: secret-value
"""

BOB_CONFOUNDED = {
    "mcpServers": {
        "leanproxy": {"command": "/usr/local/bin/leanproxy-mcp",
                      "args": ["server", "run", "--stdio"],
                      "disabled": False, "enabled": True},
        # Same upstream as the proxied 'codebase-memory', different name.
        "codebase-memory-mcp": {"command": "/usr/bin/true",
                                "disabled": False, "enabled": True},
        "context7": {"type": "http", "url": "https://mcp.context7.com/mcp",
                     "disabled": False},
    }
}


class TestSweepConfigPreparation(unittest.TestCase):
    def _configs(self, d):
        lp = os.path.join(d, "leanproxy_servers.yaml")
        bob = os.path.join(d, "mcp.json")
        with open(lp, "w") as fh:
            fh.write(LP_CONFOUNDED)
        os.chmod(lp, 0o600)
        with open(bob, "w") as fh:
            json.dump(BOB_CONFOUNDED, fh, indent=2)
        return lp, bob

    def test_check_reports_both_refusals_before_anything_is_applied(self):
        with tempfile.TemporaryDirectory() as d:
            lp, bob = self._configs(d)
            ready, problems = sweepcfg.check(bob, lp)
            self.assertFalse(ready)
            joined = " ".join(problems)
            self.assertIn("codebase-memory-mcp", joined)
            self.assertIn("context7", joined)

    def test_apply_clears_every_gate_abbench_would_refuse_on(self):
        """The real assertion: after apply, abbench's OWN checks pass. Not a
        restatement of what the script edited — the gates it has to satisfy."""
        with tempfile.TemporaryDirectory() as d:
            lp, bob = self._configs(d)
            sweepcfg.apply(bob, lp)

            lp_text = open(lp).read()
            bob_cfg = json.load(open(bob))

            self.assertEqual(abbench.detect_confound(bob_cfg, lp_text), [])
            direct = abbench.direct_entries_for_proxied_servers(lp_text)
            self.assertEqual(sorted(direct), ["codebase-memory"])

            for arm in abbench.ARMS:
                cfg = abbench.arm_config(
                    arm, "/usr/bin/true", bob_cfg, ballast=None,
                    direct_servers=direct if arm == "native" else None,
                    lp_config_path=lp, log_file=os.path.join(d, "lp.log"))
                active = sorted(n for n, e in cfg["mcpServers"].items()
                                if not e.get("disabled"))
                want = ["codebase-memory"] if arm == "native" else ["leanproxy"]
                self.assertEqual(active, want, f"{arm} arm loads {active}")

    def test_restore_returns_both_files_byte_for_byte(self):
        with tempfile.TemporaryDirectory() as d:
            lp, bob = self._configs(d)
            before = (open(lp, "rb").read(), open(bob, "rb").read())

            sweepcfg.apply(bob, lp)
            self.assertNotEqual(open(lp, "rb").read(), before[0])

            sweepcfg.restore(bob, lp)
            self.assertEqual(open(lp, "rb").read(), before[0])
            self.assertEqual(open(bob, "rb").read(), before[1])

    def test_restore_leaves_no_backup_behind_to_confuse_the_next_sweep(self):
        with tempfile.TemporaryDirectory() as d:
            lp, bob = self._configs(d)
            sweepcfg.apply(bob, lp)
            sweepcfg.restore(bob, lp)
            self.assertFalse(os.path.exists(sweepcfg.backup_path(lp)))
            self.assertFalse(os.path.exists(sweepcfg.backup_path(bob)))

    def test_a_second_apply_refuses_rather_than_overwriting_the_pristine_backup(self):
        """Applying twice must not let the already-edited file become the
        backup: the operator's original would be gone, and restore would put
        the sweep configuration back as if it were production."""
        with tempfile.TemporaryDirectory() as d:
            lp, bob = self._configs(d)
            original = open(lp, "rb").read()
            sweepcfg.apply(bob, lp)

            with self.assertRaises(sweepcfg.AlreadyApplied):
                sweepcfg.apply(bob, lp)

            self.assertEqual(open(sweepcfg.backup_path(lp), "rb").read(), original)

    def test_backup_of_a_credential_bearing_config_is_not_world_readable(self):
        """The leanproxy config holds an API key. Its backup must not widen
        access to it."""
        with tempfile.TemporaryDirectory() as d:
            lp, bob = self._configs(d)
            sweepcfg.apply(bob, lp)
            mode = stat.S_IMODE(os.stat(sweepcfg.backup_path(lp)).st_mode)
            self.assertEqual(mode & 0o077, 0, f"backup mode {oct(mode)} is too open")

    def test_apply_is_a_no_op_on_a_config_that_is_already_clean(self):
        with tempfile.TemporaryDirectory() as d:
            lp = os.path.join(d, "leanproxy_servers.yaml")
            bob = os.path.join(d, "mcp.json")
            with open(lp, "w") as fh:
                fh.write("""version: "1"
servers:
  - name: codebase-memory
    transport: stdio
    enabled: true
    stdio:
      command: /usr/bin/true
      args: []
""")
            with open(bob, "w") as fh:
                json.dump({"mcpServers": {"leanproxy": {"command": "/x",
                                                        "disabled": False}}}, fh)
            before = open(lp, "rb").read()
            changed = sweepcfg.apply(bob, lp)
            self.assertEqual(changed, [])
            self.assertEqual(open(lp, "rb").read(), before)
            self.assertFalse(os.path.exists(sweepcfg.backup_path(lp)))


if __name__ == "__main__":
    unittest.main()
