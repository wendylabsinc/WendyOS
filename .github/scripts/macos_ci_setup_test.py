#!/usr/bin/env python3
"""Execute Mac base-tool setup against disposable command fixtures."""

import os
from pathlib import Path
import subprocess
import tempfile
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[2]
PROVISION = ROOT / '.github/scripts/provision-macos-ci.sh'


class MacOSSetupTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.bin = self.root / 'bin'
        self.bin.mkdir()
        self.output = self.root / 'environment'
        self.calls = self.root / 'calls'
        self.env = dict(os.environ,
                        GITHUB_ENV=str(self.output), CALLS=str(self.calls),
                        PATH=str(self.bin) + os.pathsep + os.environ['PATH'])

    def command(self, path, body):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text('#!/bin/bash\n' + body + '\n')
        path.chmod(0o755)

    def test_build_jobs_use_active_xcode_and_homebrew_python(self):
        for filename in ('build.yml', 'go-tests.yml', 'swift-tests.yml'):
            workflow = yaml.safe_load((ROOT / '.github/workflows' / filename).read_text())
            for name, job in workflow['jobs'].items():
                steps = job.get('steps', [])
                if not any(step.get('uses') == './.github/actions/setup-macos-ci' for step in steps):
                    continue
                with self.subTest(workflow=filename, job=name):
                    self.assertNotIn('XCODE_VERSION', workflow.get('env', {}))
                    for step in steps:
                        self.assertNotIn('setup-xcode', step.get('uses', ''))
                        self.assertNotIn('setup-python', step.get('uses', ''))
                        self.assertNotIn('xcode-version', step.get('with', {}))
                        self.assertNotIn('DEVELOPER_DIR', step.get('env', {}))

    def test_base_installs_only_missing_packages_and_uses_brew_python(self):
        for name in ('xcode-select', 'xcodebuild', 'xcrun', 'xcodes'):
            self.command(self.bin / name, 'echo unexpected-xcode-call >> "$CALLS"\nexit 99')
        prefix = self.root / 'brew'
        python_bin = prefix / 'opt/python@3.12/libexec/bin'
        self.command(python_bin / 'python3', 'echo managed-python >> "$CALLS"')
        self.command(prefix / 'bin/git', 'echo managed-git >> "$CALLS"')
        # Exported Bash functions keep the fixture independent of host Homebrew.
        driver = r'''
uname() { echo Darwin; }
id() { echo 501; }
brew() {
  [[ ${HOMEBREW_NO_ASK:-} == 1 ]] || { echo 'would prompt for confirmation' >&2; return 98; }
  case "$*" in
    --version) echo Homebrew-fixture ;;
    'list --versions git') return 0 ;;
    'list --versions python@3.12') return 1 ;;
    'install python@3.12') echo installed-python >> "$CALLS" ;;
    --prefix) echo "$FIXTURE_PREFIX" ;;
    '--prefix python@3.12') echo "$FIXTURE_PREFIX/opt/python@3.12" ;;
    *) echo "unexpected brew call: $*" >&2; return 99 ;;
  esac
}
export -f uname id brew
bash "$PROVISION" --base
'''
        path_output = self.root / 'path'
        result = subprocess.run(['bash', '-c', driver], capture_output=True, text=True,
                                env=dict(self.env, FIXTURE_PREFIX=str(prefix),
                                         PROVISION=str(PROVISION), GITHUB_PATH=str(path_output),
                                         DEVELOPER_DIR='/existing/Xcode.app/Contents/Developer'), timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.calls.read_text().splitlines(),
                         ['installed-python', 'managed-git', 'managed-python'])
        self.assertEqual(path_output.read_text().splitlines()[-1], str(python_bin))
        self.assertEqual(self.output.read_text(), 'HOMEBREW_NO_ASK=1\n')


if __name__ == '__main__':
    unittest.main()
