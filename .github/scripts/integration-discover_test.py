#!/usr/bin/env python3
import importlib.util
import contextlib
import io
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import threading
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).parent))
from ci_diagnostics import stderr_summary


spec = importlib.util.spec_from_file_location(
    "discovery", Path(__file__).with_name("integration-discover.py")
)
discovery = importlib.util.module_from_spec(spec)
spec.loader.exec_module(discovery)
diagnose_spec = importlib.util.spec_from_file_location(
    "diagnose", Path(__file__).with_name("integration-diagnose.py")
)
diagnose = importlib.util.module_from_spec(diagnose_spec)
diagnose_spec.loader.exec_module(diagnose)


class DiscoveryTests(unittest.TestCase):
    def test_selects_renamed_devices_and_all_required_families(self):
        devices = [("new-pi.local", "raspberry-pi"),
                   ("new-thor.local", "jetson"),
                   ("ci-iq8.local", "qualcomm")]
        self.assertEqual(discovery.select(devices, True), [d[0] for d in devices])

    def test_required_families_take_priority_over_matrix_limit(self):
        devices = [(f"other-{i}.local", None) for i in range(20)]
        devices += [(f"{family}.local", family) for family in discovery.REQUIRED]
        selected = discovery.select(devices, True)
        self.assertEqual(len(selected), 20)
        for family in discovery.REQUIRED:
            self.assertIn(f"{family}.local", selected)

    def test_missing_platform_fails_gate_but_allows_discovery_only(self):
        devices = [("thor.local", "jetson")]
        with self.assertRaisesRegex(ValueError, "raspberry-pi, qualcomm"):
            discovery.select(devices, True)
        self.assertEqual(discovery.select(devices, False), ["thor.local"])
        with self.assertRaises(ValueError):
            discovery.select([], True)

    def test_candidates_validate_and_deduplicate_advertisements(self):
        devices = [{"hostname": host, "isWendyDevice": True} for host in
                   ["New-Pi.local", "new-pi.local", "--help", "a;whoami", "a\nb"]]
        devices.append({"hostname": "not-wendy.local", "isWendyDevice": False})
        self.assertEqual(discovery.candidates({"lanDevices": devices}), ["new-pi.local"])
        self.assertEqual(discovery.candidates({"lanDevices": None}), [])

    def test_platform_names(self):
        for kind, family in [("raspberry-pi-3", "raspberry-pi"),
                             ("raspberry-pi-5", "raspberry-pi"),
                             ("jetson-agx-thor", "jetson"),
                             ("jetson-orin-nano", "jetson"),
                             ("dragonwing-iq-8275", "qualcomm"),
                             ("dragonwing-iq-9075", "qualcomm"),
                             ("qualcomm-iq8", "qualcomm"), ("x86", None)]:
            self.assertEqual(discovery.platform(kind), family)

    def test_public_info_does_not_prove_authorization(self):
        with patch.object(discovery, "cli_json", side_effect=[
            {"deviceType": "jetson-agx-thor"},
            subprocess.CalledProcessError(1, "wendy"),
        ]) as cli:
            self.assertIsNone(discovery.probe("wendy", "thor.local"))
            self.assertEqual(cli.call_count, 2)
            self.assertEqual(cli.call_args.args,
                             ("wendy", "device", "apps", "list", "--device", "thor.local"))

    def test_probe_uses_live_hardware_type(self):
        with patch.object(discovery, "cli_json", side_effect=[
            {"deviceType": "dragonwing-iq-8275"}, [],
        ]):
            self.assertEqual(discovery.probe("wendy", "renamed.local"),
                             ("renamed.local", "qualcomm"))

    def test_timed_out_device_is_skipped(self):
        with patch.object(discovery, "cli_json",
                          side_effect=subprocess.TimeoutExpired("wendy", 45)):
            self.assertIsNone(discovery.probe("wendy", "offline.local"))

    def test_failure_retains_stage_and_safe_stderr_without_authorizing_device(self):
        error = subprocess.CalledProcessError(7, ["wendy", "secret-arg"], stderr=json.dumps({
            "error": {"code": "auth_login_expired", "message": "Your Wendy login for SECRET expired at SECRET", "token": "SECRET"},
        }))
        for responses, stage in (([error], "info"), ([{}, error], "apps list")):
            with patch.object(discovery, "cli_json", side_effect=responses), patch.object(discovery, "log") as log:
                self.assertIsNone(discovery.probe("wendy", "pi.local"))
                text = log.call_args.args[0]
                self.assertIn("stage=" + stage, text)
                self.assertIn("CalledProcessError exit=7", text)
                self.assertIn("auth_login_expired", text)
                self.assertNotIn("SECRET", text)
                self.assertNotIn("secret-arg", text)


