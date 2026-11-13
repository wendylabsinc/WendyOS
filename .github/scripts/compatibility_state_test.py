#!/usr/bin/env python3
"""Output envelopes, bounded citations and state transitions; no semantic mocks."""
import copy
import json
import types
import unittest
from unittest.mock import patch

import api_review as api
import api_review_comment as comments
import compatibility_review as compat
import compatibility_state as state
import compatibility_review_test as fixtures
from api_review_comment_test import result, large_result, bot_comment, MultipartGitHub
from api_review_test import REPO, metadata, diff


class EnvelopeTests(unittest.TestCase):
    def test_whole_json_fence_has_identical_values_without_mutating_raw_text(self):
        payload={"risk":"low","decisions":[],"literal":"a ``` string"}
        text=json.dumps(payload)
        for outer in (text,'```json\n'+text+'\n```','```\r\n'+text+'\r\n```',' \n```JSON\n'+text+'\n```\t'):
            original=outer
            self.assertEqual(api.decode_model_json(outer),payload)
            self.assertEqual(outer,original)

    def test_prose_nested_or_malformed_fences_and_non_json_fail(self):
        for text in ('prefix\n```json\n{}\n```','```json\n{}\n```\nsuffix','```json\n{}','```python\n{}\n```',
                     '```json\n```json\n{}\n```\n```','{}{}','[]','null','{"x":NaN}','{"x":Infinity}',
                     '{"risk":"high","risk":"low"}','{"a":{"x":1,"x":2}}'):
            with self.subTest(text=text),self.assertRaises(compat.ReviewError):api.decode_model_json(text)

    def test_thinking_is_ignored_but_tools_and_truncation_fail(self):
        text=types.SimpleNamespace(type='text',text='```json\n{}\n```')
        thinking=types.SimpleNamespace(type='thinking',thinking='private hidden reasoning')
        message=types.SimpleNamespace(stop_reason='end_turn',content=[thinking,text])
        self.assertEqual(api.decode_model_json(api.provider_text(message)),{})
        for reason in ('max_tokens','refusal','pause_turn',None):
            with self.subTest(reason=reason),self.assertRaises(compat.ReviewError):api.provider_text(types.SimpleNamespace(stop_reason=reason,content=[text]))
        with self.assertRaises(compat.ReviewError):api.provider_text(types.SimpleNamespace(stop_reason='end_turn',content=[types.SimpleNamespace(type='tool_use'),text]))


