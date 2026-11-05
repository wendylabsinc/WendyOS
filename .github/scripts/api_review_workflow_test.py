#!/usr/bin/env python3
"""Exercise the actual trusted-source resolver with mocked GitHub responses."""

import json
import os
import re
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

WORKFLOW = Path(__file__).resolve().parents[1] / "workflows" / "api-review.yml"
DOCS_WORKFLOW = WORKFLOW.with_name("docs-review.yml")
OLD_BASE = "b" * 40
PR_HEAD = "c" * 40
CURRENT_MAIN = "a" * 40
# A run keeps the sparse list of its own YAML while reading current main's
# scripts, so YAML that predates this workflow's test does not check it out.
needs_docs_workflow = unittest.skipUnless(DOCS_WORKFLOW.is_file(), "docs-review.yml is outside this sparse checkout")


def resolver_script(text):
    lines = text.splitlines()
    start = next(index for index, line in enumerate(lines) if line.strip() == "id: trusted-automation")
    script = next(index for index in range(start, len(lines)) if lines[index].strip() == "script: |")
    indentation = len(lines[script]) - len(lines[script].lstrip()) + 2
    body = []
    for line in lines[script + 1:]:
        if line.strip() and len(line) - len(line.lstrip()) < indentation:
            break
        body.append(line[indentation:] if line.strip() else "")
    return "\n".join(body)


def review_step_script():
    lines = WORKFLOW.read_text().splitlines()
    start = next(i for i, line in enumerate(lines) if line.strip() == "id: review")
    run = next(i for i in range(start, len(lines)) if lines[i].strip() == "run: |")
    body = []
    for line in lines[run + 1:]:
        if line.strip() and len(line) - len(line.lstrip()) < 10:
            break
        body.append(line[10:] if line.strip() else "")
    return "\n".join(body)


# Parser shape from trusted main 90f16e93 and source-only parent d061f63e.
# The fixture writes a result without SDK/network activity; --help exits before
# that write, just as the actual trusted parser does.
OLD_RUNNER = r'''import argparse, json
from pathlib import Path
root = argparse.ArgumentParser()
commands = root.add_subparsers(dest="command", required=True)
review = commands.add_parser("review")
for name in ("metadata", "diff", "repo", "expected-head-sha", "expected-base-sha", "output", "model"):
    review.add_argument(f"--{name}", required=True)
review.add_argument("--previous")
review.add_argument("--pr-number", type=int, required=True)
# optional_context_argument
args = root.parse_args()
Path(args.output).write_text(json.dumps(vars(args)))
'''


HARNESS = r"""
const fs = require("fs");
const input = JSON.parse(fs.readFileSync(0, "utf8"));
const calls = [], outputs = {};
const context = {repo: {owner: "wendylabsinc", repo: "WendyOS"}, payload: {
  pull_request: {base: {sha: input.oldBase}, head: {sha: input.prHead}}
}};
const github = {rest: {git: {getRef: async args => {
  calls.push(args);
  if (input.failLookup) throw new Error("mock lookup failure");
  return {data: input.response};
}}}};
const core = {setOutput: (name, value) => {outputs[name] = value;}, info: () => {}};
const AsyncFunction = Object.getPrototypeOf(async function() {}).constructor;
(async () => {
  let failed = false;
  try {await new AsyncFunction("github", "context", "core", input.script)(github, context, core);}
  catch {failed = true;}
  console.log(JSON.stringify({calls, outputs, failed}));
})();
"""