class RedactionTests(unittest.TestCase):
    def test_adversarial_secret_material_and_terminal_injection(self):
        samples = [
            "-----BEGIN PRIVATE KEY-----\nSECRET\n-----END PRIVATE KEY-----",
            "-----BEGIN RSA PRIVATE KEY-----\nSECRET\n-----END RSA PRIVATE KEY-----",
            "-----BEGIN CERTIFICATE-----\nSECRET\n-----END CERTIFICATE-----",
            "-----BEGIN OPENSSH PRIVATE KEY-----\nSECRET",  # unterminated
            "Authorization: Bearer SECRET\nCookie: session=SECRET",
            "token=SECRET refresh_token: SECRET apiKey: SECRET password: SECRET",
            "eyJhbGciOiJub25lIn0.SECRET.signature\nhttps://user:SECRET@cloud.test/?token=SECRET",
            "\x1b[2J\x1b]52;c;SECRET\x07\r::add-mask::SECRET\n::error::SECRET",
            json.dumps({"error": {"code": "SECRET", "message": "SECRET", "privateKey": "SECRET"}, "token": "SECRET"}),
            json.dumps({"error": {"code": "auth_login_expired", "message": "SECRET", "next_steps": ["SECRET"]}}),
            json.dumps({"error": {"code": ["SECRET"], "message": {"token": "SECRET"}}}),
            '{"error":{"code":"auth_login_expired","message":"SECRET"',
            "connection refused SECRET\n-----BEGIN PRIVATE KEY-----\nSECRET\n-----END PRIVATE KEY-----",
        ]
        for sample in samples:
            with self.subTest(sample=sample):
                safe = stderr_summary(sample)
                self.assertNotIn("SECRET", safe)
                self.assertNotIn("eyJ", safe)
                self.assertNotIn("-----", safe)
                self.assertNotIn("::", safe)
                self.assertFalse(any(ord(char) < 32 for char in safe))
                self.assertLess(len(safe), 1024)
                self.assertIn("redacted", safe)

    def test_known_real_cli_error_shapes_remain_diagnostic(self):
        for message, code in (
            ("expected device identity required; obtain it from trusted enrollment, Cloud inventory, or an existing saved device binding", "error"),
            ('Your Wendy login for Wendy Labs, Inc. (auth context "wendy") expired at 2026-10-18 10:37 UTC.', "auth_login_expired"),
        ):
            safe = stderr_summary(json.dumps({"error": {"code": code, "message": message}}))
            self.assertIn("code=" + code, safe)
            self.assertNotIn("Wendy Labs", safe)
            self.assertTrue("identity required" in safe or "expiry reported" in safe)

    def test_plain_tls_errors_bytes_and_size_limit(self):
        self.assertIn("bad certificate", stderr_summary(b"remote error: tls: bad certificate\xff SECRET"))
        self.assertEqual(stderr_summary("SECRET" * 20000), "[redacted: stderr exceeds 64 KiB]")
        self.assertEqual(stderr_summary(None), "empty stderr")
        # A keyword occurring inside key material must not create a false signal.
        self.assertNotIn("connection refused", stderr_summary(
            "-----BEGIN PRIVATE KEY-----\nconnection refused\n-----END PRIVATE KEY-----"))


