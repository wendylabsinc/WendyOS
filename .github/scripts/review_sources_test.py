#!/usr/bin/env python3
"""Offline regressions for source-only inputs and generated-only state retention."""
import argparse
from contextlib import contextmanager
import hashlib
import json
import os
from pathlib import Path
import re
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import review_sources as source
import api_review
import api_review_comment as comments
import docs_review
import security_review

ROOT = Path(__file__).resolve().parents[2]
GENERATED = 'go/proto/gen/cloudpb/v2/tunnel.pb.go'
SWIFT = 'swift/WendyAgentCore/Sources/WendyCloudGRPC/Proto/cloud/tunnel.grpc.swift'
HEAD, BASE = 'a' * 40, 'b' * 40
REPO = 'wendylabsinc/WendyOS'


def diff(path=GENERATED, line='GENERATED_PAYLOAD_DO_NOT_SEND'):
    return (f'diff --git a/{path} b/{path}\n--- a/{path}\n+++ b/{path}\n'
            f'@@ -0,0 +1 @@\n+{line}\n')


def metadata(raw):
    parsed = source.parse_diff(raw)
    return dict(number=42, title='Update external contracts', body='source: service-protos',
                head={'sha': HEAD, 'repo': {'full_name': REPO}}, base={'sha': BASE},
                diff_base_sha=BASE, **{k: parsed[k] for k in ('changed_files', 'additions', 'deletions')})


@contextmanager
def working_directory(path):
    previous = Path.cwd()
    os.chdir(path)
    try:
        yield
    finally:
        os.chdir(previous)


def inline(workflow, index):
    text = (ROOT / '.github/workflows' / workflow).read_text()
    blocks = re.findall(r"          python3 << 'PYEOF'\n(.*?)          PYEOF", text, re.S)
    return compile('\n'.join(line[10:] for line in blocks[index].splitlines()), workflow, 'exec')


