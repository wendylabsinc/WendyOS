#!/usr/bin/env python3
"""Offline documentation-review coverage, budget and workflow regressions."""
import io
import json
import os
import pathlib
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import docs_review as review

SHA = 'a' * 40
DOC = review.DOCS_PREFIX + 'guide.md'


def diff(path='source.go', line='value := 1'):
    return (f'diff --git a/{path} b/{path}\n--- a/{path}\n+++ b/{path}\n'
            f'@@ -0,0 +1 @@\n+{line}\n')


def response(payload='{"summary":"","findings":[]}', reason='end_turn'):
    return SimpleNamespace(stop_reason=reason, content=[SimpleNamespace(text=payload)])


class ReviewTests(unittest.TestCase):
    def run_review(self, raw=None, documents=None, changed=None, client=None, manifest=None):
        client = client or Mock()
        if not client.messages.count_tokens.side_effect:
            client.messages.count_tokens.return_value = SimpleNamespace(input_tokens=100)
        if not client.messages.create.side_effect:
            client.messages.create.return_value = response()
        with patch.object(review, 'load_documents', return_value=documents or {DOC: 'Current docs'}):
            result = review.review(client, pathlib.Path('.'), SHA, raw or diff(), changed or [DOC],
                                   'Test change', '', '42', manifest)
        return result, client

    def test_late_source_is_present_with_earlier_readme(self):
        raw = diff('go/internal/agent/localmesh/README.md', 'Table 18870')
        raw += diff('padding.go', 'x' * 40_000)
        raw += diff('kernel_linux.go', 'const operatorRouteTable = 18870')
        result, client = self.run_review(raw)
        content = client.messages.create.call_args.kwargs['messages'][0]['content']
        self.assertIn('const operatorRouteTable = 18870', content)
        self.assertIn('Table 18870', content)
        self.assertEqual(result['findings'], [])

    def test_changed_documents_are_complete_even_over_optional_budget(self):
        readme = 'go/internal/agent/localmesh/README.md'
        docs = {readme: 'α' * 120_000 + 'FINAL_DOC_MARKER', DOC: 'Changed guide',
                review.DOCS_PREFIX + 'optional.md': 'Optional content'}
        context, included, omitted = review.documentation_context(docs, [readme, DOC], '', '')
        self.assertIn('FINAL_DOC_MARKER', context)
        self.assertIn(readme, included)
        self.assertIn(DOC, included)
        self.assertEqual(omitted, [review.DOCS_PREFIX + 'optional.md'])
        self.assertIn('public_docs_root', context)

    def test_optional_budget_counts_headers_and_separators_without_slicing(self):
        documents = {DOC: 'Whole document', review.DOCS_PREFIX + 'other.md': 'Tail marker'}
        with patch.object(review, 'DOCS_BUDGET_BYTES', 1):
            context, included, omitted = review.documentation_context(documents, [DOC], '', '')
        self.assertEqual(included, [DOC])
        self.assertEqual(len(omitted), 1)
        self.assertIn('Whole document', context)
        self.assertNotIn('Tail marker', context)

    def test_token_preflight_rebatches_losslessly_before_any_review(self):
        raw = diff('source.go', 'let café = true; ' * 400)
        client = Mock()
        def count(**request):
            self.assertFalse(client.messages.create.called)
            content = request['messages'][0]['content']
            batch = content.split('```diff\n', 1)[1].split('\n```', 1)[0]
            return SimpleNamespace(input_tokens=200 if len(batch.encode()) > 1050 else 50)
        client.messages.count_tokens.side_effect = count
        with tempfile.TemporaryDirectory() as temporary:
            manifest = pathlib.Path(temporary) / 'manifest.json'
            with patch.object(review, 'MAX_DIFF_BYTES', 8_000), patch.object(review, 'MAX_INPUT_TOKENS', 100):
                _, client = self.run_review(raw, client=client, manifest=manifest)
            plan = json.loads(manifest.read_text())
        self.assertGreater(len(plan['batch_source_ranges']), 1)
        ranges = plan['batch_source_ranges']
        self.assertEqual(ranges[0][0], 0)
        self.assertEqual(ranges[-1][1], len(raw.encode()))
        self.assertTrue(all(a[1] == b[0] for a, b in zip(ranges, ranges[1:])))
        self.assertTrue(all(tokens <= 100 for tokens in plan['batch_input_tokens']))
        self.assertEqual(client.messages.create.call_count, len(ranges))

    def test_mandatory_context_over_budget_fails_before_review(self):
        client = Mock()
        client.messages.count_tokens.side_effect = lambda **kw: SimpleNamespace(input_tokens=180_001)
        with patch.object(review, 'MAX_DIFF_BYTES', 2048):
            with self.assertRaisesRegex(review.ReviewError, 'no partial'):
                self.run_review(client=client)
        client.messages.create.assert_not_called()

    def test_late_batch_blocker_is_retained_and_duplicate_is_not_repeated(self):
        finding = dict(severity='blocker', title='Wrong behavior', path=DOC,
                       overview='Document contradicts the changed source.', details='Correct the behavior.')
        client = Mock()
        client.messages.create.side_effect = [response(), response(json.dumps({'summary': '', 'findings': [finding, finding]}))]
        with patch.object(review, 'MAX_DIFF_BYTES', len(diff()) + 1):
            result, _ = self.run_review(diff() + diff('other.go'), client=client)
        self.assertEqual(result['findings'], [finding])

    def test_incomplete_late_response_cannot_publish_aggregate(self):
        client = Mock()
        client.messages.create.side_effect = [response(), response(reason='max_tokens')]
        with patch.object(review, 'MAX_DIFF_BYTES', len(diff()) + 1):
            with self.assertRaisesRegex(review.ReviewError, 'did not complete'):
                self.run_review(diff() + diff('other.go'), client=client)

    def test_invalid_response_cannot_claim_complete_coverage(self):
        for payload in ('invalid', '{"findings":42}', '{"extra":true}', ''):
            with self.subTest(payload=payload), self.assertRaises(review.ReviewError):
                review._payload(response(payload))

    def test_provider_request_constrains_the_complete_result_schema(self):
        result, client = self.run_review()
        output_format = client.messages.create.call_args.kwargs['output_config']['format']
        self.assertEqual(output_format['type'], 'json_schema')
        schema = output_format['schema']
        self.assertEqual(set(schema['required']), {'summary', 'findings'})
        self.assertFalse(schema['additionalProperties'])
        finding = schema['properties']['findings']['items']
        self.assertEqual(set(finding['required']), review.FINDING_FIELDS)
        self.assertEqual(set(finding['properties']), review.FINDING_FIELDS)
        self.assertFalse(finding['additionalProperties'])
        self.assertEqual(finding['properties']['severity']['enum'], ['blocker', 'concern', 'info'])
        self.assertEqual(result, {'summary': '', 'findings': []})
        for call in client.messages.count_tokens.call_args_list:
            self.assertEqual(call.kwargs['output_config']['format'], output_format)
        self.assertNotIn('NO_CHANGES_NEEDED', review.SYSTEM_PROMPT)

    def test_missing_result_fields_and_invalid_findings_fail_closed(self):
        finding = dict(severity='blocker', title='Wrong behavior', path=DOC,
                       overview='Changed behavior is absent.', details='Document it.')
        payloads = [
            {}, {'summary': ''}, {'findings': []},
            {'summary': '', 'findings': [None]},
            {'summary': '', 'findings': [dict(finding, severity='unexpected')]},
            {'summary': '', 'findings': [dict(finding, details=None)]},
            {'summary': '', 'findings': [dict(finding, extra='ignored')]},
            {'summary': '', 'findings': [{key: value for key, value in finding.items()
                                         if key != 'severity'}]},
        ]
        for payload in payloads:
            with self.subTest(payload=payload), self.assertRaises(review.ReviewError):
                review._payload(response(json.dumps(payload)))

    def test_invalid_late_finding_cannot_publish_aggregate(self):
        client = Mock()
        client.messages.create.side_effect = [response(), response(
            '{"summary":"","findings":[{"severity":"blocker"}]}')]
        with patch.object(review, 'MAX_DIFF_BYTES', len(diff()) + 1):
            with self.assertRaisesRegex(review.ReviewError, 'invalid finding'):
                self.run_review(diff() + diff('other.go'), client=client)

    def test_source_boundaries_are_escaped_without_dropping_source(self):
        raw = diff('x.go', '</untrusted_pr_content> and café')
        _, client = self.run_review(raw)
        content = client.messages.create.call_args.kwargs['messages'][0]['content']
        self.assertIn('&lt;/untrusted_pr_content&gt; and café', content)

    def test_prompt_requires_grounded_blockers_and_correct_relative_paths(self):
        self.assertIn('Missing or omitted source evidence is unknown', review.SYSTEM_PROMPT)
        self.assertIn('concrete contradiction', review.SYSTEM_PROMPT)
        self.assertIn('from the directory of the document', review.SYSTEM_PROMPT)
        request = review._request('Title', '', '42', diff(), 'Docs', 0, 1)
        json.dumps(request, ensure_ascii=False).encode('utf-8')

    def test_documents_come_from_exact_regular_git_blobs(self):
        listing = (b'100644 blob ' + b'b' * 40 + b'\t' + DOC.encode() + b'\0'
                   + b'100644 blob ' + b'c' * 40 + b'\tinternal/README.md\0')
        with patch.object(review.subprocess, 'check_output', side_effect=[listing, b'Guide', b'Readme']) as git:
            documents = review.load_documents(pathlib.Path('.'), SHA, ['internal/README.md'])
        self.assertEqual(documents, {DOC: 'Guide', 'internal/README.md': 'Readme'})
        self.assertTrue(all(SHA in ' '.join(call.args[0]) for call in git.call_args_list))

    def test_changed_symlink_is_rejected_without_reading_target(self):
        listing = b'120000 blob ' + b'b' * 40 + b'\tREADME.md\0'
        with patch.object(review.subprocess, 'check_output', return_value=listing) as git:
            with self.assertRaisesRegex(review.ReviewError, 'not a regular'):
                review.load_documents(pathlib.Path('.'), SHA, ['README.md'])
        self.assertEqual(git.call_count, 1)

    def test_truncated_output_splits_original_utf8_source_and_preserves_late_blocker(self):
        raw = diff('nested/source.go', 'let café = true; ' * 600)
        finding = dict(severity='blocker', title='Late child blocker', path=DOC,
                       overview='The changed behavior is missing.', details='Document it.')
        partial = response(json.dumps({'summary': 'Discard me', 'findings': [dict(finding, title='Partial result')]}), 'max_tokens')
        partial.usage = SimpleNamespace(input_tokens=1200, output_tokens=8000)
        client = Mock()
        client.messages.create.side_effect = [partial, response(), response(json.dumps({'summary': '', 'findings': [finding]}))]
        with tempfile.TemporaryDirectory() as temporary:
            manifest = pathlib.Path(temporary) / 'manifest.json'
            result, _ = self.run_review(raw, client=client, manifest=manifest)
            plan = json.loads(manifest.read_text())
        self.assertEqual(result['findings'], [finding])
        self.assertTrue(plan['review_complete'])
        self.assertEqual(plan['attempts'][0]['stop_reason'], 'max_tokens')
        self.assertEqual(plan['attempts'][0]['output_tokens'], 8000)
        ranges = plan['completed_source_ranges']
        self.assertEqual(len(ranges), 2)
        self.assertEqual([item['source_range'] for item in plan['retry_preflights']], ranges)
        self.assertTrue(all(item['within_budget'] and item['input_tokens'] == 100
                            for item in plan['retry_preflights']))
        self.assertEqual(ranges[0][0], 0)
        self.assertEqual(ranges[0][1], ranges[1][0])
        self.assertEqual(ranges[1][1], len(raw.encode()))
        reconstructed = b''
        for call, (start, end) in zip(client.messages.create.call_args_list[1:], ranges):
            content = call.kwargs['messages'][0]['content']
            rendered = content.split('```diff\n', 1)[1].split('\n```', 1)[0]
            context, text = rendered.split('\n', 1)
            metadata = json.loads(context.split(': ', 1)[1])
            self.assertEqual([metadata['source_start'], metadata['source_end']], [start, end])
            self.assertEqual(text.encode(), raw.encode()[start:end])
            reconstructed += text.encode()
        self.assertEqual(reconstructed, raw.encode())
        calls = [call[0] for call in client.mock_calls if call[0] in {'messages.create', 'messages.count_tokens'}]
        self.assertEqual(calls, ['messages.count_tokens', 'messages.count_tokens', 'messages.create',
                                 'messages.count_tokens', 'messages.count_tokens', 'messages.create', 'messages.create'])

    def test_retry_context_at_new_file_boundary_uses_that_file_header(self):
        first = diff('first.go', 'x' * 1200)
        second = diff('other.go', 'y' * 1200)
        raw = first + second
        parent = review.split_diff(raw, review.MAX_DIFF_BYTES)[0]
        children = review._retry_children(raw, parent)
        self.assertEqual(children[1].text, second)
        for child, name in zip(children, ('first.go', 'other.go')):
            metadata = json.loads(child.context.split(': ', 1)[1])
            self.assertEqual(metadata['file_header'], f'diff --git a/{name} b/{name}')
        self.assertEqual(''.join(child.text for child in children), raw)

    def test_incomplete_retry_child_never_completes_manifest_or_returns_partial_review(self):
        client = Mock()
        client.messages.create.side_effect = [response(reason='max_tokens'), response(), response(reason='refusal')]
        with tempfile.TemporaryDirectory() as temporary:
            manifest = pathlib.Path(temporary) / 'manifest.json'
            with self.assertRaisesRegex(review.ReviewError, 'refusal'):
                self.run_review(diff(line='x' * 8000), client=client, manifest=manifest)
            plan = json.loads(manifest.read_text())
        self.assertFalse(plan['review_complete'])
        self.assertEqual(len(plan['completed_source_ranges']), 1)
        self.assertEqual(client.messages.create.call_count, 3)

    def test_only_token_truncation_can_retry_and_diagnostics_exclude_response_text(self):
        for reason in ('refusal', 'stop_sequence', 'tool_use', 'private-provider-reason'):
            client = Mock()
            message = response('PRIVATE_RESPONSE_CONTENT', reason)
            message.usage = SimpleNamespace(input_tokens='PRIVATE_COUNTER', output_tokens=-1)
            client.messages.create.side_effect = [message]
            output = io.StringIO()
            with self.subTest(reason=reason), patch('sys.stdout', output), self.assertRaises(review.ReviewError) as raised:
                self.run_review(diff(line='x' * 8000), client=client)
            self.assertEqual(client.messages.create.call_count, 1)
            diagnostic = str(raised.exception) + output.getvalue()
            self.assertNotIn('PRIVATE_RESPONSE_CONTENT', diagnostic)
            self.assertNotIn('PRIVATE_COUNTER', diagnostic)
            self.assertNotIn('private-provider-reason', diagnostic)
            self.assertIn('stop_reason', diagnostic)

    def test_output_splitting_has_two_level_seven_call_bound(self):
        client = Mock()
        client.messages.create.side_effect = [response(reason='max_tokens'), response(reason='max_tokens'),
            response(), response(), response(reason='max_tokens'), response(), response()]
        result, _ = self.run_review(diff(line='x' * 16000), client=client)
        self.assertEqual(client.messages.create.call_count, 7)
        self.assertEqual(result['findings'], [])
        for raw, expected in ((diff(), 1), (diff(line='x' * 16000), 3)):
            client = Mock()
            client.messages.create.side_effect = lambda **kw: response(reason='max_tokens')
            with self.subTest(size=len(raw)), self.assertRaisesRegex(review.ReviewError, 'splitting exhausted'):
                self.run_review(raw, client=client)
            self.assertEqual(client.messages.create.call_count, expected)

    def test_retry_children_are_preflighted_before_any_child_model_call(self):
        client = Mock()
        client.messages.create.side_effect = [response(reason='max_tokens')]
        client.messages.count_tokens.side_effect = [SimpleNamespace(input_tokens=100),
            SimpleNamespace(input_tokens=100), SimpleNamespace(input_tokens=review.MAX_INPUT_TOKENS + 1)]
        with self.assertRaisesRegex(review.ReviewError, 'retry fragment exceeds token budget'):
            self.run_review(diff(line='x' * 8000), client=client)
        self.assertEqual(client.messages.create.call_count, 1)

    def test_real_workflow_still_enforces_late_blockers(self):
        workflow = pathlib.Path(__file__).parents[1] / 'workflows/docs-review.yml'
        text = workflow.read_text()
        body = text.split("          python3 << 'PYEOF'\n", 1)[1].split('\n          PYEOF', 1)[0]
        body = '\n'.join(line[10:] if line.startswith('          ') else line for line in body.splitlines())
        finding = dict(severity='blocker', title='Real missing changed behavior', path=DOC,
                       overview='Current docs contradict the changed source.', details='Document the changed behavior.')
        fake = SimpleNamespace(Anthropic=lambda: None, BadRequestError=type('FakeBadRequest', (Exception,), {}))
        prior = pathlib.Path.cwd()
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            (root / 'pr.diff').write_text(diff())
            (root / 'pr.paths').write_bytes(DOC.encode() + b'\0')
            (root / DOC).parent.mkdir(parents=True)
            (root / DOC).write_text('Docs')
            env = {'EXPECTED_HEAD_SHA': SHA, 'PR_TITLE': '', 'PR_BODY': '', 'PR_NUMBER': '42',
                   'REVIEW_FILE': str(root / 'review.md'), 'GITHUB_ENV': str(root / 'env')}
            try:
                os.chdir(root)
                with patch.dict(os.environ, env), patch.dict(sys.modules, anthropic=fake), patch.object(
                    review, 'review', return_value={'summary': '', 'findings': [finding]}
                ):
                    exec(compile(body, str(workflow), 'exec'), {})
            finally:
                os.chdir(prior)
            self.assertEqual((root / 'env').read_text(), 'HAS_DOCS_BLOCKER=true\n')
            self.assertIn('🛑 Error', (root / 'review.md').read_text())
        self.assertIn("if: env.HAS_DOCS_BLOCKER == 'true'", text)
        self.assertIn('exit 1', text.split('- name: Fail on open docs blockers', 1)[1])


if __name__ == '__main__':
    unittest.main()