class DiagnosticTests(unittest.TestCase):
    def test_full_script_withholds_credentials_duplicated_in_metadata(self):
        secrets = ("SYNTHETIC-API-KEY-123456789", "eyJhbGciOiJub25lIn0.payload.signature",
                   "-----BEGIN PRIVATE KEY-----\nSYNTHETIC-PRIVATE-KEY\n-----END PRIVATE KEY-----")
        for secret in secrets:
            with self.subTest(secret=secret), tempfile.TemporaryDirectory() as directory:
                source = Path(directory)
                cfg = {"currentContext": secret, "auth": [{"name": secret, "apiKey": secret,
                       **{key: f"https://{secret}.example.test/private?token={secret}" for key in (
                           "cloudGRPC", "cloudDashboard", "oauthIssuer", "pkiEndpoint", "oauthResource", "pkiResource")}}],
                       "unused": "keychain:v1:refuse-before-network"}
                original = json.dumps(cfg).encode()
                (source / "config.json").write_bytes(original)
                result = subprocess.run([sys.executable, str(Path(diagnose.__file__).resolve()),
                                         "--wendy", "/nonexistent", "--hostname", "127.0.0.1"],
                                        env={**os.environ, "WENDY_CONFIG_DIR": directory},
                                        capture_output=True, text=True, timeout=10)
                self.assertEqual(result.returncode, 1)
                output = result.stdout + result.stderr
                self.assertNotIn(secret.lower(), output.lower())
                self.assertNotIn("SYNTHETIC-PRIVATE-KEY", output)
                self.assertIn("Keychain references present", output)
                self.assertNotIn('"event": "resolution"', output)
                self.assertEqual((source / "config.json").read_bytes(), original)

    def test_binding_metadata_matches_config_device_pin_for_normalization(self):
        for host in ("wendyos-ci-pi5.local", "WENDYOS-CI-PI5.LOCAL.", " wendyos-ci-pi5.local. "):
            for pin in ({}, {"assetId": "SECRET"}, {"principal": "SECRET"}):
                cfg = {"devicePins": {"wendyos-ci-pi5": pin}}
                with patch.object(diagnose, "emit") as emit:
                    diagnose.metadata({"config.json": json.dumps(cfg).encode()}, host)
                fields = emit.call_args.kwargs
                self.assertTrue(fields["saved_binding_entry_present"])
                self.assertEqual(fields["saved_binding_asset_present"], bool(pin.get("assetId")))
                self.assertEqual(fields["saved_binding_principal_present"], bool(pin.get("principal")))
                self.assertNotIn("SECRET", json.dumps(fields))
        # CLI lookup normalizes the supplied name, not arbitrary on-disk keys.
        with patch.object(diagnose, "emit") as emit:
            diagnose.metadata({"config.json": b'{"devicePins":{"pi.local":{"assetId":"SECRET"}}}'}, "pi.local")
        self.assertFalse(emit.call_args.kwargs["saved_binding_entry_present"])

    def test_hostname_validation_rejects_selectors_shell_and_empty_discovery(self):
        for host in ("", "pi", "mesh:pi", "cloud://pi", "vm:pi", "pi.local;id", "--help", "pi.local\n::error::bad"):
            with self.assertRaises(ValueError):
                diagnose.hostname(host)
        self.assertEqual(diagnose.hostname("CI-PI.local"), "ci-pi.local")

    def test_endpoints_with_credentials_query_path_and_controls(self):
        self.assertEqual(diagnose.endpoint("https://user:SECRET@cloud.wendy.dev:443/SECRET?token=SECRET#SECRET"), "wendy-v2-prod-cloud")
        self.assertEqual(diagnose.endpoint("api.wendy.dev:443"), "wendy-v2-prod-api")
        self.assertEqual(diagnose.endpoint(None), "absent")
        for value in ("file:///SECRET", "https://SECRET\x1b.test", "https://cloud.test:SECRET", "x" * 513,
                      "https://SECRET.example.test", "https://cloud.wendy.dev.SECRET.test", "https://SECRET.cloud.wendy.dev",
                      "http://cloud.wendy.dev", "https://cloud.wendy.dev:444"):
            self.assertEqual(diagnose.endpoint(value), "unknown-withheld")

    def test_known_cloud_presets_and_context_indices_survive_full_script(self):
        with tempfile.TemporaryDirectory() as directory:
            source = Path(directory)
            cfg = {"currentContext": "SYNTHETIC-CONTEXT-SECRET", "auth": [
                {"name": "SYNTHETIC-CONTEXT-SECRET", "cloudGRPC": "api.wendy.dev:443", "cloudDashboard": "https://cloud.wendy.dev"},
                {"name": "SYNTHETIC-CONTEXT-DEV", "cloudGRPC": "api.dev.wendy.sh:443", "cloudDashboard": "https://cloud.dev.wendy.sh"},
                {"name": "SYNTHETIC-CONTEXT-LEGACY", "cloudGRPC": "wendy-cloud-services-114319063177.us-central1.run.app:443",
                 "cloudDashboard": "https://cloud.wendy.sh"},
                {"name": "SYNTHETIC-CONTEXT-UNKNOWN", "cloudGRPC": "SYNTHETIC-CONTEXT-SECRET.example.test:443"}],
                "unused": "keychain:v1:refuse-before-network"}
            (source / "config.json").write_text(json.dumps(cfg))
            result = subprocess.run([sys.executable, str(Path(diagnose.__file__).resolve()),
                                     "--wendy", "/nonexistent", "--hostname", "127.0.0.1"],
                                    env={**os.environ, "WENDY_CONFIG_DIR": directory},
                                    capture_output=True, text=True, timeout=10)
            self.assertEqual(result.returncode, 1)
            self.assertNotIn("synthetic-context", result.stdout.lower())
            events = [json.loads(line) for line in result.stdout.splitlines()]
            context = next(row for row in events if row["event"] == "context")
            self.assertEqual(context["matching_context_indices"], [0])
            self.assertEqual(context["context_names"], "withheld")
            creds = [row for row in events if row["event"] == "credential"]
            self.assertEqual([row["index"] for row in creds], [0, 1, 2, 3])
            self.assertEqual([row["endpoints"]["cloudGRPC"] for row in creds], [
                "wendy-v2-prod-api", "wendy-v2-dev-api", "wendy-legacy-api", "unknown-withheld"])
            self.assertEqual([row["endpoints"]["cloudDashboard"] for row in creds], [
                "wendy-v2-prod-cloud", "wendy-v2-dev-cloud", "wendy-legacy-cloud", "absent"])

    def test_oauth_expiry_never_echoes_unparsed_data(self):
        self.assertEqual(diagnose.oauth_expiry("2026-10-18T12:37:00+02:00"), "2026-10-18T10:37:00+00:00")
        for value in ("SECRET", {"token": "SECRET"}, "2026-10-18"):
            self.assertEqual(diagnose.oauth_expiry(value), "unparseable")

    def test_metadata_is_bounded_and_never_serializes_secrets(self):
        cfg = {"currentContext": "wendy", "auth": [{"name": "wendy", "apiKey": "SECRET",
               "refreshToken": "SECRET", "certificates": [{"pemCertificate": "SECRET", "pemPrivateKey": "SECRET"}]}]}
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            diagnose.metadata({"config.json": json.dumps(cfg).encode()}, "pi.local")
        self.assertNotIn("SECRET", out.getvalue())
        self.assertIn('"key_present": true', out.getvalue())
        cfg["auth"] *= 17
        with contextlib.redirect_stdout(io.StringIO()), self.assertRaises(ValueError):
            diagnose.metadata({"config.json": json.dumps(cfg).encode()}, "pi.local")

    def test_keychain_reference_is_refused_without_resolution(self):
        for prefix in (b"keychain:", b"key\\u0063hain:"):
            data = b'{"auth":[{"name":"wendy","apiKey":"' + prefix + b'v1:SECRET"}]}'
            with contextlib.redirect_stdout(io.StringIO()), self.assertRaisesRegex(ValueError, "Keychain"):
                diagnose.metadata({"config.json": data}, "pi.local")

    def test_state_rejects_symlinks_and_oversize_files(self):
        with tempfile.TemporaryDirectory() as directory, contextlib.redirect_stdout(io.StringIO()):
            source = Path(directory)
            (source / "config.json").symlink_to(source / "elsewhere")
            with self.assertRaisesRegex(ValueError, "regular"):
                diagnose.read_state(source)
            (source / "config.json").unlink()
            (source / "config.json").write_bytes(b"x" * (diagnose.MAX_FILE + 1))
            with self.assertRaisesRegex(ValueError, "4 MiB"):
                diagnose.read_state(source)

    def test_probe_is_isolated_read_only_and_failures_are_nonzero(self):
        for outcome in (subprocess.TimeoutExpired("wendy SECRET", 45, stderr=b"SECRET"),
                        subprocess.CalledProcessError(1, "wendy SECRET", stderr="SECRET"),
                        subprocess.CompletedProcess([], 0, stdout='{"error":"SECRET"}', stderr="SECRET")):
            with tempfile.TemporaryDirectory() as directory:
                source = Path(directory)
                original = b'{"lastMCPSetupVersion":"old","auth":[],"devicePins":{}}'
                (source / "config.json").write_bytes(original)
                def run(command, **kwargs):
                    if command[0] != "/usr/bin/sandbox-exec":
                        return subprocess.CompletedProcess(command, 0)
                    self.assertIn("--read-only", command)
                    self.assertIn("--json", command)
                    self.assertNotIn("ssh", command)
                    env = kwargs["env"]
                    self.assertNotIn("WENDY_AGENT_SOCKET", env)
                    self.assertNotIn("WENDY_BROKER_URL", env)
                    self.assertEqual(env["WENDY_SECRET_STORE"], "file")
                    self.assertEqual(env["WENDY_NO_UPDATE_CHECK"], "1")
                    copy = Path(env["WENDY_CONFIG_DIR"])
                    self.assertNotEqual(copy, source)
                    self.assertNotIn("lastMCPSetupVersion", json.loads((copy / "config.json").read_text()))
                    if isinstance(outcome, Exception):
                        raise outcome
                    return outcome
                with patch.object(diagnose.subprocess, "run", side_effect=run), contextlib.redirect_stdout(io.StringIO()) as out:
                    try:
                        result = diagnose.diagnose("wendy", "pi.local", source)
                    except diagnose.DiagnosticError:
                        result = 1
                self.assertEqual(result, 1)
                self.assertNotIn("SECRET", out.getvalue())
                self.assertEqual((source / "config.json").read_bytes(), original)

    @unittest.skipUnless(sys.platform == "darwin", "macOS sandbox")
    def test_real_sandbox_blocks_persistent_writes_and_child_processes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            state = root / "state"
            state.mkdir()
            profile = ["/usr/bin/sandbox-exec", "-D", f"STATE={state}", "-p", diagnose.PROFILE]
            for command in (["/usr/bin/touch", str(root / "forbidden")],
                            ["/bin/sh", "-c", "/bin/echo child & wait"]):
                result = subprocess.run(profile + command, capture_output=True)
                self.assertNotEqual(result.returncode, 0)
            self.assertFalse((root / "forbidden").exists())
            self.assertEqual(subprocess.run(profile + ["/usr/bin/touch", str(state / "allowed")]).returncode, 0)

    @unittest.skipUnless(os.environ.get("CI_DIAGNOSTIC_WENDY") and sys.platform == "darwin", "requires freshly built macOS CLI")
    def test_current_built_cli_on_disposable_loopback_preserves_source_state(self):
        with tempfile.TemporaryDirectory() as directory, contextlib.ExitStack() as cleanup:
            # Real, deliberately rejecting TCP endpoints: no simulator, Cloud,
            # login or device setup. Both ports are exclusively owned by this test.
            stop = threading.Event()
            accepts = []
            threads = []
            def stop_listeners():
                stop.set()
                for thread in threads:
                    thread.join()
            def reject(listener):
                while not stop.is_set():
                    try:
                        conn, _ = listener.accept()
                        accepts.append(listener.getsockname()[1])
                        conn.close()
                    except socket.timeout:
                        pass
            for port in (50051, 50052):
                listener = cleanup.enter_context(socket.socket())
                listener.bind(("127.0.0.1", port))
                listener.listen()
                listener.settimeout(0.1)
                cleanup.callback(stop_listeners)
                thread = threading.Thread(target=reject, args=(listener,))
                thread.start()
                threads.append(thread)
            source = Path(directory)
            config = b'{"currentContext":"wendy","auth":[],"devicePins":{}}'
            (source / "config.json").write_bytes(config)
            with patch.object(diagnose, "emit") as emit:
                result = diagnose.diagnose(os.environ["CI_DIAGNOSTIC_WENDY"], "127.0.0.1", source)
            self.assertEqual(result, 1)
            device = next(call.kwargs for call in emit.call_args_list if call.args == ("device-info",))
            self.assertEqual(device["status"], "failed")
            self.assertIn("CalledProcessError", device["detail"])
            self.assertIn("code=device_", device["detail"])
            self.assertGreater(len(accepts), 2)  # reachability plus actual CLI dial
            self.assertEqual((source / "config.json").read_bytes(), config)
            self.assertEqual(list(source.iterdir()), [source / "config.json"])


if __name__ == "__main__":
    unittest.main()
