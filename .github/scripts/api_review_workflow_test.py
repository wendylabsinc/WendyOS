#!/usr/bin/env python3
"""Exercise the actual trusted-source resolver with mocked GitHub responses."""

import json
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


if __name__ == "__main__":
    unittest.main()
