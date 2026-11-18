#!/usr/bin/env python3
"""Shared review trust, portability and private state, using synthetic inputs."""
import argparse
import copy
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import api_review as api
import api_review_comment as comments
import compatibility_review as compat
import compatibility_state as state
import review_sources as sources
from api_review_test import metadata, diff, HEAD_SHA, BASE_SHA
from api_review_comment_test import MultipartGitHub, result, REVIEW_DIFF, HEAD, BASE, pull
import compatibility_review_test as fixtures
from compatibility_review_test import response

WORKFLOW = Path(__file__).resolve().parents[1] / 'workflows/api-review-shared.yml'
TARGET = 'example/consumer'


def repository(repo=TARGET, visibility='private'):
    return dict(full_name=repo, visibility=visibility, private=visibility == 'private')


def embedded_script(name, marker='script: |'):
    lines = WORKFLOW.read_text().splitlines()
    start = next(i for i, line in enumerate(lines) if line.strip() == '- name: ' + name)
    start = next(i for i in range(start, len(lines)) if lines[i].strip() == marker)
    indent = len(lines[start]) - len(lines[start].lstrip()) + 2
    body = []
    for line in lines[start + 1:]:
        if line.strip() and len(line) - len(line.lstrip()) < indent:
            break
        body.append(line[indent:] if line.strip() else '')
    return '\n'.join(body)


