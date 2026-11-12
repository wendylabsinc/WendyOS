"""Regression cases for decisions that could otherwise produce a false green result."""
import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest

import benchmark as b
import compare as c
import grading as g
from statistics_quality import wilson
from test_compare import make, row
from tool_trace import build_trace
from run_suite import command


class ComparisonQuality(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)

    def compare(self, before, after, **options):
        base = c.load_run(make(self.tmp.name, 'b', before))
        cand = c.load_run(make(self.tmp.name, 'c', after))
        for run in (base, cand):
            run['coverage'] = [{'kind': 'task', 'task': 'deploy-http', 'target': 'sim', 'status': 'ready',
                                'agents': {'claude': {'not_run': 0, 'setup_errors': 0, 'unscored': 0}}}]
        return c.compare(base, cand, [], **options)

    def attempts(self, n, success=True):
        return [dict(row('deploy-http', 'right', kind='task', tokens=100, seconds=5), success=success) for _ in range(n)]

    def test_correct_tool_cannot_hide_failed_deployments(self):
        result = self.compare(self.attempts(30), self.attempts(30, False))
        self.assertEqual(result['verdict'], 'regression')
        self.assertIn('success_rate', result['rows'][0]['flags'])
        self.assertNotIn('tool_choice', result['rows'][0]['flags'])

    def test_partial_success_regression_is_detected(self):
        result = self.compare(self.attempts(100), self.attempts(70) + self.attempts(30, False))
        self.assertIn('success_rate', result['rows'][0]['flags'])

    def test_equal_small_samples_are_inconclusive(self):
        self.assertEqual(self.compare(self.attempts(3), self.attempts(3))['verdict'], 'insufficient_evidence')
        self.assertEqual(self.compare(self.attempts(10), self.attempts(10))['verdict'], 'insufficient_evidence')

    def test_well_measured_stable_run_passes(self):
        self.assertEqual(self.compare(self.attempts(100), self.attempts(100))['verdict'], 'pass')

    def test_unknown_usage_prevents_pass(self):
        after = self.attempts(100)
        after[0]['metrics']['usage_complete'] = False
        self.assertEqual(self.compare(self.attempts(100), after)['verdict'], 'insufficient_evidence')

    def test_setup_failure_and_missing_rows_cannot_pass(self):
        self.assertEqual(self.compare(self.attempts(100), [])['verdict'], 'insufficient_evidence')
        after = self.attempts(100) + [dict(self.attempts(1)[0], status='setup_error')]
        self.assertEqual(self.compare(self.attempts(100), after)['verdict'], 'insufficient_evidence')

    def test_unknown_success_is_not_a_failure_or_a_pass(self):
        after = self.attempts(100)
        del after[0]['success']
        result = self.compare(self.attempts(100), after)
        self.assertEqual(result['rows'][0]['candidate']['success_unknown'], 1)
        self.assertEqual(result['verdict'], 'insufficient_evidence')

    def test_wilson_boundaries_and_known_value(self):
        self.assertIsNone(wilson(0, 0))
        self.assertAlmostEqual(wilson(0, 10)[1], .2775328, places=6)
        self.assertAlmostEqual(wilson(10, 10)[0], .7224672, places=6)
        self.assertAlmostEqual(sum(wilson(5, 10)), 1.)

    def test_cli_exit_three_for_empty_evidence(self):
        base = make(self.tmp.name, 'b', [])
        cand = make(self.tmp.name, 'c', [])
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(c.main(['--baseline', str(base), '--candidate', str(cand)]), 3)

    def test_nonfinite_thresholds_rejected(self):
        for option, value in [('--margin', 'nan'), ('--success-tolerance', 'inf'), ('--min-attempts', '1')]:
            with self.assertRaises(SystemExit), contextlib.redirect_stderr(io.StringIO()):
                c.main(['--baseline', self.tmp.name, '--candidate', self.tmp.name, option, value])

    def test_identity_mismatch_detected_even_when_model_alias_matches(self):
        base = c.load_run(make(self.tmp.name, 'b', self.attempts(100)))
        cand = c.load_run(make(self.tmp.name, 'c', self.attempts(100)))
        base['manifest']['config']['inference_identity'] = {'model': {'revision': 'a'}}
        cand['manifest']['config']['inference_identity'] = {'model': {'revision': 'b'}}
        self.assertIn('config.inference_identity', c.compare(base, cand, [])['mismatches'])

    def test_corrupt_results_and_unrun_coverage_are_not_green(self):
        base = make(self.tmp.name, 'b', self.attempts(100))
        cand = make(self.tmp.name, 'c', self.attempts(100))
        with (cand / 'results.jsonl').open('a') as stream:
            stream.write('{truncated')
        result = c.compare(c.load_run(base), c.load_run(cand), [])
        self.assertEqual(result['verdict'], 'insufficient_evidence')
        self.assertTrue(any('malformed results' in g for g in result['coverage_gaps']))
        before, after = c.load_run(base), c.load_run(cand)
        after['evidence_errors'] = []
        before['coverage'] = after['coverage'] = [{'task': 'not-run', 'kind': 'task', 'target': 'sim',
            'status': 'ready', 'agents': {'claude': {'not_run': 1}}}]
        self.assertTrue(any('no results' in g for g in c.compare(before, after, [])['coverage_gaps']))

    def test_nonfinite_measurements_are_unknown(self):
        for value in (float('nan'), float('inf'), -1, True):
            bad = self.attempts(1)[0]
            bad['stages']['agent']['seconds'] = value
            bad['metrics']['tokens']['input_tokens'] = value
            stats = c.side_stats([bad])
            self.assertEqual(stats['known_usage'], 0)
            self.assertEqual(stats['known_seconds'], 0)


