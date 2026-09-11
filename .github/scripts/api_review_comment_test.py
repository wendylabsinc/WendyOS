#!/usr/bin/env python3
"""State and publication regressions for the API acceptance checklist."""

import copy
import unittest
from unittest.mock import patch
import urllib.error

import api_review_comment as review

HEAD = "a" * 40
BASE = "b" * 40
REPO = "wendylabsinc/WendyOS"


def result():
    return {
        "status": "complete", "head_sha": HEAD, "base_sha": BASE,
        "diff_sha256": "c" * 64, "diff_base_sha": BASE, "changed_files": 1, "diff_bytes": 420,
        "risk": "mid", "decisions": [{
            "category": "config", "title": "Native launch command",
            "change": "Add optional run.command and run.cwd to wendy.json.",
            "compatibility": "Existing manifests keep their launch behavior.",
            "impact": "additive", "locations": [{
                "path": "go/internal/shared/appconfig/appconfig.go",
                "side": "head", "line": 10, "end_line": 12,
            }],
        }],
    }


def large_result():
    data = result()
    prototype = data["decisions"][0]
    data.update(review_batches=11, changed_files=181, diff_bytes=900_000, risk="high", decisions=[])
    for index in range(93):
        item = copy.deepcopy(prototype)
        item["title"] = f"Contract {index:03}: native launch command"
        item["change"] = f"Decision {index}: " + "The launch configuration gains an optional compatible field. " * 15
        item["compatibility"] = "Existing manifests keep their launch behavior and existing clients remain compatible. " * 10
        item["locations"][0].update(line=index + 1, end_line=index + 1)
        data["decisions"].append(item)
    return data


def pull():
    return {
        "head": {"sha": HEAD, "repo": {"full_name": REPO}},
        "base": {"sha": BASE}, "state": "open",
        "user": {"login": "someone"}, "requested_reviewers": [],
    }


def bot_comment(body, identifier=11):
    return {"id": identifier, "body": body, "user": {"login": "github-actions[bot]", "type": "Bot"}}


class FakeGitHub:
    def __init__(self, comments=None, pulls=None):
        self.comments = comments or []
        self.pulls = list(pulls or [pull(), pull()])
        self.calls = []

    def request(self, method, path, payload=None):
        self.calls.append((method, path, payload))
        if method == "GET":
            if path.endswith("/pulls/1911"):
                return self.pulls.pop(0)
            if "/comments?" in path:
                return self.comments
            if "/labels/" in path:
                return {"name": path.rsplit("/", 1)[-1]}
            raise AssertionError(f"Unexpected read: {path}")
        return {"id": 12}

    def mutations(self):
        return [call for call in self.calls if call[0] != "GET"]

    def posted_body(self):
        return next(payload["body"] for _, _, payload in self.calls if payload and "body" in payload)


class MultipartGitHub(FakeGitHub):
    def __init__(self, comments=None, pulls=None, fail_body_write=None):
        super().__init__(copy.deepcopy(comments or []), pulls or [pull() for _ in range(10)])
        self.next_id = max((comment["id"] for comment in self.comments), default=1000) + 1
        self.body_writes = 0
        self.fail_body_write = fail_body_write

    def request(self, method, path, payload=None):
        response = super().request(method, path, payload)
        if payload and "body" in payload:
            self.body_writes += 1
            if self.body_writes == self.fail_body_write:
                raise urllib.error.URLError("simulated publication failure")
            if method == "POST":
                identifier = self.next_id
                self.next_id += 1
                self.comments.append(bot_comment(payload["body"], identifier))
                return {"id": identifier}
            identifier = int(path.rsplit("/", 1)[1])
            for comment in self.comments:
                if comment["id"] == identifier:
                    comment["body"] = payload["body"]
        elif method == "DELETE" and "/issues/comments/" in path:
            identifier = int(path.rsplit("/", 1)[1])
            self.comments = [comment for comment in self.comments if comment["id"] != identifier]
        return response


