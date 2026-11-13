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

    def test_large_coverage_is_complete_and_bounded_across_review_parts(self):
        data=copy.deepcopy(self.report)
        data['review_batches']=14
        base=data['compatibility_coverage'][0]['directions']
        data['compatibility_coverage']=[]
        reasons=[]
        for number in range(1,15):
            directions=copy.deepcopy(base)
            for direction,row in directions.items():
                row['reason']=(f'coverage-{number}-{direction} '+('source evidence remains bounded; '*60))[:1524]
                reasons.append(row['reason'])
            data['compatibility_coverage'].append({'batch':number,'directions':directions})
        with self.assertRaises(ValueError):comments.render_comment(data,REPO)
        parts=comments.render_continuations(data,REPO)
        primary=comments.render_primary(data,REPO,1911,[100+i for i in range(len(parts))])
        self.assertGreater(len(parts),1)
        self.assertTrue(all(len(body.encode())<=comments.MAX_COMMENT_BYTES-comments.WARNING_RESERVE_BYTES for body in [primary,*parts]))
        for reason in reasons:self.assertIn(comments.inline(reason),'\n'.join(parts))
        self.assertEqual(state.parse(parts),self.records)
        self.assertNotIn(reasons[0],primary)

    def test_private_local_history_is_reusable_and_taints_the_successor(self):
        private=copy.deepcopy(self.records)
        for role in ('changed','caller','receiver'):
            for ref in private[0]['finding'][role]:ref['visibility']='private'
        for policy,entries in (('Private test guidance',[fixtures.entry(visibility='private')]),(None,[])):
            meta=api.prepare_review(metadata(),diff().decode(),REPO,entries,private_policy=policy,previous=private)
            self.assertEqual(meta['_compatibility']['previous'],private)
            self.assertEqual(meta['_compatibility']['visibility'],'private')
            with self.assertRaises(compat.ReviewError):compat.public_result({'input_visibility':meta['_compatibility']['visibility']})
        with self.assertRaises(compat.ReviewError):api.validate_prior_state({'version':2,'decisions':[],'compatibility_findings':private})

    def test_unknown_history_is_not_presented_as_a_current_failure(self):
        payload=self.resolved(self.update('not_assessed',[]),self.records)
        body=comments.render_comment(self.report_result(payload,self.records),REPO)
        visible=body.split('<summary>Evidence and affected flow</summary>')[0]
        self.assertNotIn(self.records[0]['finding']['after'],visible)
        self.assertIn('has not been confirmed or cleared',visible)

    def test_structural_source_links_never_invent_line_zero(self):
        ref=copy.deepcopy(next(iter(self.changed.values())))
        ref.update(line=0,end_line=0)
        link=comments.compatibility_code_link(ref)
        self.assertNotIn('#L0',link)
        self.assertIn('/blob/'+ref['revision']+'/',link)

    def test_disagreeing_batches_keep_prior_concern_unassessed(self):
        raw=diff().decode()+diff(path='go/other.go').decode()
        parsed=api.parse_diff(raw)
        meta=api.prepare_review(metadata(),raw,REPO,[fixtures.entry()],previous=self.records)
        batches=api.split_diff(raw,len(diff()))
        def review(batch_meta,source,repo,model):
            row=self.update('addressed') if batch_meta['review_batch']['number']==1 else self.update('not_assessed',[])
            return api.validate_model_response(row,batch_meta['_batch_evidence'],batch_meta['_compatibility'])
        with patch.object(api,'review_model',side_effect=review):
            combined=api.review_batches(meta,batches,REPO,'offline',parsed)
        self.assertEqual(combined['compatibility_updates'][0]['status'],'not_assessed')
        data=self.report_result(self.initial,self.records);data.update(combined)
        self.assertEqual(state.current(data)[0]['status'],'not_assessed')

    def test_compatibility_only_attention_uses_existing_label_without_review_requests(self):
        from api_review_comment_test import REVIEW_DIFF, HEAD, BASE
        data=result();data.update(decisions=[],review_batches=1,input_visibility='public',evidence_catalog={},compatibility_previous=[],compatibility_updates=[])
        parsed=api.parse_diff(REVIEW_DIFF.decode())
        assessment={'revisions':{'repository':REPO,'base':BASE,'head':HEAD},'visibility':'public'}
        changed=api.finding_catalog(api.evidence_catalog(parsed),assessment)
        ref=copy.deepcopy(next(iter(changed.values())))
        finding=dict(self.fixture.finding,changed=[ref],caller=[ref],receiver=[ref])
        data.update(compatibility_verdict='breaking',compatibility_findings=[finding],compatibility_coverage=[{'batch':1,'directions':self.initial['compatibility_coverage']}])
        for row in data['compatibility_coverage'][0]['directions'].values():row['evidence']=[];row['status']='not_assessed'
        data['compatibility_coverage'][0]['directions'][finding['direction']].update(status='partial',evidence=[ref])
        github=MultipartGitHub()
        self.assertTrue(comments.publish(data,REPO,1911,HEAD,BASE,REVIEW_DIFF,github))
        self.assertTrue(any(payload and payload.get('labels')==['api-review'] for _,_,payload in github.mutations()))
        self.assertFalse(any('/requested_reviewers' in path for _,path,_ in github.mutations()))
        previous=state.parse(comments.authoritative_bodies(github.comments))
        data.update(compatibility_findings=[],compatibility_verdict='not_assessed',compatibility_previous=previous,
                    prior_state_sha256=api.prior_state_digest(comments.model_prior_state([],previous)),
                    compatibility_updates=[{'prior_id':previous[0]['id'],'status':'cancelled','reason':'Current source disproves the previous concern','evidence':[ref]}])
        self.assertTrue(comments.publish(data,REPO,1911,HEAD,BASE,REVIEW_DIFF,github))
        self.assertTrue(any(method=='DELETE' and path.endswith('/labels/api-review') for method,path,_ in github.mutations()))
        self.assertFalse(any('/requested_reviewers' in path for _,path,_ in github.mutations()))

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
        bad=copy.deepcopy(self.records);bad[0]['finding']['caller']=[]
        with self.assertRaises(compat.ReviewError):state.validate_records(bad)
        payload=fixtures.response(dict(self.fixture.finding,prior_id='f'*64))
        with self.assertRaises(compat.ReviewError):self.resolved(payload,self.records)

if __name__=='__main__':unittest.main()
