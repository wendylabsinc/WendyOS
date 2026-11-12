"""Exercise the actual release gate with Git refs and attestation-policy fixtures.

The gh shim models verified metadata and API failures; cryptographic verification
is delegated to gh in production, not reimplemented or claimed by this fixture.
"""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import textwrap
import unittest

ROOT = Path(__file__).resolve().parents[2]
WORKFLOW = ROOT / '.github/workflows/build.yml'


def tag_step():
    lines = WORKFLOW.read_text().splitlines()
    start = lines.index('      - name: Tag the built commit')
    run = next(i for i in range(start, len(lines)) if lines[i] == '        run: |')
    end = next(i for i in range(run + 1, len(lines)) if lines[i] and not lines[i].startswith('          '))
    return textwrap.dedent('\n'.join(lines[run + 1:end]))


class BuiltCommitTagTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.repo = self.root / 'repo'
        subprocess.run(['git', 'init', '-q', '-b', 'main', str(self.repo)], check=True)
        self.git('config', 'user.email', 'fixture@example.invalid')
        self.git('config', 'user.name', 'Fixture')
        self.git('commit', '--allow-empty', '-qm', 'Built commit')
        self.built = self.git('rev-parse', 'HEAD')
        self.git('commit', '--allow-empty', '-qm', 'Merged while build ran')
        self.latest = self.git('rev-parse', 'HEAD')
        self.bin = self.root / 'bin'
        self.bin.mkdir()
        self.log = self.root / 'calls.jsonl'
        helper = ROOT / '.github/scripts/verify-release.py'
        if helper.exists():
            target = self.root / '.release-source/.github/scripts'
            target.mkdir(parents=True)
            shutil.copy2(helper, target / helper.name)
        self.files = []
        metadata = {}
        for name in ['wendy agent.tar.gz', 'cli.zip', 'cli.msi', 'agent.deb', 'agent.rpm', 'source.spdx.json']:
            p = self.root / 'release-assets' / name
            p.parent.mkdir(exist_ok=True)
            p.write_bytes(('built from '+self.built+' '+name).encode())
            self.files.append(str(p.relative_to(self.root)))
            metadata[str(p.relative_to(self.root))] = {'source': self.built, 'digest': hashlib.sha256(p.read_bytes()).hexdigest(), 'repository': 'fixture/repository', 'workflow': 'fixture/repository/.github/workflows/build.yml'}
        self.metadata = self.root / 'attestations.json'
        self.metadata.write_text(json.dumps(metadata))
        gh = self.bin / 'gh'
        gh.write_text('#!'+sys.executable+'\n'+textwrap.dedent('''\
            import hashlib,json,os,subprocess,sys
            from pathlib import Path
            args=sys.argv[1:]
            with open(os.environ['FIXTURE_LOG'],'a') as f:f.write(json.dumps(args)+'\\n')
            git=['git','-C',os.environ['FIXTURE_REPO']]
            def out(*a):return subprocess.check_output(git+list(a),text=True).strip()
            if args[:2]==['attestation','verify']:
                metadata=json.loads(Path(os.environ['FIXTURE_METADATA']).read_text()).get(args[2])
                def option(name):return args[args.index(name)+1] if name in args else None
                good=metadata and metadata['digest']==hashlib.sha256(Path(args[2]).read_bytes()).hexdigest()
                good=good and metadata['source']==option('--source-digest') and metadata['repository']==option('--repo') and metadata['workflow']==option('--signer-workflow')
                if not good:print('attestation policy failed',file=sys.stderr)
                sys.exit(0 if good else 1)
            assert args[0]=='api',args
            endpoint=args[1];prefix='repos/fixture/repository/git/'
            assert endpoint.startswith(prefix),endpoint
            mode=os.environ.get('FAIL_API','')
            if mode=='all':sys.exit(1)
            if endpoint.startswith(prefix+'ref/tags/'):
                ref='refs/tags/'+endpoint.removeprefix(prefix+'ref/tags/')
                result=subprocess.run(git+['show-ref','--verify','--hash',ref],capture_output=True,text=True)
                if result.returncode:sys.exit(1)
                sha=result.stdout.strip();print(json.dumps({'object':{'type':out('cat-file','-t',sha),'sha':sha}}));sys.exit(0)
            if endpoint.startswith(prefix+'tags/'):
                sha=endpoint.rsplit('/',1)[1];raw=out('cat-file','-p',sha).splitlines()
                print(json.dumps({'object':{'type':raw[1].split()[1],'sha':raw[0].split()[1]}}));sys.exit(0)
            assert endpoint==prefix+'refs',endpoint
            fields=dict(args[i+1].split('=',1) for i,arg in enumerate(args) if arg=='-f')
            if mode=='create':sys.exit(1)
            if mode.startswith('race-'):
                sha=os.environ['FIXTURE_BUILT'] if mode=='race-correct' else os.environ['FIXTURE_LATEST']
                subprocess.run(git+['update-ref',fields['ref'],sha,'0'*40],check=True);sys.exit(1)
            subprocess.run(git+['update-ref',fields['ref'],fields['sha'],'0'*40],check=True)
            print(json.dumps({'object':{'type':'commit','sha':fields['sha']}}))
            '''))
        gh.chmod(0o755)

    def git(self, *args):
        return subprocess.check_output(['git', '-C', str(self.repo), *args], text=True).strip()

    def run_step(self, **overrides):
        env = dict(os.environ, PATH=str(self.bin)+os.pathsep+os.environ['PATH'],
                   GITHUB_REPOSITORY='fixture/repository', GITHUB_SHA=self.built,
                   FILES='\n'.join(self.files), TAG='2026.11.12', FIXTURE_REPO=str(self.repo),
                   FIXTURE_LOG=str(self.log), FIXTURE_METADATA=str(self.metadata),
                   FIXTURE_BUILT=self.built, FIXTURE_LATEST=self.latest)
        env.update(overrides)
        # The marker models the following release action: bash -e must stop first.
        return subprocess.run(['bash','-e','-c',tag_step()+'\nprintf published > publication'],
                              env=env,cwd=self.root,text=True,capture_output=True,timeout=20)

    def assert_refused(self, result):
        self.assertNotEqual(result.returncode,0,result.stdout+result.stderr)
        self.assertFalse((self.root/'publication').exists())

    def calls(self):
        return [json.loads(l) for l in self.log.read_text().splitlines()]

    def test_tags_built_sha_after_main_advances_and_verifies_every_asset(self):
        result=self.run_step()
        self.assertEqual(result.returncode,0,result.stdout+result.stderr)
        self.assertEqual(self.git('rev-parse','2026.11.12'),self.built)
        self.assertEqual(self.git('rev-parse','main'),self.latest)
        verified=[a[2] for a in self.calls() if a[:2]==['attestation','verify']]
        self.assertEqual(verified,self.files)
        self.assertTrue((self.root/'publication').exists())

    def test_retry_accepts_only_existing_tag_at_built_commit(self):
        for annotated in [False,True]:
            with self.subTest(annotated=annotated):
                args=['tag']+(['-a','-m','fixture'] if annotated else [])+['2026.11.12',self.built]
                self.git(*args)
                result=self.run_step()
                self.assertEqual(result.returncode,0,result.stdout+result.stderr)
                self.assertEqual(self.git('rev-parse','2026.11.12^{}'),self.built)
                self.git('tag','-d','2026.11.12')

    def test_existing_wrong_lightweight_or_annotated_tag_stops_publication(self):
        for annotated in [False,True]:
            with self.subTest(annotated=annotated):
                self.git(*(['tag']+(['-a','-m','fixture'] if annotated else [])+['2026.11.12',self.latest]))
                self.assert_refused(self.run_step())
                self.assertEqual(self.git('rev-parse','2026.11.12^{}'),self.latest)
                self.git('tag','-d','2026.11.12')

    def test_api_or_creation_failure_stops_publication(self):
        for mode in ['all','create']:
            with self.subTest(mode=mode):
                self.assert_refused(self.run_step(FAIL_API=mode))
                self.assertEqual(self.git('tag','--list'),'')

    def test_concurrent_creator_must_point_at_same_commit(self):
        self.assert_refused(self.run_step(FAIL_API='race-wrong'))
        self.assertEqual(self.git('rev-parse','2026.11.12'),self.latest)
        self.git('tag','-d','2026.11.12')
        result=self.run_step(FAIL_API='race-correct')
        self.assertEqual(result.returncode,0,result.stdout+result.stderr)
        self.assertEqual(self.git('rev-parse','2026.11.12'),self.built)

    def test_wrong_source_signer_or_bytes_stops_before_tag_creation(self):
        before=self.metadata.read_text()
        for field,value in [('source',self.latest),('repository','other/repo'),('workflow','fixture/repository/.github/workflows/other.yml'),('digest','0'*64)]:
            with self.subTest(field=field):
                data=json.loads(before);data[self.files[-1]][field]=value;self.metadata.write_text(json.dumps(data))
                self.assert_refused(self.run_step())
                self.assertEqual(self.git('tag','--list'),'')
        self.metadata.write_text(before)
        (self.root/self.files[-1]).write_bytes(b'changed after attestation')
        self.assert_refused(self.run_step())
        self.assertEqual(self.git('tag','--list'),'')

    def test_empty_or_missing_assets_cannot_publish(self):
        self.assert_refused(self.run_step(FILES=''))
        (self.root/self.files[-1]).unlink()
        self.assert_refused(self.run_step())
        self.assertEqual(self.git('tag','--list'),'')


if __name__=='__main__':unittest.main()
