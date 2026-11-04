#!/usr/bin/env python3
"""Evaluate the committed Swift E2E job conditions across real run shapes."""

import itertools
from pathlib import Path
import re
import unittest

WORKFLOW = Path(__file__).resolve().parents[1] / "workflows" / "swift-e2e-tests.yml"
ANALYZE = "swift-e2e-analyze"
PASSED = "swift-e2e-passed"


def job_condition(text, job):
    match = re.search(rf"^  {job}:\n(?:.*\n)*?    if: >-\n((?:      .*\n)+)", text, re.M)
    if match is None:
        raise AssertionError(f"{job} has no folded if condition")
    return " ".join(match[1].split()).removeprefix("${{").removesuffix("}}").strip()


def evaluate(condition, results, attempt, event, run_cancelled=False):
    """Translate the small expression subset these gates use into Python."""
    python = re.sub(r"contains\(needs\.\*\.result, ('\w+')\)", r"(\1 in results)", condition)
    python = python.replace("always()", "True").replace("cancelled()", "run_cancelled")
    python = python.replace("github.run_attempt", "attempt").replace("github.event_name", "event")
    python = python.replace("&&", " and ").replace("||", " or ")
    python = re.sub(r"!(?!=)", " not ", python)
    names = {"results": results, "attempt": attempt, "event": event, "run_cancelled": run_cancelled}
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


if __name__ == "__main__":
    unittest.main()
