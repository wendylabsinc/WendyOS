#!/usr/bin/env python3
"""Actual Git anchors and offline GitHub publication/failure probes."""

import copy
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

import stability_review as review

REPO = "wendylabsinc/WendyOS"
BODY = "**In plain terms:** A consumer fails.\n\n**When:** Old capability.\n\n**Evidence:** Source inference.\n\n**Suggested fix:** Preserve the old behavior."


class FakeGitHub:
    def __init__(self, pr, comments=None):
        self.pr = copy.deepcopy(pr)
        self.comments = copy.deepcopy(comments or [])
        self.calls = []
        self.permission = "write"
        self.reject = False
        self.reads = 0
        self.stale_after = 100

    def request(self, method, path, payload=None):
        self.calls.append((method, path, payload))
        if method == "GET":
            if "permission" in path:
                return {"permission": self.permission}
            if path.startswith("/pulls/comments/"):
                return copy.deepcopy(next(row for row in self.comments if row["id"] == int(path.rsplit("/", 1)[1])))
            self.reads += 1
            pr = copy.deepcopy(self.pr)
            if self.reads >= self.stale_after:
                pr["head"]["sha"] = "a" * 40
            return pr
        if self.reject:
            raise ValueError("simulated rejected publication")
        if method == "POST":
            return {"id": 900, "state": "COMMENTED"}
        return {"id": 42, "body": payload["body"]}

    def pages(self, path):
        self.calls.append(("GET", path, None))
        return self.comments

    def writes(self):
        return [call for call in self.calls if call[0] != "GET"]


def owned(key="consumer-failure", identifier=42):
    return {"id": identifier, "body": BODY + f"\n\n<!-- ai-stability-review:v1:{key} -->",
            "user": {"login": "github-actions[bot]", "type": "Bot"}}


class StabilityTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.source = Path(self.temporary.name)
        self.run_git("init", "-q")
        self.run_git("config", "user.name", "Fixture")
        self.run_git("config", "user.email", "fixture@example.test")
        self.run_git("config", "commit.gpgsign", "false")
        (self.source / "consumer.txt").write_text("stable\nold behavior\nthird\nfourth\nfifth\nsixth\n")
        (self.source / "deleted.txt").write_text("old contract\n")
        self.commit()
        self.base = self.run_git("rev-parse", "HEAD").strip()
        (self.source / "consumer.txt").write_text("stable\nnew behavior\nthird\nfourth\nfifth\nsixth\n")
        (self.source / "deleted.txt").unlink()
        self.commit()
        self.head = self.run_git("rev-parse", "HEAD").strip()
        self.pr = {"number": 42, "title": "Change consumer", "body": "PR description", "state": "open",
                   "user": {"login": "author"},
                   "head": {"repo": {"full_name": REPO}, "sha": self.head},
                   "base": {"repo": {"full_name": REPO}, "sha": self.base}}
        self.result = {"status": "complete", "scope_complete": True, "examined": "All changed files and callers.",
                       "gaps": "Installed versions and executed mixed-version tests are unknown.",
                       "findings": [{"key": "consumer-failure", "path": "consumer.txt", "line": 2,
                                     "side": "RIGHT", "body": BODY}]}

    def run_git(self, *args):
        return subprocess.check_output(["git", "-C", str(self.source), *args], text=True, stderr=subprocess.DEVNULL)

    def commit(self):
        self.run_git("add", "-A")
        self.run_git("commit", "-qm", "fixture")

    def request(self, comments=None):
        return dict(review.snapshot(self.pr, REPO), previous=review.owned_findings(comments or []))

    def finish(self, github=None, request=None, publish=False):
        return review.finish(github or FakeGitHub(self.pr), request or self.request(), self.result, self.source, publish)

    def test_default_dry_run_validates_real_git_anchor_without_any_writes(self):
        github = FakeGitHub(self.pr)
        output = self.finish(github)
        self.assertEqual(output["mode"], "dry-run")
        self.assertEqual(output["review"]["event"], "COMMENT")
        self.assertIn(f"/blob/{self.head}/consumer.txt#L2", output["review"]["comments"][0]["body"])
        self.assertEqual(github.writes(), [])

    def test_left_deleted_line_is_valid_and_context_line_is_rejected(self):
        self.result["findings"][0].update(path="deleted.txt", line=1, side="LEFT")
        self.assertIn(f"/blob/{self.base}/deleted.txt#L1", self.finish()["review"]["comments"][0]["body"])
        self.result["findings"][0].update(path="consumer.txt", line=1, side="RIGHT")
        github = FakeGitHub(self.pr)
        with self.assertRaisesRegex(ValueError, "changed line"):
            self.finish(github, publish=True)
        self.assertEqual(github.calls, [])

    def test_renamed_left_anchor_uses_github_new_path_and_old_evidence_path(self):
        self.run_git("mv", "consumer.txt", "renamed.txt")
        self.commit()
        self.head = self.run_git("rev-parse", "HEAD").strip()
        self.pr["head"]["sha"] = self.head
        self.result["findings"][0].update(side="LEFT")
        comment = self.finish()["review"]["comments"][0]
        self.assertEqual(comment["path"], "renamed.txt")
        self.assertIn(f"/blob/{self.base}/consumer.txt#L2", comment["body"])

    def test_left_evidence_uses_merge_base_instead_of_diverged_target_tip(self):
        self.run_git("checkout", "--detach", self.base)
        (self.source / "other.txt").write_text("target moved\n")
        self.commit()
        self.pr["base"]["sha"] = self.run_git("rev-parse", "HEAD").strip()
        self.run_git("checkout", "--detach", self.head)
        self.result["findings"][0]["side"] = "LEFT"
        self.assertIn(f"/blob/{self.base}/consumer.txt#L2", self.finish()["review"]["comments"][0]["body"])

    def test_rerun_updates_owned_thread_without_duplicate_or_human_mutation(self):
        comments = [owned(), {**owned("spoofed", 55), "user": {"login": "human", "type": "User"}},
                    {**owned("reply", 56), "in_reply_to_id": 42}]
        github = FakeGitHub(self.pr, comments)
        output = self.finish(github, self.request(comments), publish=True)
        self.assertEqual(output["review"]["comments"], [])
        self.assertEqual(output["updates"][0]["id"], 42)
        self.assertEqual([call[0] for call in github.writes()], ["PATCH", "POST"])
        self.assertEqual(set(github.writes()[0][2]), {"body"})
        self.assertEqual(comments[2]["in_reply_to_id"], 42)

    def test_omitted_or_unanchored_old_finding_remains_unassessed(self):
        comments = [owned()]
        for unanchored in (False, True):
            with self.subTest(unanchored=unanchored):
                self.result["findings"] = ([dict(self.result["findings"][0], path="", line=0)] if unanchored else [])
                output = self.finish(FakeGitHub(self.pr, comments), self.request(comments), publish=True)
                self.assertIn("These remain unassessed, not resolved", output["review"]["body"])
                self.assertEqual(output["updates"], [])
                if unanchored:
                    self.assertIn("Findings without a diff anchor", output["review"]["body"])
                    self.assertIn(BODY, output["review"]["body"])
                self.result["findings"] = [{"key": "consumer-failure", "path": "consumer.txt", "line": 2,
                                            "side": "RIGHT", "body": BODY}]

    def test_stale_head_base_title_body_and_comment_edits_fail_before_mutation(self):
        for changed in ("head", "base", "title", "body", "comment"):
            with self.subTest(changed=changed):
                request = self.request([owned()])
                github = FakeGitHub(self.pr, [owned()])
                if changed in {"head", "base"}:
                    github.pr[changed]["sha"] = "a" * 40
                elif changed == "comment":
                    github.comments[0]["body"] += "\nHuman edit"
                else:
                    github.pr[changed] = "edited"
                with self.assertRaisesRegex(ValueError, "changed"):
                    self.finish(github, request, publish=True)
                self.assertEqual(github.writes(), [])

    def test_stale_during_publication_fails_and_does_not_submit_review(self):
        github = FakeGitHub(self.pr, [owned()])
        github.stale_after = 3
        with self.assertRaisesRegex(ValueError, "changed"):
            self.finish(github, self.request([owned()]), publish=True)
        self.assertEqual([call[0] for call in github.writes()], ["PATCH"])

    def test_edit_after_state_read_is_preserved_by_immediate_comment_recheck(self):
        from unittest.mock import patch
        for change in ("body", "owner", "identity"):
            github = FakeGitHub(self.pr, [owned()])
            request = self.request([owned()])
            def raced_read(path):
                captured = copy.deepcopy(github.comments)
                if change == "body":
                    github.comments[0]["body"] += "\nHuman's new explanation"
                elif change == "owner":
                    github.comments[0]["user"] = {"login": "human", "type": "User"}
                else:
                    github.comments[0]["body"] = owned("different-finding")["body"]
                return captured
            with self.subTest(change=change), patch.object(github, "pages", side_effect=raced_read):
                with self.assertRaisesRegex(ValueError, "Owned finding changed before update"):
                    self.finish(github, request, publish=True)
            self.assertEqual(github.writes(), [])

    def test_incomplete_missing_scope_malformed_output_and_bad_anchors_fail(self):
        original = copy.deepcopy(self.result)
        for value in ({**original, "status": "incomplete"}, {**original, "scope_complete": False},
                      {**original, "examined": ""}, {**original, "findings": original["findings"] * 2},
                      {**original, "findings": [dict(original["findings"][0], path="../escape")]},
                      {**original, "findings": [dict(original["findings"][0], line=True)]},
                      {**original, "findings": [dict(original["findings"][0], body="clean bill")]},
                      {**original, "findings": [dict(original["findings"][0], body=BODY + "<!-- spoof -->")]},
                      {**original, "findings": original["findings"] * 21}, {}):
            github = FakeGitHub(self.pr)
            self.result = value
            with self.subTest(value=value), self.assertRaises(ValueError):
                self.finish(github, publish=True)
            self.assertEqual(github.writes(), [])

    def test_wrong_checkout_and_shallow_history_fail(self):
        self.run_git("checkout", "--detach", self.base)
        with self.assertRaisesRegex(ValueError, "head"):
            self.finish()
        self.run_git("checkout", "--detach", self.head)
        (self.source / ".git/shallow").write_text(self.head + "\n")
        with self.assertRaisesRegex(ValueError, "history"):
            self.finish()

    def test_publication_rejection_and_unconfirmed_review_fail(self):
        github = FakeGitHub(self.pr)
        github.reject = True
        with self.assertRaisesRegex(ValueError, "rejected"):
            self.finish(github, publish=True)
        self.assertEqual(len(github.writes()), 1)
        from unittest.mock import patch
        with patch.object(github, "request", side_effect=lambda method, path, payload=None: copy.deepcopy(self.pr) if method == "GET" else {"state": "PENDING"}):
            with self.assertRaisesRegex(ValueError, "COMMENT"):
                self.finish(github, publish=True)

    def test_duplicate_owned_ids_fail_and_nonowned_markers_do_not_count(self):
        with self.assertRaisesRegex(ValueError, "Duplicate"):
            review.owned_findings([owned(), owned(identifier=43)])
        self.assertEqual(review.owned_findings([{**owned(), "user": {"login": "human", "type": "User"}}]), {})

    def test_prepare_same_repo_writer_gates_and_does_not_prepack_diff(self):
        github = FakeGitHub(self.pr, [owned()])
        request = review.prepare(github, REPO, 42, "writer")
        self.assertEqual(set(request), {"repository", "number", "title", "body", "base_sha", "head_sha", "previous"})
        self.assertIn("consumer-failure", request["previous"])
        self.assertEqual(github.writes(), [])
        github.permission = "read"
        with self.assertRaisesRegex(ValueError, "writer"):
            review.prepare(github, REPO, 42, "reader")
        for changed in ("fork", "deleted", "closed", "dependabot"):
            pr = copy.deepcopy(self.pr)
            if changed == "fork": pr["head"]["repo"]["full_name"] = "fork/repo"
            if changed == "deleted": pr["head"]["repo"] = None
            if changed == "closed": pr["state"] = "closed"
            if changed == "dependabot": pr["user"]["login"] = "dependabot[bot]"
            with self.subTest(changed=changed), self.assertRaises(ValueError):
                review.snapshot(pr, REPO)


if __name__ == "__main__":
    unittest.main()
