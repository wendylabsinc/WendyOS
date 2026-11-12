"""Audit 2251: execute release policy expressions and the actual version step.

No GitHub jobs are dispatched. The expression subset used here has the same
boolean/string operators in JavaScript and Actions; inputs remain typed booleans.
Tag immutability is exercised separately by tag-built-commit_test.py with real
Git references and a local API fixture.
"""
import copy
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[2]


def workflow(name):
    # BaseLoader constructs only strings/lists/maps and preserves Actions' on key.
    loader = yaml.BaseLoader((ROOT / ".github/workflows" / name).read_text())
    try:
        return loader.get_single_data()
    finally:
        loader.dispose()


def cases():
    rows = []
    for event in ("push", "pull_request", "workflow_dispatch", "release"):
        for ref in ("refs/heads/main", "refs/heads/release-candidate"):
            for publish, skip in ((False, False), (False, True), (True, False), (True, True)):
                rows.append({"github": {"event_name": event, "ref": ref, "run_id": len(rows) + 1},
                             "inputs": {"publish": publish, "skip_integration_tests": skip}})
    return rows


def evaluate(build, rows):
    script = r"""
const input = JSON.parse(require('fs').readFileSync(0, 'utf8'));
function evaluate(text, context) {
  const expression = text.trim().replace(/^\$\{\{|\}\}$/g, '');
  return new Function('github', 'inputs', 'return (' + expression + ')')
    (context.github, context.inputs);
}
console.log(JSON.stringify(input.rows.map(context => ({
  group: input.group.replace(/\$\{\{(.*?)\}\}/g, (_, expr) => evaluate(expr, context)),
  cancel: evaluate(input.cancel, context),
  integration: Boolean(evaluate(input.integration, context))
}))));
"""
    data = {"rows": rows, "group": build["concurrency"]["group"],
            "cancel": build["concurrency"]["cancel-in-progress"],
            "integration": build["jobs"]["integration-tests"]["if"]}
    result = subprocess.run(["node", "-e", script], input=json.dumps(data), text=True,
                            capture_output=True, check=True, timeout=10)
    return json.loads(result.stdout)


def assert_policy(test, build):
    rows = cases()
    observed = evaluate(build, rows)
    for row, result in zip(rows, observed):
        event = row["github"]["event_name"]
        stable = event == "workflow_dispatch" and row["inputs"]["publish"]
        test.assertEqual(result["cancel"], not stable, row)
        test.assertEqual(result["integration"], stable and not row["inputs"]["skip_integration_tests"], row)
        repeated = copy.deepcopy(row)
        repeated["github"]["run_id"] += 1000
        following = evaluate(build, [repeated])[0]
        if stable:
            test.assertNotEqual(result["group"], following["group"], "later stable release replaces the queued run")
            push = copy.deepcopy(row)
            push["github"]["event_name"] = "push"
            test.assertNotEqual(result["group"], evaluate(build, [push])[0]["group"],
                                "main push shares the stable release group")
        else:
            test.assertEqual(result["group"], following["group"], "same-ref non-release builds cannot supersede")
    test.assertNotEqual(observed[0]["group"], observed[4]["group"], "unrelated branches share a group")


class ReleasePolicyTests(unittest.TestCase):
    def test_actual_release_expressions_preserve_stable_runs_and_limit_hardware(self):
        assert_policy(self, workflow("build.yml"))

    def test_original_cancellation_and_hardware_policy_regressions_are_rejected(self):
        for regression in ("shared-group", "cancel-stable", "hardware-on-push"):
            build = workflow("build.yml")
            if regression == "shared-group":
                build["concurrency"]["group"] = "build-${{ github.ref }}"
            elif regression == "cancel-stable":
                build["concurrency"]["cancel-in-progress"] = "true"
            else:
                build["jobs"]["integration-tests"]["if"] = "github.event_name != 'workflow_dispatch' || !inputs.skip_integration_tests"
            with self.subTest(regression=regression), self.assertRaises(AssertionError):
                assert_policy(self, build)

    def test_version_step_only_classifies_explicit_publish_as_stable(self):
        step = next(s for s in workflow("build.yml")["jobs"]["determine-version"]["steps"]
                    if s.get("id") == "set-version")
        for row in cases():
            with self.subTest(row=row), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                date = root / "date"
                date.write_text("#!/bin/sh\nprintf '2026.11.12-120000\\n'\n")
                date.chmod(0o755)
                output = root / "output"
                source = step["run"].replace("${{ github.event_name }}", row["github"]["event_name"])
                source = source.replace("${{ inputs.publish }}", str(row["inputs"]["publish"]).lower())
                self.assertNotIn("${{", source, "new Actions expression requires a typed fixture input")
                result = subprocess.run(["bash", "-euo", "pipefail", "-c", source], cwd=root,
                                        env={**os.environ, "PATH": str(root) + os.pathsep + os.environ["PATH"],
                                             "GITHUB_REF": row["github"]["ref"], "GITHUB_OUTPUT": str(output)},
                                        text=True, capture_output=True, timeout=10)
                self.assertEqual(result.returncode, 0, result.stderr)
                values = dict(line.split("=", 1) for line in output.read_text().splitlines())
                stable = row["github"]["event_name"] == "workflow_dispatch" and row["inputs"]["publish"]
                self.assertEqual(values["is_release"], str(stable).lower())
                self.assertEqual(values["is_prerelease"], str(not stable).lower())
                self.assertEqual(values["tag_name"], values["version"])
                self.assertEqual(values["version"], "2026.11.12-120000" +
                                 ("" if row["github"]["ref"] == "refs/heads/main" else "-dev"))

    def test_template_queue_and_no_automatic_pr_hardware_entry_point(self):
        templates = workflow("test-templates.yml")
        self.assertEqual(templates["concurrency"]["queue"], "max")
        self.assertEqual(templates["concurrency"]["cancel-in-progress"], "false")
        self.assertEqual(set(templates["on"]), {"workflow_call", "workflow_dispatch"})
        self.assertFalse((ROOT / ".github/workflows/pr-integration-tests.yml").exists())
        build = workflow("build.yml")
        self.assertEqual(set(build["on"]), {"push", "workflow_dispatch"})
        self.assertEqual(build["on"]["push"]["branches"], ["main"])
        self.assertEqual(build["jobs"]["integration-tests"]["uses"], "./.github/workflows/integration-tests.yml")


if __name__ == "__main__":
    unittest.main()
