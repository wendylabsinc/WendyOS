#!/usr/bin/env python3
"""Require the portable reader tests on native macOS and Linux hosts."""

from pathlib import Path
import shutil
import sys
import unittest

if sys.version_info < (3, 10):
    raise SystemExit("SO-101 reader regressions require Python 3.10 or newer")
if sys.platform not in ("darwin", "linux"):
    raise SystemExit("SO-101 reader regressions require macOS or Linux")
if shutil.which("lsof") is None:
    raise SystemExit("lsof is required for the real PTY ownership test")

directory = Path(__file__).resolve().parents[2] / "Examples/SO101Reader"
suite = unittest.defaultTestLoader.discover(str(directory), pattern="test_reader.py")
if suite.countTestCases() < 37:
    raise SystemExit("The SO-101 reader suite did not discover all existing cases")
result = unittest.TextTestRunner(verbosity=2).run(suite)
if result.skipped:
    print("Required reader regressions skipped:", result.skipped, file=sys.stderr)
raise SystemExit(0 if result.wasSuccessful() and not result.skipped else 1)
