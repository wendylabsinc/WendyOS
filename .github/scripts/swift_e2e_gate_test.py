#!/usr/bin/env python3
"""Evaluate the committed Swift E2E job conditions across real run shapes."""

import itertools
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import textwrap
import unittest

WORKFLOW = Path(__file__).resolve().parents[1] / "workflows" / "swift-e2e-tests.yml"
REPORT_COMMAND = (
    Path(__file__).resolve().parents[2]
    / "swift/WendyE2ETests/Sources/SwiftE2ETestingCLI/ReportCommand.swift"
)
ANALYZE = "swift-e2e-analyze"
PASSED = "swift-e2e-passed"


def job_condition(text, job):
    match = re.search(rf"^  {job}:\n(?:.*\n)*?    if: >-\n((?:      .*\n)+)", text, re.M)
    if match is None:
        raise AssertionError(f"{job} has no folded if condition")
    return " ".join(match[1].split()).removeprefix("${{").removesuffix("}}").strip()


def evaluate(condition, results, attempt, event, run_cancelled=False, coverage=False):
    """Translate the small expression subset these gates use into Python."""
    python = re.sub(r"contains\(needs\.\*\.result, ('\w+')\)", r"(\1 in results)", condition)
    python = python.replace("always()", "True").replace("cancelled()", "run_cancelled")
    python = python.replace("github.run_attempt", "attempt").replace("github.event_name", "event")
    python = python.replace("inputs.coverage", "coverage")
    python = python.replace("&&", " and ").replace("||", " or ")
    python = re.sub(r"!(?!=)", " not ", python)
    names = {"results": results, "attempt": attempt, "event": event, "run_cancelled": run_cancelled, "coverage": coverage}
    return bool(eval(python, {"__builtins__": {}}, names))


class GateTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.text = WORKFLOW.read_text(encoding="utf-8")
        cls.analyze = job_condition(cls.text, ANALYZE)
        cls.passed = job_condition(cls.text, PASSED)

    def started(self, results, attempt="1", event="pull_request", run_cancelled=False):
        return (evaluate(self.analyze, results, attempt, event, run_cancelled),
                evaluate(self.passed, results, attempt, event, run_cancelled))

    def test_named_run_shapes(self):
        # Expected (Analyze, Record E2E Pass) for (macOS, Ubuntu) job results.
        cases = [
            ("failure on a PR", ("skipped", "failure"), "1", "pull_request", (True, False)),
            ("failure on main", ("skipped", "failure"), "1", "push", (True, False)),
            ("failure beside success", ("success", "failure"), "1", "push", (True, False)),
            ("job cancelled, run not", ("skipped", "cancelled"), "1", "push", (True, False)),
            ("job cancelled beside success", ("cancelled", "success"), "1", "pull_request", (True, False)),
            ("green first attempt on a PR", ("skipped", "success"), "1", "pull_request", (False, True)),
            ("green first attempt on main", ("skipped", "success"), "1", "push", (False, True)),
            ("green second attempt on a PR", ("skipped", "success"), "2", "pull_request", (True, False)),
            ("green second attempt on main", ("skipped", "success"), "2", "push", (True, False)),
            ("green dispatch", ("skipped", "success"), "1", "workflow_dispatch", (True, False)),
            ("failed dispatch", ("skipped", "failure"), "1", "workflow_dispatch", (True, False)),
            ("all skipped on a PR", ("skipped", "skipped"), "1", "pull_request", (False, False)),
            ("all skipped on main", ("skipped", "skipped"), "1", "push", (False, False)),
            ("all skipped dispatch", ("skipped", "skipped"), "1", "workflow_dispatch", (True, False)),
            ("all skipped rerun", ("skipped", "skipped"), "2", "push", (True, False)),
        ]
        for name, results, attempt, event, expected in cases:
            with self.subTest(name):
                self.assertEqual(self.started(results, attempt, event), expected)

    def test_cancelled_run_starts_neither(self):
        for results in (("skipped", "cancelled"), ("skipped", "failure"), ("skipped", "success")):
            with self.subTest(results):
                self.assertEqual(self.started(results, run_cancelled=True), (False, False))

    def test_coverage_never_runs_analysis_or_notifications(self):
        for results, attempt, event in itertools.product(
            itertools.product(("success", "failure", "cancelled", "skipped"), repeat=2),
            ("1", "2"), ("workflow_dispatch", "workflow_call", "push")
        ):
            for condition in (self.analyze, self.passed):
                self.assertFalse(evaluate(condition, results, attempt, event, coverage=True))

    def test_every_run_starts_at_most_one_and_never_loses_a_finished_job(self):
        outcomes = ("success", "failure", "cancelled", "skipped")
        for macos, ubuntu, attempt, event, run_cancelled in itertools.product(
            outcomes, outcomes, ("1", "2", "3"), ("pull_request", "push", "workflow_dispatch"), (False, True)
        ):
            results = (macos, ubuntu)
            with self.subTest(results=results, attempt=attempt, event=event, run_cancelled=run_cancelled):
                analyze, passed = self.started(results, attempt, event, run_cancelled)
                self.assertFalse(analyze and passed)
                if not run_cancelled and results != ("skipped", "skipped"):
                    self.assertTrue(analyze or passed)

    def test_passed_job_needs_every_analyze_dependency(self):
        needs = {job: re.search(rf"^  {job}:\n(?:.*\n)*?    needs:\n((?:      - .*\n)+)", self.text, re.M)[1]
                 for job in (ANALYZE, PASSED)}
        self.assertEqual(needs[ANALYZE], needs[PASSED])

    def test_report_reads_the_review_outcome_before_continue_on_error(self):
        # A step's conclusion is always success under continue-on-error.
        self.assertTrue("WENDY_E2E_REVIEW_OUTCOME: ${{ steps.ai-review.outcome }}" in self.text)

    def test_report_command_reads_the_variable_the_workflow_sets(self):
        self.assertIn('environment["WENDY_E2E_REVIEW_OUTCOME"]', REPORT_COMMAND.read_text(encoding="utf-8"))

    def test_host_setup_leaves_successful_attempt_creation_to_the_runner(self):
        self.run_host_setup(0)

    def test_host_setup_failure_preserves_its_artifact(self):
        self.run_host_setup(23)

    def run_host_setup(self, exit_status):
        action = WORKFLOW.parents[1] / "actions/swift-e2e-run/action.yml"
        block = action.read_text().split("    - name: Set up Swift E2E host (Unix)\n", 1)[1].split("\n    - name:", 1)[0]
        script = textwrap.dedent(block.split("      run: |\n", 1)[1])
        with tempfile.TemporaryDirectory(prefix="swift-e2e-host-setup-") as directory:
            root = Path(directory)
            setup = root / "swift/Scripts/E2ESetup.sh"
            setup.parent.mkdir(parents=True)
            setup.write_text(f"echo real-setup-shell-receipt\nexit {exit_status}\n")
            result = subprocess.run(["bash", "-c", script], cwd=root, capture_output=True, text=True,
                                    env=dict(os.environ, RUNNER_TEMP=str(root / "runner-temp"), RUN_ID="test-attempt"))
            self.assertEqual(result.returncode, exit_status, result.stderr)
            run = root / "runner-temp/wendy/test-attempt"
            if exit_status:
                self.assertIn("real-setup-shell-receipt", (run / "attempt.log").read_text())
                self.assertEqual(json.loads((run / "attempt.json").read_text())["exitStatus"], exit_status)
            else:
                self.assertFalse(run.exists(), "Successful setup must not precreate a coverage attempt")


if __name__ == "__main__":
    unittest.main()