class DestinationTests(unittest.TestCase):
    def test_missing_unknown_and_forged_visibility_fail(self):
        for bad in ({}, None, dict(repository(), full_name='example/foreign'),
                    dict(repository(), full_name=None), dict(repository(), private=False),
                    dict(repository(), visibility='internal'), dict(repository(), private='true')):
            with self.subTest(bad=bad), self.assertRaises(compat.ReviewError):
                compat.repository_destination(bad, TARGET)

    def test_public_guard_stays_strict_and_local_outputs_cannot_gain_destination(self):
        private = dict(input_visibility='private', compatibility_findings=[])
        with self.assertRaises(compat.ReviewError):
            compat.public_result(private)
        with self.assertRaises(compat.ReviewError):
            compat.publication_result(private, repository(), TARGET)
        private.update(publication_target=dict(repository=TARGET, visibility='private'), publication_scope='local_only')
        with self.assertRaises(compat.ReviewError):
            compat.publication_result(private, repository(), TARGET)
        private['publication_scope'] = 'target_repository'
        compat.publication_result(private, repository(), TARGET)
        for destination in (repository('example/foreign'), repository(visibility='public')):
            with self.assertRaises(compat.ReviewError):
                compat.publication_result(private, destination, destination['full_name'])
        for ref in (dict(repository='example/foreign', visibility='private', path='source.py', side='head'),
                    dict(repository=TARGET, visibility='public', path='source.py', side='head'),
                    dict(repository=TARGET, visibility='private', path='source.py', side='counterpart')):
            with self.assertRaises(compat.ReviewError):
                compat.publication_result(dict(private, compatibility_previous=[dict(finding=dict(caller=[ref]))]), repository(), TARGET)

    def test_private_finding_publish_fetch_reassess_and_stale_rejection(self):
        fixture = fixtures.EvidenceTests(); fixture.setUp()
        finding = dict(fixture.finding, caller=fixture.finding['changed'])
        class DestinationGitHub(MultipartGitHub):
            def request(self, method, path, payload=None):
                if method == 'GET' and path == '/repos/' + TARGET:
                    self.calls.append((method, path, payload))
                    return repository()
                return super().request(method, path, payload)
        def current_pull():
            value = pull()
            value['head'].update(sha=HEAD_SHA, repo=dict(full_name=TARGET))
            value['base']['sha'] = BASE_SHA
            return value
        github = DestinationGitHub(pulls=[current_pull() for _ in range(12)])
        with tempfile.TemporaryDirectory() as directory:
            directory = Path(directory)
            meta = metadata(); meta['head']['repo'] = dict(full_name=TARGET)
            (directory / 'meta').write_text(json.dumps(meta))
            (directory / 'repo').write_text(json.dumps(repository()))
            (directory / 'diff').write_bytes(diff())
            args = argparse.Namespace(metadata=str(directory/'meta'), diff=str(directory/'diff'), repo=TARGET,
                pr_number=meta['number'], expected_head_sha=HEAD_SHA, expected_base_sha=BASE_SHA,
                output=str(directory/'result'), source_repository=None, repository_metadata=str(directory/'repo'),
                previous=None, model='offline')
            def resolved_response(meta, text, repo, model):
                assessment = meta['_compatibility']
                payload = response(finding) if not assessment['previous'] else response()
                if assessment['previous']:
                    payload['compatibility_updates'] = [dict(prior_id=assessment['previous'][0]['id'],
                        status='not_assessed', reason='Current bounded evidence does not reassess this concern', evidence=[])]
                return compat.validate(payload, api.finding_catalog(api.evidence_catalog(meta['_batch_evidence']), assessment),
                                       assessment['catalog'], previous=assessment['previous'])
            with patch.object(api, 'review_model', side_effect=resolved_response):
                self.assertEqual(api.command_review(args), 0)
            reviewed = json.loads((directory/'result').read_text())
            self.assertEqual(reviewed['input_visibility'], 'private')
            self.assertTrue(all(ref['visibility'] == 'private' for ref in reviewed['compatibility_findings'][0]['changed']))
            self.assertTrue(comments.publish(reviewed, TARGET, 1911, HEAD_SHA, BASE_SHA, diff(), github, destination_bound=True))
            self.assertFalse(any('/requested_reviewers' in call[1] for call in github.mutations()))
            self.assertTrue(all(call[1].startswith('/repos/' + TARGET + '/') for call in github.mutations()))
            comments.fetch_state(TARGET, 1911, directory/'prior', github, destination_bound=True)
            prior = json.loads((directory/'prior').read_text())
            self.assertEqual(len(prior['compatibility_findings']), 1)
            args.previous = str(directory/'prior')
            with patch.object(api, 'review_model', side_effect=resolved_response):
                self.assertEqual(api.command_review(args), 0)
            second = json.loads((directory/'result').read_text())
            dry = comments.DryRunGitHub(github)
            before = len(github.mutations())
            self.assertTrue(comments.publish(second, TARGET, 1911, HEAD_SHA, BASE_SHA, diff(), dry, destination_bound=True))
            self.assertEqual(len(github.mutations()), before)
            records = state.current(second)
            self.assertEqual(records[0]['id'], prior['compatibility_findings'][0]['id'])
            self.assertEqual(records[0]['status'], 'not_assessed')
            github.pulls = [dict(current_pull(), state='closed')]
            with self.assertRaises(ValueError):
                comments.publish(second, TARGET, 1911, HEAD_SHA, BASE_SHA, diff(), github, destination_bound=True)
            self.assertEqual(len(github.mutations()), before)
            foreign = copy.deepcopy(second)
            foreign['compatibility_previous'][0]['finding']['caller'][0]['repository'] = 'example/foreign'
            with self.assertRaises(compat.ReviewError):
                comments.publish(foreign, TARGET, 1911, HEAD_SHA, BASE_SHA, diff(), github, destination_bound=True)
            self.assertEqual(len(github.mutations()), before)

    def test_api_decisions_in_new_repository_never_request_human_review(self):
        value = result()
        pulls = [dict(pull(), head=dict(sha=HEAD, repo=dict(full_name=TARGET))) for _ in range(4)]
        github = MultipartGitHub(pulls=pulls)
        self.assertTrue(comments.publish(value, TARGET, 1911, HEAD, BASE, REVIEW_DIFF, github))
        self.assertFalse(any('/requested_reviewers' in call[1] for call in github.mutations()))