class TrustedSourceTests(unittest.TestCase):
    def run_resolver(self, response, fail_lookup=False, workflow=WORKFLOW):
        execution = subprocess.run(
            ["node", "-e", HARNESS], check=True, capture_output=True, text=True, timeout=10,
            input=json.dumps({"script": resolver_script(workflow.read_text()), "response": response,
                              "oldBase": OLD_BASE, "prHead": PR_HEAD, "failLookup": fail_lookup}))
        result = json.loads(execution.stdout)
        self.assertEqual(result["calls"], [{"owner": "wendylabsinc", "repo": "WendyOS", "ref": "heads/main"}])
        return result

    def test_retained_pr_base_cannot_pin_obsolete_automation(self):
        result = self.run_resolver({"object": {"type": "commit", "sha": CURRENT_MAIN}})
        self.assertFalse(result["failed"])
        self.assertEqual(result["outputs"], {"sha": CURRENT_MAIN})
        self.assertNotIn(result["outputs"]["sha"], (OLD_BASE, PR_HEAD))

    def test_invalid_ref_responses_fail_without_old_base_or_head_fallback(self):
        responses = [{}, {"object": {"type": "tag", "sha": CURRENT_MAIN}}]
        responses += [{"object": {"type": "commit", "sha": value}} for value in
                      (None, True, 123, [CURRENT_MAIN], {}, "", "A" * 40, "../main", "a" * 39, CURRENT_MAIN + "\n")]
        for response in responses:
            with self.subTest(response=response):
                result = self.run_resolver(response)
                self.assertTrue(result["failed"])
                self.assertEqual(result["outputs"], {})

    def test_lookup_failure_does_not_fall_back_to_obsolete_or_untrusted_source(self):
        result = self.run_resolver({}, fail_lookup=True)
        self.assertTrue(result["failed"])
        self.assertEqual(result["outputs"], {})

    def test_checkout_pin_and_evidence_guards_remain_separate(self):
        text = WORKFLOW.read_text()
        automatic_job = text.split("  validate-candidate-tests:", 1)[0]
        self.assertIn("ref: ${{ steps.trusted-automation.outputs.sha }}", automatic_job)
        self.assertNotIn("ref: ${{ github.event.pull_request.base.sha }}", automatic_job)
        self.assertIn("EXPECTED_BASE_SHA: ${{ github.event.pull_request.base.sha }}", automatic_job)
        self.assertIn(".head.sha == $head and .base.sha == $base", automatic_job)
        self.assertIn('merge-base "$EXPECTED_BASE_SHA" "$EXPECTED_HEAD_SHA"', automatic_job)
        self.assertIn("github.event.pull_request.head.repo.full_name == github.repository", automatic_job)
        self.assertIn("persist-credentials: false", automatic_job)
        # Otherwise the docs workflow test below would skip on every run.
        self.assertIn(".github/workflows/docs-review.yml", automatic_job)

    @needs_docs_workflow
    def test_sparse_checkout_without_docs_workflow_still_passes(self):
        # The copy also skips this test, so it cannot recurse.
        with tempfile.TemporaryDirectory() as directory:
            script = Path(directory) / ".github" / "scripts" / Path(__file__).name
            workflow = Path(directory) / ".github" / "workflows" / WORKFLOW.name
            script.parent.mkdir(parents=True)
            workflow.parent.mkdir(parents=True)
            shutil.copy(__file__, script)
            shutil.copy(WORKFLOW, workflow)
            execution = subprocess.run(
                [sys.executable, str(script)], capture_output=True, text=True, timeout=60)
        self.assertEqual(execution.returncode, 0, execution.stderr)
        self.assertIn("skipped=", execution.stderr)

    @needs_docs_workflow
    def test_docs_review_runs_current_main_when_pr_base_predates_its_scripts(self):
        result = self.run_resolver({"object": {"type": "commit", "sha": CURRENT_MAIN}}, workflow=DOCS_WORKFLOW)
        self.assertFalse(result["failed"])
        self.assertEqual(result["outputs"], {"sha": CURRENT_MAIN})
        self.assertTrue(self.run_resolver({"object": {"type": "tag", "sha": CURRENT_MAIN}}, workflow=DOCS_WORKFLOW)["failed"])
        self.assertTrue(self.run_resolver({}, fail_lookup=True, workflow=DOCS_WORKFLOW)["failed"])
        text = DOCS_WORKFLOW.read_text()
        self.assertIn("ref: ${{ steps.trusted-automation.outputs.sha }}", text)
        self.assertNotIn("ref: ${{ github.event.pull_request.base.sha }}", text)
        self.assertLess(text.index("id: trusted-automation"), text.index("actions/checkout@"))
        self.assertIn("persist-credentials: false", text)
        # The reviewed change stays the PR's own diff against its retained base.
        self.assertIn("EXPECTED_BASE_SHA: ${{ github.event.pull_request.base.sha }}", text)
        self.assertIn('merge-base "$EXPECTED_BASE_SHA" "$EXPECTED_HEAD_SHA"', text)


