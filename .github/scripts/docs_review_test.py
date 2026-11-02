#!/usr/bin/env python3
"""Offline documentation-review coverage, budget and workflow regressions."""
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


def response(payload='NO_CHANGES_NEEDED', reason='end_turn'):
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
        client.messages.create.side_effect = [response(), response(json.dumps({'findings': [finding, finding]}))]
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
