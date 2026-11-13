#!/usr/bin/env python3
"""Upload release packages and require exact native APT/DNF repository downloads.

Runs only in the existing authorized GitHub release job. It never installs Wendy,
creates a cloud VM, or grants new credentials. Containers contain no credentials
while installing the Google helpers. The existing short-lived WIF file is copied
into a private read-only mount only for the native repository acceptance phase.
"""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import platform
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import uuid

HERE = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location('owned', HERE / 'run-owned-media-test.py')
OWNED = importlib.util.module_from_spec(SPEC); SPEC.loader.exec_module(OWNED)
CONSUMER = HERE / 'consume-linux-repository.py'
# Official Docker Library amd64 child manifests. No emulation: native clients
# resolve/download both architectures, without executing candidate binaries.
IMAGES = {
    'deb': 'docker.io/library/ubuntu@sha256:77eb101c607d6e3d4f5ad0c8a02c2136c689b98aba1f359e6502ba517e5c57ec',
    'rpm': 'docker.io/library/rockylinux@sha256:d644d203142cd5b54ad2a83a203e1dee68af2229f8fe32f52a30c6e1d3c3a9e0',
}
APT_PREPARE = r'''set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
apt-get update --error-on=any
apt-get install -y --no-install-recommends python3 ca-certificates curl gnupg
curl --fail --silent --show-error --proto '=https' --max-time 30 https://packages.cloud.google.com/apt/doc/apt-key.gpg -o /tmp/google-apt.asc
gpg --batch --dearmor -o /usr/share/keyrings/google-apt.gpg /tmp/google-apt.asc
printf '%s\n' 'deb [signed-by=/usr/share/keyrings/google-apt.gpg] https://packages.cloud.google.com/apt apt-transport-artifact-registry-stable main' > /etc/apt/sources.list.d/google-transport.list
apt-get update --error-on=any
apt-get install -y --no-install-recommends apt-transport-artifact-registry
curl --fail --silent --show-error --proto '=https' --max-time 30 https://us-central1-apt.pkg.dev/doc/repo-signing-key.gpg -o /tmp/candidate-repository.asc
gpg --batch --dearmor -o /usr/share/keyrings/candidate-repository.gpg /tmp/candidate-repository.asc
dpkg-query -W > /work/deb-dependencies.txt
sha256sum /usr/lib/apt/methods/ar+https /usr/share/keyrings/candidate-repository.gpg > /work/deb-helper-hashes.txt
test ! -e /usr/bin/wendy && test ! -e /usr/bin/wendy-agent
'''
RPM_PREPARE = r'''set -euo pipefail
dnf -y --setopt=install_weak_deps=False install python3 ca-certificates curl-minimal dnf-plugins-core
curl --fail --silent --show-error --proto '=https' --max-time 30 https://packages.cloud.google.com/yum/doc/rpm-package-key.gpg -o /tmp/google-rpm.asc
rpm --import /tmp/google-rpm.asc
cat > /etc/yum.repos.d/google-transport.repo <<'EOF'
[ar-plugin]
name=Google Artifact Registry DNF helper
baseurl=https://packages.cloud.google.com/yum/repos/dnf-plugin-artifact-registry-el9-stable
enabled=1
gpgcheck=1
repo_gpgcheck=0
sslverify=1
gpgkey=file:///tmp/google-rpm.asc
EOF
dnf -y --setopt=install_weak_deps=False install dnf-plugin-artifact-registry
rpm -qa --qf '%{NAME} %{VERSION}-%{RELEASE} %{ARCH}\n' | sort > /work/rpm-dependencies.txt
sha256sum /usr/libexec/ar-token > /work/rpm-helper-hashes.txt
test ! -e /usr/bin/wendy && test ! -e /usr/bin/wendy-agent
'''


def require(ok, message):
    if not ok:
        raise RuntimeError(message)


def sha(path):
    digest = hashlib.sha256()
    with Path(path).open('rb') as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b''):
            digest.update(block)
    return digest.hexdigest()


