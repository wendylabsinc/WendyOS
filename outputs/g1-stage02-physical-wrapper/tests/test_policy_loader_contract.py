from __future__ import annotations

import ast
from pathlib import Path
import unittest


ROOT = Path(__file__).resolve().parents[1]


class PolicyLoaderContractTests(unittest.TestCase):
    def test_modified_python_is_syntax_valid(self) -> None:
        for relative in (
            "physical_overlay/physical_io/service.py",
            "physical_overlay/runtime/physical_policy.py",
            "physical_overlay/runtime/inference_client.py",
        ):
            ast.parse((ROOT / relative).read_text(), filename=relative)

    def test_play_button_runs_motion_zero_preflight_first(self) -> None:
        source = (ROOT / "physical_overlay/physical_io/service.py").read_text()
        self.assertIn("/api/reference-residual/preflight", source)
        self.assertIn("Checking robot, inference, active contract, and camera", source)
        self.assertLess(
            source.index("fetch('/api/reference-residual/preflight'"),
            source.index("fetch('/api/reference-residual/run-policy'"),
        )

    def test_reference_and_policy_identity_are_remote_contract_data(self) -> None:
        client = (ROOT / "physical_overlay/runtime/inference_client.py").read_text()
        runner = (ROOT / "physical_overlay/runtime/physical_policy.py").read_text()
        self.assertIn('self.client.request("GET", "/contract")', client)
        self.assertIn('proposal.get("reference_target_q_43")', runner)
        self.assertIn('proposal.get("checkpoint_sha256"', runner)
        self.assertIn("active checkpoint changed after Play-policy preflight", runner)

    def test_safe_retreat_reverses_sent_path_without_auto_release(self) -> None:
        service = (ROOT / "physical_overlay/physical_io/service.py").read_text()
        runner = (ROOT / "physical_overlay/runtime/physical_policy.py").read_text()
        self.assertIn("/api/reference-residual/retreat", service)
        self.assertIn("reference-residual-safe-retreat-request.v1", service)
        self.assertIn("reversed(command_history)", runner)
        self.assertIn('"automatic_release": False', runner)
        self.assertIn('arm_sdk_weight=1.0', runner)
        self.assertIn("hold retreat endpoint until explicit controlled release", runner)

    def test_execution_rate_is_selected_per_run_and_server_validated(self) -> None:
        service = (ROOT / "physical_overlay/physical_io/service.py").read_text()
        runner = (ROOT / "physical_overlay/runtime/physical_policy.py").read_text()
        self.assertIn('"execution_hz"', service)
        self.assertIn("execution_hz must be 15, 25, 30, or 40", service)
        self.assertIn("ALLOWED_EXECUTION_HZ = (15.0, 25.0, 30.0, 40.0)", runner)
        self.assertIn("self.tick_s = 1.0 / selected_execution_hz", runner)
        self.assertIn('"control_hz": self.execution_hz', runner)


if __name__ == "__main__":
    unittest.main()
