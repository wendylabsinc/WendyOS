"""Exercise the pin guard with real Git history and no network access."""

import os
from pathlib import Path
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[2] / "Proto/shared-grpc-api-contracts.sh"
REMOTE = "https://github.com/wendylabsinc/service-protos.git"


class ContractAncestryTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.source = self.root / "source"
        self.source.mkdir()
        self.git("init", "--quiet", "--initial-branch=master")
        self.git("config", "user.name", "Fixture")
        self.git("config", "user.email", "fixture@example.invalid")
        self.git("config", "commit.gpgsign", "false")
        self.git("commit", "--quiet", "--allow-empty", "-m", "root")
        self.pin = self.git("rev-parse", "HEAD").stdout.strip()
        self.git("commit", "--quiet", "--allow-empty", "-m", "master tip")
        self.git("checkout", "--quiet", "-b", "unmerged", self.pin)
        self.git("commit", "--quiet", "--allow-empty", "-m", "branch only")
        self.unmerged = self.git("rev-parse", "HEAD").stdout.strip()
        self.git("checkout", "--quiet", "master")
        self.env = os.environ.copy()
        self.env.update({
            "GIT_CONFIG_COUNT": "1",
            "GIT_CONFIG_KEY_0": f"url.{self.source.as_uri()}.insteadOf",
            "GIT_CONFIG_VALUE_0": REMOTE,
            "GIT_TERMINAL_PROMPT": "0",
            "TMPDIR": str(self.root),
        })

    def git(self, *args):
        return subprocess.run(
            ["git", "-C", str(self.source), *args],
            check=True, capture_output=True, text=True,
        )

    def check_pin(self, pin, expected, message="", cwd=None):
        before = set(self.root.iterdir())
        result = subprocess.run(
            ["bash", "-c", 'source "$1"; SHARED_GRPC_API_CONTRACTS_REF="$2"; '
             'check_shared_grpc_api_contracts_ancestry', "guard", str(SCRIPT), pin],
            env=self.env, cwd=cwd, capture_output=True, text=True,
        )
        self.assertEqual(result.returncode == 0, expected, result.stderr)
        self.assertIn(message, result.stderr)
        self.assertEqual(set(self.root.iterdir()), before, "temporary Git repo leaked")

    def test_merged_ancestor_passes(self):
        self.check_pin(self.pin, True)

    def test_master_tip_passes(self):
        self.check_pin(self.git("rev-parse", "master").stdout.strip(), True)

    def test_branch_only_pin_fails(self):
        self.check_pin(self.unmerged, False, "not an ancestor")

    def test_shallow_consumer_and_wrong_local_master_do_not_control_check(self):
        consumer = self.root / "consumer"
        subprocess.run(
            ["git", "clone", "--quiet", "--depth=1", "--branch=unmerged",
             self.source.as_uri(), str(consumer)], check=True,
        )
        subprocess.run(["git", "-C", str(consumer), "branch", "master"], check=True)
        shallow = subprocess.check_output(
            ["git", "-C", str(consumer), "rev-parse", "--is-shallow-repository"], text=True,
        ).strip()
        self.assertEqual(shallow, "true")
        self.check_pin(self.pin, True, cwd=consumer)
        self.check_pin(self.unmerged, False, "not an ancestor", cwd=consumer)

    def test_empty_and_invalid_pins_fail(self):
        for pin in ("", "master", self.pin[:8], "g" * 40, "-" * 40):
            with self.subTest(pin=pin):
                self.check_pin(pin, False, "full 40-character commit SHA")

    def test_unknown_commit_fails(self):
        self.check_pin("0" * 40, False, "could not fetch")

    def test_missing_master_fails_even_when_main_contains_pin(self):
        self.git("branch", "-m", "master", "main")
        self.check_pin(self.pin, False, "could not fetch")

    def test_fetch_failure_fails(self):
        self.env["GIT_CONFIG_KEY_0"] = f"url.{(self.root / 'missing').as_uri()}.insteadOf"
        self.check_pin(self.pin, False, "could not fetch")


if __name__ == "__main__":
    unittest.main()
