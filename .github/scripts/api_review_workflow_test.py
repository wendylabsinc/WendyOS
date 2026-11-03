#!/usr/bin/env python3
"""Exercise the actual trusted-source resolver with mocked GitHub responses."""

import json
from pathlib import Path
import subprocess
import unittest

WORKFLOW = Path(__file__).resolve().parents[1] / "workflows" / "api-review.yml"
OLD_BASE = "b" * 40
PR_HEAD = "c" * 40
CURRENT_MAIN = "a" * 40


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
    def run_resolver(self, response, fail_lookup=False):
        execution = subprocess.run(
            ["node", "-e", HARNESS], check=True, capture_output=True, text=True, timeout=10,
            input=json.dumps({"script": resolver_script(WORKFLOW.read_text()), "response": response,
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
        self.assertIn('name: api-review-${{ github.run_id }}', publisher)
        model_step = review_job.split("- name: Review API decisions and testing risk", 1)[1].split("- name: Preserve", 1)[0]
        self.assertNotIn("GH_TOKEN", model_step)
        self.assertIn("--source-repository review-automation", model_step)
        self.assertIn("persist-credentials: false", review_job)
        self.assertIn("persist-credentials: false", publisher)
        tests = tail.split("  validate-candidate:", 1)[0]
        self.assertNotIn(": write", tests)
        self.assertNotIn("ANTHROPIC_API_KEY", tests)
        self.assertIn("api_review_compatibility_test.py", tests)


if __name__ == "__main__":
    unittest.main()
