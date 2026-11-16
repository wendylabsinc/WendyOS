#!/usr/bin/env python3
"""Keep security scans tied to the release toolchain and resolved lockfiles."""

import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

from ci_execution_test import ROOT, load_yaml, workflow


class DependencyGatesTests(unittest.TestCase):
    def test_scans_block_the_required_test_result(self):
        jobs = workflow("go-tests.yml")["jobs"]
        scan = jobs["dependency-security"]
        self.assertEqual(scan["uses"], "./.github/workflows/security.yml")
        self.assertNotIn("if", scan)
        self.assertNotIn("continue-on-error", scan)
        self.assertIn("dependency-security", jobs["test"]["needs"])
        gate = jobs["test"]["steps"][0]
        self.assertEqual(gate["env"]["DEPENDENCY_SECURITY_RESULT"],
                         "${{ needs.dependency-security.result }}")
        self.assertIn('test "$DEPENDENCY_SECURITY_RESULT" = success', gate["run"])
        security = workflow("security.yml")
        self.assertIn("workflow_call", security["on"])
        self.assertIn("schedule", security["on"])
        # Called and standalone scans must not cancel one another.
        self.assertIn("github.workflow", security["concurrency"]["group"])
        for job in security["jobs"].values():
            self.assertNotIn("if", job)
            self.assertNotIn("continue-on-error", job)
            for step in job["steps"]:
                self.assertNotIn("continue-on-error", step)

    def test_npm_audits_both_shipped_lockfiles_including_tooling(self):
        job = workflow("security.yml")["jobs"]["npm-audit"]
        directories = job["strategy"]["matrix"]["directory"]
        self.assertEqual(set(directories), {"go/internal/cli/assets/docs", "web-client"})
        step = next(s for s in job["steps"] if s.get("name") == "Audit the complete resolved dependency graph")
        self.assertEqual(step["working-directory"], "${{ matrix.directory }}")
        for directory in directories:
            with self.subTest(directory=directory), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                manifest = json.loads((ROOT / directory / "package.json").read_text())
                locked = json.loads((ROOT / directory / "package-lock.json").read_text())["packages"][""]
                self.assertEqual(manifest["dependencies"], locked["dependencies"])
                self.assertEqual(manifest["devDependencies"], locked["devDependencies"])
                # Exercise the workflow command. An advisory/network failure must
                # reach CI; it cannot be masked with `|| true` or an omitted graph.
                binary = root / "npm"
                binary.write_text('#!/bin/sh\nprintf "%s\\n" "$PWD" "$@" > "$CALLS"\nexit 42\n')
                binary.chmod(0o755)
                calls = root / "calls"
                result = subprocess.run(["bash", "-euo", "pipefail", "-c", step["run"]],
                                        cwd=ROOT / directory, capture_output=True, text=True, timeout=10,
                                        env=dict(os.environ, PATH=f"{root}{os.pathsep}{os.environ['PATH']}", CALLS=str(calls)))
                self.assertEqual(result.returncode, 42, result.stderr)
                arguments = calls.read_text().splitlines()
                self.assertEqual(Path(arguments[0]).resolve(), (ROOT / directory).resolve())
                self.assertEqual(arguments[1:], ["audit", "--package-lock-only", "--audit-level=high"])

    def test_go_scanner_and_release_builds_use_the_security_minimum(self):
        version = re.search(r"^go (\d+)\.(\d+)\.(\d+)$", (ROOT / "go.mod").read_text(), re.M)
        self.assertIsNotNone(version)
        self.assertGreaterEqual(tuple(map(int, version.groups())), (1, 27, 4))
        for name in ("security.yml", "build.yml", "nightly-os-update.yml"):
            for job in workflow(name)["jobs"].values():
                for step in job.get("steps", []):
                    if step.get("uses", "").startswith("actions/setup-go@"):
                        self.assertEqual(step["with"]["go-version-file"], "go.mod", name)
                        self.assertNotIn("go-version", step["with"])
        scan = workflow("security.yml")["jobs"]["govulncheck"]
        self.assertEqual(scan["runs-on"], "ubuntu-24.04")
        install = next(s for s in scan["steps"] if s.get("name") == "Install govulncheck")
        self.assertRegex(install["run"], r"^go install golang.org/x/vuln/cmd/govulncheck@v\d+\.\d+\.\d+$")
        run = next(s for s in scan["steps"] if s.get("name") == "Run govulncheck")
        self.assertEqual(run["working-directory"], "go")
        self.assertEqual(run["run"], "/usr/bin/time -v govulncheck ./...")
        setup = load_yaml(ROOT / ".github/actions/setup-go-ci/action.yml")["runs"]["steps"][0]
        self.assertEqual(setup["with"]["go-version-file"], "go.mod")

    def test_node_security_floors_across_workflows_and_device_image(self):
        # Floors introduced by #2471. Add a reviewed floor when adopting a new
        # release line; a floating major must not silently select an old patch.
        floors = {22: (22, 23, 4), 24: (24, 22, 1)}
        versions = []
        for path in (ROOT / ".github/workflows").glob("*.yml"):
            for job in load_yaml(path).get("jobs", {}).values():
                for step in job.get("steps", []):
                    if step.get("uses", "").startswith("actions/setup-node@"):
                        versions.append((str(path.relative_to(ROOT)), step["with"]["node-version"]))
        dockerfile = (ROOT / "Examples/ClaudeOnDevice/Dockerfile").read_text()
        versions.append(("ClaudeOnDevice", re.search(r"^FROM node:([\d.]+)-", dockerfile, re.M).group(1)))
        versions.append(("web-client engine", json.loads((ROOT / "web-client/package.json").read_text())["engines"]["node"].removeprefix(">=")))
        self.assertGreaterEqual(len(versions), 9)
        for source, version in versions:
            with self.subTest(source=source):
                self.assertRegex(version, r"^\d+\.\d+\.\d+$")
                parsed = tuple(map(int, version.split(".")))
                self.assertIn(parsed[0], floors)
                self.assertGreaterEqual(parsed, floors[parsed[0]])


if __name__ == "__main__":
    unittest.main()