class PolicyTests(unittest.TestCase):
    def test_actual_generator_outputs_only(self):
        for path in (GENERATED, SWIFT, 'go/proto/gen/agentpb/v2/service_grpc.pb.go',
                     'swift/WendyAgentCore/Sources/OpenTelemetryGRPC/Proto/logs.pb.swift',
                     'Examples/WendyNotify/Sources/WendyNotify/Generated/wendy_system_v1_notifications.pb.swift'):
            self.assertTrue(source.is_generated_path(path), path)
        for path in ('go/proto/gen/README.md', 'go/proto/gen/new.proto', 'go/proto/gen/go.sum',
                     'go/proto/gen/generate.sh', 'go/go.sum', 'vendor/important.go',
                     'untrusted/example.pb.go', 'swift/Sources/Untrusted/Proto/foo.pb.swift',
                     'Proto/cloud/tunnel.proto', 'go/scripts/generate-proto.sh',
                     'swift/Scripts/GenerateProto.sh', 'go/proto/service-protos.version',
                     'go/proto/gen/../hidden.pb.go'):
            self.assertFalse(source.is_generated_path(path), path)

    def test_thermal_notify_exact_outputs_excluded_and_source_kept(self):
        root = 'Examples/ThermalNotify/Sources/ThermalNotify/Generated/'
        outputs = [root + name + suffix for name in (
            'wendy_system_v1_notifications',
            'wendy_agent_services_v1_wendy_agent_v1_video_service',
        ) for suffix in ('.pb.swift', '.grpc.swift')]
        generated = ''.join(diff(path) for path in outputs)
        handwritten = ''.join(diff(path, '// Code generated. SOURCE_MARKER') for path in (
            root + 'Handwritten.swift', root + 'unknown.pb.swift', root + 'notes.proto',
            root + 'nested/wendy_system_v1_notifications.pb.swift',
            'Examples/Other/Sources/Other/Generated/wendy_system_v1_notifications.pb.swift',
            'Examples/ThermalNotify/regenerate-grpc.sh',
            'Examples/ThermalNotify/Package.swift', 'Examples/ThermalNotify/Package.resolved',
            'Examples/ThermalNotify/build.stagefile.lock.yaml',
            'Proto/wendy/system/v1/notifications.proto',
            'Proto/wendy/agent/services/v1/wendy_agent_v1_video_service.proto',
        ))
        kept, coverage = source.source_diff(generated + handwritten)
        self.assertEqual(kept, handwritten)
        self.assertEqual([item['path'] for item in coverage['excluded_generated_files']], outputs)
        kept, coverage = source.source_diff(generated)
        self.assertEqual(kept, '')
        self.assertEqual(coverage['source_files'], 0)
        self.assertEqual(len(coverage['excluded_generated_files']), 4)

    def test_thermal_notify_boundary_renames_and_modes_remain_reviewable(self):
        output = 'Examples/ThermalNotify/Sources/ThermalNotify/Generated/wendy_system_v1_notifications.pb.swift'
        handwritten = 'Examples/ThermalNotify/Sources/ThermalNotify/Notifications.swift'
        for old, new in ((output, handwritten), (handwritten, output)):
            raw = f'diff --git a/{old} b/{new}\nsimilarity index 100%\nrename from {old}\nrename to {new}\n'
            self.assertEqual(source.source_diff(raw)[0], raw)
        raw = f'diff --git a/{output} b/{output}\nold mode 100644\nnew mode 100755\n'
        self.assertEqual(source.source_diff(raw)[0], raw)

    def test_mixed_exact_bytes_accounting_and_no_header_policy(self):
        pin = diff('go/scripts/generate-proto.sh', 'SCHEMA_REVISION=upstream-commit')
        unknown = diff('custom/generated.go', '// Code generated. DO NOT EDIT. SOURCE_MARKER')
        raw = diff() + pin + diff(SWIFT) + unknown
        kept, coverage = source.source_diff(raw)
        self.assertEqual(kept, pin + unknown)
        self.assertEqual(coverage['source_files'], 2)
        self.assertEqual(coverage['source_bytes'] + coverage['excluded_generated_bytes'], len(raw.encode()))
        self.assertEqual(coverage['diff_sha256'], hashlib.sha256(raw.encode()).hexdigest())

    def test_renames_and_copies_across_boundary_are_reviewed_both_ways(self):
        for old, new in ((GENERATED, 'go/handwritten.go'), ('go/handwritten.go', GENERATED),
                         (GENERATED, SWIFT)):
            for action in ('rename', 'copy'):
                raw = f'diff --git a/{old} b/{new}\nsimilarity index 100%\n{action} from {old}\n{action} to {new}\n'
                kept, _ = source.source_diff(raw)
                self.assertEqual(bool(kept), not all(source.is_generated_path(p) for p in (old, new)))

    def test_quoted_utf8_generated_paths_and_deletions(self):
        raw = diff('go/proto/gen/café.pb.go')
        quoted = raw.replace('a/go/proto/gen/café.pb.go', '"a/go/proto/gen/caf\\303\\251.pb.go"').replace(
            'b/go/proto/gen/café.pb.go', '"b/go/proto/gen/caf\\303\\251.pb.go"')
        self.assertEqual(source.source_diff(quoted)[0], '')
        deleted = (f'diff --git a/{GENERATED} b/{GENERATED}\ndeleted file mode 100644\n'
                   f'--- a/{GENERATED}\n+++ /dev/null\n@@ -1 +0,0 @@\n-old\n')
        self.assertEqual(source.source_diff(deleted)[0], '')

    def test_modes_symlinks_submodules_remain_reviewable(self):
        for records in ('old mode 100644\nnew mode 100755\n',
                        'index 1111111..2222222 120000\n', 'new file mode 160000\n'):
            raw = f'diff --git a/{GENERATED} b/{GENERATED}\n' + records
            self.assertEqual(source.source_diff(raw)[0], raw)

    def test_invalid_full_input_is_rejected_before_exclusion(self):
        for raw in (diff().replace('@@ -0,0 +1 @@', '@@ -0,0 +1,2 @@'),
                    diff().replace('+++ b/' + GENERATED, '+++ b/go/hidden.go'),
                    diff() + diff(), 'untrusted preamble\n' + diff()):
            with self.assertRaises(source.ReviewError):
                source.source_diff(raw)
        raw = diff()
        meta = metadata(raw)
        meta['additions'] += 1
        with self.assertRaisesRegex(security_review.ReviewError, 'Changed-line'):
            security_review.build_input_manifest(meta, raw.encode(), 42, HEAD)

    def test_prompts_state_upstream_inspection_limit_and_no_generated_review(self):
        for prompt in (api_review.system_prompt(), security_review.system_prompt(), docs_review.SYSTEM_PROMPT):
            self.assertIn('Do not review generated code', prompt)
            self.assertIn('upstream source is not supplied', prompt)
            self.assertIn('do not establish API or behavior coverage', prompt)


