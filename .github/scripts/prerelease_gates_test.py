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
        # These suites also keep a narrow PR trigger for the files only they test.
        scoped = {'validate-simulator': 'so101-simulator.yml'}
        self.assertEqual(set(build['pre-release-checks']['needs']),
                         set(suites) | set(scoped) | {'test-templates'})
        for name, filename in {**suites, **scoped}.items():
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
        for filename in scoped.values():
            with self.subTest(workflow=filename):
                events = workflow(filename)['on']
                self.assertIn('workflow_call', events)
                self.assertIn('workflow_dispatch', events)
                self.assertNotIn('push', events)

    def test_simulator_prs_run_only_for_the_runtime_image_and_viewer(self):
        paths = workflow('so101-simulator.yml')['on']['pull_request']['paths']
        self.assertIn('go/simulator/so101/**', paths)
        self.assertIn('web-client/desktop/**', paths)
        # The PR Go unit tests cover Go-only changes; see pr-checks.yml.
        self.assertFalse([path for path in paths if path.startswith('go/internal/')])

    def test_required_contract_context_is_reported_for_every_pr(self):
        checks = workflow('pr-checks.yml')
        self.assertFalse(checks['on']['pull_request'])
        job = checks['jobs']['shared-grpc-api-contracts']
        self.assertEqual(job['name'], 'Shared gRPC API contracts')
        self.assertNotIn('if', job)
        release_job = dict(workflow('go-tests.yml')['jobs']['shared-grpc-api-contracts'])
        self.assertEqual(release_job.pop('if'), '${{ !inputs.media_only }}')
        self.assertEqual(job, release_job)

    def test_binary_sboms_wait_for_signed_windows_archives(self):
        jobs = workflow('build.yml')['jobs']
        self.assertIn('package-windows', jobs['sbom-binaries']['needs'])
        self.assertNotIn('sbom-binaries', jobs['package-windows']['needs'])
        signed = next(step for step in jobs['package-windows']['steps']
                      if step.get('name') == 'Upload signed zip')
        self.assertEqual(signed['with']['overwrite'], 'true')

    def test_media_only_is_opt_in_and_never_used_by_release(self):
        go = workflow('go-tests.yml')
        for event in ('workflow_call', 'workflow_dispatch'):
            media = go['on'][event]['inputs']['media_only']
            self.assertEqual(media['type'], 'boolean')
            self.assertEqual(media['default'], 'false')
        release = workflow('build.yml')['jobs']['validate-go']
        self.assertNotIn('media_only', release.get('with', {}))
        self.assertEqual(go['jobs']['test']['if'], '${{ always() && !inputs.media_only }}')

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
