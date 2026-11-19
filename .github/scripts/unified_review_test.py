#!/usr/bin/env python3
"""Unified evidence contract, grouping, acceptance/history and dry-run examples."""
import copy
import json
import unittest
from unittest.mock import patch

import api_review as api
import api_review_comment as comments
import compatibility_review as compat
import compatibility_state
import unified_review as unified
from api_review_test import metadata, model_decision, REPO
from api_review_comment_test import result
import compatibility_review_test as evidence_fixtures


def fixture(payload):
    """Explicitly upgrade old API fixtures; no runtime chain is invented."""
    value = copy.deepcopy(payload)
    findings = value.pop("compatibility_findings", [])
    if findings:
        if len(value["decisions"]) != 1 or len(findings) != 1:
            raise AssertionError("Test must explicitly attach each concern to its change")
    for item in value["decisions"]:
        item.update(plain_terms="Existing consumers observe this contract change.",
                    coverage="Only the supplied source change was inspected; peer versions remain unassessed.",
                    concern=findings[0] if findings else None)
        if item.get("impact") == "breaking" and not findings:
            # Historical API inventory fixtures classified breaks without a runtime chain.
            # Keep these API mechanics fixtures behavioral; dedicated concern tests prove breaks.
            item["impact"] = "behavioral"
    return value


def singleton_groups(matches):
    return {"matches": [dict(row, members=[row["current_index"]], representative=row["current_index"])
                        for row in matches]}


