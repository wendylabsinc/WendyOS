import unittest

from readiness import error_body, expected_key_refusal, transient_transport


class RestartReadinessTests(unittest.TestCase):
    def test_only_transport_errors_may_retry(self):
        self.assertTrue(transient_transport({"code": "device_unreachable", "retryable": True, "message": "error reading from server: EOF"}))
        self.assertTrue(transient_transport({"code": "grpc_unavailable", "message": "connection refused"}))
        for error in (
            {"code": "device_unreachable", "retryable": False, "message": "EOF"},
            {"code": "device_auth_required", "retryable": True, "message": "does not trust this computer's key"},
            {"code": "device_unreachable", "retryable": True, "message": "certificate identity mismatch"},
            {"code": "grpc_permission_denied", "message": "permission denied"},
            {"code": "tool_not_found", "message": "missing binary"},
            {"code": "grpc_unavailable", "message": "application not ready"},
        ):
            self.assertFalse(transient_transport(error), error)

    def test_denial_is_an_expected_protective_result_only_when_selected(self):
        denial = {"code": "device_auth_required", "message": "device is Closed and does not trust this computer's key (sha256:fixture)"}
        self.assertTrue(expected_key_refusal(denial))
        self.assertFalse(transient_transport(denial))
        self.assertFalse(expected_key_refusal({"code": "device_unreachable", "message": "EOF"}))

    def test_reads_the_actual_envelope_and_rejects_missing_diagnostics(self):
        body = error_body('notice\n{"error":{"code":"device_unreachable","retryable":true,"message":"EOF"}}\n')
        self.assertTrue(transient_transport(body))
        with self.assertRaises(RuntimeError):
            error_body("generic failed command")


if __name__ == "__main__":
    unittest.main()
