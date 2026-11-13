"""The release inventory must have provenance from each producing job."""
import copy
import fnmatch
import os
import subprocess
import tempfile
from pathlib import Path
import unittest
import yaml

ROOT = Path(__file__).resolve().parents[2]


def assert_provenance(test, workflow):
    jobs = workflow['jobs']
    release = jobs['release']
    ancestors = set()

    def visit(name):
        if name in ancestors:
            return
        ancestors.add(name)
        needs = jobs[name].get('needs', [])
        for dependency in ([needs] if isinstance(needs, str) else needs):
            visit(dependency)
    visit('release')
    checked = 0
    for name, job in jobs.items():
        previous = []
        for step in job.get('steps', []):
            action = step.get('uses', '').split('@')[0]
            if action == 'actions/attest-build-provenance':
                test.assertNotIn('if', step, f'{name}: provenance must not be optional')
                test.assertFalse(step.get('continue-on-error', False))
                previous += step['with']['subject-path'].splitlines()
            if action != 'actions/upload-artifact':
                continue
            path = step['with']['path']
            # The release file selector excludes the dependency cache and the
            # unpacked app. The signed app ZIP has its own attested upload.
            if step.get('name') in ['Upload Release build cache', 'Upload notarized app bundle']:
                continue
            test.assertIn(name, ancestors, f'{name}: release must wait for this producer')
            test.assertTrue(any(fnmatch.fnmatchcase(path, pattern) for pattern in previous),
                            f'{name}: no producing-job attestation before upload of {path}')
            test.assertEqual(job.get('permissions', {}).get('id-token'), 'write', name)
            test.assertEqual(job.get('permissions', {}).get('attestations'), 'write', name)
            checked += 1
    test.assertGreaterEqual(checked, 13, 'release producer inventory unexpectedly shrank')
    return checked


class ReleaseProvenanceWorkflowTests(unittest.TestCase):
    def setUp(self):
        self.workflow = yaml.safe_load((ROOT / '.github/workflows/build.yml').read_text())

    def test_every_uploaded_release_file_is_attested_at_its_producer(self):
        assert_provenance(self, self.workflow)

    def test_unattested_package_or_missing_producer_is_rejected(self):
        for mutation in ['remove-attestation', 'remove-permission', 'remove-dependency']:
            with self.subTest(mutation=mutation):
                bad = copy.deepcopy(self.workflow)
                job = bad['jobs']['package-windows']
                if mutation == 'remove-attestation':
                    job['steps'] = [s for s in job['steps'] if s.get('name') != 'Attest signed MSI']
                elif mutation == 'remove-permission':
                    del job['permissions']['id-token']
                else:
                    bad['jobs']['release']['needs'].remove('package-windows')
                with self.assertRaises(AssertionError):
                    assert_provenance(self, bad)

    def test_publication_uses_verified_inventory_and_commit_and_cannot_ignore_failure(self):
        release = self.workflow['jobs']['release']
        steps = release['steps']
        gate = next(s for s in steps if s.get('name') == 'Tag the built commit')
        publish = next(s for s in steps if s.get('name') == 'Create Release')
        inventory = next(s for s in steps if s.get('name') == 'List files')
        checkout = next(s for s in steps if s.get('name') == 'Checkout release verification')
        self.assertLess(steps.index(inventory), steps.index(checkout))
        self.assertLess(steps.index(checkout), steps.index(gate))
        self.assertLess(steps.index(gate), steps.index(publish))
        self.assertIn('FILES<<EOF', inventory['run'])
        self.assertEqual(checkout['with']['path'], '.release-source')
        self.assertNotIn('ref', checkout['with'])  # current built SHA, never moving main
        self.assertEqual(gate['run'].strip(), 'python3 .release-source/.github/scripts/verify-release.py')
        for step in [gate, publish]:
            self.assertFalse(step.get('continue-on-error', False))
            self.assertNotIn('if', step)  # default success(), not always()
        self.assertEqual(publish['with']['files'], '${{ env.FILES }}')
        self.assertEqual(publish['with']['target_commitish'], '${{ github.sha }}')
        self.assertEqual(publish['with']['tag_name'], gate['env']['TAG'])
        self.assertEqual(release['permissions']['attestations'], 'read')
        self.assertFalse(any('attest-build-provenance@' in s.get('uses', '') for s in steps),
                         'release consumer must verify upstream provenance, not mint its own')

    def test_actual_inventory_selects_all_publishable_formats_and_excludes_build_cache(self):
        release = self.workflow['jobs']['release']
        inventory = next(s for s in release['steps'] if s.get('name') == 'List files')
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            assets = ['nested/agent with spaces.tar.gz', 'cli.zip', 'cli.msi',
                      'agent.deb', 'agent.rpm', 'source.spdx.json']
            for name in assets + ['macos-release-build-cache.tar.zst', 'app/Contents/MacOS/agent']:
                path = root / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text('fixture')
            output = root / 'environment'
            result = subprocess.run(['bash', '-e', '-c', inventory['run']], cwd=root,
                                    env=dict(os.environ, GITHUB_ENV=str(output)),
                                    text=True, capture_output=True, timeout=10)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(output.read_text().splitlines(),
                             ['FILES<<EOF'] + sorted('./'+a for a in assets) + ['EOF'])

    def test_required_ci_runs_both_fixture_suites_on_gate_and_workflow_changes(self):
        build_events = self.workflow.get('on', self.workflow.get(True))
        self.assertIn('.github/scripts/verify-release.py', build_events['push']['paths'])
        ci = yaml.safe_load((ROOT / '.github/workflows/go-tests.yml').read_text())
        events = ci.get('on', ci.get(True))
        for event in ['push', 'pull_request']:
            self.assertIn('.github/scripts/**', events[event]['paths'])
            self.assertIn('.github/workflows/**', events[event]['paths'])
        job = ci['jobs']['automation-regressions']
        self.assertNotIn('if', job)
        self.assertTrue(any('run-automation-regressions.py' in s.get('run', '') for s in job['steps']))
        driver = (ROOT / '.github/scripts/run-automation-regressions.py').read_text()
        for suite in ['tag-built-commit_test.py', 'release_provenance_test.py']:
            self.assertIn('"'+suite+'"', driver)


if __name__ == '__main__':
    unittest.main()