class UnifiedTests(unittest.TestCase):
    def setUp(self):
        self.evidence = evidence_fixtures.EvidenceTests(); self.evidence.setUp()
        self.parsed = api.parse_diff(self.evidence.patch)
        self.meta = api.prepare_review(metadata(), self.evidence.patch, REPO, [evidence_fixtures.entry()])
        self.assessment = self.meta["_compatibility"]
        self.assessment["unified"] = True

    def payload(self, concern=True):
        f = dict(self.evidence.finding, prior_id="", caller=[next(iter(self.evidence.changed))])
        value = evidence_fixtures.response(f if concern else None)
        item = model_decision()
        item["locations"] = [{"evidence_id": next(iter(self.evidence.changed))}]
        item["impact"] = "breaking" if concern else "additive"
        value["decisions"] = [item]
        return fixture(value)

    def validated(self, concern=True):
        return unified.validate(self.payload(concern), self.parsed, self.assessment)

    def test_single_inventory_schema_and_runtime_evidence(self):
        shape = api.model_schema(api.evidence_catalog(self.parsed), self.assessment)
        self.assertNotIn("compatibility_findings", shape["properties"])
        from anthropic.lib._parse._transform import transform_schema
        self.assertEqual(shape, transform_schema(copy.deepcopy(shape)))
        payload = self.validated()
        self.assertNotIn("compatibility_findings", payload)
        self.assertEqual(payload["decisions"][0]["concern"]["changed"][0]["kind"], "changed_source")
        for role in ("changed", "caller", "receiver"):
            bad = self.payload(); bad["decisions"][0]["concern"][role] = ["invented"]
            with self.assertRaises(api.ReviewError):
                unified.validate(bad, self.parsed, self.assessment)
        bad = self.payload(); bad["decisions"][0]["plain_terms"] = ""
        with self.assertRaises(api.ReviewError):
            unified.validate(bad, self.parsed, self.assessment)

    def test_valid_item_cannot_disappear_during_shape_repair(self):
        previous = self.payload()
        fixed = self.validated()
        unified.preserve(previous, fixed, self.parsed, self.assessment)
        fixed["decisions"][0]["plain_terms"] = "Different observation"
        with self.assertRaises(api.ReviewError):
            unified.preserve(previous, fixed, self.parsed, self.assessment)

    def report(self, concern=True):
        value = self.validated(concern)
        value["compatibility_coverage"] = [{"batch": 1, "directions": value["compatibility_coverage"]}]
        value["decisions"] = [dict(d, prior_ids=[], relationship="new", reason="") for d in value["decisions"]]
        return {**result(), **value, "unified_review": True, "input_visibility": "public", "compatibility_previous": []}

    def test_render_one_item_plain_intro_and_no_active_preview_state(self):
        report = self.report()
        body = comments.render_comment(report, REPO)
        self.assertEqual(body.count("**In plain terms:**"), 1)
        self.assertEqual(body.count("Consider retaining"), 1)
        self.assertNotIn("**Before:**", body)
        self.assertNotIn("**After:**", body)
        self.assertNotIn("**Direction:**", body)
        self.assertIn("<summary>Source evidence and limits</summary>", body)
        self.assertNotIn("## Compatibility review", body)
        self.assertEqual(len(comments.parse_comment_state([body])), 1)
        self.assertEqual(len(compatibility_state.parse([body])), 1)
        preview = unified.preview([body])
        self.assertNotIn("<!--", preview)
        self.assertNotIn("- [ ]", preview)
        self.assertNotIn("- [x]", preview)
        self.assertIn("**Change:**", preview)

    def test_acceptance_is_separate_from_concern_and_new_concern_needs_review(self):
        old = self.report(False)
        body = comments.render_comment(old, REPO).replace("- [ ] Accept", "- [x] Accept")
        prior = comments.parse_comment_state([body])
        self.assertEqual(prior[0]["state"], "accepted")
        current = self.report(True)
        current["decisions"][0].update(prior_ids=[prior[0]["id"]], relationship="unchanged")
        # Even a mistaken unchanged match cannot carry human acceptance into a new concern.
        carried, _ = comments.reconcile_records(current, prior)
        self.assertEqual(carried[0]["state"], "needs_re_review")
        current["decisions"][0].update(relationship="changed", reason="New concern")
        accepted_body = comments.render_comment(current, REPO, prior).replace("- [ ] Re-review", "- [x] Re-review")
        accepted = comments.parse_comment_state([accepted_body])
        self.assertEqual(accepted[0]["state"], "accepted")
        self.assertEqual(compatibility_state.parse([accepted_body])[0]["status"], "open")

    def test_legacy_state_round_trip_and_unassessed_concern_history(self):
        legacy = comments.render_comment(result(), REPO).replace("- [ ] Accept", "- [x] Accept")
        prior = comments.parse_comment_state([legacy])
        current = self.report(False)
        current["decisions"][0].update(prior_ids=[prior[0]["id"]], relationship="unchanged")
        body = comments.render_comment(current, REPO, prior)
        self.assertEqual(comments.parse_comment_state([body])[0]["state"], "accepted")
        concern = compatibility_state.current(unified.projection(self.report(True)))[0]
        current["compatibility_previous"] = [concern]
        body = comments.render_comment(current, REPO)
        history = compatibility_state.parse([body])
        self.assertEqual(history[0]["id"], concern["id"])
        self.assertEqual(history[0]["status"], "not_assessed")
        self.assertIn("Prior concern history", body)

    def reconcile(self, groups, items=None, prior=None):
        import anthropic
        import httpx
        payload = self.report()
        payload["decisions"] = items or [dict(payload["decisions"][0]) for _ in range(2)]
        for item in payload["decisions"]:
            for field in ("prior_ids", "relationship", "reason"):
                item.pop(field, None)
        def transport(request):
            shape = json.loads(request.content)["output_config"]["format"]["schema"]
            from anthropic.lib._parse._transform import transform_schema
            self.assertEqual(shape, transform_schema(copy.deepcopy(shape)))
            return httpx.Response(200, json={"id": "msg_test", "type": "message", "role": "assistant", "model": "test",
                "stop_reason": "end_turn", "content": [{"type": "text", "text": json.dumps({"matches": groups})}],
                "usage": {"input_tokens": 1, "output_tokens": 1}})
        with httpx.Client(transport=httpx.MockTransport(transport)) as http:
            client = anthropic.Anthropic(api_key="offline-key", http_client=http)
            with patch.object(anthropic, "Anthropic", return_value=client):
                return unified.reconcile(payload, prior or {"decisions": []}, "test")

    def group(self, members, representative=0):
        return dict(current_index=0, members=members, representative=representative, prior_ids=[], relationship="new", reason="")

    def test_consolidation_keeps_one_original_observation_and_all_indices(self):
        value = self.reconcile([self.group([0, 1])])
        self.assertEqual(len(value["decisions"]), 1)
        self.assertEqual(value["consolidation"]["groups"], [[0, 1]])
        for members in ([0], [0, 0], [0, 2], [True, 1]):
            with self.subTest(members=members), self.assertRaises(api.ReviewError):
                self.reconcile([self.group(members)])

    def test_grouping_does_not_erase_other_prior_concern_identities(self):
        concern = compatibility_state.current(unified.projection(self.report()))[0]
        concern["id"] = "f" * 64
        value = self.reconcile([self.group([0, 1])], prior={"decisions": [], "compatibility_findings": [concern]})
        self.assertEqual(value["compatibility_updates"][0]["status"], "not_assessed")

    def test_oversized_unified_state_fails_before_publication(self):
        report = self.report(False)
        report["decisions"][0]["change"] = "x" * 33000
        with self.assertRaisesRegex(ValueError, "marker limit"):
            comments.render_comment(report, REPO)

    def test_private_unified_result_cannot_be_published(self):
        for visibility in (None, "private"):
            report = self.report(False); report["input_visibility"] = visibility
            with self.assertRaises(api.ReviewError):
                compat.public_result(report)

    def test_multipart_round_trip_retains_every_item_and_concern(self):
        report = self.report()
        with patch.object(comments, "MAX_COMMENT_BYTES", 18000):
            pages = comments.render_continuations(report, REPO)
        self.assertEqual(len(comments.parse_comment_state(pages)), 1)
        self.assertEqual(len(compatibility_state.parse(pages)), 1)
        self.assertTrue(all(len(p.encode()) < 14000 for p in pages))


if __name__ == "__main__":
    unittest.main()