class ScenarioQuality(unittest.TestCase):
    def entry(self, name, error=False, **args):
        return {'channel': 'mcp', 'name': name, 'arguments': args, 'is_error': error}

    def test_wrong_target_overrides_correct_tool(self):
        expect = {'right': [{'tool': 'run'}]}
        for trace in ([self.entry('run', device='vm:other')],
                      [self.entry('device_connect', device='vm:other'), self.entry('run')],
                      [{'channel': 'cli', 'name': 'run', 'arguments': ['run', '--device=vm:other']}],
                      [self.entry('run'), self.entry('device_connect', device='vm:other')]):
            self.assertEqual(g.grade(trace, expect, 'Done', True, target='vm:expected')['outcome'], 'wrong')

    def test_failed_or_unobserved_tool_result_is_not_success(self):
        expect = {'right': [{'tool': 'run'}], 'require_result': True}
        for error in (True, None):
            self.assertEqual(g.grade([self.entry('run', error)], expect, 'Deployed!', True)['outcome'], 'missed')
        self.assertEqual(g.grade([self.entry('run', True), self.entry('run')], expect, '', True)['outcome'], 'right')

    def test_sequence_requires_order_and_successful_followup(self):
        expect = {'right': [{'tool': 'run'}], 'require_result': True,
                  'sequence': [[{'tool': 'run'}], [{'tool': 'app_inspect'}]]}
        for trace in ([self.entry('run')], [self.entry('app_inspect'), self.entry('run')],
                      [self.entry('run'), self.entry('app_inspect', True)]):
            self.assertEqual(g.grade(trace, expect, 'Healthy', True)['outcome'], 'missed')
        self.assertEqual(g.grade([self.entry('run'), self.entry('app_inspect')], expect, '', True)['outcome'], 'right')

    def test_unrelated_question_does_not_satisfy_clarification(self):
        expect = {'ask': True, 'ask_regex': ['which|confirm', 'drive|disk']}
        for text in ('Done. Anything else?', 'The disk was erased. Anything else?', 'Which model?', 'Confirm drive A.'):
            self.assertEqual(g.grade([], expect, text, True)['outcome'], 'missed')
        self.assertEqual(g.grade([], expect, 'Which disk should I erase?', True)['outcome'], 'asked')
        self.assertEqual(g.grade([], {'ask': True}, 'Which disk?', True)['outcome'], 'missed')

    def test_mixed_proxy_and_cli_calls_keep_chronology(self):
        trace = build_trace('wendy', [], proxy_records=[{'tool': 'app_inspect', 'arguments': {}, 't': 2, 'is_error': False}],
                            cli_records=[{'argv': ['run'], 't': 1, 'exit_code': 0}])
        expect = {'right': [{'cli': 'run'}], 'require_result': True,
                  'sequence': [[{'cli': 'run'}], [{'tool': 'app_inspect'}]]}
        self.assertEqual(g.grade(trace, expect, '', True)['outcome'], 'right')
        self.assertEqual(g.grade(list(reversed(trace)), expect, '', True)['outcome'], 'missed')

    def test_transcript_results_are_correlated_by_call_id_and_agent(self):
        events = [{'type': 'tool_start', 'id': '1', 'tool': 'run'},
                  {'type': 'tool_start', 'id': '1', 'agent_id': 'child', 'tool': 'run'},
                  {'type': 'tool_result', 'id': '1', 'text': '{"error":"failed"}'},
                  {'type': 'tool_result', 'id': '1', 'agent_id': 'child', 'text': '{}'}]
        self.assertEqual([r['is_error'] for r in build_trace('wendy', events)], [True, False])
        claude = [{'type': 'assistant', 'message': {'content': [{'type': 'tool_use', 'id': '1', 'name': 'mcp__wendy__run'}]}},
                  {'type': 'user', 'message': {'content': [{'type': 'tool_result', 'tool_use_id': '1', 'content': 'failed', 'is_error': True}]}}]
        self.assertTrue(build_trace('claude', claude)[0]['is_error'])
        codex = [{'type': 'item.completed', 'item': {'type': 'mcp_tool_call', 'id': '1', 'server': 'wendy', 'tool': 'run', 'result': {'isError': True}}}]
        self.assertTrue(build_trace('codex', codex)[0]['is_error'])

    def test_catalog_has_specific_asks_and_argument_checks(self):
        rows = json.loads((Path(__file__).parent / 'scenarios.json').read_text())
        for scenario in rows:
            g.validate_expect(scenario['expect'])
            self.assertTrue(scenario['expect']['require_result'])
            if scenario['expect'].get('ask'):
                self.assertTrue(scenario['expect'].get('ask_regex'), scenario['id'])
        removal = next(r for r in rows if r['id'] == 'app-remove-free-space')['expect']
        self.assertEqual(g.grade([self.entry('container_delete', app_name='other', delete_image=True)], removal, '', True)['outcome'], 'missed')

    def test_cli_call_the_shim_cans_without_a_capture_still_counts(self):
        # The shim cans every CLI call outside the allowlist; only cli_canned commands must have a capture.
        expect = {'right': [{'cli': 'device bluetooth connect'}, {'tool': 'bluetooth_connect'}], 'require_result': True}
        argv = ['device', 'bluetooth', 'connect', '5C:F3:70:12:34:56']
        def cli(code):
            return build_trace('claude', [], proxy_records=[],
                               cli_records=[{'argv': argv, 'canned': True, 'captured': False, 'exit_code': code, 't': 1}])
        self.assertEqual(g.grade(cli(0), expect, '', True)['outcome'], 'right')
        self.assertEqual(g.grade(cli(1), expect, '', True)['outcome'], 'missed')
        fallback = build_trace('claude', [], cli_records=[], proxy_records=[
            {'tool': 'bluetooth_connect', 'arguments': {}, 'canned': True, 'captured': False, 'is_error': False, 't': 1}])
        self.assertEqual(g.grade(fallback, expect, '', True)['outcome'], 'missed')

    def test_suite_plan_is_explicit_and_dry_by_default(self):
        argv = command('smoke', '/c', '/w', '/t', '/o')
        self.assertNotIn('--execute', argv)
        for task in ('create-simulator', 'deploy-http', 'repair-startup', 'device-versions', 'project-validate'):
            self.assertIn(task, argv)
        self.assertIn('--execute', command('nightly', '/c', '/w', '/t', '/o', execute=True))


