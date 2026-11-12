#!/usr/bin/env python3
"""Run offline review/workflow regressions without review labels or credentials."""

import importlib.util
from pathlib import Path
import sys
import unittest

DIRECTORY = Path(__file__).resolve().parent
SUITES = (
    "api_review_test.py",
    "api_review_comment_test.py",
    "api_review_run_test.py",
    "api_review_workflow_test.py",
    "review_diff_test.py",
    "review_sources_test.py",
    "docs_review_test.py",
    "security_review_test.py",
    "swift_e2e_gate_test.py",
    "ci_execution_test.py",
    "shared_contracts_test.py",
    "dependency_gates_test.py",
    "plugin_pin_test.py",
    "tag-built-commit_test.py",
    "release_provenance_test.py",
    "release_policy_test.py",
    "janitor_report_test.py",
    "runner_cleanup_test.py",
    "gpu_fixture_selection_test.py",
    "gpu_hardware_test.py",
    "npu_hardware_test.py",
    "orin_wpa3_rehearsal_test.py",
    "accelerator_renumbering_test.py",
    "so101_reader_acceptance_test.py",
    "tensorrt_restart_test.py",
    "oci_retry_test.py",
    "signing_ci_test.py",
    "stagefile_images_test.py",
    "sensorlink_audio_loopback_test.py",
    "hosted_audio_modules_test.py",
    "lite_camera_cleanup_test.py",
    "integration-discover_test.py",
)


def main():
    suite = unittest.TestSuite()
    for filename in SUITES:
        path = DIRECTORY / filename
        spec = importlib.util.spec_from_file_location(path.stem, path)
        module = importlib.util.module_from_spec(spec)
        sys.modules[path.stem] = module
        spec.loader.exec_module(module)
        tests = unittest.defaultTestLoader.loadTestsFromModule(module)
        if not tests.countTestCases():
            raise RuntimeError(f"No tests discovered in {filename}")
        suite.addTests(tests)
    result = unittest.TextTestRunner(verbosity=2).run(suite)
    if result.skipped:
        print("Required automation regressions skipped:", result.skipped, file=sys.stderr)
    return 0 if result.wasSuccessful() and not result.skipped else 1


if __name__ == "__main__":
    sys.exit(main())
