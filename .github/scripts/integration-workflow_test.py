#!/usr/bin/env python3
"""Check the release DAG and run its status checks with synthetic job results.

Requires PyYAML. No network, containers or devices are used.
"""
import ast
import copy
import os
from pathlib import Path
import re
import subprocess
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[2]
TOKEN = re.compile(r"\s*('(?:[^']|'')*'|&&|\|\||==|!=|!|\(|\)|[A-Za-z_][A-Za-z0-9_.-]*)")


def dependencies(job):
    value = job.get("needs", [])
    return [value] if isinstance(value, str) else value


class Condition:
    """Evaluate the Boolean/status subset used by these job conditions."""

    def __init__(self, expression, values, results):
        expression = str(expression).strip().removeprefix("${{").removesuffix("}}").strip()
        self.tokens = []
        while expression:
            match = TOKEN.match(expression)
            if not match:
                raise ValueError(f"Unsupported condition syntax: {expression!r}")
            self.tokens.append(match.group(1))
            expression = expression[match.end():].strip()
        self.position = 0
        self.values = values
        self.results = results

    def accept(self, token):
        if self.position < len(self.tokens) and self.tokens[self.position] == token:
            self.position += 1
            return True
        return False

    def value(self):
        result = self.disjunction()
        if self.position != len(self.tokens):
            raise ValueError("Unparsed condition tokens")
        return bool(result)

    def disjunction(self):
        left = self.conjunction()
        while self.accept("||"):
            right = self.conjunction()
            left = bool(left) or bool(right)
        return left

    def conjunction(self):
        left = self.comparison()
        while self.accept("&&"):
            right = self.comparison()
            left = bool(left) and bool(right)
        return left

    def comparison(self):
        left = self.atom()
        if self.accept("=="):
            return left == self.atom()
        if self.accept("!="):
            return left != self.atom()
        return left

    def atom(self):
        if self.accept("!"):
            return not self.atom()
        if self.accept("("):
            value = self.disjunction()
            if not self.accept(")"):
                raise ValueError("Missing closing parenthesis")
            return value
        token = self.tokens[self.position]
        self.position += 1
        if token.startswith("'"):
            return token[1:-1].replace("''", "'")
        if token in ("true", "false"):
            return token == "true"
        if self.accept("("):
            if not self.accept(")"):
                raise ValueError("Only zero-argument status functions are supported")
            return {"always": True,
                    "success": all(result == "success" for result in self.results),
                    "failure": "failure" in self.results,
                    "cancelled": "cancelled" in self.results}[token]
        return self.values[token]


def enabled(job, statuses, **overrides):
    values = {"github.event_name": "workflow_dispatch", "inputs.publish": True,
              "inputs.jobs": "integration", "inputs.platform": "all",
              "needs.determine-version.outputs.is_release": "true",
              "needs.determine-version.outputs.is_prerelease": "false"}
    values.update({f"needs.{name}.result": result for name, result in statuses.items()})
    values.update(overrides)
    expression = job.get("if", "success()")
    results = [statuses[name] for name in dependencies(job)]
    # GitHub implicitly applies success() unless a status function is present.
    if not re.search(r"\b(always|success|failure|cancelled)\s*\(", expression):
        if any(result != "success" for result in results):
            return False
    return Condition(expression, values, results).value()


def ancestors(jobs, name, visiting=()):
    if name in visiting:
        raise ValueError(f"Workflow dependency cycle: {visiting + (name,)}")
    found = set()
    for dependency in dependencies(jobs[name]):
        found.add(dependency)
        found.update(ancestors(jobs, dependency, visiting + (name,)))
    return found


def gate_result(jobs, name, gate_status, memo=None):
    memo = {} if memo is None else memo
    if name == "integration-tests":
        return gate_status
    if name in memo:
        return memo[name]
    statuses = {dependency: gate_result(jobs, dependency, gate_status, memo)
                for dependency in dependencies(jobs[name])}
    memo[name] = "success" if enabled(jobs[name], statuses) else "skipped"
    return memo[name]


class WorkflowTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.release = yaml.safe_load((ROOT / ".github/workflows/build.yml").read_text())
        cls.hardware_text = (ROOT / ".github/workflows/integration-tests.yml").read_text()
        cls.hardware = yaml.safe_load(cls.hardware_text)

    def test_discovery_input_validation_rejects_invalid_candidates(self):
        check = next(step for step in self.hardware["jobs"]["discover"]["steps"]
                     if step.get("name") == "Validate qualification inputs")
        base = dict(os.environ, INPUT_JOBS="integration", INPUT_PLATFORM="all",
                    SOURCE_REF="a" * 40, OS_VERSION="nightly-20261104", OS_SHA="b" * 40,
                    OS_REPO="wendylabsinc/WendyOS-Builder", OS_RUN="123",
                    INPUT_HOST="", INPUT_TESTS="",
                    GITHUB_REPOSITORY="wendylabsinc/WendyOS-Builder", GITHUB_RUN_ID="123")
        # Execute the workflow's actual shell code, including on macOS Bash 3.2,
        # where a failed [[ ... ]] does not reliably trigger errexit.
        for overrides in ({}, {"OS_VERSION": "", "OS_SHA": "", "OS_REPO": "", "OS_RUN": ""}):
            with self.subTest(valid=overrides):
                result = subprocess.run(["bash", "-c", check["run"]], env=dict(base, **overrides),
                                        capture_output=True, timeout=5)
                self.assertEqual(result.returncode, 0, result.stderr.decode())
        for overrides in ({"INPUT_JOBS": "invalid"}, {"INPUT_PLATFORM": "invalid"},
                          {"SOURCE_REF": "main"}, {"OS_VERSION": "invalid/version"},
                          {"OS_VERSION": ""}, {"OS_SHA": "invalid"}, {"OS_SHA": ""},
                          {"OS_REPO": "unrelated/repository"}, {"OS_REPO": ""},
                          {"OS_RUN": "456"}, {"OS_RUN": ""},
                          {"INPUT_HOST": "pi.local"}, {"INPUT_TESTS": "selected-fixture"},
                          {"INPUT_JOBS": "discover"}):
            with self.subTest(invalid=overrides):
                result = subprocess.run(["bash", "-c", check["run"]], env=dict(base, **overrides),
                                        capture_output=True, timeout=5)
                self.assertNotEqual(result.returncode, 0, f"accepted invalid inputs: {overrides}")

    def test_release_artifacts_exist_before_integration_downloads(self):
        jobs = self.release["jobs"]
        self.assertTrue({"determine-version", "build", "build-cli-linux", "build-go-macos"}
                        <= ancestors(jobs, "integration-tests"))
        configuration = jobs["integration-tests"]["with"]
        self.assertEqual(configuration["platform"], "all")
        self.assertEqual(configuration["jobs"], "integration")
        self.assertEqual(configuration["source_ref"], "${{ github.sha }}")
        self.assertEqual(configuration["candidate_version"], "${{ needs.determine-version.outputs.version }}")
        self.assertFalse(configuration.get("hostname"))
        self.assertFalse(configuration.get("tests"))

    def test_every_stable_publisher_is_blocked_by_unsuccessful_qualification(self):
        jobs = self.release["jobs"]
        publishers = {name for name in jobs if name.startswith("publish-")} | {"release"}
        self.assertTrue({"publish-agent-gcs", "publish-install-scripts", "publish-linux-repos",
                         "publish-aur", "publish-winget"} <= publishers)
        for name in sorted(publishers):
            with self.subTest(job=name):
                self.assertIn("integration-tests", ancestors(jobs, name))
                self.assertEqual(gate_result(jobs, name, "success"), "success")
                for status in ("failure", "skipped", "cancelled"):
                    self.assertEqual(gate_result(jobs, name, status), "skipped",
                                     f"{name} can publish after hardware qualification is {status}")

    def test_gate_oracle_detects_an_always_publisher_bypass(self):
        jobs = copy.deepcopy(self.release["jobs"])
        jobs["publish-agent-gcs"]["if"] = "always()"
        self.assertEqual(gate_result(jobs, "publish-agent-gcs", "failure"), "success")

    def test_default_workflow_uses_registry_without_a_second_fixture_list(self):
        script = (ROOT / "go/scripts/test-ci.sh").read_text()
        registry = re.search(r"(?m)^ALL_TESTS=\((.*?)^\)", script, re.S | re.M)
        self.assertIsNotNone(registry)
        names = registry.group(1).split()
        self.assertIn("host-sensor-loopback", names)
        for name in names:
            self.assertNotRegex(self.hardware_text, rf"(?<![A-Za-z0-9_-]){re.escape(name)}(?![A-Za-z0-9_-])")
        # PyYAML's YAML 1.1 resolver reads the unquoted GitHub "on" key as True.
        events = self.hardware.get("on", self.hardware.get(True))
        self.assertEqual(events["workflow_call"]["inputs"]["tests"]["default"], "")

    def test_final_gate_runs_after_failed_or_skipped_required_jobs(self):
        final = self.hardware["jobs"]["qualification"]
        required = {"discover", "integration-test-macos", "integration-test-linux"}
        self.assertEqual(set(dependencies(final)), required)
        self.assertFalse(final.get("continue-on-error"))
        for failed in required:
            for result in ("failure", "skipped", "cancelled"):
                statuses = {name: "success" for name in required}
                statuses[failed] = result
                self.assertTrue(enabled(final, statuses), f"final gate skipped after {failed}={result}")

    def test_final_status_check_rejects_every_missing_required_job(self):
        final = self.hardware["jobs"]["qualification"]
        checks = [step for step in final["steps"] if "DISCOVER_RESULT" in step.get("env", {})]
        self.assertEqual(len(checks), 1)
        check = checks[0]
        self.assertFalse(check.get("continue-on-error"))
        required = {"all": ("DISCOVER_RESULT", "MACOS_RESULT", "LINUX_RESULT"),
                    "macos": ("DISCOVER_RESULT", "MACOS_RESULT"),
                    "linux": ("DISCOVER_RESULT", "LINUX_RESULT")}
        for platform, names in required.items():
            base = dict(os.environ, PLATFORM=platform, DISCOVER_RESULT="success",
                        MACOS_RESULT="success", LINUX_RESULT="success")
            good = subprocess.run(["bash", "-c", check["run"]], env=base, capture_output=True, timeout=5)
            self.assertEqual(good.returncode, 0, good.stderr.decode())
            for name in names:
                for status in ("failure", "skipped", "cancelled"):
                    with self.subTest(platform=platform, job=name, status=status):
                        failed = subprocess.run(["bash", "-c", check["run"]],
                                                env=dict(base, **{name: status}), capture_output=True, timeout=5)
                        self.assertNotEqual(failed.returncode, 0, f"qualified {platform} with {name}={status}")

    def test_final_gate_checks_receipts_for_the_discovered_matrix(self):
        final = self.hardware["jobs"]["qualification"]
        checks = [step for step in final["steps"] if "integration-results.py" in step.get("run", "")]
        self.assertEqual(len(checks), 1)
        check = checks[0]
        self.assertFalse(check.get("continue-on-error"))
        self.assertEqual(check["env"]["HOSTS"], "${{ needs.discover.outputs.matrix }}")
        self.assertEqual(check["env"]["SOURCE_SHA"], "${{ needs.discover.outputs.source_sha }}")
        self.assertEqual(check["env"]["CANDIDATE_VERSION"], "${{ inputs.candidate_version }}")
        self.assertIn('--candidate-version "$CANDIDATE_VERSION"', check["run"])
        self.assertIn("--require-matrix", check["run"])
        self.assertIn("--hosts", check["run"])
        for platform in ("macos", "linux"):
            job = self.hardware["jobs"][f"integration-test-{platform}"]
            self.assertFalse(job.get("continue-on-error"))
            self.assertIs(job["strategy"]["fail-fast"], False)
            self.assertEqual(job["strategy"]["matrix"]["hostname"],
                             "${{ fromJson(needs.discover.outputs.matrix) }}")

    def test_device_workflows_wrap_mutations_in_cross_repository_lease(self):
        for platform in ("macos", "linux"):
            job = self.hardware["jobs"][f"integration-test-{platform}"]
            scripts = [step["run"] for step in job["steps"]
                       if "integration-run.py" in step.get("run", "")]
            self.assertEqual(len(scripts), 1)
            python = re.search(r"python3 - <<'PYREMOTE'\n(.*?)\nPYREMOTE", scripts[0], re.S)
            self.assertIsNotNone(python)
            tree = ast.parse(python.group(1))
            assignments = [node for node in ast.walk(tree) if isinstance(node, ast.Assign)
                           and any(isinstance(target, ast.Name) and target.id == "args" for target in node.targets)]
            wrappers = [node for node in assignments
                        if isinstance(node.value, ast.BinOp) and isinstance(node.value.op, ast.Add)
                        and isinstance(node.value.right, ast.Name) and node.value.right.id == "args"
                        and any(isinstance(item, ast.Constant) and str(item.value).endswith("/integration-lease.py")
                                for item in ast.walk(node.value.left))]
            self.assertEqual(len(wrappers), 1, "device command must be the child of the persistent lease wrapper")
            constants = [node.value for node in ast.walk(wrappers[0].value.left) if isinstance(node, ast.Constant)]
            for argument in ("--device", "--owner", "--release-marker", "--"):
                self.assertIn(argument, constants)
            earlier = [node for node in assignments if node.lineno < wrappers[0].lineno]
            self.assertTrue(any(isinstance(item, ast.Constant) and str(item.value).endswith("/integration-run.py")
                                for node in earlier for item in ast.walk(node)))

    def test_artifact_uploads_retain_hidden_evidence_directories(self):
        nightly = yaml.safe_load((ROOT / ".github/workflows/nightly-os-update.yml").read_text())
        for workflow in (self.hardware, nightly):
            for name, job in workflow["jobs"].items():
                for step in job.get("steps", []):
                    if step.get("uses", "").startswith("actions/upload-artifact@"):
                        with self.subTest(workflow=workflow["name"], job=name):
                            self.assertEqual(step["if"], "always()")
                            self.assertIs(step["with"]["include-hidden-files"], True)

    def test_nightly_and_template_mutations_are_lease_children(self):
        cases = (("nightly-os-update.yml", "update", ".github/scripts/nightly-device-check.py"),
                 ("test-templates.yml", "deploy-to-device", "go/scripts/test-templates.sh"))
        for filename, job_name, child in cases:
            with self.subTest(workflow=filename):
                workflow = yaml.safe_load((ROOT / ".github/workflows" / filename).read_text())
                scripts = [step["run"] for step in workflow["jobs"][job_name]["steps"]
                           if child in step.get("run", "")]
                self.assertEqual(len(scripts), 1)
                python = re.search(r"python3 [^\n]*<<'PYREMOTE'\n(.*?)\nPYREMOTE", scripts[0], re.S)
                self.assertIsNotNone(python)
                tree = ast.parse(python.group(1))
                commands = [node.value for node in ast.walk(tree) if isinstance(node, ast.Assign)
                            and isinstance(node.value, ast.List)
                            and any(isinstance(target, ast.Name) and target.id == "args"
                                    for target in node.targets)]
                commands = [[constant.value for item in command.elts for constant in ast.walk(item)
                             if isinstance(constant, ast.Constant)] for command in commands]
                leased = [command for command in commands if ".github/scripts/integration-lease.py" in command]
                self.assertEqual(len(leased), 1)
                command = leased[0]
                self.assertLess(command.index(".github/scripts/integration-lease.py"), command.index("--"))
                self.assertLess(command.index("--"), command.index(child))
                for argument in ("--wendy", "--device", "--owner", "--command-timeout-seconds"):
                    self.assertIn(argument, command[:command.index("--")])


if __name__ == "__main__":
    unittest.main()
