"""Execute only Janitor's local report writers. Never run cleanup or Slack actions."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
import yaml

WORKFLOW = Path(__file__).resolve().parents[1] / 'workflows/runner-janitor.yml'


class JanitorReportTests(unittest.TestCase):
    def setUp(self):
        self.workflow = yaml.safe_load(WORKFLOW.read_text())
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.env = dict(os.environ, RUNNER_TEMP=str(self.root), GITHUB_OUTPUT=str(self.root/'output'),
                        GITHUB_PATH=str(self.root/'path'), GITHUB_WORKSPACE=str(self.root),
                        CHANNEL='#fixture-only', RUNNER='Mac Runner with spaces #2', RUNNER_NAME='Mac Runner with spaces #2',
                        RESULT='failure', FREE='12', FREE_BEFORE='10', FREE_AFTER='12',
                        DRY_RUN='false', FINDINGS='cache <old> & "quoted"', FAILED='Build app; Sign package',
                        RUN_URL='https://example.invalid/run/42')

    def step(self, job, name):
        return next(s for s in self.workflow['jobs'][job]['steps'] if s.get('name') == name)

    def execute(self, job, name, **env):
        # Deliberately execute only the extracted local run block, never a uses
        # action, GitHub workflow dispatch, cleanup or message publication.
        return subprocess.run(['bash', '-c', self.step(job, name)['run']],
                              env=self.env | env, cwd=self.root, capture_output=True, text=True, timeout=10)

    def test_yaml_preserves_complete_workflow_job_and_channel_names(self):
        self.assertEqual(self.workflow['name'], 'Janitor')
        self.assertEqual(self.workflow['jobs']['feed']['name'], 'Post report to #feed-janitor')
        for job, channel in [('alarm', '#ci'), ('feed', '#feed-janitor')]:
            post = self.step(job, 'Post to '+channel)
            self.assertTrue(post['uses'].startswith('slackapi/slack-github-action@'))
            self.assertEqual(post['with']['payload-file-path'], '${{ steps.'+('alarm' if job=='alarm' else 'report')+'.outputs.payload }}')

    def test_alarm_payload_distinguishes_missing_runner_failure_and_cancellation(self):
        for runner, result, expected in [('', 'failure', 'did not run'), ('Mac Runner #2', 'failure', 'failed'), ('Mac Runner #2', 'cancelled', 'did not finish (cancelled)')]:
            with self.subTest(runner=runner,result=result):
                run = self.execute('alarm', 'Write alarm', RUNNER=runner, RESULT=result)
                self.assertEqual(run.returncode,0,run.stderr)
                payload = json.loads((self.root/'alarm-payload.json').read_text())
                self.assertEqual(payload['channel'], '#fixture-only')
                self.assertIn('*Janitor '+expected+'*', payload['text'])
                self.assertIn('`'+(runner or 'wendy-macos-26')+'`', payload['text'])
                self.assertIn('payload='+str(self.root/'alarm-payload.json'), (self.root/'output').read_text())

    def test_report_keeps_spaces_and_escapes_findings_as_data(self):
        for result, icon in [('success',':white_check_mark:'), ('failure',':x:'), ('cancelled',':warning:')]:
            with self.subTest(result=result):
                run = self.execute('feed','Write report', RESULT=result, DRY_RUN='true')
                self.assertEqual(run.returncode,0,run.stderr)
                payload = json.loads((self.root/'feed-payload.json').read_text())
                self.assertEqual(payload['channel'],'#fixture-only')
                text = payload['text']
                self.assertIn(icon+' *Janitor*',text)
                self.assertIn('`Mac Runner with spaces #2`',text)
                self.assertIn('dry run',text)
                self.assertIn('cache &lt;old&gt; &amp; "quoted"',text)
                self.assertIn('Failed: Build app; Sign package',text)
                self.assertIn('<https://example.invalid/run/42|View run>',text)

    def test_offline_runner_report_does_not_invent_disk_measurements(self):
        run = self.execute('feed','Write report', FREE_BEFORE='', FREE_AFTER='', FINDINGS='', FAILED='')
        self.assertEqual(run.returncode,0,run.stderr)
        text = json.loads((self.root/'feed-payload.json').read_text())['text']
        self.assertIn('did not run on the Mac',text)
        self.assertNotIn('Free:',text)

    def test_runner_identity_is_recorded_before_checkout_or_disk_probe_can_fail(self):
        steps = self.workflow['jobs']['mac']['steps']
        before = self.step('mac','Record free space before')
        checkout = next(s for s in steps if s.get('uses','').startswith('actions/checkout@'))
        self.assertLess(steps.index(before),steps.index(checkout))
        tools = self.root/'bin';tools.mkdir()
        df = tools/'df';df.write_text('#!/bin/sh\nexit 9\n');df.chmod(0o755)
        run = self.execute('mac','Record free space before',PATH=str(tools)+os.pathsep+os.environ['PATH'])
        self.assertNotEqual(run.returncode,0)
        self.assertEqual((self.root/'output').read_text(),'runner=Mac Runner with spaces #2\n')


if __name__=='__main__':
    unittest.main()