class ReviewInputTests(unittest.TestCase):
    def run_api(self, raw, previous=None):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            (path / 'meta').write_text(json.dumps(metadata(raw)))
            (path / 'diff').write_text(raw)
            args = argparse.Namespace(metadata=str(path / 'meta'), diff=str(path / 'diff'), previous=None,
                repo=REPO, pr_number=42, expected_head_sha=HEAD, expected_base_sha=BASE,
                model='offline', output=str(path / 'result'))
            with patch.object(api_review, 'review_model', return_value={'risk': 'mid', 'decisions': []}) as model:
                code = api_review.command_review(args)
            return code, json.loads((path / 'result').read_text()), model

    def test_api_source_only_and_external_pin_stays_in_model_input(self):
        pin = diff('go/scripts/generate-proto.sh', 'SCHEMA_REVISION=upstream-commit')
        code, result, model = self.run_api(diff() + pin)
        self.assertEqual(code, 0)
        self.assertEqual(result['status'], 'complete')
        self.assertNotIn('GENERATED_PAYLOAD_DO_NOT_SEND', str(model.call_args))
        self.assertIn('SCHEMA_REVISION=upstream-commit', str(model.call_args))
        self.assertEqual(result['source_coverage']['source_files'], 1)
        self.assertIn('not AI-reviewed', '\n'.join(comments.review_intro(result, REPO)))

    def test_api_generated_only_no_model_and_no_publication_mutations(self):
        code, result, model = self.run_api(diff())
        self.assertEqual(code, 0)
        self.assertEqual(result['status'], 'skipped')
        model.assert_not_called()
        github = Mock()
        github.request.return_value = dict(head={'sha': HEAD, 'repo': {'full_name': REPO}},
                                           base={'sha': BASE}, state='open')
        self.assertTrue(comments.publish(result, REPO, 42, HEAD, BASE, diff().encode(), github))
        self.assertEqual([call.args[0] for call in github.request.call_args_list], ['GET'])
        with self.assertRaises(ValueError):
            comments.publish(result, REPO, 42, HEAD, BASE, diff('source.go').encode(), github)

    def test_security_mixed_no_generated_model_payload(self):
        raw = diff() + diff('Proto/cloud/service.proto', 'message Contract {}')
        manifest = security_review.build_input_manifest(metadata(raw), raw.encode(), 42, HEAD)
        client = Mock()
        client.messages.create.return_value = SimpleNamespace(stop_reason='end_turn', content=[
            SimpleNamespace(text=json.dumps({'summary': '', 'findings': [], 'compliance_summary': ''}))])
        security_review.review_batches(client, 'offline', metadata(raw), raw, '', manifest)
        self.assertNotIn('GENERATED_PAYLOAD_DO_NOT_SEND', str(client.messages.create.call_args))
        self.assertIn('message Contract {}', str(client.messages.create.call_args))

    def test_security_generated_only_preserves_previous_block(self):
        raw = diff()
        manifest = security_review.build_input_manifest(metadata(raw), raw.encode(), 42, HEAD)
        self.assertEqual(manifest['batch_count'], 0)
        fake = Mock()
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            for name, data in [('meta', json.dumps(metadata(raw))), ('manifest', json.dumps(manifest)),
                               ('diff', raw), ('previous', security_review.BLOCKING_MARKER + '\nPRIOR_BLOCKER')]:
                (path / name).write_text(data)
            args = argparse.Namespace(metadata=path/'meta', manifest=path/'manifest', diff=path/'diff',
                previous=path/'previous', model='offline', review_output=path/'review', result_output=path/'result')
            with patch.dict(sys.modules, {'anthropic': fake}):
                security_review.command_review(args)
            fake.Anthropic.assert_not_called()
            self.assertIn('PRIOR_BLOCKER', (path/'review').read_text())
            self.assertTrue(json.loads((path/'result').read_text())['has_blocking'])
            self.assertIn('No AI review was performed', (path/'review').read_text())

    def test_docs_generated_only_no_model_or_context_calls(self):
        client = Mock()
        with patch.object(docs_review, 'load_documents') as load:
            result = docs_review.review(client, ROOT, HEAD, diff(), [GENERATED], '', '', '42')
        load.assert_not_called()
        client.messages.count_tokens.assert_not_called()
        client.messages.create.assert_not_called()
        self.assertIn('no AI review', result['summary'])

    def test_docs_mixed_filters_full_context_as_well_as_patch(self):
        client = Mock()
        client.messages.count_tokens.return_value = SimpleNamespace(input_tokens=100)
        client.messages.create.return_value = SimpleNamespace(stop_reason='end_turn', content=[
            SimpleNamespace(text='{"summary":"","findings":[]}')])
        with patch.object(docs_review, 'load_documents', return_value={GENERATED: 'GENERATED_CONTEXT', 'README.md': 'SOURCE_DOC'}):
            docs_review.review(client, ROOT, HEAD, diff() + diff('source.go', 'SOURCE_CODE'),
                               [GENERATED, 'source.go'], '', '', '42')
        request = str(client.messages.create.call_args)
        self.assertNotIn('GENERATED_CONTEXT', request)
        self.assertNotIn('GENERATED_PAYLOAD_DO_NOT_SEND', request)
        self.assertIn('SOURCE_CODE', request)
        self.assertIn('SOURCE_DOC', request)


