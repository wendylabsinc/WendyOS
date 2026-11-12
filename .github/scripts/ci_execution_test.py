#!/usr/bin/env python3
"""Exercise workflow failure/concurrency behavior with local command fixtures."""

import json
import fnmatch
import os
from pathlib import Path
import shlex
import subprocess
import tempfile
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[2]


def load_yaml(path):
    # BaseLoader only constructs strings/lists/maps and preserves GitHub's `on` key.
    loader = yaml.BaseLoader(path.read_text())
    try:
        return loader.get_single_data()
    finally:
        loader.dispose()


def workflow(name):
    return load_yaml(ROOT / ".github/workflows" / name)


class CIExecutionTests(unittest.TestCase):
    def test_integration_matrix_requires_platforms_except_explicit_target_or_discovery(self):
        step = next(s for s in workflow("integration-tests.yml")["jobs"]["discover"]["steps"]
                    if s.get("id") == "scan")
        for hostname, jobs, required in [("", "all", True), ("", "integration", True),
                                         ("", "discover", False), ("selected.local", "all", False)]:
            with self.subTest(hostname=hostname, jobs=jobs), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                ssh = root / "ssh"
                ssh.write_text('''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
Path(os.environ["SSH_CALL"]).write_text(json.dumps(sys.argv[1:]))
print('["selected.local"]')
''')
                ssh.chmod(0o755)
                call = root / "ssh.json"
                output = root / "output"
                result = subprocess.run(["bash", "-euo", "pipefail", "-c", step["run"]], cwd=ROOT,
                                        env=dict(os.environ, PATH=f"{root}{os.pathsep}{os.environ['PATH']}",
                                                 SSH_CALL=str(call), INPUT_HOSTNAME=hostname, INPUT_JOBS=jobs,
                                                 GITHUB_OUTPUT=str(output), GITHUB_STEP_SUMMARY=""),
                                        capture_output=True, text=True, timeout=10)
                self.assertEqual(result.returncode, 0, result.stderr)
                command = shlex.split(json.loads(call.read_text())[-1])
                self.assertIn(str(ROOT / ".github/scripts/integration-discover.py"), command)
                self.assertEqual("--require-platforms" in command, required, command)
                self.assertEqual(output.read_text().strip(), 'matrix=["selected.local"]')

    def test_main_e2e_runs_do_not_cancel_each_other_but_pr_pushes_do(self):
        policy = workflow("swift-e2e-tests.yml")["concurrency"]
        # Evaluate the actual expressions; do not duplicate their implementation.
        script = r"""
const input = JSON.parse(require('fs').readFileSync(0, 'utf8'));
function expression(text, github) {
  return new Function('github', 'return (' + text.trim().replace(/^\$\{\{|\}\}$/g, '') + ')')(github);
}
function group(event) {
  return input.group.replace(/\$\{\{(.*?)\}\}/g, (_, expr) => expression(expr, event));
}
const a = {ref: 'refs/heads/main', run_id: 1};
const b = {ref: 'refs/heads/main', run_id: 2};
const p = {ref: 'refs/pull/42/merge', run_id: 3};
const q = {ref: 'refs/pull/42/merge', run_id: 4};
const other = {ref: 'refs/pull/43/merge', run_id: 5};
if (group(a) === group(b) || expression(input.cancel, a)) throw Error('main runs cancel each other');
if (group(p) !== group(q) || !expression(input.cancel, p)) throw Error('stale PR runs survive');
if (group(p) === group(other)) throw Error('unrelated PRs share a group');
"""
        subprocess.run(["node", "-e", script], input=json.dumps({
            "group": policy["group"], "cancel": policy["cancel-in-progress"]
        }), text=True, check=True, timeout=10)

    def test_failed_dependency_refresh_never_installs_from_stale_indexes(self):
        jobs = workflow("go-tests.yml")["jobs"]
        for job in ("unit-tests", "vet"):
            with self.subTest(job=job), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                calls = root / "calls"
                apt = root / "apt-get"
                apt.write_text('#!/bin/sh\nprintf "%s\\n" "$*" >> "$CALLS"\nexit 42\n')
                sudo = root / "sudo"
                sudo.write_text('#!/bin/sh\n[ "$1" = apt-get ] || exit 99\nexec "$@"\n')
                apt.chmod(0o755)
                sudo.chmod(0o755)
                step = next(s for s in jobs[job]["steps"] if s.get("name") == "Install system dependencies")
                result = subprocess.run(["bash", "-euo", "pipefail", "-c", step["run"]],
                                        env=dict(os.environ, PATH=f"{root}{os.pathsep}{os.environ['PATH']}", CALLS=str(calls)),
                                        capture_output=True, text=True, timeout=10)
                self.assertEqual(result.returncode, 42, result.stderr)
                self.assertEqual(calls.read_text().splitlines(), ["update"])

    def test_race_budget_remains_bounded_and_vet_is_separate(self):
        jobs = workflow("go-tests.yml")["jobs"]
        self.assertEqual(jobs["unit-tests"]["runs-on"], "runs-on,runner=2cpu-linux-x64")
        self.assertGreater(int(jobs["unit-tests"]["timeout-minutes"]), 0)
        self.assertLessEqual(int(jobs["unit-tests"]["timeout-minutes"]), 30)
        command = next(s["run"] for s in jobs["unit-tests"]["steps"] if s.get("name") == "Run tests")
        self.assertIn("-race", command)
        self.assertIn("-gcflags=all=-c=1", command)
        self.assertIn("-vet=off", command)
        self.assertIn("-timeout 600s", command)
        self.assertTrue(any(s.get("run") == "go vet ./..." for s in jobs["vet"]["steps"]))

    def test_automation_gate_is_part_of_required_test_without_an_opt_in(self):
        config = workflow("go-tests.yml")
        job = config["jobs"]["automation-regressions"]
        self.assertNotIn("if", job)
        self.assertFalse(any("if" in step for step in job["steps"]))
        self.assertIn("pull_request", config["on"])
        self.assertIn(".github/scripts/**", config["on"]["pull_request"]["paths"])
        self.assertIn(".github/dependabot.yml", config["on"]["pull_request"]["paths"])
        for event in ("push", "pull_request"):
            for filename in (".github/ci-tests/python-gpu/main.py", ".github/ci-tests/python-onnx-gpu/main.py",
                             ".github/regression-coverage/manual/jetson-display-rehearsal.py",
                             ".github/regression-coverage/manual/fixtures/jetson-display/main.c",
                             "swift/Scripts/SetupSigningCI.sh", "swift/Scripts/Build.sh",
                             "Examples/SO101Reader/reader.py", "Examples/SO101Reader/test_reader.py",
                             "Examples/WendyNotify/Sources/WendyNotify/Router.swift",
                             "Examples/WendyNotify/Tests/WendyNotifyTests/RequestPathTests.swift"):
                self.assertTrue(any(fnmatch.fnmatchcase(filename, pattern) for pattern in config["on"][event]["paths"]),
                                f"{event} omits regression source {filename}")
        self.assertTrue(any("api-snapshot-smoke.py" in s.get("run", "")
                            for s in config["jobs"]["vet"]["steps"]))
        self.assertIn("automation-regressions", config["jobs"]["test"]["needs"])
        self.assertIn('test "$AUTOMATION_RESULT" = success', config["jobs"]["test"]["steps"][0]["run"])
        self.assertTrue(any("run-automation-regressions.py" in s.get("run", "") for s in job["steps"]))

    def test_dependabot_tracks_every_repository_module(self):
        config = load_yaml(ROOT / ".github/dependabot.yml")
        directories = []
        for update in config["updates"]:
            if update["package-ecosystem"] == "gomod":
                directories.extend(update.get("directories", [update.get("directory")]))
        # Git's tracked files exclude downloaded dependencies and temporary fixtures.
        tracked = subprocess.check_output(["git", "ls-files", "-z"], cwd=ROOT).decode().split("\0")
        discovered = {"/" if p == "go.mod" else "/" + str(Path(p).parent)
                      for p in tracked if p == "go.mod" or p.endswith("/go.mod")}
        self.assertEqual(set(directories), discovered)
        self.assertEqual(len(directories), len(set(directories)))


if __name__ == "__main__":
    unittest.main()
