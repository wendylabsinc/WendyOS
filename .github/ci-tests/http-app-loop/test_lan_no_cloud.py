"""Offline actual-CLI contact control; no real device or Cloud endpoint is used."""
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import threading
import unittest
from unittest.mock import patch

from prepare_lan_auth import prepare_config


class LANNoCloudTests(unittest.TestCase):
    def test_unreachable_lan_never_contacts_cloud_for_executed_fixture_paths(self):
        cli = Path(os.environ["WENDY_E2E_CONTROL_CLI"]).resolve()
        contacts = []
        stopped = threading.Event()
        with tempfile.TemporaryDirectory(prefix="e2e-no-cloud-") as directory:
            home = Path(directory) / "cli/home"
            config = home / ".wendy/config.json"
            config.parent.mkdir(parents=True)
            cert, key = Path(directory) / "cert.pem", Path(directory) / "key.pem"
            tenant = "11111111-1111-4111-8111-111111111111"
            principal = f"spiffe://wendy.sh/tenant/{tenant}/operator/synthetic"
            asset = "22222222-2222-4222-8222-222222222222"
            subprocess.run([
                "openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
                "-subj", "/CN=e2e-offline", "-addext", f"subjectAltName=URI:{principal}", "-keyout", str(key), "-out", str(cert),
            ], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            with socket.socket() as agent, socket.socket() as cloud:
                # Binding without listening keeps the exact Agent port unavailable.
                agent.bind(("127.0.0.1", 0))
                cloud.bind(("127.0.0.1", 0))
                cloud.listen()
                cloud.settimeout(0.1)
                endpoint = f"127.0.0.1:{cloud.getsockname()[1]}"
                target = f"127.0.0.1:{agent.getsockname()[1]}"
                config.write_text(json.dumps({
                    "currentContext": "synthetic",
                    "devicePins": {"127.0.0.1": {"cloudGRPC": endpoint, "assetId": asset,
                                                "principal": f"spiffe://wendy.sh/tenant/{tenant}/device/{asset}"}},
                    "auth": [{"name": "synthetic", "cloudGRPC": endpoint,
                              "certificates": [{"pemCertificate": cert.read_text(),
                                                "pemPrivateKey": key.read_text(),
                                                "organizationId": 0, "userId": "synthetic", "principalUri": principal}]}],
                }))
                env = dict(os.environ, HOME=str(home), WENDY_CONFIG_DIR=str(config.parent),
                           WENDY_E2E_TEST_TIMEOUT_SECONDS="300", WENDY_AGENT_SOCKET="",
                           WENDY_SECRET_STORE="file", WENDY_ANALYTICS="false")
                with patch.dict(os.environ, env, clear=True):
                    prepare_config(config)
                project = home / "e2e-offline"
                project.mkdir()
                (project / "compose.yml").write_text(json.dumps({"services": {"web": {
                    "image": "docker.io/library/python@sha256:303398d5c9f110790bce60d64f902e51e1a061e33292985c72bf6cd07960bf09",
                }}}))
                commands = [
                    ["run", "--prefix", str(project), "--skip-cloud-registration", "--detach"],
                    ["device", "apps", "list"],
                    ["device", "apps", "start", "e2e-missing", "--detach"],
                    ["device", "apps", "stop", "e2e-missing"],
                    ["device", "apps", "remove", "e2e-missing", "--force"],
                    ["device", "info", "--read-only"],
                ]

                def count_connections():
                    while not stopped.is_set():
                        try:
                            connection, _ = cloud.accept()
                        except socket.timeout:
                            continue
                        contacts.append(1)
                        connection.close()

                thread = threading.Thread(target=count_connections)
                thread.start()
                try:
                    for command in commands:
                        with self.subTest(command=command[:3]):
                            result = subprocess.run(
                                [str(cli), "--json", "--device", target, *command],
                                env=dict(env, WENDY_RUN_NO_CLOUD_FALLBACK="1"),
                                capture_output=True, text=True, timeout=12,
                            )
                            self.assertNotEqual(result.returncode, 0)
                            self.assertEqual(contacts, [], "guarded CLI contacted Cloud after failed LAN")
                    # Without the supported guard the same actual run must contact
                    # the local counter, proving that zero contacts is not vacuous.
                    positive = None
                    try:
                        positive = subprocess.run(
                            [str(cli), "--json", "--device", target, *commands[0]],
                            env=dict(env, WENDY_RUN_NO_CLOUD_FALLBACK="0"),
                            capture_output=True, text=True, timeout=12,
                        )
                    except subprocess.TimeoutExpired:
                        pass
                    self.assertGreater(len(contacts), 0, "unguarded run did not exercise the Cloud counter: " + (positive.stderr if positive else "timed out"))
                    print(json.dumps({"guarded_paths": len(commands), "guarded_cloud_contacts": 0,
                                      "unguarded_positive_contacts": len(contacts)}))
                finally:
                    stopped.set()
                    thread.join(timeout=1)
                    self.assertFalse(thread.is_alive())


if __name__ == "__main__":
    unittest.main()
