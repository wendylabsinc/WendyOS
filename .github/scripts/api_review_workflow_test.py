#!/usr/bin/env python3
"""Exercise the actual trusted-source resolver with mocked GitHub responses."""

import itertools
import json
import os
import re
import textwrap
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


def resolver_script(text, step_id="trusted-automation"):
    lines = text.splitlines()
    start = next(index for index, line in enumerate(lines) if line.strip() == "id: " + step_id)
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


def review_workflows():
    return [p for name in ("api-review", "docs-review", "integration-test-review", "security-review")
            if (p := WORKFLOW.with_name(name + ".yml")).is_file()]


def first_job_expression(text):
    lines = text.splitlines()
    start = next(i for i, line in enumerate(lines) if line.startswith("    if:"))
    parts = []
    for line in lines[start + 1:]:
        if not line.startswith("      "):
            break
        parts.append(line.strip())
    return " ".join(parts)


def target_pr():
    return {"number": 42, "state": "open", "draft": False, "user": {"login": "author"}, "labels": [],
            "head": {"sha": PR_HEAD, "repo": {"full_name": "wendylabsinc/WendyOS"}},
            "base": {"sha": OLD_BASE, "ref": "main", "repo": {"full_name": "wendylabsinc/WendyOS"}},
            "title": "A title with `untrusted` text", "body": "Untrusted body\nwith multiple lines"}


def target_env():
    return {"TARGET_PR_NUMBER": "42", "TARGET_HEAD_SHA": PR_HEAD, "TARGET_BASE_SHA": ""}


def execute_targets(path, cases):
    run = subprocess.run(["node", "-e", r'''
const input = JSON.parse(require("fs").readFileSync(0, "utf8"));
const AsyncFunction = Object.getPrototypeOf(async function() {}).constructor;
(async () => {
  const results = [];
  for (const c of input.cases) {
    const outputs = {}, exported = {}, calls = [];
    const context = {repo: {owner: "wendylabsinc", repo: "WendyOS"}, actor: c.actor || "author",
      eventName: c.event || "workflow_dispatch", ref: c.ref || "refs/heads/main"};
    const github = {rest: {pulls: {get: async args => {
      calls.push(args); if (c.failLookup) throw new Error("lookup failed"); return {data: c.pr};
    }}}};
    const core = {setOutput: (k, v) => {outputs[k] = v}, exportVariable: (k, v) => {exported[k] = v}};
    let ok = true, error = "";
    try {await new AsyncFunction("github", "context", "core", "process", input.script)(github, context, core, {env: c.env});}
    catch (e) {ok = false; error = String(e)}
    results.push({ok, error, outputs, exported, calls});
  }
  console.log(JSON.stringify(results));
})();
'''], input=json.dumps({"script": resolver_script(path.read_text(), "review-target"), "cases": cases}),
                         text=True, capture_output=True, check=True)
    return json.loads(run.stdout)


