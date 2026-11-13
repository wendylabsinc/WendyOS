"""Run cleanup selection against a fake Docker CLI and isolated disk readings."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
import yaml

ROOT = Path(__file__).resolve().parents[2]


class RunnerCleanupTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.bin = self.root/'bin'; self.bin.mkdir()
        self.log = self.root/'docker.jsonl'
        self.env = dict(os.environ, PATH=str(self.bin)+os.pathsep+os.environ['PATH'],
                        CLEANUP_LOG=str(self.log), GITHUB_RUN_ID='123',
                        GITHUB_WORKSPACE=str(self.root), RUNNER_NAME='fixture runner',
                        MIN_FREE_GIB='25', FIXTURE_FREE_KIB=str(25*1024*1024))
        self.builders = ['wendy-99-1-test-device', 'wendy-99-1-test-device-oci',
                         'wendy-99-1-test-device-mtls', 'wendy-1234-1-test-device',
                         'wendy-123-1-active-device', 'wendy-123-2-active-device',
                         'default', 'wendy', 'wendy-mtls', 'wendy-oci',
                         'wendy-50e78bd982d0-oci', 'wendy-invalid-1-job',
                         'wendy-99-1-', 'wendy-99-1-bad name']
        self.env['FIXTURE_BUILDERS'] = json.dumps(self.builders + ['wendy-99-1-test-device0', self.builders[0]])
        self.env['FIXTURE_NODE'] = 'wendy-99-1-test-device0'
        docker = self.bin/'docker'
        docker.write_text('#!'+sys.executable+'\n'+'''import json,os,sys
args=sys.argv[1:]
with open(os.environ['CLEANUP_LOG'],'a') as f:f.write(json.dumps(args)+'\\n')
if args[:2]==['buildx','ls']:
 if os.environ.get('FIXTURE_LIST_FAIL'):sys.exit(1)
 print('\\n'.join(json.loads(os.environ['FIXTURE_BUILDERS'])));sys.exit(0)
if args[:2]==['buildx','inspect']:
 sys.exit(1 if args[2]==os.environ['FIXTURE_NODE'] else 0)
if args[:2]==['buildx','rm']:
 assert len(args)==4 and args[3]=='--force',args
 if args[2]==os.environ.get('FIXTURE_RM_FAIL'):sys.exit(1)
 sys.exit(0)
raise AssertionError('unexpected Docker command: '+repr(args))
''');docker.chmod(0o755)
        df = self.bin/'df'
        df.write_text('#!'+sys.executable+'\n'+'''import os,sys
if os.environ.get('FIXTURE_DF_FAIL'):sys.exit(1)
print('Filesystem 1024-blocks Used Available Capacity Mounted on')
print('fixture 999999999 1 '+os.environ['FIXTURE_FREE_KIB']+' 1% /fixture')
''');df.chmod(0o755)

    def sweep(self, **env):
        return subprocess.run(['bash',str(ROOT/'.github/scripts/remove-stale-ci-builders.sh')],
                              env=self.env|env,capture_output=True,text=True,timeout=10)

    def removed(self):
        return [a[2] for a in map(json.loads,self.log.read_text().splitlines()) if a[:2]==['buildx','rm']]

    def test_only_stale_owned_builder_names_are_removed_once(self):
        result = self.sweep()
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertEqual(self.removed(), sorted(self.builders[:4]))
        self.assertIn('Stale CI builders: 4',result.stdout)

    def test_dry_run_and_list_failure_never_remove_builders(self):
        for options in [{'DRY_RUN':'true'}, {'FIXTURE_LIST_FAIL':'1'}]:
            with self.subTest(options=options):
                result = self.sweep(**options)
                self.assertEqual(result.returncode,0,result.stderr)
                self.assertEqual(self.removed(),[])
        self.assertIn('Could not list buildx builders',result.stdout)

    def test_missing_or_malformed_run_identity_cannot_sweep(self):
        for run_id in ['', 'not-a-run']:
            result = self.sweep(GITHUB_RUN_ID=run_id)
            self.assertEqual(result.returncode,0,result.stderr)
            self.assertIn('Cannot identify the current run',result.stdout)
            self.assertFalse(self.log.exists())

    def test_failed_removal_does_not_expand_selection_or_stop_later_cleanup(self):
        result = self.sweep(FIXTURE_RM_FAIL=self.builders[0])
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertEqual(self.removed(), sorted(self.builders[:4]))

    def test_disk_action_changes_select_required_regressions(self):
        workflow = yaml.safe_load((ROOT/'.github/workflows/go-tests.yml').read_text())
        events = workflow.get('on',workflow.get(True))
        for event in ['push','pull_request']:
            self.assertIn('.github/actions/check-runner-disk/**',events[event]['paths'])
            self.assertIn('.github/scripts/**',events[event]['paths'])

    def test_disk_threshold_and_invalid_readings_stop_work_without_deleting_anything(self):
        action = yaml.safe_load((ROOT/'.github/actions/check-runner-disk/action.yml').read_text())
        script = action['runs']['steps'][0]['run']
        for free, minimum, success in [(25*1024*1024,'25',True), (25*1024*1024-1,'25',False),
                                        ('not-a-number','25',False), (100*1024*1024,'bad',False)]:
            with self.subTest(free=free,minimum=minimum):
                result = subprocess.run(['bash','-c',script],cwd=self.root,
                                        env=self.env|{'FIXTURE_FREE_KIB':str(free),'MIN_FREE_GIB':minimum},
                                        text=True,capture_output=True,timeout=10)
                self.assertEqual(result.returncode==0,success,result.stdout+result.stderr)
        self.assertFalse(self.log.exists(),'disk gate must not invoke Docker cleanup')


if __name__=='__main__':unittest.main()
