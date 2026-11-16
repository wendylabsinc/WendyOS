#!/usr/bin/env python3
"""Exercise the publishing boundary against failed, cancelled and skipped tests."""

import fnmatch
import json
import os
import subprocess
import unittest

from ci_execution_test import workflow


class PrereleaseGatesTests(unittest.TestCase):
    def test_broad_suites_run_before_release_and_are_available_manually(self):
        build = workflow('build.yml')['jobs']
        suites = {'validate-go': 'go-tests.yml', 'validate-swift': 'swift-tests.yml',
                  'validate-e2e': 'swift-e2e-tests.yml', 'validate-sbom': 'sbom.yml'}
        self.assertEqual(set(build['pre-release-checks']['needs']), set(suites) | {'test-templates'})
        for name, filename in suites.items():
            with self.subTest(workflow=filename):
                self.assertEqual(build[name]['uses'], './.github/workflows/' + filename)
                self.assertNotIn('if', build[name])
                self.assertNotIn('continue-on-error', build[name])
        for filename in (*suites.values(), 'security.yml', 'plugin-launcher.yml'):
            with self.subTest(workflow=filename):
                events = workflow(filename)['on']
                self.assertIn('workflow_call', events)
                self.assertIn('workflow_dispatch', events)
                self.assertNotIn('pull_request', events)
                self.assertNotIn('push', events)

    def test_required_contract_context_is_reported_for_every_pr(self):
        checks = workflow('pr-checks.yml')
        self.assertFalse(checks['on']['pull_request'])
        job = checks['jobs']['shared-grpc-api-contracts']
        self.assertEqual(job['name'], 'Shared gRPC API contracts')
        self.assertNotIn('if', job)
        self.assertEqual(job, workflow('go-tests.yml')['jobs']['shared-grpc-api-contracts'])

    def test_gate_executes_and_rejects_every_unsuccessful_suite(self):
        gate = workflow('build.yml')['jobs']['pre-release-checks']
        self.assertEqual(gate['if'], 'always()')
        step = gate['steps'][0]
        self.assertEqual(step['env']['RESULTS'], '${{ toJSON(needs) }}')
        baseline = {name: {'result': 'success'} for name in gate['needs']}
        cases = [('all passed', baseline, True)]
        for name in baseline:
            for result in ('failure', 'cancelled', 'skipped'):
                cases.append((f'{name}: {result}', dict(baseline, **{name: {'result': result}}), False))
        for description, results, expected in cases:
            with self.subTest(case=description):
                run = subprocess.run(['bash', '-euo', 'pipefail', '-c', step['run']],
                                     env=dict(os.environ, RESULTS=json.dumps(results)),
                                     capture_output=True, text=True, timeout=10)
                self.assertEqual(run.returncode == 0, expected, run.stderr)

    def test_release_download_excludes_test_results_and_caches(self):
        steps = workflow('build.yml')['jobs']['release']['steps']
        patterns = [step['with']['pattern'] for step in steps
                    if step.get('uses', '').startswith('actions/download-artifact@')]
        for name in ['wendy-cli-darwin-arm64-1.tar.gz', 'wendy-agent-macos-1.zip',
                     'linux-packages-deb-arm64', 'sbom-source-1']:
            self.assertTrue(any(fnmatch.fnmatchcase(name, pattern) for pattern in patterns), name)
        for name in ['native-app-results-ios', 'agent-performance-smoke',
                     'e2e-run-123', 'macos-release-build-cache', 'swiftpm-build-mac']:
            self.assertFalse(any(fnmatch.fnmatchcase(name, pattern) for pattern in patterns), name)

    def test_every_publisher_refuses_failed_cancelled_or_skipped_validation(self):
        jobs = workflow('build.yml')['jobs']
        publishers = ['release', 'publish-agent-gcs', 'publish-install-scripts', 'publish-linux-repos']
        script = r'''
const jobs = JSON.parse(require('fs').readFileSync(0, 'utf8'));
for (const [name, job] of Object.entries(jobs)) {
  const condition = job.if.replace(/needs\.([\w-]+)/g, (_, key) => `needs['${key}']`);
  const evaluate = new Function('needs', 'github', 'inputs', 'always', `return (${condition})`);
  for (const event of ['push', 'workflow_dispatch']) {
    for (const result of ['success', 'failure', 'cancelled', 'skipped']) {
      const needs = Object.fromEntries(job.needs.map(key => [key, {
        result: 'success', outputs: {is_release: 'true', is_prerelease: 'false'}
      }]));
      needs['pre-release-checks'].result = result;
      const actual = evaluate(needs, {event_name: event}, {publish: true}, () => true);
      if (actual !== (result === 'success')) throw Error(`${name}: ${event} allowed ${result}`);
    }
  }
}
'''
        for name in publishers:
            self.assertIn('pre-release-checks', jobs[name]['needs'])
        subprocess.run(['node', '-e', script], input=json.dumps({name: jobs[name] for name in publishers}),
                       text=True, check=True, timeout=10)


if __name__ == '__main__':
    unittest.main()