class TrustedSourceTests(unittest.TestCase):
    def run_resolver(self, response, fail_lookup=False, workflow=WORKFLOW):
        execution = subprocess.run(
            ["node", "-e", HARNESS], check=True, capture_output=True, text=True, timeout=10,
            input=json.dumps({"script": resolver_script(workflow.read_text()), "response": response,
                              "oldBase": OLD_BASE, "prHead": PR_HEAD, "failLookup": fail_lookup}))
        result = json.loads(execution.stdout)
        self.assertEqual(result["calls"], [{"owner": "wendylabsinc", "repo": "WendyOS", "ref": "heads/main"}])
        return result

    def test_review_event_gates(self):
        # Evaluate the actual event subscriptions AND job expressions. Labels
        # (including formerly recognized review labels) cannot opt a draft in.
        for name in ("api-review", "docs-review", "integration-test-review", "security-review"):
            path = WORKFLOW.with_name(name + ".yml")
            if not path.is_file():
                continue
            text = path.read_text()
            expression = first_job_expression(text).replace("github.event.pull_request.labels.*.name", "labels")
            events = re.search(r"types: \[(.*?)\]", text)[1].split(", ")
            self.assertEqual(events, ["opened", "synchronize", "reopened", "ready_for_review"]
                             if name == "security-review" else ["ready_for_review"])
            self.assertNotIn("labeled", text)
            for label in ("review: api", "review: docs", "review: integration-tests", "review: security"):
                self.assertNotIn(label, text)
            cases = []
            for action, draft, fork, bot_author, bot_actor, requested, suggestion in itertools.product(
                    ("opened", "synchronize", "reopened", "labeled", "ready_for_review", "converted_to_draft"),
                    (False, True), (False, True), (False, True), (False, True), (False, True), (False, True)):
                labels = (["review: api", "review: docs", "review: integration-tests", "review: security"] if requested else [])
                if suggestion:
                    labels.append("ai-suggestion")
                author = "dependabot[bot]" if bot_author else "author"
                actor = "dependabot[bot]" if bot_actor else "author"
                event = {"action": action, "pull_request": {
                    "number": 42, "draft": draft, "head": {"repo": {"full_name": "fork" if fork else "repo"}},
                    "user": {"login": author}}}
                expected = action == "ready_for_review" and not (draft or fork or bot_author or bot_actor)
                if name == "security-review":
                    # Preserve the bot job's original admission, including drafts;
                    # IS_DEPENDABOT and deterministic policy authenticate identity.
                    bot_policy = bot_author and action in ("opened", "synchronize", "reopened")
                    expected = not fork and not suggestion and (bot_policy or expected)
                cases.append({"github": {"event_name": "pull_request", "event": event,
                                         "repository": "repo", "actor": actor},
                              "inputs": {}, "labels": labels, "expected": expected})
            for mode in ("validate", "review"):
                cases.append({"github": {"event_name": "workflow_dispatch", "event": {}, "repository": "repo"},
                              "inputs": {"pr_number": "42", "mode": mode}, "labels": [],
                              "expected": name != "api-review" or mode == "review"})
            execution = subprocess.run(["node", "-e", r'''
const input = JSON.parse(require("fs").readFileSync(0, "utf8"));
const evaluate = new Function("github", "inputs", "labels", "contains", "return " + input.expression);
for (const c of input.cases) {
  const actual = evaluate(c.github, c.inputs, c.labels, (items, value) => items.includes(value));
  if (actual !== c.expected) throw new Error(JSON.stringify({c, actual}));
}
'''], input=json.dumps({"expression": expression, "cases": cases}), text=True, capture_output=True)
            self.assertEqual(execution.returncode, 0, execution.stderr)
            group = next(line.strip().removeprefix('group: ').strip('"')
                         for line in text.splitlines() if line.startswith("  group:"))
            self.assertEqual(group, "${{ github.workflow }}-${{ github.event.pull_request.number || inputs.pr_number }}")

    def test_manual_target_resolution_fails_closed(self):
        for path in review_workflows():
            cases = [{"pr": target_pr(), "env": target_env(), "event": "workflow_dispatch", "ok": True}]
            for field, value in (("state", "closed"), ("draft", True), ("draft", None), ("number", 43)):
                pr = target_pr(); pr[field] = value
                cases.append({"pr": pr, "env": target_env(), "ok": False})
            for field in ("head", "base"):
                for value in ("fork/repo", None):
                    pr = target_pr(); pr[field]["repo"]["full_name"] = value
                    cases.append({"pr": pr, "env": target_env(), "ok": False})
                pr = target_pr(); pr[field]["sha"] = "invalid"
                cases.append({"pr": pr, "env": target_env(), "ok": False})
            pr = target_pr(); pr["head"]["sha"] = "d" * 40
            cases.append({"pr": pr, "env": target_env(), "ok": False})
            for number in ("", "0", "-1", "1/../../", "1.5", "1e2", "9007199254740992", "42\n"):
                env = target_env(); env["TARGET_PR_NUMBER"] = number
                cases.append({"pr": target_pr(), "env": env, "ok": False})
            for head in ("", "HEAD", "C" * 40, "c" * 39, "c" * 40 + "\n"):
                env = target_env(); env["TARGET_HEAD_SHA"] = head
                cases.append({"pr": target_pr(), "env": env, "ok": False})
            env = target_env(); env["TARGET_BASE_SHA"] = "d" * 40
            cases.append({"pr": target_pr(), "env": env, "event": "pull_request", "ok": False})
            cases.append({"pr": target_pr(), "env": target_env(), "ref": "refs/heads/untrusted", "ok": False})
            cases.append({"pr": target_pr(), "env": target_env(), "actor": "dependabot[bot]", "ok": False})
            pr = target_pr(); pr["user"]["login"] = "dependabot[bot]"
            cases.append({"pr": pr, "env": target_env(), "ok": False})
            cases.append({"pr": target_pr(), "env": target_env(), "failLookup": True, "ok": False})
            if path != WORKFLOW:
                pr = target_pr(); pr["base"]["ref"] = "other"
                cases.append({"pr": pr, "env": target_env(), "ok": False})
            if path.name == "security-review.yml":
                pr = target_pr(); pr["labels"] = [{"name": "ai-suggestion"}]
                cases.append({"pr": pr, "env": target_env(), "ok": False})
            results = execute_targets(path, cases)
            for case, result in zip(cases, results):
                with self.subTest(workflow=path.name, case=case):
                    self.assertEqual(result["ok"], case["ok"], result)
                    if case["ok"]:
                        self.assertEqual(result["outputs"]["head"], PR_HEAD)
                        self.assertEqual(result["outputs"]["base"], OLD_BASE)
                        self.assertEqual(result["outputs"]["number"], "42")
                        self.assertEqual(result["outputs"]["title"], case["pr"]["title"])
                        self.assertEqual(result["outputs"]["body"], case["pr"]["body"])
                        self.assertEqual(result["calls"], [{"owner": "wendylabsinc", "repo": "WendyOS", "pull_number": 42}])
                    else:
                        self.assertEqual(result["outputs"], {})

    def test_stale_or_draft_targets_cannot_reach_model_or_publisher(self):
        for path in review_workflows():
            text = path.read_text()
            for purpose in ("model use", "publication"):
                name = "Recheck review target before " + purpose
                step = text.split("      - name: " + name + "\n", 1)[1].split("      - name:", 1)[0]
                script = textwrap.dedent(step.split("        run: |\n", 1)[1])
                cases = [target_pr()]
                for field, value in (("draft", True), ("state", "closed")):
                    pr = target_pr(); pr[field] = value; cases.append(pr)
                for field in ("head", "base"):
                    pr = target_pr(); pr[field]["sha"] = "d" * 40; cases.append(pr)
                    pr = target_pr(); pr[field]["repo"]["full_name"] = "fork/repo"; cases.append(pr)
                with tempfile.TemporaryDirectory() as directory:
                    fake = Path(directory) / "gh"
                    fake.write_text('#!/bin/sh\ncat "$PR_FIXTURE"\n')
                    fake.chmod(0o755)
                    metadata = Path(directory) / "pr.json"
                    for index, pr in enumerate(cases):
                        metadata.write_text(json.dumps(pr))
                        env = {**os.environ, "PATH": directory + os.pathsep + os.environ["PATH"],
                               "PR_FIXTURE": str(metadata), "REPO": "wendylabsinc/WendyOS", "PR_NUMBER": "42",
                               "EXPECTED_HEAD_SHA": PR_HEAD, "EXPECTED_BASE_SHA": OLD_BASE}
                        run = subprocess.run(["bash", "-c", script], env=env, capture_output=True, text=True)
                        self.assertEqual(run.returncode == 0, index == 0, (path, purpose, pr, run.stderr))
            self.assertLess(text.index("id: review-target"), text.index("actions/checkout@"))
            self.assertLess(text.index("Recheck review target before model use"), text.index("ANTHROPIC_API_KEY:"))
            if path == WORKFLOW:
                publisher = text.split("      - name: Publish API decision checklist and labels", 1)[1]
                self.assertIn("steps.publication-target.outcome == 'success'", publisher.split("env:", 1)[0])
            else:
                self.assertNotIn("github.event.pull_request.title", text)
                self.assertNotIn("github.event.pull_request.body", text)

    def test_api_dispatch_keeps_nonpublishing_validation_as_default(self):
        text = WORKFLOW.read_text()
        self.assertIn("default: validate", text)
        self.assertIn("options: [validate, review]", text)
        validation = text.split("  validate-candidate:", 1)[1]
        self.assertIn("inputs.mode == 'validate'", validation)
        self.assertIn("publish --dry-run", validation)
        self.assertIn("pull-requests: read", validation)
        self.assertNotIn("pull-requests: write", validation)
        self.assertNotIn("issues: write", validation)
        self.assertIn("needs: validate-candidate-tests", validation)

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
        self.assertIn('core.exportVariable("EXPECTED_BASE_SHA", pr.base.sha)', automatic_job)
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
        self.assertIn("EXPECTED_BASE_SHA: ${{ steps.review-target.outputs.base }}", text)
        self.assertIn('merge-base "$EXPECTED_BASE_SHA" "$EXPECTED_HEAD_SHA"', text)


if __name__ == "__main__":
    unittest.main()
