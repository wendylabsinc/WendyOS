"""Exercise the release pin script's provenance and atomic-publication boundary.

The gh fixture validates the actual signer selectors passed by the script and
models gh rejecting metadata. It does not substitute for GitHub's cryptographic
attestation verifier.
"""
import hashlib
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
TAG = "2026.11.12-010203"


class PinProvenanceTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.work = Path(self.temp.name)
        self.launcher = self.work / "wendy"
        shutil.copy2(ROOT / "plugins/wendy/scripts/wendy", self.launcher)
        self.before = self.launcher.read_bytes()
        self.mode = stat.S_IMODE(self.launcher.stat().st_mode)
        self.bin = self.work / "bin"
        self.bin.mkdir()
        self.log = self.work / "verify.jsonl"
        self.env = dict(os.environ, PATH=str(self.bin)+os.pathsep+os.environ['PATH'],
                        PIN_FIXTURE_LOG=str(self.log), PIN_FIXTURE_REJECT="", PIN_FIXTURE_INTERRUPT="")
        self.shim("curl", '''
args = sys.argv[1:]
output = Path(args[args.index('-o')+1])
url = args[-1]
payload = ('asset:' + url.rsplit('/', 1)[-1]).encode()
if os.environ.get('PIN_FIXTURE_INTERRUPT') in url and os.environ.get('PIN_FIXTURE_INTERRUPT'):
    output.write_bytes(payload[:5])
    sys.exit(18)
output.write_bytes(payload)
''')
        self.shim("gh", '''
args = sys.argv[1:]
with open(os.environ['PIN_FIXTURE_LOG'], 'a') as log:
    log.write(json.dumps(args)+'\\n')
assert args[:2] == ['attestation', 'verify'], args
repo = args[args.index('--repo')+1]
workflow = args[args.index('--signer-workflow')+1]
assert repo == 'wendylabsinc/WendyOS', args
assert workflow == 'wendylabsinc/WendyOS/.github/workflows/build.yml', args
assert Path(args[2]).is_file()
reject = os.environ.get('PIN_FIXTURE_REJECT')
metadata_repo = 'untrusted/repository' if reject == 'repository' else repo
metadata_workflow = 'wendylabsinc/WendyOS/.github/workflows/untrusted.yml' if reject == 'workflow' else workflow
sys.exit(0 if (metadata_repo, metadata_workflow) == (repo, workflow) else 1)
''')

    def shim(self, name, body):
        path = self.bin/name
        path.write_text('#!'+sys.executable+'\nimport os,sys,json\nfrom pathlib import Path\n'+body)
        path.chmod(0o755)

    def run_pin(self):
        return subprocess.run(['sh', str(ROOT/'scripts/pin-cli.sh'), '--launcher', str(self.launcher),
                               '--min', TAG, TAG], env=self.env, cwd=self.work,
                              capture_output=True, text=True, timeout=20)

    def test_verified_assets_publish_all_platform_hashes(self):
        result = self.run_pin()
        self.assertEqual(result.returncode, 0, result.stderr)
        calls = [json.loads(line) for line in self.log.read_text().splitlines()]
        self.assertEqual(len(calls), 5)
        output = self.launcher.read_text()
        for platform in ['darwin_arm64', 'linux_amd64', 'linux_arm64', 'windows_amd64', 'windows_arm64']:
            extension = 'zip' if platform.startswith('windows') else 'tar.gz'
            name = 'wendy-cli-'+platform.replace('_','-')+'-'+TAG+'.'+extension
            digest = hashlib.sha256(('asset:'+name).encode()).hexdigest()
            self.assertIn('SHA256_'+platform+'="'+digest+'"', output)
        self.assertEqual(stat.S_IMODE(self.launcher.stat().st_mode), self.mode)

    def test_wrong_signer_repository_or_workflow_preserves_pin(self):
        for rejected in ['repository', 'workflow']:
            with self.subTest(rejected=rejected):
                self.env['PIN_FIXTURE_REJECT'] = rejected
                result = self.run_pin()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('failed build-provenance verification', result.stderr)
                self.assertEqual(self.launcher.read_bytes(), self.before)
                self.assertEqual(stat.S_IMODE(self.launcher.stat().st_mode), self.mode)

    def test_interrupted_later_asset_preserves_entire_pin(self):
        self.env['PIN_FIXTURE_INTERRUPT'] = 'linux-arm64'
        result = self.run_pin()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('could not download', result.stderr)
        self.assertEqual(len(self.log.read_text().splitlines()), 2)
        self.assertEqual(self.launcher.read_bytes(), self.before)
        self.assertEqual(stat.S_IMODE(self.launcher.stat().st_mode), self.mode)


if __name__ == '__main__':
    unittest.main()