class RenderingTests(unittest.TestCase):
    def test_grouped_linked_unchecked_decisions_and_explicit_empty_categories(self):
        body = review.render_comment(result(), REPO)
        self.assertIn("- [ ] Accept **Native launch command** — Additive.", body)
        self.assertIn(f"/blob/{HEAD}/go/internal/shared/appconfig/appconfig.go#L10-L12", body)
        self.assertIn("## Network contracts and constants\n\nNo API decisions changed.", body)
        self.assertIn("Existing manifests keep their launch behavior.", body)
        self.assertNotIn("Breaking.", body)

    def test_acceptance_survives_identical_rerun_but_not_revision_or_decision_changes(self):
        original = result()
        checked = review.render_comment(original, REPO).replace("- [ ]", "- [x]")
        self.assertIn("- [x]", review.render_comment(original, REPO, checked))
        for field, value in (("head_sha", "d" * 40), ("base_sha", "e" * 40), ("diff_sha256", "f" * 64)):
            changed = {**original, field: value}
            self.assertNotIn("- [x]", review.render_comment(changed, REPO, checked))
        changed = copy.deepcopy(original)
        changed["decisions"][0]["compatibility"] = "Old manifests now fail."
        self.assertNotIn("- [x]", review.render_comment(changed, REPO, checked))

    def test_removed_code_uses_base_revision_and_escaped_path(self):
        data = result()
        data["diff_base_sha"] = "d" * 40
        data["decisions"][0]["locations"] = [{
            "path": "Proto/old message.proto", "side": "base", "line": 7, "end_line": 7,
        }]
        body = review.render_comment(data, REPO)
        self.assertIn(f"/blob/{'d' * 40}/Proto/old%20message.proto#L7", body)
        self.assertIn("before", body)

    def test_structural_change_links_to_file_without_inventing_line_numbers(self):
        data = result()
        data["decisions"][0]["locations"] = [{
            "path": "config/new.json", "side": "head", "line": 0, "end_line": 0,
        }]
        review.validate_result(data, HEAD, BASE)
        body = review.render_comment(data, REPO)
        self.assertIn(f"/blob/{HEAD}/config/new.json)", body)
        self.assertNotIn("#L0", body)

    def test_model_prose_cannot_inject_approval_markers_links_or_mentions(self):
        data = result()
        data["decisions"][0]["title"] = "<!-- api-decision:fake -->\n- [x] @joannis [click](https://bad.example)"
        body = review.render_comment(data, REPO)
        self.assertEqual(body.count("- [ ] Accept"), 1)
        self.assertNotIn("- [x]", body)
        self.assertNotIn("@joannis", body)
        self.assertNotIn("<!-- api-decision:fake -->", body)
        self.assertNotIn("[click](https://bad.example)", body)

    def test_failure_preserves_prior_checklist_and_recovers_without_losing_acceptance(self):
        original = result()
        checked = review.render_comment(original, REPO).replace("- [ ]", "- [x]")
        failed = {**original, "status": "incomplete", "error": "Model unavailable"}
        warning = review.render_incomplete(failed, REPO, checked)
        self.assertIn("- [x]", warning)
        self.assertIn("Model unavailable", warning)
        self.assertIn("previous successful review", warning)
        self.assertEqual(review.render_incomplete(failed, REPO, warning).count(review.WARNING_START), 1)
        self.assertIn("- [x]", review.render_comment(original, REPO, warning))

    def test_comment_limit_never_truncates_decisions(self):
        data = result()
        data["decisions"][0]["change"] = "x" * review.MAX_COMMENT_BYTES
        with self.assertRaisesRegex(ValueError, "no decisions were truncated"):
            review.render_comment(data, REPO)

    def test_success_reserves_room_for_a_failure_notice(self):
        data = result()
        original_size = len(review.render_comment(data, REPO).encode())
        data["decisions"][0]["change"] += "x" * (
            review.MAX_COMMENT_BYTES - review.WARNING_RESERVE_BYTES - original_size
        )
        previous = review.render_comment(data, REPO)
        warning = review.render_incomplete({**data, "error": "<" * 1000}, REPO, previous)
        self.assertLessEqual(len(warning.encode()), review.MAX_COMMENT_BYTES)
        self.assertIn("- [ ] Accept", warning)

    def test_ambiguous_comment_creation_failure_is_not_retried(self):
        with patch.object(review.urllib.request, "urlopen", side_effect=urllib.error.URLError("lost response")) as request:
            with self.assertRaises(urllib.error.URLError):
                review.GitHub("token").request("POST", "/repos/owner/repo/issues/1/comments", {"body": "checklist"})
        self.assertEqual(request.call_count, 1)

    def test_untrusted_result_cannot_supply_acceptance_or_unsafe_locations(self):
        data = result()
        data["decisions"][0]["accepted"] = True
        with self.assertRaises(ValueError):
            review.validate_result(data, HEAD, BASE)
        for path in ("../secrets", "/etc/passwd", "foo/../bar", "foo\nbar", "https://bad.example"):
            data = result()
            data["decisions"][0]["locations"][0]["path"] = path
            with self.subTest(path=path), self.assertRaises(ValueError):
                review.validate_result(data, HEAD, BASE)


