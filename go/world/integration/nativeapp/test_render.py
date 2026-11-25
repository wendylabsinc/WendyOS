"""Native export failures are terminal even when WendyDesktop stays open."""
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest import mock

from integration.nativeapp import render


class RenderReceiptTests(unittest.TestCase):
    def run_failure(self, receipt_name, receipt, expected):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            binary = root / 'WendyDesktop'
            binary.write_text('#!' + sys.executable + '\nimport json,os,sys,time\nfrom pathlib import Path\n'
                'out=Path(sys.argv[sys.argv.index("--world-render-output")+1])\n'
                '(out/"owned.pid").write_text(str(os.getpid()))\n'
                '(out/' + repr(receipt_name) + ').write_text(' + repr(json.dumps(receipt)) + ')\n'
                'time.sleep(20)\n')
            binary.chmod(0o700)
            output = root / 'export'
            argv = ['render', '--timeline', str(root), '--out', str(output), '--app', str(binary), '--duration', '1', '--fps', '1']
            with mock.patch.object(sys, 'argv', argv), mock.patch.object(render, 'timeline_inputs', return_value=(b'{"run_id":"test"}', b'{}\n', [{}], {})):
                with self.assertRaisesRegex((RuntimeError, ValueError), expected):
                    render.main()
            pid = int((output / 'owned.pid').read_text())
            with self.assertRaises(ProcessLookupError):
                os.kill(pid, 0)
            self.assertFalse((output / 'result.json').exists())

    def test_native_error_receipt_fails_without_waiting_for_app_exit(self):
        self.run_failure('render-error.json', {'error': 'fixture render failure'}, 'fixture render failure')

    def test_completion_receipt_must_match_requested_export(self):
        self.run_failure('render.json', {'frame_count': 1, 'fps': 2, 'run_id': 'test'}, 'receipt differs')


if __name__ == '__main__':
    unittest.main()
