import json
import os
from pathlib import Path
import stat
import ssl
import subprocess
import tempfile
import time
import unittest
from unittest.mock import patch

from prepare_lan_auth import LANFixtureError, prepare_config


class LANAuthTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.material = tempfile.TemporaryDirectory(prefix="e2e-lan-auth-material-")
        cls.cert = Path(cls.material.name) / "cert.pem"
        cls.key = Path(cls.material.name) / "key.pem"
        subprocess.run([
            "openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
            "-subj", "/CN=e2e-synthetic", "-keyout", str(cls.key), "-out", str(cls.cert),
        ], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        date = subprocess.check_output(["openssl", "x509", "-noout", "-enddate", "-in", str(cls.cert)], text=True)
        cls.expires = ssl.cert_time_to_seconds(date.strip().split("=", 1)[1])

    @classmethod
    def tearDownClass(cls):
        cls.material.cleanup()

    def setUp(self):
        environment = patch.dict(os.environ, {
            "WENDY_CONFIG_DIR": "", "WENDY_AGENT_SOCKET": "",
            "WENDY_E2E_TEST_TIMEOUT_SECONDS": "300",
        })
        environment.start()
        self.addCleanup(environment.stop)
        self.sandbox = tempfile.TemporaryDirectory(prefix="e2e-lan-auth-test-")
        self.addCleanup(self.sandbox.cleanup)
        self.path = Path(self.sandbox.name) / "cli/home/.wendy/config.json"
        self.path.parent.mkdir(parents=True)
        self.config = {
            "currentContext": "synthetic",
            "devicePins": {"synthetic.local": {"spki": "synthetic-pin"}},
            "unknownTopLevelSecret": "synthetic-secret",
            "auth": [{
                "name": "synthetic", "cloudGRPC": "unused.invalid:443",
                "refreshToken": "synthetic-refresh", "apiKey": "synthetic-access",
                "oauthIssuer": "https://unused.invalid", "oauthExpiresAt": "2000-01-01T00:00:00Z",
                "dpopPrivateKey": "synthetic-dpop", "serviceAccount": "synthetic-account",
                "registryPrivateKey": "synthetic-registry", "pkiEndpoint": "https://unused.invalid",
                "futureCredential": "synthetic-future",
                "certificates": [{"pemCertificate": self.cert.read_text(),
                                  "pemPrivateKey": self.key.read_text(), "principalUri": "synthetic"}],
            }],
        }
        self.path.write_text(json.dumps(self.config))

    def test_removes_all_renewable_material_and_preserves_held_identity(self):
        prepare_config(self.path)
        clean = json.loads(self.path.read_text())
        self.assertEqual(set(clean), {"currentContext", "devicePins", "auth", "analytics"})
        self.assertEqual(set(clean["auth"][0]), {"name", "cloudGRPC", "certificates"})
        self.assertEqual(clean["auth"][0]["certificates"], self.config["auth"][0]["certificates"])
        self.assertEqual(clean["devicePins"], self.config["devicePins"])
        self.assertEqual(stat.S_IMODE(self.path.stat().st_mode), 0o600)
        # Expired OAuth metadata is irrelevant to the still-valid X509 leaf.

    def test_expired_certificate_fails_after_removing_tokens(self):
        with patch("prepare_lan_auth.time.time", return_value=time.time() + 2 * 86400):
            with self.assertRaisesRegex(LANFixtureError, "expired"):
                prepare_config(self.path)
        self.assertNotIn("refreshToken", json.loads(self.path.read_text())["auth"][0])

    def test_not_yet_valid_certificate_fails(self):
        with patch("prepare_lan_auth.time.time", return_value=time.time() - 86400):
            with self.assertRaisesRegex(LANFixtureError, "not yet valid"):
                prepare_config(self.path)

    def test_valid_certificate_near_renewal_margin_is_refused(self):
        # Still valid, but it would cross the maximum renewal margin within
        # the enforced test bound plus runner shutdown grace.
        with patch("prepare_lan_auth.time.time", return_value=self.expires - 1259):
            with self.assertRaisesRegex(LANFixtureError, "1260s signed validity"):
                prepare_config(self.path)
        self.assertNotIn("refreshToken", json.loads(self.path.read_text())["auth"][0])

    def test_agent_socket_is_rejected_before_any_subprocess(self):
        with patch.dict(os.environ, {"WENDY_AGENT_SOCKET": "/tmp/unrelated-agent.sock"}):
            with patch("prepare_lan_auth.subprocess.run") as command:
                with self.assertRaisesRegex(LANFixtureError, "overrides --device"):
                    prepare_config(self.path)
                command.assert_not_called()

    def test_unbounded_or_excessive_timeout_is_refused(self):
        for timeout in ["", "0", "301", "not-a-duration"]:
            with self.subTest(timeout=timeout):
                with patch.dict(os.environ, {"WENDY_E2E_TEST_TIMEOUT_SECONDS": timeout}):
                    with self.assertRaisesRegex(LANFixtureError, "--test-timeout 300"):
                        prepare_config(self.path)

    def test_config_override_cannot_bypass_sanitized_home(self):
        before = self.path.read_bytes()
        with patch.dict(os.environ, {"WENDY_CONFIG_DIR": str(Path(self.sandbox.name) / "other")}):
            with self.assertRaisesRegex(LANFixtureError, "override"):
                prepare_config(self.path)
        self.assertEqual(self.path.read_bytes(), before)

    def test_canonical_home_is_refused_without_modifying_it(self):
        original = Path(self.sandbox.name) / "canonical/.wendy/config.json"
        original.parent.mkdir(parents=True)
        original.write_text(json.dumps(self.config))
        before = original.read_bytes()
        with self.assertRaisesRegex(LANFixtureError, "isolated"):
            prepare_config(original)
        self.assertEqual(original.read_bytes(), before)


if __name__ == "__main__":
    unittest.main()