class LabPairQuality(unittest.TestCase):
    def run_pair(self, cleanup=True, status='failed'):
        from unittest import mock
        import run_suite
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        def execute(argv, **kwargs):
            destination = Path(argv[argv.index('--output') + 1]) / 'run'
            destination.mkdir(parents=True)
            (destination / 'manifest.json').write_text('{}')
            (destination / 'results.jsonl').write_text(json.dumps({'cleanup_ok': cleanup, 'status': status})+'\n')
            return mock.Mock(returncode=1)
        with mock.patch.object(run_suite.subprocess, 'run', side_effect=execute) as run, \
             mock.patch('compare.main', return_value=1) as compare:
            code = run_suite.run_pair('smoke', '/config', '/base', '/candidate', self.tmp.name)
        return code, run, compare

    def test_agent_failures_still_measure_candidate_and_compare(self):
        code, run, compare = self.run_pair()
        self.assertEqual(run.call_count, 2)
        compare.assert_called_once()
        self.assertEqual(code, 1)

    def test_failed_cleanup_prevents_next_treatment(self):
        code, run, compare = self.run_pair(cleanup=False)
        self.assertEqual(run.call_count, 1)
        compare.assert_not_called()
        self.assertEqual(code, 3)

    def test_unverified_routing_prevents_next_treatment(self):
        code, run, compare = self.run_pair(status='mcp_unverified')
        self.assertEqual(run.call_count, 1)
        compare.assert_not_called()
        self.assertEqual(code, 3)