class StatefulTests(unittest.TestCase):
    def setUp(self):
        self.fixture=fixtures.EvidenceTests();self.fixture.setUp()
        self.changed=self.fixture.changed;self.extra=self.fixture.extra
        self.initial=self.resolved(fixtures.response(self.fixture.finding))
        self.report=self.report_result(self.initial)
        self.records=state.current(self.report)
        self.identifier=self.records[0]['id']

    def resolved(self,payload,previous=None):
        return compat.validate(payload,self.changed,self.extra,previous=previous or [])

    def report_result(self,payload,previous=None):
        data=result();data.update(payload,input_visibility='public',compatibility_previous=previous or [],
                                  compatibility_coverage=[{'batch':1,'directions':payload['compatibility_coverage']}])
        return data

    def update(self,status,evidence=None):
        payload=fixtures.response()
        payload['compatibility_updates']=[{'prior_id':self.identifier,'status':status,'reason':'Current source establishes the reassessment',
                                          'evidence':list(self.changed)[:1] if evidence is None else evidence}]
        return payload

    def test_same_workflow_keeps_identity_across_wording_and_line_movement(self):
        raw=fixtures.response(dict(self.fixture.finding,prior_id=self.identifier,after='The retained value can no longer be read'))
        payload=self.resolved(raw,self.records)
        second=state.current(self.report_result(payload,self.records))
        self.assertEqual(len(second),1);self.assertEqual(second[0]['id'],self.identifier)
        self.assertEqual(second[0]['status'],'open')
        body=comments.render_comment(self.report_result(payload,self.records),REPO)
        self.assertIn('#### ⚠️ Concern — Open:',body)
        self.assertIn('<summary>Evidence and affected flow</summary>',body)
        self.assertNotIn('boundary_paths',body)
        self.assertEqual(state.parse([body]),second)

    def test_addressed_cancelled_not_assessed_and_reopening(self):
        for status in ('addressed','cancelled','not_assessed'):
            payload=self.resolved(self.update(status),self.records)
            updated=state.current(self.report_result(payload,self.records))
            self.assertEqual(updated[0]['id'],self.identifier);self.assertEqual(updated[0]['status'],status)
            body=comments.render_comment(self.report_result(payload,self.records),REPO)
            self.assertIn({'addressed':'✅ Addressed','cancelled':'🚫 Cancelled','not_assessed':'Not reassessed'}[status],body)
            self.assertEqual(state.parse([body]),updated)
            reopened=self.resolved(fixtures.response(dict(self.fixture.finding,prior_id=self.identifier)),updated)
            self.assertEqual(state.current(self.report_result(reopened,updated))[0]['status'],'open')

    def test_missing_assessment_never_clears_prior_concern(self):
        with self.assertRaisesRegex(compat.ReviewError,'omitted'):self.resolved(fixtures.response(),self.records)
        payload=self.resolved(self.update('not_assessed',[]),self.records)
        current=state.current(self.report_result(payload,self.records))
        self.assertEqual(current[0]['status'],'not_assessed')
        for status in ('addressed','cancelled'):
            with self.subTest(status=status),self.assertRaises(compat.ReviewError):self.resolved(self.update(status,[]),self.records)
        # The renderer independently preserves an omitted old concern, too.
        bare=self.report_result(fixtures.response(),self.records)
        self.assertEqual(state.current(bare)[0]['status'],'not_assessed')

    def test_no_automatic_approval_or_acceptance_state_change(self):
        before=comments.parse_comment_state([comments.render_comment(result(),REPO)])
        data=copy.deepcopy(self.report);data['decisions']=result()['decisions']
        after=comments.parse_comment_state([comments.render_comment(data,REPO)])
        self.assertEqual(before,after)
        self.assertEqual(set(self.records[0]),{'id','status','finding','resolution'})

    def test_prior_state_digest_covers_findings_without_exposing_acceptance(self):
        decisions=comments.parse_comment_state([comments.render_comment(result(),REPO)])
        old=comments.model_prior_state(decisions)
        new=comments.model_prior_state(decisions,self.records)
        self.assertNotEqual(api.prior_state_digest(old),api.prior_state_digest(new))
        meta=api.prepare_review(metadata(),diff().decode(),REPO,[fixtures.entry()],previous=self.records)
        request=json.loads(api.user_prompt(meta,diff().decode(),REPO))
        self.assertEqual(request['prior_compatibility_findings'],self.records)
        self.assertIn(self.identifier,request['response_schema']['properties']['compatibility_findings']['items']['properties']['prior_id']['enum'])
        self.assertNotIn('accepted_version',json.dumps(request))

    def test_only_linked_bot_history_is_authoritative(self):
        body=comments.render_comment(self.report,REPO)
        orphan=bot_comment(body,55)
        primary=bot_comment(comments.render_comment(result(),REPO),11)
        self.assertEqual(state.parse(comments.authoritative_bodies([orphan,primary])),[])
        primary['body']=comments.render_primary(self.report,REPO,1911,[55])
        self.assertEqual(state.parse(comments.authoritative_bodies([orphan,primary])),self.records)
        self.assertEqual(state.parse(comments.authoritative_bodies([])),[])

    def test_whole_finding_blocks_and_state_survive_multipart_rendering(self):
        data=large_result()
        data.update(self.report)
        data['decisions']=large_result()['decisions']
        parts=comments.render_continuations(data,REPO)
        self.assertGreater(len(parts),1)
        self.assertEqual(state.parse(parts),self.records)
        self.assertEqual(sum(part.count('compatibility-finding:v1:') for part in parts),1)

    def test_citation_bound_is_communicated_and_never_silently_truncated(self):
        request=json.loads(api.user_prompt(self.fixture.meta,diff().decode(),REPO))
        self.assertEqual(request['response_limits']['references_per_list'],8)
        self.assertIn('at most 8',api.system_prompt())
        schema=request['response_schema']['properties']['compatibility_coverage']['properties']['old_caller_new_peer']['properties']['evidence']
        self.assertIn('At most 8',schema['description'])
        payload=fixtures.response()
        payload['compatibility_coverage']['old_caller_new_peer'].update(status='partial',evidence=[next(iter(self.changed))]*9)
        saved=copy.deepcopy(payload)
        with self.assertRaises(compat.ReviewError):self.resolved(payload)
        self.assertEqual(payload,saved)

    def test_code_fences_and_source_quotes_cannot_inject_either_state(self):
        marker=state.marker(self.records[0])
        for body in ('```text\n'+marker+'\n```', '~~~~\n'+marker+'\n~~~~', '+ '+marker, '> '+marker):
            self.assertEqual(state.parse([body]),[])
        api_body=comments.render_comment(result(),REPO)
        self.assertEqual(comments.parse_comment_state(['```text\n'+api_body+'\n```']),[])
        self.assertEqual(state.parse(['```text\n'+marker+'\n```\n'+marker]),self.records)

    def test_unknown_duplicate_and_private_history_fail_closed(self):
        bad=copy.deepcopy(self.records);bad[0]['finding']['caller'][0]['visibility']='private'
        with self.assertRaises(compat.ReviewError):state.validate_records(bad)
        with self.assertRaises(compat.ReviewError):state.validate_records(self.records*2)
        payload=fixtures.response(dict(self.fixture.finding,prior_id='f'*64))
        with self.assertRaises(compat.ReviewError):self.resolved(payload,self.records)

if __name__=='__main__':unittest.main()
