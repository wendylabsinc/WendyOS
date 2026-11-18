#!/usr/bin/env python3
"""Execute Mac toolchain setup against disposable command and app fixtures."""

import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
SELECT = ROOT / '.github/scripts/select-macos-ci-xcode.sh'
PROVISION = ROOT / '.github/scripts/provision-macos-ci.sh'


class MacOSSetupTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.apps = self.root / 'Applications with spaces'
        self.apps.mkdir()
        self.bin = self.root / 'bin'
        self.bin.mkdir()
        self.output = self.root / 'environment'
        self.calls = self.root / 'calls'
        self.env = dict(os.environ, XCODES_DIRECTORY=str(self.apps),
                        GITHUB_ENV=str(self.output), CALLS=str(self.calls),
                        PATH=str(self.bin) + os.pathsep + os.environ['PATH'])
        self.command(self.bin / 'xcrun', 'printf "%s:%s\\n" "$DEVELOPER_DIR" "$*" >> "$CALLS"\nexit "${SDK_STATUS:-0}"')

    def command(self, path, body):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text('#!/bin/bash\n' + body + '\n')
        path.chmod(0o755)

    def app(self, version):
        developer = self.apps / f'Xcode-{version}.app/Contents/Developer'
        self.command(developer / 'usr/bin/xcodebuild', f'echo "Xcode {version}"\necho "Build version fixture"')
        return developer

    def select(self, version, **env):
        return subprocess.run(['bash', str(SELECT), version], env=dict(self.env, **env),
                              capture_output=True, text=True, timeout=10)

    def test_selects_exact_version_without_changing_global_selection(self):
        self.app('26.3')
        wanted = self.app('26.5.0')
        self.command(self.bin / 'xcode-select', 'echo unexpected-global-selection >> "$CALLS"\nexit 99')
        result = self.select('26.5', DEVELOPER_DIR='/wrong/global/selection')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.output.read_text(), f'DEVELOPER_DIR={wanted}\n')
        calls = self.calls.read_text().splitlines()
        self.assertEqual(len(calls), 2)
        self.assertTrue(all(line.startswith(str(wanted) + ':') for line in calls))

    def test_missing_version_reports_provisioning_command(self):
        self.app('26.3')
        result = self.select('26.5')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('provision-macos-ci.sh 26.5', result.stderr)
        self.assertFalse(self.output.exists())

    def test_missing_sdk_does_not_export_unusable_xcode(self):
        self.app('26.5')
        result = self.select('26.5', SDK_STATUS='1')
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.output.exists())

    def test_invalid_version_is_rejected_before_running_tools(self):
        result = self.select('26.5; touch unexpected')
        self.assertEqual(result.returncode, 2)
        self.assertFalse(self.calls.exists())

    def test_base_installs_only_missing_packages_and_uses_brew_python(self):
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
                                         PROVISION=str(PROVISION), GITHUB_PATH=str(path_output)), timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.calls.read_text().splitlines(),
                         ['installed-python', 'managed-git', 'managed-python'])
        self.assertEqual(path_output.read_text().splitlines()[-1], str(python_bin))
        self.assertIn('HOMEBREW_NO_ASK=1', self.output.read_text())


if __name__ == '__main__':
    unittest.main()