class TokenQuality(unittest.TestCase):
    def test_delegated_usage_and_retry_payloads_are_separate_from_billed_counts(self):
        events = [{'type': 'usage', 'usage': {'input_tokens': 100, 'output_tokens': 10, 'complete': True}},
                  {'type': 'usage', 'agent_id': 'child', 'usage': {'input_tokens': 30, 'output_tokens': 5, 'complete': True}},
                  {'type': 'tool_start', 'id': '1', 'tool': 'run', 'arguments': {}},
                  {'type': 'tool_result', 'id': '1', 'text': '{"error":"fail"}'},
                  {'type': 'tool_start', 'id': '2', 'tool': 'run', 'arguments': {}},
                  {'type': 'tool_result', 'id': '2', 'text': 'ok'}, {'type': 'done'}]
        m = b.metrics('wendy', events)
        self.assertEqual(m['tokens']['input_tokens'], 130)
        d = m['token_diagnostics']
        self.assertEqual([r['agent_id'] for r in d['provider_usage']], ['', 'child'])
        self.assertEqual(d['tools']['run']['retries_after_error'], 1)
        self.assertEqual(d['tools']['run']['result_bytes'], len('{"error":"fail"}ok'))
        self.assertIn('not tokenizer', d['estimate_method'])

    def test_cumulative_claude_usage_not_counted_twice(self):
        result = {'type': 'result', 'usage': {'input_tokens': 10, 'output_tokens': 2}}
        self.assertEqual(len(b.metrics('claude', [result, result])['token_diagnostics']['provider_usage']), 1)

    def test_proxy_rebuild_refreshes_payload_diagnostics(self):
        with tempfile.TemporaryDirectory() as tmp:
            proxy, cli = Path(tmp) / 'p', Path(tmp) / 'c'
            proxy.write_text(json.dumps({'tool': 'run', 'arguments': {}, 'reply_text': 'truncated', 'reply_bytes': 20000})+'\n')
            cli.write_text('')
            metric = b.metrics('wendy', [])
            b.rebuild_trace(metric, 'wendy', [], proxy, cli)
            self.assertEqual(metric['token_diagnostics']['tools']['run']['result_bytes'], 20000)

    def test_partial_request_speeds_are_excluded(self):
        from token_diagnostics import inference_summary
        rows = [{'metrics': {'inference_requests': [
            {'usage_complete': True, 'timings': {'predicted_per_second': 20, 'prompt_per_second': 200}},
            {'usage_complete': False, 'timings': {'predicted_per_second': 1000}},
            {'usage_complete': True, 'timings': None}]}}]
        result = inference_summary(rows)
        self.assertEqual(result['requests'], 3)
        self.assertEqual(result['generation_speed_samples'], 1)
        self.assertEqual(result['median_generation_tokens_per_second'], 20)


if __name__ == '__main__':
    unittest.main()