def validate_packages(packages, version):
    require(len(packages) == 8, 'require eight native package receipts')
    expected = {(fmt, name, arch) for fmt in ('deb', 'rpm') for name in ('wendy', 'wendy-agent')
                for arch in ('amd64', 'arm64')}
    require({(p['format'], p['name'], p['architecture']) for p in packages} == expected,
            'candidate identities incomplete or duplicate')
    for p in packages:
        require(p['version'] == (version if p['format'] == 'deb' else version.replace('-', '_') + '-1'),
                'native package version disagrees with release version')
        require(Path(p['filename']).name == p['filename'] and re.fullmatch(r'[A-Za-z0-9_.+~:-]+', p['filename']),
                'invalid package filename')
        require(re.fullmatch(r'[0-9a-f]{64}', p['sha256']) and 0 < p['size'] < 1024 ** 3, 'invalid package receipt')


def validate_uploads(packages, receipts):
    require(len(receipts) == 8, 'UPLOAD_RECEIPTS: missing upload outcome')
    require({(r['format'], r['filename'], r['sha256']) for r in receipts if r['exit_code'] == 0} ==
            {(p['format'], p['filename'], p['sha256']) for p in packages},
            'UPLOAD_RECEIPTS: successful upload hashes differ from candidates')


def validate_wif(path):
    require(path.is_file() and not path.is_symlink(), 'existing WIF credential must be a regular file')
    require(path.stat().st_size <= 65536, 'unexpected WIF credential size')
    data = json.loads(path.read_text())
    require(data.get('type') == 'external_account', 'require existing short-lived WIF credentials, never a private key')
    source = data.get('credential_source', {})
    require(isinstance(source.get('url'), str) and source['url'].startswith('https://') and
            'file' not in source and 'executable' not in source, 'require action-produced HTTPS WIF token source')
    return data


def hosted_guard():
    require(sys.platform == 'linux' and platform.machine() == 'x86_64', 'requires native hosted Linux amd64')
    require(os.environ.get('GITHUB_ACTIONS') == 'true' and os.environ.get('RUNNER_ENVIRONMENT') == 'github-hosted',
            'refusing publication outside the authorized hosted release job')
    require(os.environ.get('GITHUB_EVENT_NAME') in ('push', 'workflow_dispatch') and
            os.environ.get('WENDY_STABLE_RELEASE') == 'true', 'requires existing stable-release publication gate')
    require(re.fullmatch(r'[0-9a-f]{40}', os.environ.get('GITHUB_SHA', '')), 'missing immutable source revision')
    for tool in ('docker', 'gcloud', 'git'):
        require(shutil.which(tool), 'missing release tool ' + tool)


def drain_container(name, owner, env):
    inspected = subprocess.run(['docker', 'inspect', name], env=env, capture_output=True, text=True, timeout=15)
    require(inspected.returncode == 0, 'container creation/drain is unproven')
    record = json.loads(inspected.stdout)[0]
    require(record['Config']['Labels'].get('wendy.repository-owner') == owner, 'container ownership changed')
    container = record['Id']
    removed = subprocess.run(['docker', 'rm', '--force', container], env=env, capture_output=True, text=True, timeout=30)
    require(removed.returncode == 0, 'owned container removal failed')
    listing = subprocess.run(['docker', 'ps', '--all', '--quiet', '--no-trunc', '--filter', 'id=' + container],
                             env=env, capture_output=True, text=True, timeout=15)
    require(listing.returncode == 0 and not listing.stdout.strip(), 'owned container remains')


