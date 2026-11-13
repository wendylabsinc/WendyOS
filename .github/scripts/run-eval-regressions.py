#!/usr/bin/env python3
"""Run the local eval contracts without model calls or device access."""

from pathlib import Path
import sys
import unittest


def main():
    directory = Path(__file__).resolve().parents[2] / "evals" / "agent-experience"
    files = sorted(directory.glob("test_*.py"))
    if not files:
        raise RuntimeError("No eval regression modules found")
    sys.path.insert(0, str(directory))
    suite = unittest.TestSuite()
    for path in files:
        tests = unittest.defaultTestLoader.loadTestsFromName(path.stem)
        if not tests.countTestCases():
            raise RuntimeError(f"No tests found in {path.name}")
        suite.addTests(tests)
    result = unittest.TextTestRunner(verbosity=2).run(suite)
    if result.skipped:
        print("Required eval regressions skipped:", result.skipped, file=sys.stderr)
    return 0 if result.wasSuccessful() and not result.skipped else 1


if __name__ == "__main__":
    sys.exit(main())