class PortabilityTests(unittest.TestCase):
    def test_prompt_uses_actual_repository(self):
        self.assertIn('API decisions in example/consumer.', api.system_prompt(repo=TARGET))
        self.assertNotIn('API decisions in WendyOS', api.system_prompt(repo=TARGET))

    def test_other_repositories_do_not_use_wendyos_map(self):
        parsed = api.parse_diff(diff().decode())
        with patch.object(compat, 'git', side_effect=subprocess.CalledProcessError(1, 'git')) as git:
            _, limits = compat.collect_context('/unused', TARGET, BASE_SHA, HEAD_SHA, parsed, visibility='private')
        self.assertEqual(limits['boundary_paths'], [])
        self.assertEqual(limits['selection'], 'changed_windows_only')
        self.assertEqual(limits['boundary_read_bytes'], 0)
        self.assertTrue(any('unmapped callers' in value for value in limits['limitations']))
        self.assertFalse(any('/auth' in str(call) for call in git.call_args_list))

    def test_generated_bindings_and_vendor_implementations_preserve_inputs(self):
        for path in ('proto/synthetic/v1/service.pb.go', 'vendor/example/package/impl.go',
                     'Sources/WendyKit/Generated/service.pb.swift',
                     'Sources/WendyKMSDRM/vendor/drm.h', 'Sources/CStbTrueType/include/stb_truetype.h'):
            self.assertTrue(sources.is_generated_path(path), path)
        for path in ('proto/service.proto', 'proto/handwritten.go', 'vendor/example/schema.proto',
                     'vendor/example/go.mod', 'vendor/example/go.sum', 'GRPCGenerated.swift',
                     'opentelemetry/proto/trace/v1/trace.proto', 'Sources/WendyKit/Protos/service.proto',
                     'Sources/WendyKit/Generated/config.json', 'Sources/CStbTrueType/shim.c',
                     'Sources/CStbTrueType/include/cstbtruetype.h', 'Package.resolved', 'regen-agent-stubs.sh'):
            self.assertFalse(sources.is_generated_path(path), path)
        for path in ('web/source.test.tsx', 'web/source.spec.ts', 'conformance/synthetic/vector.json'):
            self.assertEqual(compat.source_kind(path), 'test_definition')


