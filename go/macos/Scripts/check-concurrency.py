#!/usr/bin/env python3
"""Reject unstructured Swift task construction in the apps and their tests."""
from pathlib import Path
import re
import sys

root = Path(__file__).resolve().parents[1]
constructor = re.compile(r"\bTask\s*(?:<[^>]*>\s*)?(?:\{|\(|\.\s*(?:detached|init)\s*(?:\{|\())")
violations = []
for directory in ("Sources", "Tests", "Tests-iOS"):
    for path in sorted((root / directory).rglob("*.swift")):
        source = path.read_text()
        for match in constructor.finditer(source):
            number = source.count("\n", 0, match.start()) + 1
            violations.append(f"{path}:{number}: error: Use a view-owned task, async let, or a task group.")
if violations:
    print("\n".join(violations), file=sys.stderr)
    sys.exit(1)
print("Structured concurrency check passed: zero unstructured task constructors.")
