#!/usr/bin/env python3
"""Build/run inherited Stagefiles with an existing CLI on hosted Linux CI.

Creates one unique docker-container BuildKit daemon. Never run this harness on
a developer's Docker installation: that driver needs elevated container access.
No Go compilation, devices, deployment, shared-builder pruning or default changes.
"""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import tarfile
import uuid
import zipfile

# Existing HelloPython lockfile pin. Registry index bytes were independently
# SHA-256 verified on 2026-11-12; this index includes Linux amd64 and arm64.
BASE_REF = 'python:3.11-slim'
BASE_DIGEST = 'sha256:90744cff8f32887f075c47d747a173ff333e9e98801667af93c357fa9f5e28ff'
MODULE = 'usr/local/lib/python3.11/site-packages/stagefixture/__init__.py'


def wheel(directory, version):
    name = f'stagefixture-{version}-py3-none-any.whl'
    dist = f'stagefixture-{version}.dist-info'
    files = {
        'stagefixture/__init__.py': f'VERSION = {version!r}\n'.encode(),
        dist+'/METADATA': f'Metadata-Version: 2.1\nName: stagefixture\nVersion: {version}\n'.encode(),
        dist+'/WHEEL': b'Wheel-Version: 1.0\nGenerator: wendy-regression\nRoot-Is-Purelib: true\nTag: py3-none-any\n',
    }
    records = []
    for path, data in files.items():
        digest = base64.urlsafe_b64encode(hashlib.sha256(data).digest()).decode().rstrip('=')
        records.append(f'{path},sha256={digest},{len(data)}')
    files[dist+'/RECORD'] = ('\n'.join(records)+f'\n{dist}/RECORD,,\n').encode()
    with zipfile.ZipFile(directory/name, 'w') as archive:
        for path, data in files.items():
            entry = zipfile.ZipInfo(path, (2020, 1, 1, 0, 0, 0))
            entry.external_attr = 0o100644 << 16
            archive.writestr(entry, data)
    return name


def stagefile(version, override='child', source='app.txt', copy_from='local', inherited_wheel=True):
    # JSON strings are valid YAML scalars. Everything else is fixed fixture data.
    wheel_copy = f'''      - from: local
        paths: [stagefixture-{version}-py3-none-any.whl]
        dest: /wheels/
''' if inherited_wheel else ''
    return f'''version: 1
stages:
  - name: native
    from: {BASE_REF}
    workdir: /inherited
    env:
      INHERITED: parent
      OVERRIDE: parent
      PIP_NO_INDEX: "1"
      PIP_DISABLE_PIP_VERSION_CHECK: "1"
    copy:
      - from: local
        paths: [marker.txt]
        dest: /inherited/
{wheel_copy}  - name: app
    from: native
    env:
      OVERRIDE: {json.dumps(override)}
    install:
      pip:
        - packages: [/wheels/stagefixture-{version}-py3-none-any.whl]
    copy:
      - from: {copy_from}
        paths: [{json.dumps(source)}]
        dest: /inherited/app.txt
    cmd: [python, -c, "import stagefixture; print(stagefixture.VERSION)"]
'''


def installed_layer(archive_path, diff_ids, version):
    """Inspect docker-save members in memory; never extract untrusted tar paths."""
    matches = []
    with tarfile.open(archive_path) as archive:
        manifest = json.load(archive.extractfile('manifest.json'))
        if len(manifest) != 1 or len(manifest[0]['Layers']) != len(diff_ids):
            raise AssertionError('saved image and inspected layer list disagree')
        for layer_path, digest in zip(manifest[0]['Layers'], diff_ids):
            with tarfile.open(fileobj=archive.extractfile(layer_path), mode='r|*') as layer:
                for member in layer:
                    if member.name.removeprefix('./') == MODULE:
                        data = layer.extractfile(member).read()
                        if data != f'VERSION = {version!r}\n'.encode():
                            raise AssertionError('installed wheel has unexpected contents')
                        matches.append(digest)
    if len(matches) != 1:
        raise AssertionError(f'expected one installed wheel layer, found {matches}')
    return matches[0]


def assert_backend(calls, backend):
    builds = any(call[:2] == ['buildx', 'build'] for call in calls)
    loads = any(call[:1] == ['load'] for call in calls)
    if (builds, loads) != ((True, False) if backend == 'dockerfile' else (False, True)):
        raise AssertionError(f'{backend} took the wrong build/export path: {calls}')


def assert_cached_pip(output):
    vertices = re.findall(r'(?m)^#(\d+) .*pip install[^\n]*stagefixture', output)
    if not any(re.search(r'(?m)^#'+vertex+r' CACHED\s*$', output) for vertex in vertices):
        raise AssertionError('source-only edit did not report a cached pip-install vertex')