class TrustedRunnerInterfaceTests(unittest.TestCase):
    def run_step(self, supported=False, broken_help=False):
        with tempfile.TemporaryDirectory() as root:
            directory = Path(root)
            scripts = directory / "review-automation/.github/scripts"
            scripts.mkdir(parents=True)
            source = OLD_RUNNER
            if supported:
                source = source.replace("# optional_context_argument",
                                        'review.add_argument("--source-repository")')
            if broken_help:
                source = "raise SystemExit(9)"
            trusted = scripts / "api_review.py"
            trusted.write_text(source)
            # A fallback to PR-head code must never become a compatibility fix.
            candidate = directory / ".github/scripts/api_review.py"
            candidate.parent.mkdir(parents=True)
            candidate.write_text('raise RuntimeError("candidate code was executed")')
            summary = directory / "summary"
            (directory / "previous-api-review.json").write_text("{}")
            env = {"PATH": os.environ["PATH"], "RUNNER_TEMP": root,
                   "GITHUB_STEP_SUMMARY": str(summary), "REPO": "wendylabsinc/WendyOS",
                   "PR_NUMBER": "42", "EXPECTED_HEAD_SHA": PR_HEAD,
                   "EXPECTED_BASE_SHA": OLD_BASE, "CLAUDE_MODEL": "offline-fixture"}
            result = subprocess.run(["bash", "-c", review_step_script()], cwd=root,
                                    env=env, capture_output=True, text=True, timeout=10)
            output = directory / "api-review-result.json"
            value = json.loads(output.read_text()) if output.exists() else None
            notice = summary.read_text() if summary.exists() else ""
            if not supported and not broken_help:
                # Negative control reproduces the reported pre-fix parser
                # failure, rather than merely asserting a workflow substring.
                control_output = directory / "control-result.json"
                control_args = ["python3", str(trusted), "review", "--source-repository", root]
                for name in ("metadata", "diff", "repo", "expected-head-sha", "expected-base-sha", "model"):
                    control_args.extend([f"--{name}", "fixture"])
                control_args.extend(["--output", str(control_output), "--pr-number", "42"])
                control = subprocess.run(control_args, env=env, capture_output=True, text=True, timeout=10)
                self.assertEqual(control.returncode, 2)
                self.assertIn("unrecognized arguments: --source-repository", control.stderr)
                self.assertFalse(control_output.exists())
            return result, value, notice

    def test_new_workflow_runs_old_trusted_parser_with_explicit_coverage_notice(self):
        result, value, notice = self.run_step()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("source_repository", value)
        self.assertIn("previous-api-review.json", value["previous"])
        self.assertIn("::notice::", result.stdout)
        self.assertIn("have not activated", notice)
        self.assertIn("no unchanged-source context was reviewed", notice)

    def test_new_trusted_parser_receives_source_repository(self):
        result, value, notice = self.run_step(supported=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(value["source_repository"], "review-automation")
        self.assertEqual(notice, "")
        self.assertNotIn("::notice::", result.stdout)

    def test_failed_capability_probe_fails_closed_without_candidate_fallback(self):
        result, value, notice = self.run_step(broken_help=True)
        self.assertEqual(result.returncode, 9)
        self.assertIsNone(value)
        self.assertEqual(notice, "")
        self.assertNotIn("candidate code", result.stderr)


class CompatibilityWorkflowTests(unittest.TestCase):
    def test_ready_and_stacked_base_coverage(self):
        text = WORKFLOW.read_text()
        trigger = text.split("concurrency:", 1)[0]
        self.assertIn("[opened, ready_for_review, synchronize, reopened, edited]", trigger)
        self.assertNotIn("branches:", trigger)
        review_job = text.split("  publish-api-review:", 1)[0]
        self.assertIn("github.event.pull_request.draft == false", review_job)

    def test_automatic_gate_covers_version_gap_events_and_rejects_untrusted_prs(self):
        lines = WORKFLOW.read_text().split("  publish-api-review:", 1)[0].splitlines()
        start = next(i for i, line in enumerate(lines) if line.strip() == "if: >-")
        end = next(i for i in range(start + 1, len(lines)) if lines[i].strip().startswith("runs-on:"))
        expression = " ".join(line.strip() for line in lines[start + 1:end])
        script = "const x=JSON.parse(require('fs').readFileSync(0,'utf8')); console.log(new Function('github','return ('+x.expression+')')(x.github));"
        for action, draft, repo, actor, author, changed_base, expected in [
            ("opened", False, "repo", "writer", "writer", False, True),
            ("ready_for_review", False, "repo", "writer", "writer", False, True),
            ("synchronize", False, "repo", "writer", "writer", False, True),
            ("reopened", False, "repo", "writer", "writer", False, True),
            ("edited", False, "repo", "writer", "writer", True, True),
            ("edited", False, "repo", "writer", "writer", False, False),
            ("opened", True, "repo", "writer", "writer", False, False),
            ("synchronize", False, "fork", "writer", "writer", False, False),
            ("synchronize", False, "repo", "dependabot[bot]", "writer", False, False),
            ("synchronize", False, "repo", "writer", "dependabot[bot]", False, False),
        ]:
            github = {"event_name": "pull_request", "repository": "repo", "actor": actor,
                      "event": {"action": action, "changes": {"base": {} if changed_base else None},
                                "pull_request": {"draft": draft, "head": {"repo": {"full_name": repo}},
                                                 "base": {"ref": "kb.stacked-parent.1"}, "user": {"login": author}}}}
            with self.subTest(action=action, draft=draft, repo=repo, actor=actor, author=author):
                actual = subprocess.run(["node", "-e", script], check=True, text=True, capture_output=True,
                                        input=json.dumps({"expression": expression, "github": github}), timeout=10)
                self.assertEqual(json.loads(actual.stdout), expected)

    def test_description_edits_cannot_cancel_reviewing_source_changes(self):
        line = next(line for line in WORKFLOW.read_text().splitlines() if line.strip().startswith("group:"))
        expression = re.findall(r"\$\{\{(.*?)\}\}", line)[-1]
        script = "const x=JSON.parse(require('fs').readFileSync(0,'utf8')); console.log(new Function('github','inputs','return ('+x.expression+')')(x.github,{}));"
        for action, base_change, expected in [("synchronize", False, 42), ("ready_for_review", False, 42),
                                               ("edited", True, 42), ("edited", False, 9900)]:
            github = {"run_id": 9900, "event": {"action": action,
                      "changes": {"base": {} if base_change else None}, "pull_request": {"number": 42}}}
            with self.subTest(action=action, base_change=base_change):
                result = subprocess.run(["node", "-e", script], check=True, capture_output=True, text=True,
                                        input=json.dumps({"expression": expression, "github": github}), timeout=10)
                self.assertEqual(json.loads(result.stdout), expected)

    def test_artifact_name_spoofing_cannot_supply_publisher_data(self):
        text = WORKFLOW.read_text()
        review_job, tail = text.split("  publish-api-review:", 1)
        publisher = tail.split("  validate-candidate-tests:", 1)[0]
        self.assertIn("artifact_id: ${{ steps.review-result.outputs.artifact-id }}", review_job)
        upload = review_job.split("- name: Preserve complete API review result", 1)[1]
        self.assertIn("id: review-result", upload)
        self.assertIn("actions/upload-artifact@", upload)
        self.assertIn("needs.detect-api-changes.outputs.artifact_id != ''", publisher)
        download = publisher.split("uses: actions/download-artifact@", 1)[1].split("- name: Set up Python", 1)[0]
        self.assertIn("artifact-ids: ${{ needs.detect-api-changes.outputs.artifact_id }}", download)
        # Another job may squat the display name, but cannot set the trusted
        # job's upload ID. A failed trusted upload must not select a fallback.
        self.assertNotIn("name:", download)
        self.assertNotIn("pattern:", download)

    def test_model_and_publisher_credentials_are_isolated(self):
        text = WORKFLOW.read_text()
        review_job, tail = text.split("  publish-api-review:", 1)
        publisher, tail = tail.split("  validate-candidate-tests:", 1)
        self.assertNotIn(": write", review_job)
        self.assertNotIn("Bootstrap", review_job)
        self.assertNotIn("ANTHROPIC_API_KEY", publisher)
        self.assertIn("issues: write", publisher)
        self.assertIn("needs.detect-api-changes.outputs.trusted_sha", publisher)
        self.assertIn("actions/download-artifact@", publisher)
        self.assertIn('artifact-ids: ${{ needs.detect-api-changes.outputs.artifact_id }}', publisher)
        model_step = review_job.split("- name: Review API decisions and testing risk", 1)[1].split("- name: Preserve", 1)[0]
        self.assertNotIn("GH_TOKEN", model_step)
        self.assertIn("context_args=(--source-repository review-automation)", model_step)
        self.assertIn("persist-credentials: false", review_job)
        self.assertIn("persist-credentials: false", publisher)
        tests = tail.split("  validate-candidate:", 1)[0]
        self.assertNotIn(": write", tests)
        self.assertNotIn("ANTHROPIC_API_KEY", tests)
        self.assertIn("api_review_compatibility_test.py", tests)


if __name__ == "__main__":
    unittest.main()