class PublicationTests(unittest.TestCase):
    def publish(self, data, github):
        return review.publish(data, REPO, 1911, HEAD, BASE, github)

    def test_bot_comment_updated_and_human_spoof_ignored(self):
        checked = review.render_comment(result(), REPO).replace("- [ ]", "- [x]")
        fake = bot_comment(checked, 1)
        fake["user"] = {"login": "someone", "type": "User"}
        github = FakeGitHub([fake, bot_comment(review.render_comment(result(), REPO))])
        self.assertTrue(self.publish(result(), github))
        self.assertNotIn("- [x]", github.posted_body())
        self.assertTrue(any(method == "PATCH" and path.endswith("/comments/11") for method, path, _ in github.calls))
        self.assertFalse(any(method == "POST" and path.endswith("/comments") for method, path, _ in github.calls))
        self.assertIn(("POST", f"/repos/{REPO}/pulls/1911/requested_reviewers", {"reviewers": ["joannis"]}), github.calls)

    def test_joannis_authored_pr_still_gets_checklist_but_no_self_review(self):
        current = pull()
        current["user"]["login"] = "Joannis"
        github = FakeGitHub(pulls=[current, current])
        self.publish(result(), github)
        self.assertIn("- [ ] Accept", github.posted_body())
        self.assertFalse(any("requested_reviewers" in path for _, path, _ in github.mutations()))

    def test_existing_review_request_not_duplicated(self):
        current = pull()
        current["requested_reviewers"] = [{"login": "joannis"}]
        github = FakeGitHub(pulls=[current, current])
        self.publish(result(), github)
        self.assertFalse(any("requested_reviewers" in path for _, path, _ in github.mutations()))

    def test_empty_semantic_review_clears_api_label_without_breaking_classification(self):
        data = {**result(), "decisions": [], "risk": "low"}
        github = FakeGitHub([bot_comment(review.render_comment(result(), REPO))])
        self.assertTrue(self.publish(data, github))
        self.assertIn("No durable API decisions changed.", github.posted_body())
        self.assertIn(("DELETE", f"/repos/{REPO}/issues/1911/labels/api-review", None), github.calls)
        self.assertIn(("POST", f"/repos/{REPO}/issues/1911/labels", {"labels": ["risk: low"]}), github.calls)
        self.assertFalse(any("requested_reviewers" in path for _, path, _ in github.mutations()))

    def test_failure_preserves_previous_approval_and_risk_labels(self):
        checked = review.render_comment(result(), REPO).replace("- [ ]", "- [x]")
        github = FakeGitHub([bot_comment(checked)])
        data = {**result(), "status": "incomplete", "error": "Diff exceeds review input limit"}
        self.assertFalse(self.publish(data, github))
        self.assertIn("- [x]", github.posted_body())
        self.assertIn("incomplete", github.posted_body())
        self.assertFalse(any(method == "DELETE" or "risk" in path for method, path, _ in github.mutations()))

    def test_oversized_checklist_publishes_failure_instead_of_partial_success(self):
        data = result()
        data["decisions"][0]["change"] = "x" * review.MAX_COMMENT_BYTES
        github = FakeGitHub()
        self.assertFalse(self.publish(data, github))
        self.assertIn("incomplete", github.posted_body())
        self.assertNotIn("- [ ]", github.posted_body())

    def test_stale_head_base_closed_or_fork_pr_never_mutates(self):
        stale_pulls = []
        for side in ("head", "base"):
            current = pull()
            current[side]["sha"] = "f" * 40
            stale_pulls.append(current)
        closed = pull()
        closed["state"] = "closed"
        stale_pulls.append(closed)
        fork = pull()
        fork["head"]["repo"]["full_name"] = "outsider/WendyOS"
        stale_pulls.append(fork)
        for stale in stale_pulls:
            for pulls in ([stale], [pull(), stale]):
                github = FakeGitHub(pulls=pulls)
                with self.subTest(pulls=pulls), self.assertRaisesRegex(ValueError, "refusing stale"):
                    self.publish(result(), github)
                self.assertEqual(github.mutations(), [])


