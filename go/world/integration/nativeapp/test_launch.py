"""Approval completes on validated output, independently of desktop lifetime."""
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock
from integration import nativeapp


class ReviewTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.source = self.root / 'review.json'
        self.original = b'{"rooms":[{"id":"office"}]}'
        self.source.write_bytes(self.original)
        self.output = self.root / 'approved.json'
        self.process = subprocess.Popen([sys.executable, '-c', 'import time;time.sleep(20)'])
        self.addCleanup(nativeapp.stop_owned, self.process)

    def wait(self, **kwargs):
        return nativeapp.wait_review(self.process, self.source, self.original, self.output,
                                     nativeapp.validate_schedule, timeout=.04, interval=.005, **kwargs)

    def test_approval_completes_while_desktop_stays_running(self):
        self.output.write_text('{"revisit_seconds":{"office":300}}')
        self.assertEqual(self.wait(), {'revisit_seconds': {'office': 300}})
        self.assertIsNone(self.process.poll())

    def test_changed_input_refuses_saved_approval(self):
        self.output.write_text('{"revisit_seconds":{"office":300}}')
        self.source.write_text('{"rooms":[{"id":"other"}]}')
        with self.assertRaisesRegex(ValueError, 'input changed'):
            self.wait()

    def test_cancel_receipt_does_not_require_app_exit(self):
        self.output.write_text('{"action":"cancel"}')
        with self.assertRaises(nativeapp.ReviewCancelled):
            self.wait()
        self.assertIsNone(self.process.poll())

    def test_caller_cancel_and_deadline_do_not_kill_unowned_app(self):
        with self.assertRaises(nativeapp.ReviewCancelled):
            self.wait(cancel=lambda: True)
        with self.assertRaises(TimeoutError):
            self.wait()
        self.assertIsNone(self.process.poll())

    def test_app_exit_without_output_is_not_approval(self):
        nativeapp.stop_owned(self.process)
        with self.assertRaises(nativeapp.ReviewCancelled):
            self.wait()

    def test_refuses_invalid_output(self):
        for raw in ('{"revisit_seconds":{"other":300}}',
                    '{"revisit_seconds":{"office":300},"execute":true}',
                    '{"revisit_seconds":{"office":true}}',
                    '{"revisit_seconds":{"office":300,"office":300}}',
                    '{"revisit_seconds":{"office":NaN}}', 'x'*16385):
            with self.subTest(raw=raw[:80]):
                self.output.write_text(raw)
                with self.assertRaises(ValueError):
                    self.wait()
        self.output.unlink()
        self.output.symlink_to(self.source)
        with self.assertRaises(OSError):
            self.wait()

    def test_fresh_output_required_before_launch(self):
        self.output.write_text('{}')
        with mock.patch.object(subprocess, 'Popen') as spawn:
            with self.assertRaisesRegex(ValueError, 'fresh output'):
                nativeapp.review('/missing', '--patrol-schedule-review', self.source, self.output, nativeapp.validate_schedule)
            spawn.assert_not_called()

    def test_review_only_cleans_its_owned_child(self):
        executable = self.root / 'WendyDesktop'
        executable.write_text('#!' + sys.executable + '\nimport json,sys,time\nfrom pathlib import Path\nPath(sys.argv[4]).write_text(json.dumps({"revisit_seconds":{"office":300}}))\ntime.sleep(20)\n')
        executable.chmod(0o700)
        result = nativeapp.review(executable, '--patrol-schedule-review', self.source, self.output, nativeapp.validate_schedule, timeout=3)
        self.assertEqual(result, {'revisit_seconds': {'office': 300}})
        self.assertIsNone(self.process.poll())

    def test_explicit_integrated_bundle_resolution(self):
        binary = self.root / 'WendyDesktop.app/Contents/MacOS/WendyDesktop'
        binary.parent.mkdir(parents=True)
        binary.write_text('test')
        binary.chmod(0o700)
        self.assertEqual(nativeapp.executable(self.root / 'WendyDesktop.app'), str(binary.resolve()))

    def test_offline_selection_bound_to_reviewed_ready_inventory(self):
        entries = [{'profile': {'id': 'kit-a'}, 'state': 'ready'}]
        nativeapp.validate_selection({'action': 'activate', 'id': 'kit-a'}, entries)
        for choice in ({'action': 'activate', 'id': 'kit-b'}, {'action': 'activate', 'id': 'kit-a', 'execute': True}, {'action': 'prepare', 'id': 'kit-a'}):
            with self.assertRaises(ValueError):
                nativeapp.validate_selection(choice, entries)

    def test_tour_targets_bound_to_reviewed_positions(self):
        source = {'rooms': [{'id': 'office'}], 'rendezvous_options': [
            {'id': 'start', 'x': 0, 'y': 0, 'yaw': 0},
            {'id': 'office', 'x': 2, 'y': 1, 'yaw': 0}]}
        value = {'tour': {'method': 'inspect-selected-rooms-once-and-return-v1', 'stops': [
            {'room_id': 'office', 'position': [2, .3, -1], 'yaw': 0}]},
            'return': {'position': [0, .3, 0], 'yaw': 0, 'before_expiry_seconds': 0}}
        nativeapp.validate_schedule(value, source, tour=True)
        value['tour']['stops'][0]['position'][0] = 3
        with self.assertRaisesRegex(ValueError, 'target differs'):
            nativeapp.validate_schedule(value, source, tour=True)


if __name__ == '__main__':
    unittest.main()
