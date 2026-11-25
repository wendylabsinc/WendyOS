"""Run with: uv run --with jsonschema python swift/Scripts/E2ECoverageSchemas_test.py"""

import json
from pathlib import Path
import unittest

from jsonschema import Draft202012Validator


def validator(name):
    schemas = Path(__file__).resolve().parents[1] / "WendyE2ETests" / "Support" / "Schemas"
    schema = json.loads((schemas / f"wendy-e2e-{name}.schema.json").read_text())
    Draft202012Validator.check_schema(schema)
    return Draft202012Validator(schema)


class CoverageSchemasTests(unittest.TestCase):
    def test_attempt_coverage_requested(self):
        attempt = validator("attempt")
        manifest = {
            "kind": "wendy-e2e-attempt", "version": 1, "attemptID": "local.0001",
            "createdAt": "2026-11-25T10:00:00Z", "exitStatus": 0,
            "git": {"commit": None, "branch": None, "ref": None, "remote": None, "dirty": False},
            "github": dict.fromkeys(
                ("repository", "workflow", "runID", "runAttempt", "job", "actor", "sha")
            ),
            "target": dict.fromkeys(
                ("cliOS", "cliAddress", "cliUser", "agentOS", "agentAddress", "agentUser", "transport")
            ),
            "paths": {
                "attemptDirectory": "attempt", "outputDirectory": "output", "cliRunDirectory": "cli",
                "cliBinDirectory": "cli/bin", "agentRunDirectory": "agent", "agentBinDirectory": None,
            },
            "test": {"filters": [], "isolation": "per-test", "parallel": False},
            "tools": dict.fromkeys(("swift", "go", "wendy", "wendyPath")),
        }
        attempt.validate(manifest)
        for value in (True, False):
            with self.subTest(value=value):
                attempt.validate({**manifest, "coverageRequested": value})
        for value in ("true", "false", 1, 0, None):
            with self.subTest(value=value):
                self.assertFalse(attempt.is_valid({**manifest, "coverageRequested": value}))
        self.assertFalse(attempt.is_valid({**manifest, "unexpected": True}))

    def test_run_attempt_provenance(self):
        coverage = validator("coverage")
        source = {"sourceID": "a" * 64, "revision": "b" * 40}
        for status in ("collected", "captured", "no-data", "incomplete", "not-collected"):
            for fields, valid in [
                (source, True),
                ({}, status in ("incomplete", "not-collected")),
                ({"sourceID": source["sourceID"]}, False),
                ({"revision": source["revision"]}, False),
                ({**source, "revision": "invalid"}, False),
            ]:
                with self.subTest(status=status, fields=fields):
                    attempt = {"attempt": "local.0001", "status": status, **fields}
                    run = {"status": "incomplete", "attempts": [attempt], "errors": []}
                    self.assertEqual(coverage.is_valid(run), valid)

    def test_optional_spec_comparison_fields(self):
        diff = validator("diff")
        compared = {
            "status": "compared", "baseRevision": "a" * 40, "sourceRevision": "b" * 40,
            "range": "base..head", "specs": [],
        }
        for fields, valid in [
            ({}, True),  # Older saved comparisons
            ({"specsStatus": "compared"}, True),
            ({"specsStatus": "unavailable", "specsReason": "Ambiguous spec identity"}, True),
            ({"specsStatus": "unknown"}, False),
            ({"specsStatus": "unavailable", "specsReason": 42}, False),
            ({"unexpected": True}, False),
        ]:
            with self.subTest(fields=fields):
                self.assertEqual(diff.is_valid({**compared, **fields}), valid)
        del compared["sourceRevision"]
        self.assertFalse(diff.is_valid({**compared, "specsStatus": "compared"}))


if __name__ == "__main__":
    unittest.main()
