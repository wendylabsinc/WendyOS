#!/usr/bin/env python3
"""Unified evidence contract, grouping, acceptance/history and dry-run examples."""
import copy
import json
import re
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

    def test_prose_schema_describes_existing_bounds_without_unsupported_keywords(self):
        schema = api.model_schema(api.evidence_catalog(self.parsed), self.assessment)
        fields = schema["properties"]["decisions"]["items"]["properties"]
        for name, maximum in (("title", 200), ("change", 2000), ("compatibility", 2000), ("plain_terms", 1600), ("coverage", 1600)):
            self.assertIn("non-empty single-line", fields[name]["description"])
            self.assertIn(str(maximum), fields[name]["description"])
        self.assertIn("who notices what", fields["plain_terms"]["description"])
        concern = fields["concern"]["anyOf"][1]
        for name in compat.FINDING_TEXT_FIELDS:
            self.assertIn(str(compat.MAX_FINDING_TEXT), concern["properties"][name]["description"])
            self.assertIn("Single-line", concern["properties"][name]["description"])
        for row in schema["properties"]["compatibility_coverage"]["properties"].values():
            self.assertIn(str(compat.MAX_COVERAGE_REASON), row["properties"]["reason"]["description"])
            self.assertIn("Single-line", row["properties"]["reason"]["description"])
        from anthropic.lib._parse._transform import transform_schema
        self.assertEqual(schema, transform_schema(copy.deepcopy(schema)))
        for unsupported in ('"minLength"', '"maxLength"', '"pattern"'):
            self.assertNotIn(unsupported, json.dumps(schema))

    def test_prose_guards_reject_blank_multiline_controls_and_oversize_with_precise_diagnostics(self):
        for name, maximum in (("title", 200), ("change", 2000), ("compatibility", 2000), ("plain_terms", 1600), ("coverage", 1600)):
            cases = [("", "non-empty"), ("  ", "non-empty"), (None, "must be a string"),
                     ("line one\nline two", "newline (LF)"), ("one\rtwo", "carriage return (CR)"),
                     ("one\ttwo", "tab"), ("one\x00two", "U+0000"), ("one\x7ftwo", "U+007F"),
                     ("x" * (maximum + 1), f"{maximum}-character bound")]
            for value, message in cases:
                with self.subTest(field=name, message=message):
                    payload = self.payload(False)
                    payload["decisions"][0][name] = value
                    original = copy.deepcopy(payload)
                    with self.assertRaisesRegex(api.ReviewError, re.escape(f"decisions[0].{name}") + ".*" + re.escape(message)):
                        unified.validate(payload, self.parsed, self.assessment)
                    self.assertEqual(payload, original)  # never normalize or hand-repair provider text
            payload = self.payload(False)
            payload["decisions"][0][name] = "x" * maximum
            unified.validate(payload, self.parsed, self.assessment)

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

    def test_distinct_failures_and_additions_keep_all_observations_and_evidence(self):
        # The independent reproduction's two changed read paths are separate
        # contracts, even though their concern assessment/direction are equal.
        source = """diff --git a/registry.py b/registry.py
--- a/registry.py
+++ b/registry.py
@@ -1,2 +1,2 @@
 def read(record):
-    return record["legacy"]
+    return record["current"]
diff --git a/export.py b/export.py
--- a/export.py
+++ b/export.py
@@ -1,2 +1,2 @@
 def export(row):
-    return row["name"]
+    return row["title"]
"""
        parsed = api.parse_diff(source)
        assessment = api.prepare_review(metadata(), source, REPO)["_compatibility"]
        ids = {ref["path"]: n for n, ref in api.evidence_catalog(parsed).items() if ref["side"] == "head"}
        payload = self.payload()
        payload["decisions"] = []
        for path, action in (("registry.py", "Read an existing saved registry"), ("export.py", "Export an existing row")):
            item = self.payload()["decisions"][0]
            item.update(title=action, locations=[{"evidence_id": ids[path]}])
            item["concern"].update(action=action, consumer=path + " consumers", changed=[ids[path]], caller=[ids[path]], receiver=[ids[path]])
            payload["decisions"].append(item)
        payload["compatibility_coverage"]["state_reader_writer"]["evidence"] = list(ids.values())
        for concerns in (True, False):
            value = copy.deepcopy(payload)
            if not concerns:
                value["compatibility_verdict"] = "not_assessed"
                for item in value["decisions"]:
                    item.update(impact="additive", concern=None)
            checked = unified.validate(value, parsed, assessment)
            checked.update(review_batches=1, compatibility_previous=[])
            checked["compatibility_coverage"] = [{"batch": 1, "directions": checked["compatibility_coverage"]}]
            with patch.object(api, "reconcile_decisions", wraps=api.reconcile_decisions) as identity:
                output = unified.reconcile(checked, {"decisions": []}, "offline")
            identity.assert_called_once()
            self.assertEqual(len(output["decisions"]), 2)
            self.assertEqual([{k: d[k] for k in unified.BASE_FIELDS | unified.EXTRA_FIELDS} for d in output["decisions"]], checked["decisions"])
            compat.validate_aggregate(unified.projection(output), api.finding_catalog(api.evidence_catalog(parsed), assessment), assessment["catalog"])

    def test_identity_response_cannot_drop_a_current_observation(self):
        import anthropic
        import httpx
        payload = self.report(False)
        payload["decisions"].append(copy.deepcopy(payload["decisions"][0]))
        prior = comments.model_prior_state(comments.parse_comment_state([comments.render_comment(self.report(False), REPO)]))
        # This is precisely the old lossy grouping response. The existing
        # identity schema requires one independent match for every input item.
        response = {"matches": [{"current_index": 0, "members": [0, 1], "representative": 0,
                                 "prior_ids": [], "relationship": "new", "reason": ""}]}
        def transport(request):
            return httpx.Response(200, json={"id": "msg_test", "type": "message", "role": "assistant", "model": "test",
                "stop_reason": "end_turn", "content": [{"type": "text", "text": json.dumps(response)}],
                "usage": {"input_tokens": 1, "output_tokens": 1}})
        with httpx.Client(transport=httpx.MockTransport(transport)) as http:
            client = anthropic.Anthropic(api_key="offline-key", http_client=http)
            with patch.object(anthropic, "Anthropic", return_value=client), self.assertRaises(api.ReviewError):
                unified.reconcile(payload, prior, "test")
        self.assertEqual(len(payload["decisions"]), 2)
        self.assertEqual(len(payload["model_responses"]), 2)

    def test_missing_concern_in_first_item_accepts_valid_shape_repair(self):
        import anthropic
        import httpx
        fixed = self.payload(False)
        previous = copy.deepcopy(fixed)
        del previous["decisions"][0]["concern"]
        unified.preserve(previous, self.validated(False), self.parsed, self.assessment)
        responses = iter((previous, fixed))
        def transport(request):
            return httpx.Response(200, json={"id": "msg_test", "type": "message", "role": "assistant", "model": "test",
                "stop_reason": "end_turn", "content": [{"type": "text", "text": json.dumps(next(responses))}],
                "usage": {"input_tokens": 1, "output_tokens": 1}})
        with httpx.Client(transport=httpx.MockTransport(transport)) as http:
            client = anthropic.Anthropic(api_key="offline-key", http_client=http)
            with patch.object(anthropic, "Anthropic", return_value=client):
                output = api.review_model(self.meta, self.evidence.patch, REPO, "test")
        self.assertEqual(output, self.validated(False))

    def test_repair_can_correct_impact_contradicting_a_valid_concern(self):
        previous = self.payload()
        previous["decisions"][0]["impact"] = "behavioral"
        with self.assertRaises(api.ReviewError):
            unified.validate(previous, self.parsed, self.assessment)
        fixed = self.validated()
        unified.preserve(previous, fixed, self.parsed, self.assessment)
        removed = copy.deepcopy(fixed)
        removed["decisions"][0].update(impact="additive", concern=None)
        with self.assertRaises(api.ReviewError):
            unified.preserve(previous, removed, self.parsed, self.assessment)

    def test_malformed_first_item_cannot_change_valid_neighbor_or_lower_risk(self):
        complete = self.payload(False)
        complete["decisions"].append(copy.deepcopy(complete["decisions"][0]))
        complete["decisions"][1]["title"] = "Another valid contract"
        complete["risk"] = "high"
        previous = copy.deepcopy(complete)
        del previous["decisions"][0]["concern"]
        fixed = unified.validate(complete, self.parsed, self.assessment)
        unified.preserve(previous, fixed, self.parsed, self.assessment)
        changed = copy.deepcopy(fixed)
        changed["decisions"][1]["plain_terms"] = "A different observation"
        with self.assertRaises(api.ReviewError):
            unified.preserve(previous, changed, self.parsed, self.assessment)
        with self.assertRaises(api.ReviewError):
            unified.preserve(previous, dict(fixed, risk="low"), self.parsed, self.assessment)

    def test_different_concern_of_same_class_requires_fresh_acceptance(self):
        old = self.report()
        body = comments.render_comment(old, REPO).replace("- [ ] Accept", "- [x] Accept")
        prior = comments.parse_comment_state([body])
        history = compatibility_state.parse([body])
        current = copy.deepcopy(old)
        current["decisions"][0]["concern"].update(prior_id="", consumer="Another existing consumer", action="A distinct operation",
            condition="A different required field is absent", after="The other operation fails on that field")
        current["decisions"][0].update(prior_ids=[prior[0]["id"]], relationship="unchanged")
        current.update(compatibility_previous=history, compatibility_updates=[{"prior_id": history[0]["id"], "status": "not_assessed",
            "reason": "The old concern is not reassessed by this different observation.", "evidence": []}])
        records, _ = comments.reconcile_records(current, prior)
        self.assertEqual(records[0]["state"], "needs_re_review")
        self.assertIsNone(records[0]["accepted_version"])
        self.assertTrue(unified.concern_changed(prior[0]["decision"], records[0]["decision"]))
        new_history = compatibility_state.parse([comments.render_comment(current, REPO, prior)])
        self.assertEqual(len(new_history), 2)
        self.assertEqual({r["status"] for r in new_history}, {"open", "not_assessed"})

    def test_same_concern_identity_preserves_acceptance_after_wording_and_line_movement(self):
        old = self.report()
        body = comments.render_comment(old, REPO).replace("- [ ] Accept", "- [x] Accept")
        prior = comments.parse_comment_state([body])
        history = compatibility_state.parse([body])
        current = copy.deepcopy(old)
        current["decisions"][0]["concern"].update(prior_id=history[0]["id"], after="The same existing read fails to parse the record.")
        current["decisions"][0]["concern"]["changed"][0]["line"] += 1
        current["decisions"][0]["concern"]["changed"][0]["end_line"] += 1
        current["decisions"][0].update(prior_ids=[prior[0]["id"]], relationship="unchanged")
        current["compatibility_previous"] = history
        records, _ = comments.reconcile_records(current, prior)
        self.assertEqual(records[0]["state"], "accepted")
        self.assertEqual(records[0]["accepted_version"], prior[0]["accepted_version"])
        self.assertFalse(unified.concern_changed(prior[0]["decision"], records[0]["decision"]))

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
