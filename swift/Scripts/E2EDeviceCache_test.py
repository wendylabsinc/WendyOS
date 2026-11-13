import contextlib
import io
import json
import os
from pathlib import Path
import tempfile
import subprocess
import unittest

from E2EDeviceCache import prepare


class DeviceCacheTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.cli = self.root / "wendy"
        self.cli.write_text('''#!/usr/bin/env python3
import json, os, pathlib, sys
home = pathlib.Path(os.environ["HOME"])
pathlib.Path(__file__).with_suffix(".home").write_text(str(home))
config = home / ".wendy"
assert os.environ["WENDY_CONFIG_DIR"] == str(config)
assert sys.argv[1:] == ["--json", "--device", "orin.local", "device", "info"]
if (config / "config.json").read_text() == "reject":
    sys.exit(7)
files = sorted(p.name for p in config.iterdir())
seed = json.loads((config / "devices.json").read_text()) if "devices.json" in files else None
entry = {"id": "orin", "hostname": "orin.local", "ip": "192.0.2.1", "port": 50052,
         "mtls": True, "orgId": 42, "interfaceName": "en0", "lastSeen": "2026-11-13T00:00:00Z"}
(config / "devices.json").write_text(json.dumps({"version": 1, "devices": [entry,
    {**entry, "id": "other", "hostname": "other.local"}]}))
(config / "tls-sessions").mkdir()
(config / "tls-sessions" / "session.tlssession").write_text("must not be copied")
print(json.dumps({"version": "test", "preflightHome": str(home), "files": files, "seed": seed}))
''')
        self.cli.chmod(0o700)
        self.auth = self.root / "auth.json"
        self.auth.write_text("dedicated test auth")
        self.fixture = self.root / "fixture.json"
        self.output = self.root / "run" / "cache.json"

    def prepare(self):
        stdout = io.StringIO()
        with contextlib.redirect_stdout(stdout):
            prepare(str(self.cli), self.auth, "orin.local", self.fixture, self.output)
        return json.loads(stdout.getvalue())

    def test_missing_fixture_is_prepared_without_importing_inventory_or_tls(self):
        observed = self.prepare()
        self.assertEqual(observed["files"], ["config.json"])
        self.assertFalse(Path(observed["preflightHome"]).exists())
        fixture = json.loads(self.fixture.read_text())
        self.assertEqual(fixture["e2eTarget"], "orin.local")
        self.assertEqual(len(fixture["devices"]), 1)
        self.assertEqual(fixture["devices"][0]["orgId"], 42)
        self.assertEqual(fixture["devices"][0]["interfaceName"], "en0")
        self.assertEqual(os.stat(self.fixture).st_mode & 0o777, 0o600)
        self.assertEqual(sorted(p.name for p in self.output.parent.iterdir()), ["cache.json"])

    def test_warm_preflight_uses_seed_and_leaves_prepared_fixture_immutable(self):
        self.prepare()
        document = json.loads(self.fixture.read_text())
        document["devices"][0]["ip"] = "192.0.2.99"
        document["devices"][0]["lastSeen"] = "2000-01-01T00:00:00Z"
        self.fixture.write_text(json.dumps(document))
        before = self.fixture.read_bytes()
        observed = self.prepare()
        self.assertEqual(self.fixture.read_bytes(), before)
        self.assertEqual(observed["files"], ["config.json", "devices.json"])
        self.assertEqual(observed["seed"]["devices"][0]["ip"], "192.0.2.99")
        self.assertFalse(Path(observed["preflightHome"]).exists())
        self.assertEqual(json.loads(self.output.read_text())["devices"][0]["ip"], "192.0.2.1")

    def test_invalid_or_foreign_fixture_is_rejected_before_cli(self):
        for document in ["{", '{"version": 2}', json.dumps({
            "version": 1, "e2eTarget": "other.local", "devices": [],
        })]:
            with self.subTest(document=document):
                self.fixture.write_text(document)
                with self.assertRaises((ValueError, KeyError)):
                    self.prepare()
                self.assertFalse(self.output.exists())

    def test_rejects_unusable_entries_and_multiple_device_inventory(self):
        self.prepare()
        valid = json.loads(self.fixture.read_text())
        variants = [
            {**valid, "version": 2},
            {**valid, "devices": valid["devices"] * 2},
        ]
        for change in [{"hostname": "other.local"}, {"ip": "bad"}, {"port": 0}, {"mtls": False}]:
            variants.append({**valid, "devices": [{**valid["devices"][0], **change}]})
        self.output.unlink()
        for document in variants:
            with self.subTest(document=document):
                self.fixture.write_text(json.dumps(document))
                with self.assertRaises(ValueError):
                    self.prepare()
                self.assertFalse(self.output.exists())

    def test_failed_preflight_does_not_publish_fixture_and_cleans_auth_home(self):
        self.auth.write_text("reject")
        with self.assertRaises(subprocess.CalledProcessError):
            self.prepare()
        self.assertFalse(self.fixture.exists())
        self.assertFalse(self.output.exists())
        self.assertFalse(Path(self.cli.with_suffix(".home").read_text()).exists())


if __name__ == "__main__":
    unittest.main()
