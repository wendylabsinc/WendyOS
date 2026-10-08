#!/usr/bin/env python3
"""Full state-fetch/review/publication runs through the pinned SDK, offline only."""

import argparse
import copy
import hashlib
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
from contextlib import redirect_stderr

import anthropic
import httpx

import api_review
import api_review_comment as comments
from api_review_test import metadata, diff, decision, model_decision, HEAD_SHA, BASE_SHA, REPO
from api_review_comment_test import result, bot_comment, MultipartGitHub, pull


class FullRunTests(unittest.TestCase):
    def previous(self, legacy=False):
        data = result()
        data.update(head_sha="e" * 40, diff_sha256=hashlib.sha256(diff()).hexdigest(), diff_bytes=len(diff()),
                    decisions=[dict(decision(title=f"Contract {index}"), prior_ids=[],
                                    relationship="new", reason="") for index in range(11)])
        body = comments.render_comment(data, REPO, diff=diff().decode())
        if legacy:
            body = comments.STATE_MARKER_RE.sub(
                lambda match: "<!-- api-decision:" + comments.decode_state_marker(match.group(1))["id"] + " -->",
                body)
        return body.replace("- [ ] Accept", "- [x] Accept")

    def run_flow(self, *, relationship="unchanged", failure=None, late_checkbox=False,
                 stale=False, changed_state=False, legacy=False, impact_change=False, dry_run=False,
                 withdraw_prior=False, competing_claims=False):
        previous = self.previous(legacy=legacy)
        source = MultipartGitHub([bot_comment(previous)])
        github = comments.DryRunGitHub(source) if dry_run else source
        requests = []
        stderr = io.StringIO()
        records = comments.parse_comment_state([previous])
        ids = [record["id"] for record in records]
        ids_by_title = {record["decision"]["title"]: record["id"] for record in records}

        def transport(request):
            payload = json.loads(request.content)
            requests.append(payload)
            # create() sends the raw schema, unlike parse(). Assert the wire
            # schema fits the SDK's documented supported subset, not just mocks.
            from anthropic.lib._parse._transform import transform_schema
            schema = payload["output_config"]["format"]["schema"]
            self.assertEqual(schema, transform_schema(copy.deepcopy(schema)))
            if len(requests) == 1:
                response = {"risk": "mid", "decisions": [
                    model_decision(title=f"Contract {index}", impact="additive" if impact_change else "breaking")
                    for index in range(11)]}
            else:
                prompt = json.loads(payload["messages"][0]["content"])
                self.assertTrue(all("state" not in item for item in prompt["prior_decisions"]))
                self.assertNotIn("accepted", json.dumps(prompt).lower())
                if failure == "http":
                    return httpx.Response(400, json={"type": "error", "error": {
                        "type": "invalid_request_error", "message": "private-provider-response"}})
                if failure == "transport":
                    raise httpx.ConnectError("private-request-url", request=request)
                if failure == "unexpected":
                    raise RuntimeError("private-provider-payload")
                response = {"matches": [{"current_index": index, "prior_ids": [ids_by_title[f"Contract {index}"]],
                                        "relationship": relationship,
                                        "reason": "Material contract change" if relationship != "unchanged" else ""}
                                       for index in range(11)]}
                if competing_claims:
                    response["matches"][1]["prior_ids"] = response["matches"][0]["prior_ids"][:]
                if withdraw_prior:
                    response["matches"][-1].update(prior_ids=[], relationship="new", reason="")
                if failure == "invalid":
                    response["matches"].pop()
                if failure == "incomplete":
                    return self.message(response, stop_reason="max_tokens")
                if failure == "malformed_json":
                    message = self.message(response)
                    value = message.json()
                    value["content"][0]["text"] = "not-json"
                    return httpx.Response(200, json=value)
            return self.message(response)

        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            prior_path = directory / "previous.json"
            comments.fetch_state(REPO, 1911, prior_path, github)
            (directory / "metadata.json").write_text(json.dumps(metadata(number=1911)))
            (directory / "diff").write_bytes(diff())
            args = argparse.Namespace(metadata=str(directory / "metadata.json"), diff=str(directory / "diff"),
                                      previous=str(prior_path), repo=REPO, pr_number=1911,
                                      expected_head_sha=HEAD_SHA, expected_base_sha=BASE_SHA,
                                      model="test-model", output=str(directory / "result.json"))
            with httpx.Client(transport=httpx.MockTransport(transport)) as http:
                client = anthropic.Anthropic(api_key="offline-fixture-key", http_client=http, max_retries=0)
                constructor = ([client, ValueError("private-credential-initialization")]
                               if failure == "initialization" else None)
                with patch.object(anthropic, "Anthropic", return_value=client,
                                  side_effect=constructor), redirect_stderr(stderr):
                    code = api_review.command_review(args)
            output = json.loads((directory / "result.json").read_text())
            if late_checkbox:
                github.comments[0]["body"] = previous.replace("- [x] Accept", "- [ ] Accept", 1)
            if changed_state:
                match = comments.STATE_MARKER_RE.search(previous)
                changed = comments.decode_state_marker(match.group(1))
                changed["decision"]["change"] = "Another run changed this contract."
                changed["version"] = comments.decision_id(changed["decision"])
                github.comments[0]["body"] = previous.replace(match.group(), comments.state_marker(changed))
            if stale:
                current = pull()
                current["head"]["sha"] = "d" * 40
                github.pulls = [current]
                with self.assertRaisesRegex(ValueError, "refusing stale"):
                    comments.publish(output, REPO, 1911, HEAD_SHA, BASE_SHA, diff(), github)
                self.assertEqual(github.mutations(), [])
                return
            published = comments.publish(output, REPO, 1911, HEAD_SHA, BASE_SHA, diff(), github)
            body = github.bodies[-1] if dry_run else github.posted_body()
            if dry_run:
                self.assertEqual(source.mutations(), [])
                expected_accepted = 9 if competing_claims else 10 if withdraw_prior else 11
                self.assertIn(f'"accepted": {expected_accepted}', github.summary())
            self.assertEqual(len(requests), 1 if failure == "initialization" else 2)
            if failure:
                self.assertEqual(code, 1)
                self.assertEqual(output["status"], "incomplete")
                self.assertEqual(output["decisions"], [])
                self.assertFalse(published)
                self.assertIn(comments.WARNING_START, body)
                self.assertEqual(body.count("- [x]"), 11)
                risk_names = {label["name"] for label in comments.RISK_LABELS.values()}
                for _, path, payload in github.mutations():
                    if "/labels" in path:
                        self.assertFalse(any(name in path for name in risk_names))
                        if payload:
                            self.assertFalse(risk_names.intersection(payload.get("labels", [])))
                for text in (json.dumps(output), stderr.getvalue(), body):
                    self.assertNotIn("private-", text)
                    self.assertNotIn("offline-fixture-key", text)
                if failure == "initialization":
                    self.assertIn("initialization, unexpected", output["error"])
                if failure == "http":
                    self.assertIn("request, APIStatusError, HTTP 400", output["error"])
                if failure == "transport":
                    self.assertIn("request, APIConnectionError", output["error"])
                if failure == "unexpected":
                    # The SDK wraps unexpected transport exceptions before
                    # they reach our boundary, without exposing their text.
                    self.assertIn("request, APIConnectionError", output["error"])
            elif changed_state:
                self.assertFalse(published)
                self.assertIn("state changed", body)
                self.assertEqual(body.count("- [x]"), 11)
            else:
                self.assertEqual(code, 0)
                self.assertEqual(output["status"], "complete")
                self.assertTrue(published)
                self.assertNotIn(comments.WARNING_START, body)
                current = [r for r in comments.parse_comment_state([body]) if r["state"] != "withdrawn"]
                self.assertEqual(len(current), 11)
                if competing_claims:
                    self.assertEqual(body.count("- [x]"), 9)
                    reopened = [r for r in current if r["state"] == "needs_re_review"]
                    self.assertEqual(len(reopened), 2)
                    self.assertTrue(all(r["accepted_version"] is None for r in reopened))
                    self.assertTrue(all(r["id"] not in ids for r in reopened))
                    self.assertIn("Multiple current decisions", body)
                    preserved = [r for r in current if r["state"] == "accepted"]
                    self.assertEqual(len(preserved), 9)
                    self.assertTrue(all(r["id"] == ids_by_title[r["decision"]["title"]] for r in preserved))
                    historical = [r for r in comments.parse_comment_state([body]) if r["state"] == "withdrawn"]
                    self.assertEqual(len(historical), 2)
                    self.assertTrue(all(r["accepted_version"] is not None for r in historical))
                elif withdraw_prior:
                    self.assertEqual(body.count("- [x]"), 10)
                    withdrawn = [r for r in comments.parse_comment_state([body]) if r["state"] == "withdrawn"]
                    self.assertEqual(len(withdrawn), 1)
                    self.assertIsNotNone(withdrawn[0]["accepted_version"])
                elif relationship == "unchanged" and not impact_change:
                    self.assertEqual(body.count("- [x]"), 10 if late_checkbox else 11)
                    self.assertEqual({r["id"] for r in current}, set(ids))
                    self.assertEqual({r["decision"]["title"]: r["id"] for r in current}, ids_by_title)
                else:
                    self.assertTrue(all(r["state"] == "needs_re_review" for r in current))
                    self.assertEqual(body.count("- [x]"), 0)

    @staticmethod
    def message(payload, stop_reason="end_turn"):
        return httpx.Response(200, json={"id": "msg_offline", "type": "message", "role": "assistant",
                                       "model": "test-model", "stop_reason": stop_reason,
                                       "stop_sequence": None,
                                       "content": [{"type": "text", "text": json.dumps(payload)}],
                                       "usage": {"input_tokens": 1, "output_tokens": 1}})

    def test_complete_run_preserves_eleven_accepted_decisions(self):
        self.run_flow()

    def test_complete_run_dry_publication_makes_no_github_mutations(self):
        self.run_flow(dry_run=True)

    def test_complete_run_migrates_eleven_legacy_acceptances(self):
        self.run_flow(legacy=True)

    def test_complete_run_competing_claims_reopen_without_losing_other_acceptances(self):
        self.run_flow(competing_claims=True, legacy=True, dry_run=True)

    def test_complete_run_withdraws_legacy_decision_without_corrupting_history(self):
        self.run_flow(legacy=True, dry_run=True, withdraw_prior=True)

    def test_complete_run_reopens_changed_and_ambiguous_decisions(self):
        for relationship in ("changed", "ambiguous"):
            with self.subTest(relationship=relationship):
                self.run_flow(relationship=relationship)

    def test_complete_run_impact_change_overrides_model_unchanged(self):
        self.run_flow(impact_change=True)

    def test_complete_run_honors_late_human_uncheck(self):
        self.run_flow(late_checkbox=True)

    def test_failed_runs_keep_acceptance_and_honest_incomplete_status(self):
        for failure in ("initialization", "http", "transport", "unexpected", "invalid", "incomplete", "malformed_json"):
            with self.subTest(failure=failure):
                self.run_flow(failure=failure)

    def test_stale_head_refuses_all_mutations(self):
        self.run_flow(stale=True)

    def test_prior_state_race_preserves_old_checklist(self):
        self.run_flow(changed_state=True)


if __name__ == "__main__":
    unittest.main()
