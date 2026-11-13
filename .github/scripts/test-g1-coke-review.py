#!/usr/bin/env python3
"""Require the existing Coke review suite in its exact locked Python environment.

Run with `uv run --project Examples/G1CokeDemo --frozen --group dev --python 3.12
python .github/scripts/test-g1-coke-review.py --output OWNED_NEW_DIRECTORY`.
No model downloads, camera, DDS initialization, motion or graphical display.
"""
import argparse
import hashlib
import importlib.util
import importlib.metadata
import json
import os
from pathlib import Path
import re
import sys
import tomllib
import xml.etree.ElementTree as ET

ROOT = Path(__file__).resolve().parents[2]
EXAMPLE = ROOT / "Examples/G1CokeDemo"
SPEC = importlib.util.spec_from_file_location("owned_coke_process", ROOT / ".github/scripts/run-owned-media-test.py")
GUARDIAN = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GUARDIAN)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    output = args.output.resolve()
    output.mkdir(mode=0o700, parents=True, exist_ok=False)
    evidence = {"audit": "2022", "status": "running", "versions": {}, "sources": {}}
    try:
        if sys.version_info[:2] != (3, 12):
            raise RuntimeError("use the shipped Coke Python 3.12 runtime")
        project = tomllib.loads((EXAMPLE / "pyproject.toml").read_text())
        lock = tomllib.loads((EXAMPLE / "uv.lock").read_text())
        dependencies = project["project"]["dependencies"] + project["dependency-groups"]["dev"]
        for requirement in dependencies:
            name = re.split(r"[<>=\[]", requirement, maxsplit=1)[0].lower()
            versions = {package["version"] for package in lock["package"] if package["name"].lower() == name}
            actual = importlib.metadata.version(name)
            if versions != {actual}:
                raise RuntimeError(f"{name}: installed {actual} differs from shipped lock {versions}")
            evidence["versions"][name] = actual
        files = [EXAMPLE / "pyproject.toml", EXAMPLE / "uv.lock"]
        files += sorted(path for folder in ("runtime", "physical_io", "coke_demo", "tests") for path in (EXAMPLE / folder).rglob("*.py"))
        evidence["sources"] = {str(path.relative_to(ROOT)): hashlib.sha256(path.read_bytes()).hexdigest() for path in files}
        environment = os.environ.copy()
        environment["PYTHONPATH"] = str(EXAMPLE)
        environment["PYTEST_DISABLE_PLUGIN_AUTOLOAD"] = "1"
        environment["PYTHONDONTWRITEBYTECODE"] = "1"
        report = output / "junit.xml"
        with (output / "review.log").open("w") as log:
            result = GUARDIAN.run([sys.executable, "-m", "pytest", "-q", "-p", "no:cacheprovider", "--junitxml", str(report), "tests/test_review_regressions.py", "tests/test_transport_boundaries.py"], 120, cwd=EXAMPLE, env=environment, stdout=log)
        evidence["exit_code"] = result
        if result != 0:
            raise RuntimeError("required Coke review suite failed; see review.log")
        cases = ET.parse(report).findall(".//testcase")
        if len(cases) < 40 or any(case.find(tag) is not None for case in cases for tag in ("skipped", "failure", "error")):
            raise RuntimeError("all 40+ required review cases must execute and pass; skips are failures")
        evidence["tests"] = [case.attrib for case in cases]
        evidence["status"] = "passed"
        print(f"COKE_LOCKED_REVIEW_VERIFIED: {len(cases)} cases without skips")
    except BaseException as error:
        evidence["status"] = "failed"
        evidence["error"] = f"{type(error).__name__}: {error}"
        raise
    finally:
        (output / "evidence.json").write_text(json.dumps(evidence, indent=2) + "\n")


if __name__ == "__main__":
    main()
