import contextlib
import io
import json
import os
from pathlib import Path
import select
import tempfile
import subprocess
import unittest
from unittest import mock

from E2EDeviceCache import main, prepare


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
assert sys.argv[1:] == ["--json", "--device", "orin.local", "device", "info", "--read-only"]
mode = json.loads((config / "config.json").read_text()).get("testMode")
if mode == "reject":
    sys.exit(7)
if mode == "hang":
    import time
    time.sleep(10)
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
        self.auth_config = {"devicePins": {"orin": {"principal": "spiffe://wendy.sh/tenant/11111111-1111-1111-1111-111111111111/device/test-orin"}}}
        self.auth.write_text(json.dumps(self.auth_config))
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
        for change in [{"hostname": "other.local"}, {"ip": "bad"}, {"port": 0}]:
            variants.append({**valid, "devices": [{**valid["devices"][0], **change}]})
        self.output.unlink()
        for document in variants:
            with self.subTest(document=document):
                self.fixture.write_text(json.dumps(document))
                with self.assertRaises(ValueError):
                    self.prepare()
                self.assertFalse(self.output.exists())

    def test_failed_preflight_does_not_publish_fixture_and_cleans_auth_home(self):
        self.auth.write_text(json.dumps({**self.auth_config, "testMode": "reject"}))
        with self.assertRaises(subprocess.CalledProcessError):
            self.prepare()
        self.assertFalse(self.fixture.exists())
        self.assertFalse(self.output.exists())
        self.assertFalse(Path(self.cli.with_suffix(".home").read_text()).exists())

    def test_timed_out_preflight_fails_loudly_and_cleans_auth_home(self):
        self.auth.write_text(json.dumps({**self.auth_config, "testMode": "hang"}))
        stderr = io.StringIO()
        arguments = ["E2EDeviceCache.py", "--cli", str(self.cli), "--auth-config", str(self.auth),
                     "--target", "orin.local", "--fixture", str(self.fixture), "--output", str(self.output)]
        with mock.patch("E2EDeviceCache.PREFLIGHT_TIMEOUT_SECONDS", 1), \
                mock.patch("sys.argv", arguments), contextlib.redirect_stderr(stderr):
            self.assertEqual(main(), 1)
        self.assertIn("timed out", stderr.getvalue())
        self.assertFalse(self.fixture.exists())
        self.assertFalse(self.output.exists())
        self.assertFalse(Path(self.cli.with_suffix(".home").read_text()).exists())

    def test_cache_bits_and_aliases_cannot_supply_a_missing_target_binding(self):
        self.prepare()
        self.output.unlink()
        for config in [{}, {"devicePins": {"other": self.auth_config["devicePins"]["orin"]}}]:
            with self.subTest(config=config):
                self.auth.write_text(json.dumps(config))
                with self.assertRaisesRegex(ValueError, "existing saved device principal"):
                    self.prepare()
                self.assertFalse(self.output.exists())


@unittest.skipUnless(
    os.environ.get("WENDY_E2E_CACHE_TEST_CLI") and os.environ.get("WENDY_E2E_CACHE_TEST_SERVER"),
    "set explicit built CLI and plaintext test server paths for the real regression",
)
class RealDeviceCacheTests(unittest.TestCase):
    def test_plaintext_cache_bit_never_supplies_authentication(self):
        server = subprocess.Popen([os.environ["WENDY_E2E_CACHE_TEST_SERVER"]], stdout=subprocess.PIPE, text=True)
        try:
            self.assertTrue(select.select([server.stdout], [], [], 5)[0], "server did not start")
            port = int(server.stdout.readline().strip().rsplit(":", 1)[1])
            target = f"cache-test.local:{port}"
            cache = {"version": 1, "devices": [{
                "id": "cache-test", "displayName": "cache-test", "hostname": "cache-test.local",
                "ip": "127.0.0.1", "port": port, "mtls": True, "lastSeen": "2026-11-13T00:00:00Z",
            }]}
            with tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                config = root / ".wendy"
                config.mkdir()
                auth = config / "config.json"
                auth.write_text("{}")
                (config / "devices.json").write_text(json.dumps(cache))
                fixture = root / "fixture.json"
                fixture.write_text(json.dumps({**cache, "e2eTarget": target}))
                output = root / "snapshot.json"
                cli = os.environ["WENDY_E2E_CACHE_TEST_CLI"]
                env = {k: v for k, v in os.environ.items() if not k.startswith("WENDY_")}
                env.update(HOME=str(root), WENDY_CONFIG_DIR=str(config), WENDY_ANALYTICS="false")
                # Positive control: the real CLI can read the plaintext server
                # with {} even though the cached metadata says mTLS.
                result = subprocess.run([cli, "--json", "--device", target, "device", "info", "--read-only"],
                                        env=env, capture_output=True, text=True, timeout=10)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(json.loads(result.stdout)["version"], "test-plaintext")
                with self.assertRaisesRegex(ValueError, "existing saved device principal"):
                    prepare(cli, auth, target, fixture, output)
                self.assertFalse(output.exists())
                # A binding from the supplied config makes the CLI itself refuse
                # this same plaintext endpoint; the helper never manufactures it.
                auth.write_text(json.dumps({"devicePins": {"cache-test": {
                    "principal": "spiffe://wendy.sh/tenant/11111111-1111-1111-1111-111111111111/device/expected",
                }}}))
                with self.assertRaises(subprocess.CalledProcessError):
                    prepare(cli, auth, target, fixture, output)
                self.assertFalse(output.exists())
        finally:
            server.terminate()
            try:
                server.wait(timeout=5)
            except subprocess.TimeoutExpired:
                server.kill()
                server.wait(timeout=5)
            server.stdout.close()


if __name__ == "__main__":
    unittest.main()