def run(args):
    hosted_guard()
    require(re.fullmatch(r'[a-z][a-z0-9-]{4,61}[a-z0-9]', args.project), 'invalid project ID')
    require(re.fullmatch(r'[0-9][A-Za-z0-9.+_~-]*', args.version), 'invalid release version')
    source = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=HERE, text=True, timeout=10).strip()
    require(source == os.environ['GITHUB_SHA'], 'checked-out source differs from published release')
    credentials = Path(os.environ.get('GOOGLE_APPLICATION_CREDENTIALS', '')).resolve(strict=True)
    validate_wif(credentials)  # Validate without logging or retaining its contents.
    packages_dir = args.packages.resolve(strict=True)
    output = args.evidence.resolve(); output.mkdir(mode=0o700, parents=True, exist_ok=False)
    work = Path(tempfile.mkdtemp(prefix='wendy-repo-consumers-', dir=os.environ.get('RUNNER_TEMP')))
    for name in ('credential', 'docker-config'):
        (work / name).mkdir(mode=0o700)
    env = dict(os.environ, DOCKER_CONFIG=str(work / 'docker-config'))
    report = dict(status='failed', source=source, run_id=os.environ.get('GITHUB_RUN_ID'),
                  images=IMAGES, uploads=[], containers={}, source_sha256={p.name: sha(p) for p in
                  (Path(__file__), CONSUMER, HERE / 'run-owned-media-test.py')})
    owned_containers = []
    interrupted = 0

    def stop(signum, frame):
        nonlocal interrupted
        interrupted = 128 + signum

    previous = {sig: signal.signal(sig, stop) for sig in (signal.SIGINT, signal.SIGTERM)}
    counter = 0
    deadline = time.monotonic() + 2100

    def execute(command, name, timeout=180):
        nonlocal counter
        require(not interrupted, 'release validation interrupted')
        timeout = min(timeout, deadline - time.monotonic())
        require(timeout > 0, 'release validation exceeded its 35-minute deadline')
        counter += 1
        log = output / f'{counter:02d}-{name}.log'
        with log.open('w') as stream:
            code = OWNED.run(command, timeout, env=env, stdout=stream)
        require(code == 0, f'{name} failed with exit {code}; see {log.name}')
        require(not interrupted, 'release validation interrupted')
        return log

    try:
        for fmt, image in IMAGES.items():
            execute(['docker', 'pull', '--platform=linux/amd64', image], fmt + '-pull', 240)
            name = 'wendy-repo-' + uuid.uuid4().hex
            owner = uuid.uuid4().hex
            owned_containers.append((name, owner))  # Before create, including failed/interrupted create.
            created = execute(['docker', 'create', '--name', name, '--label', 'wendy.repository-owner=' + owner,
                               '--platform=linux/amd64', '--security-opt=no-new-privileges', '--pids-limit=128',
                               '--memory=2g', '--cpus=2', '--cap-drop=ALL', '--cap-add=CHOWN', '--cap-add=DAC_OVERRIDE',
                               '--cap-add=FOWNER', '--cap-add=SETUID', '--cap-add=SETGID',
                               '--mount', f'type=bind,src={CONSUMER},dst=/consumer.py,readonly',
                               '--mount', f'type=bind,src={packages_dir / fmt},dst=/packages,readonly',
                               '--mount', f'type=bind,src={work / "credential"},dst=/credential,readonly',
                               '--mount', f'type=bind,src={output},dst=/work',
                               image, 'sleep', 'infinity'], fmt + '-create', 30)
            container = created.read_text().strip()
            require(re.fullmatch(r'[0-9a-f]{64}', container), 'missing exact container ID')
            report['containers'][fmt] = container
            execute(['docker', 'start', container], fmt + '-start', 30)
            execute(['docker', 'exec', container, 'bash', '-c', APT_PREPARE if fmt == 'deb' else RPM_PREPARE],
                    fmt + '-prepare', 420)
            execute(['docker', 'exec', container, 'python3', '/consumer.py', 'inventory', fmt,
                     '/packages', '/work/' + fmt + '-packages.json'], fmt + '-inventory', 60)
        packages = sum([json.loads((output / (fmt + '-packages.json')).read_text()) for fmt in IMAGES], [])
        validate_packages(packages, args.version)
        plan = dict(project=args.project, location='us-central1', apt='wendy-apt', yum='wendy-yum',
                    source=source, packages=packages)
        (output / 'plan.json').write_text(json.dumps(plan, indent=2) + '\n')
        report['plan_sha256'] = sha(output / 'plan.json')
        for p in packages:
            path = packages_dir / p['format'] / p['filename']
            require(path.is_file() and not path.is_symlink() and sha(path) == p['sha256'], 'candidate changed before upload')
            fmt = 'apt' if p['format'] == 'deb' else 'yum'
            command = ['gcloud', 'artifacts', fmt, 'upload', 'wendy-' + fmt, '--location=us-central1',
                       '--project=' + args.project, '--source=' + str(path), '--quiet']
            log = output / ('upload-' + p['filename'] + '.log')
            with log.open('w') as stream:
                remaining = min(180, deadline - time.monotonic())
                require(remaining > 0, 'release validation exceeded its 35-minute deadline')
                code = OWNED.run(command, remaining, env=env, stdout=stream)
            receipt = dict(format=p['format'], filename=p['filename'], sha256=p['sha256'],
                           exit_code=code, log_sha256=sha(log), repository='wendy-' + fmt)
            report['uploads'].append(receipt)
            (output / 'upload-receipts.json').write_text(json.dumps(report['uploads'], indent=2) + '\n')
            require(code == 0 and not interrupted, 'Artifact Registry rejected or interrupted candidate upload')
            require(sha(path) == p['sha256'], 'candidate changed during upload')
        validate_uploads(packages, report['uploads'])
        # All dependency installation has finished. Only now expose the existing
        # action's WIF credential; it never enters container layers or evidence.
        copied = work / 'credential/wif.json'
        copied.write_bytes(credentials.read_bytes()); copied.chmod(0o600)
        validate_wif(copied)
        report['consumers'] = []
        for fmt, container in report['containers'].items():
            for arch in ('amd64', 'arm64'):
                name = fmt + '-' + arch
                execute(['docker', 'exec', container, 'python3', '/consumer.py', 'consume', fmt, arch,
                         '/work/' + name], name + '-consume', 420)
                result = json.loads((output / name / 'result.json').read_text())
                require(result['status'] == 'passed' and result['plan_sha256'] == report['plan_sha256'] and
                        result['format'] == fmt and result['architecture'] == arch, 'native consumer result mismatch')
                report['consumers'].append(result)
        report['status'] = 'passed'
    finally:
        # Signals are recorded throughout cleanup, including repeated cancellation.
        # Docker owns these containers; killing a local CLI does not drain them.
        # Resolve the nonce label, remove only that exact ID, and require absence.
        try:
            cleanup = []
            for name, owner in reversed(owned_containers):
                try:
                    drain_container(name, owner, env)
                    cleanup.append(dict(name=name, status='drained'))
                except Exception as error:
                    cleanup.append(dict(name=name, status='unproven', error=str(error)))
            report['cleanup'] = cleanup
            # Credential file is task-owned and never retained, even when container
            # drain is unproven. Other scratch survives for disposable-runner cleanup.
            (work / 'credential/wif.json').unlink(missing_ok=True)
            if all(item['status'] == 'drained' for item in cleanup):
                shutil.rmtree(work)
            else:
                report['status'] = 'failed'; report['retained_scratch'] = str(work)
            if interrupted:
                report['status'] = 'failed'; report['interrupted'] = interrupted
            (output / 'result.json').write_text(json.dumps(report, indent=2) + '\n')
        finally:
            for sig, handler in previous.items():
                signal.signal(sig, handler)
    require(report['status'] == 'passed', 'release repository acceptance or cleanup failed')
    print('RELEASE_REPOSITORY_CONSUMERS_VERIFIED')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--project', required=True)
    parser.add_argument('--version', required=True)
    parser.add_argument('--packages', type=Path, required=True)
    parser.add_argument('--evidence', type=Path, required=True)
    run(parser.parse_args())