class MultipartTests(unittest.TestCase):
    def publish(self, data, github):
        return review.publish(data, REPO, 1911, HEAD, BASE, github)

    def previous_comments(self, data):
        pages = review.render_continuations(data, REPO)
        comments = [bot_comment(body, index) for index, body in enumerate(pages, start=11)]
        root = review.render_primary(data, REPO, 1911, [comment["id"] for comment in comments])
        comments.append(bot_comment(root, 99))
        return comments

    def test_93_long_decisions_are_complete_bounded_and_published_before_primary(self):
        data = large_result()
        with self.assertRaises(ValueError):
            review.render_comment(data, REPO)
        github = MultipartGitHub()
        self.assertTrue(self.publish(data, github))
        primary = next(comment for comment in github.comments if review.part_index(comment["body"]) == 0)
        pages = [comment for comment in github.comments if review.part_index(comment["body"]) > 0]
        self.assertGreater(len(pages), 1)
        self.assertGreater(primary["id"], max(comment["id"] for comment in pages))
        self.assertIn("93 decisions", primary["body"])
        self.assertIn("11 complete batch(es)", primary["body"])
        for page in pages:
            self.assertIn(f"#issuecomment-{page['id']}", primary["body"])
        bodies = "\n".join(page["body"] for page in pages)
        self.assertEqual(bodies.count("- [ ] Accept"), 93)
        for item in data["decisions"]:
            identifier = f"<!-- api-decision:{review.decision_id(item)} -->"
            self.assertEqual(bodies.count(identifier), 1)
            self.assertIn(review.inline(item["change"]), bodies)
            self.assertIn(review.inline(item["compatibility"]), bodies)
            self.assertIn(review.code_link(REPO, data, item["locations"][0]), bodies)
        for comment in github.comments:
            self.assertLessEqual(len(comment["body"].encode()), review.MAX_COMMENT_BYTES - review.WARNING_RESERVE_BYTES)
        writes = [(index, payload["body"]) for index, (method, _, payload) in enumerate(github.calls)
                  if method != "GET" and payload and "body" in payload]
        self.assertEqual(review.part_index(writes[-1][1]), 0)
        label_mutations = [index for index, (method, path, _) in enumerate(github.calls)
                           if method != "GET" and "/labels" in path]
        self.assertGreater(min(label_mutations), writes[-1][0])

    def test_rerun_preserves_acceptance_and_finds_primary_created_after_parts(self):
        data = large_result()
        comments = self.previous_comments(data)
        comments[0]["body"] = comments[0]["body"].replace("- [ ]", "- [x]", 1)
        old_parts = {comment["id"] for comment in comments if review.part_index(comment["body"]) > 0}
        github = MultipartGitHub(comments)
        self.assertTrue(self.publish(data, github))
        self.assertTrue(any(method == "PATCH" and path.endswith("/comments/99") for method, path, _ in github.calls))
        self.assertEqual(sum(comment["body"].count("- [x] Accept") for comment in github.comments), 1)
        self.assertTrue(old_parts.isdisjoint(comment["id"] for comment in github.comments))
        root_write = next(index for index, (method, path, _) in enumerate(github.calls)
                          if method == "PATCH" and path.endswith("/comments/99"))
        deletions = [index for index, (method, path, _) in enumerate(github.calls)
                     if method == "DELETE" and "/issues/comments/" in path]
        self.assertGreater(min(deletions), root_write)

    def test_mixed_revision_pages_cannot_revive_stale_acceptance(self):
        data = large_result()
        comments = self.previous_comments(data)
        old_revision = review.revision_marker({**data, "head_sha": "d" * 40})
        comments[1]["body"] = comments[1]["body"].replace(review.revision_marker(data), old_revision).replace("- [ ]", "- [x]")
        github = MultipartGitHub(comments)
        self.assertTrue(self.publish(data, github))
        self.assertEqual(sum(comment["body"].count("- [x] Accept") for comment in github.comments), 0)

    def test_missing_old_page_is_already_clean_but_other_cleanup_errors_fail(self):
        data = large_result()
        for status in (404, 500):
            with self.subTest(status=status):
                github = MultipartGitHub(self.previous_comments(data))
                original_request = github.request

                def request(method, path, payload=None):
                    if method == "DELETE" and "/issues/comments/" in path:
                        error = urllib.error.HTTPError(path, status, "simulated deletion failure", None, None)
                        error.close()
                        raise error
                    return original_request(method, path, payload)

                with patch.object(github, "request", side_effect=request):
                    if status == 404:
                        self.assertTrue(self.publish(data, github))
                    else:
                        with self.assertRaises(urllib.error.HTTPError):
                            self.publish(data, github)

    def test_partial_write_failure_preserves_prior_primary_pages_and_risk_labels(self):
        data = large_result()
        comments = self.previous_comments(data)
        comments[0]["body"] = comments[0]["body"].replace("- [ ]", "- [x]", 1)
        original_pages = {comment["id"]: comment["body"] for comment in comments if review.part_index(comment["body"]) > 0}
        github = MultipartGitHub(comments, fail_body_write=2)
        self.assertFalse(self.publish(data, github))
        current = {comment["id"]: comment["body"] for comment in github.comments}
        for identifier, body in original_pages.items():
            self.assertEqual(current[identifier], body)
            self.assertIn(f"#issuecomment-{identifier}", current[99])
        self.assertIn(review.WARNING_START, current[99])
        self.assertIn("Could not publish every", current[99])
        self.assertFalse(any(method == "DELETE" or "/labels/risk" in path for method, path, _ in github.mutations()))
        self.assertFalse(any(method == "POST" and path.endswith("/labels") and any(label.startswith("risk:") for label in payload["labels"])
                             for method, path, payload in github.mutations()))

    def test_stale_revision_after_continuations_never_commits_primary(self):
        data = large_result()
        comments = self.previous_comments(data)
        stale = pull()
        stale["head"]["sha"] = "d" * 40
        github = MultipartGitHub(comments, pulls=[pull(), pull(), stale, stale])
        with self.assertRaisesRegex(ValueError, "refusing stale"):
            self.publish(data, github)
        self.assertFalse(any(method == "PATCH" or method == "DELETE" or "/labels" in path for method, path, _ in github.mutations()))
        self.assertEqual(next(comment["body"] for comment in github.comments if comment["id"] == 99), comments[-1]["body"])

    def test_oversized_later_decision_is_rejected_before_any_part_is_written(self):
        data = large_result()
        data["decisions"][-1]["change"] = "x" * review.MAX_COMMENT_BYTES
        github = MultipartGitHub()
        self.assertFalse(self.publish(data, github))
        bodies = [payload["body"] for _, _, payload in github.calls if payload and "body" in payload]
        self.assertEqual(len(bodies), 1)
        self.assertIn("individual API decision", bodies[0])
        self.assertNotIn("- [ ]", bodies[0])


if __name__ == "__main__":
    unittest.main()