class Harness:
    def __init__(self, cli, output):
        self.cli, self.output = cli, output
        self.fixture_id = 'wendy-stagefile-'+uuid.uuid4().hex[:12]
        self.builder = self.fixture_id+'-oci'
        self.docker = shutil.which('docker')
        if not self.docker:
            raise RuntimeError('Docker is required; this gate must not skip')
        self.env = dict(os.environ, WENDY_CONFIG_DIR=str(output/'config'),
                        WENDY_BUILDX_BUILDER=self.fixture_id, BUILDX_BUILDER=self.builder,
                        BUILDKIT_PROGRESS='plain', DOCKER_BUILDKIT='1', NO_COLOR='1')
        for key in ('BUILDKIT_HOST', 'WENDY_BUILDKIT_HOST', 'WENDY_AGENT_SOCKET'):
            self.env.pop(key, None)
        self.images, self.containers, self.builder_created = [], [], False
        self.evidence = {'status': 'running', 'base': BASE_REF+'@'+BASE_DIGEST, 'cli_sha256': hashlib.sha256(cli.read_bytes()).hexdigest(),
                         'builder': self.builder, 'cases': []}

    def run(self, argv, label, cwd=None, check=True, timeout=300):
        log = self.output/(label+'.log')
        with log.open('w') as stream:
            process = subprocess.Popen([str(a) for a in argv], cwd=cwd, env=self.env,
                                       stdin=subprocess.DEVNULL, stdout=stream, stderr=subprocess.STDOUT,
                                       start_new_session=True)
            try:
                code = process.wait(timeout=timeout)
            except BaseException:
                os.killpg(process.pid, signal.SIGTERM)
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait()
                raise
        output = log.read_text()
        if check and code:
            raise RuntimeError(f'{label} exited {code}: {output[-6000:]}')
        return code, output

    def inspect(self, image, label):
        _, output = self.run([self.docker, 'image', 'inspect', image], label)
        return json.loads(output)[0]

    def build(self, project, backend, phase, success=True):
        label = backend+'-'+phase
        calls_path = self.output/'docker-calls.jsonl'
        calls_path.write_text('')
        code, output = self.run([self.cli, 'build', '--device', '127.0.0.1:1', '--builder', 'docker',
                                 '--stagefile-backend', backend, '--dockerfile', 'build.stagefile.yaml'],
                                label, project, check=success, timeout=600)
        calls = [json.loads(line) for line in calls_path.read_text().splitlines()]
        (self.output/(label+'.docker.json')).write_text(json.dumps(calls, indent=2))
        if success:
            assert_backend(calls, backend)
        elif code == 0:
            raise AssertionError(label+' unexpectedly built an image')
        return output

    def observe(self, image, backend, phase, version, text, override):
        label = backend+'-'+phase
        info = self.inspect(image, label+'-inspect')
        # Keep each successfully built image under an owned tag until cleanup,
        # so subsequent builds do not leave our previous results dangling.
        saved_tag = image.rsplit(':', 1)[0]+':'+phase
        self.run([self.docker, 'tag', image, saved_tag], label+'-tag')
        self.images.append(saved_tag)
        expected = {'version': version, 'text': text, 'marker': 'from-native',
                    'cwd': '/inherited', 'inherited': 'parent', 'override': override}
        program = "import json,os,pathlib,stagefixture; print(json.dumps(dict(version=stagefixture.VERSION,text=pathlib.Path('app.txt').read_text().strip(),marker=pathlib.Path('marker.txt').read_text().strip(),cwd=os.getcwd(),inherited=os.environ.get('INHERITED'),override=os.environ.get('OVERRIDE'))))"
        name = self.fixture_id+'-'+label
        self.containers.append(name)
        _, actual = self.run([self.docker, 'run', '--rm', '--name', name, '--network', 'none',
                             '--read-only', image, 'python', '-c', program], label+'-runtime', timeout=60)
        if json.loads(actual) != expected:
            raise AssertionError(f'{label} runtime mismatch: {actual}; expected {expected}')
        archive = self.output/(label+'.tar')
        try:
            self.run([self.docker, 'save', '--output', archive, image], label+'-save')
            pip_layer = installed_layer(archive, info['RootFS']['Layers'], version)
        finally:
            archive.unlink(missing_ok=True)
        result = {'case': label, 'image': info['Id'], 'pip_layer': pip_layer,
                  'layers': info['RootFS']['Layers'], 'runtime': expected}
        self.evidence['cases'].append(result)
        return result

    def execute(self):
        _, info = self.run([self.docker, 'info', '--format', '{{json .}}'], 'docker-info')
        info = json.loads(info)
        arch = {'x86_64': 'amd64', 'aarch64': 'arm64', 'amd64': 'amd64', 'arm64': 'arm64'}.get(info['Architecture'])
        if info['OSType'] != 'linux' or not arch:
            raise RuntimeError('fixture needs a native Linux amd64/arm64 Docker daemon')
        self.run([self.docker, 'buildx', 'version'], 'buildx-version')
        self.run([self.docker, 'buildx', 'create', '--name', self.builder,
                  '--driver', 'docker-container'], 'create-builder')
        self.builder_created = True
        self.run([self.docker, 'buildx', 'inspect', '--bootstrap', self.builder], 'bootstrap-builder')
        # Audit actual Docker subprocesses while forwarding every call unchanged.
        bindir = self.output/'bin'; bindir.mkdir()
        wrapper = bindir/'docker'
        wrapper.write_text('#!'+sys.executable+'\nimport json,os,sys\nwith open('+repr(str(self.output/'docker-calls.jsonl'))+', "a") as out: out.write(json.dumps(sys.argv[1:])+"\\n")\nos.execv('+repr(self.docker)+', ["docker", *sys.argv[1:]])\n')
        wrapper.chmod(0o755)
        self.env['PATH'] = str(bindir)+os.pathsep+self.env['PATH']
        for backend in ('dockerfile', 'llb'):
            project = self.output/backend; project.mkdir()
            app = self.fixture_id+'-'+backend; image = app+':latest'; self.images.append(image)
            (project/'wendy.json').write_text(json.dumps({'appId': app, 'platform': 'linux/'+arch, 'entitlements': []}))
            # Stagefile pins belong in its lockfile, not in the `from` string:
            # the compiler appends the locked digest when generating each FROM.
            (project/'build.stagefile.lock.yaml').write_text(json.dumps({'version': 1, 'images': {BASE_REF: BASE_DIGEST}}))
            (project/'marker.txt').write_text('from-native\n')
            wheel(project, '1.0')
            (project/'app.txt').write_text('first\n')
            source = project/'build.stagefile.yaml'
            source.write_text(stagefile('1.0'))
            self.build(project, backend, 'initial')
            first = self.observe(image, backend, 'initial', '1.0', 'first', 'child')
            (project/'app.txt').write_text('edited\n')
            output = self.build(project, backend, 'source-edit')
            assert_cached_pip(output)
            edited = self.observe(image, backend, 'source-edit', '1.0', 'edited', 'child')
            if edited['pip_layer'] != first['pip_layer']:
                raise AssertionError(backend+' source-only edit changed installed pip layer')
            source.write_text(stagefile('1.0', override='changed'))
            self.build(project, backend, 'env-edit')
            self.observe(image, backend, 'env-edit', '1.0', 'edited', 'changed')
            wheel(project, '2.0')
            source.write_text(stagefile('2.0', override='changed'))
            self.build(project, backend, 'wheel-edit')
            changed = self.observe(image, backend, 'wheel-edit', '2.0', 'edited', 'changed')
            if changed['pip_layer'] == first['pip_layer']:
                raise AssertionError(backend+' changed wheel reused stale installed layer')
            # Fixtures remove required input or name an invalid stage. A failed
            # command must not replace the last successfully loaded image.
            negatives = [('missing-copy', stagefile('2.0', source='absent.txt'), 'absent.txt'),
                         ('unknown-stage', stagefile('2.0', copy_from='unknown_stage'), 'unknown_stage'),
                         ('lost-inheritance', stagefile('2.0', inherited_wheel=False), 'stagefixture-2.0')]
            for phase, content, diagnostic in negatives:
                source.write_text(content)
                output = self.build(project, backend, phase, success=False)
                if diagnostic not in output:
                    raise AssertionError(f'{backend}-{phase} failed before its intended boundary: {output}')
                if self.inspect(image, backend+'-'+phase+'-inspect')['Id'] != changed['image']:
                    raise AssertionError(phase+' replaced the previous image despite failing')
                self.evidence['cases'].append({'case': backend+'-'+phase, 'expected_failure': diagnostic})
        self.evidence['status'] = 'passed'

    def cleanup(self):
        errors = []
        def clean(argv, label, required=False, timeout=30):
            try:
                code, output = self.run(argv, label, check=False, timeout=timeout)
                if required and code:
                    errors.append(label+': '+output)
            except Exception as error:
                errors.append(label+': '+str(error))
        for name in self.containers:
            clean([self.docker, 'rm', '--force', name], 'cleanup-'+name)
        for image in self.images:
            clean([self.docker, 'image', 'rm', image], 'cleanup-'+image.replace(':', '-'))
        if self.builder_created:
            clean([self.docker, 'buildx', 'rm', '--force', self.builder], 'cleanup-builder', required=True, timeout=120)
        self.evidence['cleanup_errors'] = errors
        (self.output/'evidence.json').write_text(json.dumps(self.evidence, indent=2)+'\n')
        if errors:
            raise RuntimeError('owned builder cleanup failed: '+str(errors))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('cli', type=Path)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    if (os.environ.get('GITHUB_ACTIONS') != 'true'
            or os.environ.get('RUNNER_ENVIRONMENT') != 'github-hosted'
            or not sys.platform.startswith('linux')):
        parser.error('live fixture runs only on hosted Linux CI; do not create a privileged local BuildKit daemon')
    cli = args.cli.resolve(strict=True)
    if not os.access(cli, os.X_OK):
        parser.error('CLI is not executable')
    output = args.output.resolve(); output.mkdir(parents=True, exist_ok=False)
    harness = Harness(cli, output)
    try:
        harness.execute()
    except Exception as error:
        harness.evidence.update(status='failed', error=str(error))
        raise
    finally:
        harness.cleanup()
    print('PASS Stagefile release binary: both backends, inheritance, rebuilds and failure controls')


if __name__ == '__main__':
    main()
