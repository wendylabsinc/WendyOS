#!/usr/bin/env python3
import contextlib
import copy
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location(
    "refresh_gate_auth", Path(__file__).with_name("refresh-gate-auth.py"))
gate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gate)

STATUS = {"loggedIn": True, "sessions": [{
    "context": "wendy", "current": True, "cloudGrpc": "api.wendy.dev:443",
    "loginPath": "OIDC", "principalUri": gate.PRINCIPAL_PREFIX + "ci-test",
    "certificate": {"expiresAt": "2099-01-01T00:00:00Z", "expired": False},
}]}
PRIVATE = "private-token\n::error::private-link\n-----BEGIN PRIVATE KEY-----"


class RefreshTests(unittest.TestCase):
    def run_refresh(self, outputs):
        out = io.StringIO()
        with patch.object(gate, "cli", side_effect=outputs) as cli, contextlib.redirect_stdout(out):
            result = gate.refresh("/candidate/wendy")
        self.assertNotIn(PRIVATE, out.getvalue())
        self.assertNotIn("BEGIN PRIVATE KEY", out.getvalue())
        if result:
            self.assertIn("::error::", out.getvalue())
            self.assertIn("ci@wendy.sh", out.getvalue())
            self.assertIn("auth login", out.getvalue())
            self.assertIn("auth use wendy", out.getvalue())
        return result, out.getvalue(), cli.call_args_list

    def test_expired_leaf_can_be_refreshed_through_stored_login(self):
        before = copy.deepcopy(STATUS)
        before["sessions"][0]["certificate"] = {
            "expiresAt": "2000-01-01T00:00:00Z", "expired": True}
        result, out, calls = self.run_refresh([json.dumps(before), PRIVATE, json.dumps(STATUS)])
        self.assertEqual(result, 0)
        self.assertIn("refresh completed", out)
        self.assertEqual([c.args[1:] for c in calls],
                         [("auth", "status"), ("auth", "refresh-certs"), ("auth", "status")])

    def test_context_refusals_never_attempt_refresh(self):
        variants = [{"loggedIn": False, "sessions": []}, {"sessions": None}, []]
        for field, value in (("current", False), ("context", "default"),
                             ("cloudGrpc", "legacy.example:443"), ("loginPath", "legacy"),
                             ("principalUri", "spiffe://other/tenant/ci"), ("certificate", None)):
            status = copy.deepcopy(STATUS)
            status["sessions"][0][field] = value
            variants.append(status)
        duplicate = copy.deepcopy(STATUS)
        duplicate["sessions"] *= 2
        variants.append(duplicate)
        for status in variants:
            with self.subTest(status=status):
                result, _, calls = self.run_refresh([json.dumps(status)])
                self.assertEqual(result, 1)
                self.assertEqual(len(calls), 1)

    def test_missing_cli_invalid_output_refusal_and_timeout_are_safe(self):
        failures = [FileNotFoundError(PRIVATE), PRIVATE,
                    subprocess.CalledProcessError(1, PRIVATE, output=PRIVATE),
                    subprocess.TimeoutExpired(PRIVATE, 60, output=PRIVATE)]
        for failure in failures:
            with self.subTest(failure=type(failure)):
                result, _, _ = self.run_refresh([failure])
                self.assertEqual(result, 1)
        for failure in failures[2:]:
            with self.subTest(refresh_failure=type(failure)):
                result, out, calls = self.run_refresh([json.dumps(STATUS), failure])
                self.assertEqual(result, 1)
                self.assertIn("expired or refused", out)
                self.assertEqual(len(calls), 2)

    def test_zero_exit_without_usable_certificate_fails(self):
        for field, value in (("certificate", None), ("current", False),
                             ("principalUri", gate.PRINCIPAL_PREFIX + "another-user")):
            after = copy.deepcopy(STATUS)
            after["sessions"][0][field] = value
            result, _, _ = self.run_refresh([json.dumps(STATUS), "", json.dumps(after)])
            self.assertEqual(result, 1)
        for cert in ({"expiresAt": "2000-01-01T00:00:00Z", "expired": False},
                     {"expiresAt": "2099-01-01T00:00:00Z", "expired": True},
                     {"expiresAt": "invalid", "expired": False}):
            after = copy.deepcopy(STATUS)
            after["sessions"][0]["certificate"] = cert
            result, _, _ = self.run_refresh([json.dumps(STATUS), "", json.dumps(after)])
            self.assertEqual(result, 1)

    def test_subprocess_contract_closes_stdin_and_discards_stderr(self):
        with tempfile.TemporaryDirectory() as tmp:
            executable = Path(tmp) / "candidate with spaces"
            executable.write_text("#!/usr/bin/env python3\nimport json,os,sys\n"
                                  "assert sys.stdin.read() == ''\n"
                                  "assert sys.argv[1:] == ['auth','status','--json']\n"
                                  "assert os.environ['CI'] == 'true'\n"
                                  "assert os.environ['WENDY_SECRET_STORE'] == 'file'\n"
                                  "assert os.environ['WENDY_NO_UPDATE_CHECK'] == '1'\n"
                                  "print('private-token', file=sys.stderr)\n"
                                  "print('{}')\n")
            executable.chmod(0o700)
            with patch.dict(os.environ, {"CI": "false", "WENDY_SECRET_STORE": "keychain"}):
                self.assertEqual(json.loads(gate.cli(str(executable), "auth", "status")), {})


if __name__ == "__main__":
    unittest.main()