class WorkflowTests(unittest.TestCase):
    def integration(self, raw, response_text="NO_CHANGES_NEEDED"):
        fake = Mock()
        fake.Anthropic.return_value.messages.create.return_value = SimpleNamespace(
            stop_reason='end_turn', content=[SimpleNamespace(text=response_text)])
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            (path / '.github/scripts').mkdir(parents=True)
            (path / '.github/scripts/review_sources.py').touch()
            (path / 'pr.diff').write_text(raw)
            (path / 'pr_meta.json').write_text(json.dumps(metadata(raw)))
            env = dict(GITHUB_ENV=str(path/'env'), REVIEW_FILE=str(path/'review.md'),
                       EXPECTED_HEAD_SHA=HEAD, PR_TITLE='Test', PR_BODY='', PR_NUMBER='42')
            with working_directory(path), patch.dict(os.environ, env), patch.dict(sys.modules, {'anthropic': fake}), \
                 patch('subprocess.check_output', return_value=b''):
                try:
                    exec(inline('integration-test-review.yml', 0), {})
                except SystemExit as error:
                    self.assertEqual(error.code, 0)
            fake.review_body = (path/'review.md').read_text() if (path/'review.md').exists() else ''
            return fake

    def test_integration_mixed_filters_before_model(self):
        fake = self.integration(diff() + diff('source.go', 'SOURCE_CODE'))
        request = str(fake.Anthropic.return_value.messages.create.call_args)
        self.assertNotIn('GENERATED_PAYLOAD_DO_NOT_SEND', request)
        self.assertIn('SOURCE_CODE', request)
        self.assertIn('Do not review generated code', request)

    def test_integration_generated_only_no_model(self):
        self.integration(diff()).Anthropic.assert_not_called()

    def test_integration_large_source_retains_excerpt_budget_and_discloses_omission(self):
        fake = self.integration(diff('source.go', 'X' * 30000 + 'OMITTED_SOURCE_MARKER'))
        request = str(fake.Anthropic.return_value.messages.create.call_args)
        self.assertIn('Partial source review', request)
        self.assertIn('Omitted source is unreviewed', request)
        self.assertNotIn('OMITTED_SOURCE_MARKER', request)
        fake.Anthropic.return_value.messages.create.assert_called_once()
        payload = {'summary': '', 'findings': [{'severity': 'info', 'title': 'Source test',
            'name': 'python-source', 'overview': 'Source-only suggestion', 'details': 'Complete details'}]}
        fake = self.integration(diff('source.go', 'é' * 30000 + 'OMITTED_SOURCE_MARKER'), json.dumps(payload))
        self.assertIn('Partial source review', fake.review_body)
        self.assertIn('Omitted source is unreviewed', fake.review_body)
        self.assertNotIn('OMITTED_SOURCE_MARKER', str(fake.Anthropic.return_value.messages.create.call_args))

    def test_skip_preserves_prior_docs_and_integration_comments_and_blockers(self):
        for workflow, marker in [('docs-review.yml', '<!-- ai-docs-review:v1 -->'),
                                  ('integration-test-review.yml', '<!-- ai-integration-test-review:v1 -->')]:
            for blocking, excerpted in ((False, False), (True, False), (False, True), (True, True)):
                if excerpted and workflow != 'integration-test-review.yml':
                    continue
                body = marker + ('\n#### 🛑 Error Prior blocker' if blocking else '\nPrior suggestion')
                previous = [{'id': 1, 'user': {'login': 'github-actions[bot]', 'type': 'Bot'}, 'body': body}]
                response = Mock()
                response.__enter__ = Mock(return_value=response)
                response.__exit__ = Mock(return_value=False)
                response.read.return_value = json.dumps(previous).encode()
                with tempfile.TemporaryDirectory() as directory:
                    env = dict(SOURCE_ONLY_REVIEW='false' if excerpted else 'true',
                               INTEGRATION_SOURCE_EXCERPTED='true' if excerpted else 'false',
                               COMMENT_FILE=directory+'/missing', GH_TOKEN='offline',
                               REPO=REPO, PR_NUMBER='42')
                    with patch.dict(os.environ, env), patch('urllib.request.urlopen', return_value=response) as request:
                        if blocking:
                            with self.assertRaisesRegex(RuntimeError, 'blockers remain open'):
                                exec(inline(workflow, 2), {})
                        else:
                            with self.assertRaises(SystemExit):
                                exec(inline(workflow, 2), {})
                    self.assertEqual([call.args[0].method for call in request.call_args_list], ['GET'])


if __name__ == '__main__':
    unittest.main()
