import json
import unittest
from contextlib import redirect_stdout
from io import StringIO
from unittest.mock import patch

import check_release


class MonorepoCompatibilityTests(unittest.TestCase):
    def test_only_core_tags_select_core_snapshots(self):
        snapshot = {'version': '0.1.0-seed.1', 'c': {'functions': []}}
        previous = {'version': '0.0.1', 'c': {'functions': []}}
        output = StringIO()
        with patch.object(check_release, 'check_version'), \
                patch.object(check_release, 'base_version', return_value=snapshot['version']), \
                patch('pathlib.Path.read_text', return_value=json.dumps(snapshot)), \
                patch.object(check_release, 'identity', return_value={'channel': 'pr'}), \
                patch.object(check_release.subprocess, 'check_output', side_effect=[
                    'v2026.10.24\nv9.0.0\ngo/babel/v8.0.0\ngo/wendy-core/v0.0.1\n',
                    json.dumps(previous).encode(),
                ]) as git, redirect_stdout(output):
            check_release.main()
        self.assertEqual(git.call_args.args[0], [
            'git', 'show', 'go/wendy-core/v0.0.1:go/wendy-core/documentation/versions/0.0.1.json'])
        self.assertIn('checked against go/wendy-core/v0.0.1', output.getvalue())