class WorkflowTests(unittest.TestCase):
    def test_actual_pin_resolver_rejects_missing_mismatched_and_untrusted_pins(self):
        sha = 'a' * 40
        template = ('  review:\n    uses: wendylabsinc/WendyOS/.github/workflows/api-review-shared.yml@' + sha +
                    '\n    with:\n      automation_sha: ' + sha + '\n')
        harness = r'''
const input = JSON.parse(require('fs').readFileSync(0, 'utf8'));
Object.assign(process.env, {AUTOMATION_SHA: input.sha, DRY_RUN: input.dry,
 GITHUB_WORKFLOW_REF: 'example/consumer/.github/workflows/api-review.yml@refs/heads/main',
 GITHUB_WORKFLOW_SHA: 'b'.repeat(40)});
const calls = [];
const context = {repo: {owner: 'example', repo: 'consumer'}, eventName: input.event};
const github = {rest: {repos: {getContent: async args => {calls.push(args); return {data: {
 encoding: 'base64', content: Buffer.from(input.caller).toString('base64')}};}}}};
(async () => {let failed = false; try {await new (Object.getPrototypeOf(async function(){}).constructor)(
 'context','github', input.script)(context, github);} catch {failed = true;}
 console.log(JSON.stringify({failed, calls}));})();
'''
        for caller, pin, event, dry, fails in (
                (template, sha, 'pull_request', 'false', False),
                (template.replace('automation_sha: ' + sha, 'automation_sha: ' + 'c'*40), sha, 'pull_request', 'false', True),
                (template, '../main', 'pull_request', 'false', True),
                ('', sha, 'pull_request', 'false', True),
                (template, sha, 'workflow_dispatch', 'false', True),
                (template, sha, 'workflow_dispatch', 'true', False)):
            proc = subprocess.run(['node', '-e', harness], text=True, capture_output=True, check=True,
                input=json.dumps(dict(script=embedded_script('Validate immutable automation pin'),
                                     caller=caller, sha=pin, event=event, dry=dry)))
            actual = json.loads(proc.stdout)
            self.assertEqual(actual['failed'], fails, (caller, event, proc.stderr))
            if not fails:
                self.assertEqual(actual['calls'][0]['ref'], 'main' if event == 'pull_request' else 'b'*40)

    def test_artifact_guard_refreshes_visibility_after_inference(self):
        script = embedded_script('Guard destination privacy before artifact upload', 'run: |')
        with tempfile.TemporaryDirectory() as directory:
            directory = Path(directory)
            (directory/'review-automation').symlink_to(WORKFLOW.parents[2], target_is_directory=True)
            private = dict(input_visibility='private', publication_scope='target_repository',
                           publication_target=dict(repository=TARGET, visibility='private'))
            (directory/'api-review-result.json').write_text(json.dumps(private))
            (directory/'api-review-repository.json').write_text(json.dumps(repository()))
            binary = directory/'bin'; binary.mkdir()
            (binary/'python3').symlink_to(__import__('sys').executable)
            gh = binary/'gh'
            gh.write_text('#!/bin/sh\ncat "$CURRENT_REPOSITORY"\n')
            gh.chmod(0o755)
            current = directory/'fresh.json'
            for visibility, expected in (('private', 0), ('public', 1)):
                current.write_text(json.dumps(repository(visibility=visibility)))
                proc = subprocess.run(['bash','-c',script], cwd=directory, text=True, capture_output=True,
                    env=dict(os.environ, REPO=TARGET, RUNNER_TEMP=str(directory), CURRENT_REPOSITORY=str(current),
                             PATH=str(binary)+os.pathsep+os.environ['PATH']))
                self.assertEqual(proc.returncode, expected, proc.stderr)
            self.assertEqual(json.loads((directory/'api-review-repository.json').read_text())['visibility'], 'private')
            self.assertEqual(json.loads((directory/'api-review-current-repository.json').read_text())['visibility'], 'public')

    def test_actual_event_gates_keep_opt_in_fork_and_bot_exclusions(self):
        import itertools
        lines = WORKFLOW.read_text().splitlines()
        expressions = []
        for start, line in enumerate(lines):
            if line == '    if: >-':
                parts = []
                for part in lines[start + 1:]:
                    if not part.startswith('      '):
                        break
                    parts.append(part.strip())
                expressions.append(' '.join(parts).replace('github.event.pull_request.labels.*.name', 'labels'))
        self.assertEqual(len(expressions), 2)
        cases = []
        for enabled, matching, fork, bot, dry in itertools.product((False,True), repeat=5):
            for event in ('pull_request', 'workflow_dispatch', 'push'):
                for action in ('opened','labeled'):
                    eligible = enabled and (action != 'labeled' or matching) and not fork and not bot
                    actor = 'dependabot[bot]' if bot else 'author'
                    cases.append(dict(github=dict(event_name=event, actor=actor, repository=TARGET,
                        event=dict(action=action, label=dict(name='review: api' if matching else 'unrelated'),
                        pull_request=dict(user=dict(login=actor), head=dict(repo=dict(full_name='example/fork' if fork else TARGET))))),
                        labels=['review: api'] if enabled else [], inputs=dict(dry_run=dry),
                        expected=[event == 'pull_request' and not dry and eligible,
                                  event == 'workflow_dispatch' or event == 'pull_request' and dry and eligible]))
        script = r"""
const input = JSON.parse(require('fs').readFileSync(0,'utf8'));
for (const c of input.cases) for (const [i, expression] of input.expressions.entries()) {
 const actual = new Function('github','inputs','labels','contains','return '+expression)(
  c.github,c.inputs,c.labels,(items,value)=>items.includes(value));
 if (actual !== c.expected[i]) throw new Error(JSON.stringify({c,i,actual}));
}
"""
        proc = subprocess.run(['node','-e',script], input=json.dumps(dict(expressions=expressions,cases=cases)),
                              text=True,capture_output=True)
        self.assertEqual(proc.returncode,0,proc.stderr)

    def test_actual_empty_key_step_fails_before_model_execution(self):
        script = embedded_script('Review API decisions and compatibility', 'run: |')
        for value in ('', ' \t\n'):
            proc = subprocess.run(['bash', '-c', script], env=dict(os.environ, ANTHROPIC_API_KEY=value),
                                  text=True, capture_output=True)
            self.assertEqual(proc.returncode, 1)
            self.assertIn('ANTHROPIC_API_KEY is unavailable', proc.stdout)
            self.assertNotIn('python', proc.stderr)


if __name__ == '__main__':
    unittest.main()
