#!/usr/bin/env python3
"""Offline trial boundary/wiring tests; synthetic responses do not prove recall."""
import copy
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import anthropic
import httpx

import compatibility_trial as trial
from api_review_test import finding


class TrialTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.repo = self.root / 'objects'
        self.repo.mkdir()
        self.base, self.head = trial.fixture(self.repo)
        self.meta = {'number': 1, 'title': 'Export options', 'body': ''}

    def prepared(self, name='inputs'):
        raw, source, context, _, _ = trial.prepare_case(self.repo, self.base, self.head, self.meta)
        directory = self.root / name
        with patch.multiple(trial, BASE=self.base, HEAD=self.head, TARGET_METADATA=self.meta,
                            DIFF_HASH=trial.digest(raw), CONTEXT_HASH=trial.digest(trial.encoded(context))):
            digest = trial.prepare(self.repo, directory)
        return directory, digest

    def test_freeze_is_deterministic_and_control_uses_normal_unchanged_context(self):
        first, digest = self.prepared()
        second, other = self.prepared('second')
        self.assertEqual(digest, other)
        manifest = trial.verify(first, digest)
        self.assertEqual(len(manifest['cases']), 2)
        context = json.loads((first / 'case-2.context.json').read_text())
        self.assertEqual({f['path'] for f in context['files']}, {'export_reader.py'})
        self.assertIn('codec.py', (first / 'case-2.diff').read_text())
        self.assertLess(context['bytes'], 48_000)
        request = json.loads((first / 'case-2.request.json').read_text())
        schema = request['output_config']['format']['schema']
        self.assertEqual(schema['required'], ['compatibility_findings'])
        self.assertEqual(set(schema['properties']), {'compatibility_findings'})
        self.assertNotIn('prior_decisions', request['messages'][0]['content'])
        self.assertNotIn('expected_findings', request['messages'][0]['content'])
        self.assertNotIn('tools', request)
        self.assertEqual(request['model'], trial.MODEL)

    def test_fingerprint_drift_and_generated_paths_stop_before_provider_or_blob(self):
        with patch.multiple(trial, BASE=self.base, HEAD=self.head, DIFF_HASH='0' * 64), self.assertRaises(trial.review.ReviewError):
            trial.prepare(self.repo, self.root / 'bad')
        with patch.object(trial, 'is_generated_path', return_value=True), patch.object(trial, 'git', wraps=trial.git) as git:
            with self.assertRaisesRegex(trial.review.ReviewError, 'generated'):
                trial.prepare_case(self.repo, self.base, self.head, self.meta)
            self.assertEqual(git.call_count, 1)
            self.assertIn('--name-only', git.call_args.args)

    def test_all_frozen_files_and_modules_verified_before_either_call(self):
        directory, digest = self.prepared()
        (directory / 'case-2.request.json').write_text('{}')
        from unittest.mock import Mock
        client = Mock()
        with self.assertRaises(trial.review.ReviewError):
            trial.run(directory, digest, client)
        client.messages.create.assert_not_called()

    def flow(self, first_status=200, first_payload=None, stop_reason='end_turn'):
        directory, digest = self.prepared()
        requests = []
        def transport(request):
            value = json.loads(request.content)
            requests.append(value)
            from anthropic.lib._parse._transform import transform_schema
            schema = value['output_config']['format']['schema']
            self.assertEqual(schema, transform_schema(copy.deepcopy(schema)))
            if len(requests) == 1 and first_status != 200:
                return httpx.Response(first_status, json={'type':'error','error':{'type':'api_error','message':'private-sentinel'}})
            payload = {'compatibility_findings': []} if len(requests) > 1 or first_payload is None else first_payload
            return httpx.Response(200, headers={'request-id':'offline-id'}, json={
                'id':'msg_fixture','type':'message','role':'assistant','model':trial.MODEL,
                'stop_reason':stop_reason if len(requests) == 1 else 'end_turn','stop_sequence':None,
                'content':[{'type':'text','text':json.dumps(payload)}],
                'usage':{'input_tokens':123,'output_tokens':17}})
        with httpx.Client(transport=httpx.MockTransport(transport)) as http:
            with anthropic.Anthropic(api_key='offline-fixture-key', http_client=http, max_retries=0) as client:
                code = trial.run(directory, digest, client)
                with self.assertRaises(FileExistsError):
                    trial.run(directory, digest, client)
        self.assertEqual(len(requests), 2)
        return directory, code

    def test_sdk_makes_two_calls_once_and_records_actual_usage_with_no_reconciliation(self):
        directory, code = self.flow()
        self.assertEqual(code, 0)
        result = json.loads((directory / 'case-1.result.json').read_text())
        self.assertEqual(result['usage']['input_tokens'], 123)
        self.assertIsNone(result['billed_cost_usd'])
        self.assertEqual(result['provider_request_id'], 'offline-id')
        self.assertTrue((directory / 'case-1.provider-response.json').is_file())

    def test_provider_errors_truncation_and_bad_schema_never_retry_or_report_clean(self):
        for kwargs in ({'first_status':500}, {'stop_reason':'max_tokens'},
                       {'first_payload':{'compatibility_findings':[], 'decisions':[]}}):
            with self.subTest(kwargs=kwargs):
                # Separate temporary input directories for independent offline flows.
                directory, code = self.flow(**kwargs)
                self.assertEqual(code, 1)
                report = (directory / 'case-1.report.md').read_text()
                self.assertNotIn('No concrete failure identified', report)
                self.assertNotIn('private-sentinel', report)
                self.assertEqual(json.loads((directory / 'case-2.result.json').read_text())['status'], 'complete')
                import shutil
                shutil.rmtree(directory)

    def test_resolved_changed_evidence_uses_existing_validator_and_renderer(self):
        directory, _ = self.prepared()
        source = (directory / 'case-1.diff').read_text()
        evidence = trial.review.evidence_catalog(trial.review.parse_diff(source))
        identifier = next(iter(evidence))
        import shutil
        shutil.rmtree(directory)
        directory, code = self.flow(first_payload={'compatibility_findings':[finding(locations=[{'evidence_id':identifier}])]})
        self.assertEqual(code, 0)
        self.assertIn('Legacy export reader', (directory / 'case-1.report.md').read_text())

    def test_workflow_has_only_manual_read_only_trial_and_skips_normal_candidate(self):
        workflow = Path(__file__).resolve().parents[1] / 'workflows/api-review.yml'
        text = workflow.read_text()
        job = text.split('  compatibility-only-trial:', 1)[1]
        self.assertIn("github.event_name == 'workflow_dispatch'", job)
        self.assertIn('github.run_attempt == 1', job)
        self.assertIn('needs: validate-candidate-tests', job)
        self.assertIn('contents: read', job)
        self.assertNotIn(': write', job)
        self.assertNotIn('GH_TOKEN', job)
        self.assertNotIn('publish --', job)
        self.assertIn("!inputs.compatibility_only_trial", text.split('  validate-candidate:', 1)[1].split('  compatibility-only-trial:', 1)[0])
        before_key = job.split('ANTHROPIC_API_KEY:', 1)[0]
        self.assertIn("verify(Path(sys.argv[1]), os.environ['FROZEN_TRIAL_SHA256'])", before_key)


if __name__ == '__main__':
    unittest.main()
