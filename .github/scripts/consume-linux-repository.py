#!/usr/bin/env python3
"""Native repository download-only acceptance, inside the owned release container.

APT uses Google's transport and signed metadata. DNF uses Google's OAuth plugin;
Artifact Registry does not support RPM repository GPG signatures. Both consumers
must independently recover the exact uploaded package bytes. No Wendy install.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess


def require(ok, message):
    if not ok:
        raise RuntimeError(message)


def sha(path):
    with Path(path).open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest() if hasattr(hashlib, 'file_digest') else _sha(stream)


def _sha(stream):
    digest = hashlib.sha256()
    for block in iter(lambda: stream.read(1024 * 1024), b''):
        digest.update(block)
    return digest.hexdigest()


def command(args, *, cwd=None, env=None):
    result = subprocess.run(args, cwd=cwd, env=env, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, text=True, timeout=180)
    # Helpers must not run in debug mode. These outputs contain repository URLs
    # and package identities, never credential JSON or Authorization headers.
    require(result.returncode == 0,
            f'native client failed ({result.returncode}): {args[0]}\n{result.stdout[-8000:]}\n{result.stderr[-8000:]}')
    return result.stdout


def metadata(path, fmt):
    if fmt == 'deb':
        values = command(['dpkg-deb', '-f', str(path), 'Package', 'Version', 'Architecture']).splitlines()
        # dpkg-deb prefixes requested field names when querying several fields.
        values = [line.split(': ', 1)[-1] for line in values]
    else:
        with path.open('rb') as stream:
            require(stream.read(6).hex() == 'edabeedb0300', 'REPOSITORY_RPM_V4_LEAD')
        values = command(['rpm', '-qp', '--qf', '%{NAME}\n%{VERSION}-%{RELEASE}\n%{ARCH}\n', str(path)]).splitlines()
    require(len(values) == 3, 'native package metadata is incomplete')
    name, version, arch = values
    require(name in ('wendy', 'wendy-agent') and re.fullmatch(r'[0-9][A-Za-z0-9.+_~:-]*', version),
            'unexpected package name/version')
    architecture = {'amd64': 'amd64', 'arm64': 'arm64', 'x86_64': 'amd64', 'aarch64': 'arm64'}.get(arch)
    require(architecture is not None, 'unexpected package architecture')
    require(arch == ({'amd64': 'x86_64', 'arm64': 'aarch64'}[architecture] if fmt == 'rpm' else architecture),
            'format/architecture mismatch')
    return dict(name=name, version=version, architecture=architecture, format=fmt,
                filename=path.name, size=path.stat().st_size, sha256=sha(path))


def inventory(directory, fmt):
    paths = sorted(directory.glob('*'))
    require(len(paths) == 4 and all(p.is_file() and not p.is_symlink() and p.suffix == '.' + fmt for p in paths),
            'require exactly four regular candidate packages per format')
    packages = [metadata(p, fmt) for p in paths]
    require({(p['name'], p['architecture']) for p in packages} ==
            {(name, arch) for name in ('wendy', 'wendy-agent') for arch in ('amd64', 'arm64')},
            'candidate package identities are duplicate or incomplete')
    require(len({p['version'] for p in packages}) == 1, 'candidate package versions disagree')
    return packages


def verify_downloads(downloads, expected):
    paths = sorted(downloads.iterdir())
    require(len(paths) == len(expected) and all(p.is_file() and not p.is_symlink() for p in paths),
            'REPOSITORY_DOWNLOAD_SET: unexpected files')
    actual = [metadata(path, expected[0]['format']) for path in paths]
    fields = ('format', 'name', 'version', 'architecture', 'size', 'sha256')
    require(sorted(tuple(item[k] for k in fields) for item in actual) ==
            sorted(tuple(item[k] for k in fields) for item in expected),
            'REPOSITORY_DOWNLOAD_HASH: native repository bytes differ from uploaded candidates')
    return actual


def consume(plan, fmt, arch, output):
    require(re.fullmatch(r'[a-z][a-z0-9-]{4,61}[a-z0-9]', plan['project']), 'invalid project ID')
    require(plan['location'] == 'us-central1' and plan['apt'] == 'wendy-apt' and plan['yum'] == 'wendy-yum',
            'unexpected release repository')
    expected = [p for p in plan['packages'] if p['format'] == fmt and p['architecture'] == arch]
    require(len(expected) == 2 and {p['name'] for p in expected} == {'wendy', 'wendy-agent'}, 'missing pair')
    output.mkdir(mode=0o755, parents=True, exist_ok=False)
    downloads = output / 'downloads'; downloads.mkdir()
    for name in ('cache', 'lists', 'repos', 'home', 'persist', 'logs'):
        (output / name).mkdir()
    (output / 'lists/partial').mkdir()
    env = {'PATH': '/usr/sbin:/usr/bin:/sbin:/bin', 'HOME': str(output / 'home'), 'LC_ALL': 'C',
           'GOOGLE_APPLICATION_CREDENTIALS': '/credential/wif.json'}
    require(Path('/credential/wif.json').is_file(), 'required WIF credential missing')
    if fmt == 'deb':
        require(Path('/usr/lib/apt/methods/ar+https').is_file(), 'Google APT transport missing')
        (output / 'repos/candidate.list').write_text(
            f'deb [arch={arch} signed-by=/usr/share/keyrings/candidate-repository.gpg] '
            f'ar+https://us-central1-apt.pkg.dev/projects/{plan["project"]} wendy-apt main\n')
        settings = {'Dir::Etc::main': '/dev/null', 'Dir::Etc::parts': '-',
                    'Dir::Etc::sourcelist': str(output / 'repos/candidate.list'), 'Dir::Etc::sourceparts': '-',
                    'Dir::State::status': '/dev/null', 'Dir::State::lists': str(output / 'lists'),
                    'Dir::Cache': str(output / 'cache'), 'APT::Architecture': arch,
                    'APT::Sandbox::User': 'root', 'Acquire::Retries': '0', 'Acquire::http::Timeout': '30',
                    'Acquire::https::Timeout': '30', 'APT::Update::Error-Mode': 'any',
                    'Acquire::AllowInsecureRepositories': 'false', 'APT::Get::AllowUnauthenticated': 'false',
                    'Acquire::gar::Service-Account-JSON': '/credential/wif.json', 'Debug::Acquire::gar': 'false'}
        config = output / 'apt.conf'
        config.write_text(''.join(key + ' ' + json.dumps(value) + ';\n' for key, value in settings.items()) +
                          f'#clear APT::Architectures;\nAPT::Architectures {{ "{arch}"; }};\n')
        env['APT_CONFIG'] = str(config)
        base = ['apt-get']
        update = [*base, 'update']
        fetch = [*base, 'download', *[f'{p["name"]}:{arch}={p["version"]}' for p in expected]]
    else:
        version = command(['dnf', '--version'], env=env).splitlines()[0]
        require(version.startswith('4.'), 'required DNF4 client unavailable')
        require(Path('/usr/libexec/ar-token').is_file(), 'Google DNF token helper missing')
        (output / 'repos/candidate.repo').write_text(
            '[wendy-yum]\nname=Exact release candidate\n'
            f'baseurl=https://us-central1-yum.pkg.dev/projects/{plan["project"]}/wendy-yum\n'
            'enabled=1\ngpgcheck=0\nrepo_gpgcheck=0\nsslverify=1\n')
        config = output / 'dnf.conf'
        config.write_text('[main]\n' + ''.join(f'{key}={value}\n' for key, value in {
            'reposdir': output / 'repos', 'cachedir': output / 'cache', 'persistdir': output / 'persist',
            'logdir': output / 'logs', 'plugins': 1, 'keepcache': 0, 'timeout': 30, 'retries': 0,
            'metadata_expire': 0, 'skip_if_unavailable': 0}.items()))
        rpm_arch = {'amd64': 'x86_64', 'arm64': 'aarch64'}[arch]
        base = ['dnf', '--config', str(config), '--forcearch=' + rpm_arch,
                '--disablerepo=*', '--enablerepo=wendy-yum']
        update = [*base, '--refresh', 'makecache']
        fetch = [*base, 'download', '--downloaddir=' + str(downloads),
                 *[f'{p["name"]}-{p["version"]}.{rpm_arch}' for p in expected]]
    (output / 'metadata.log').write_text(command(update, env=env))
    (output / 'download.log').write_text(command(fetch, env=env, cwd=downloads))
    actual = verify_downloads(downloads, expected)
    # The exact-byte boundary must distinguish even a successful native download
    # of a different candidate. Never upload a deliberately malformed RPM.
    wrong = [dict(p) for p in expected]; wrong[0]['sha256'] = '0' * 64
    try:
        verify_downloads(downloads, wrong)
    except RuntimeError as error:
        require(str(error).startswith('REPOSITORY_DOWNLOAD_HASH:'), 'unexpected hash control failure')
    else:
        raise RuntimeError('wrong candidate hash was accepted')
    result = dict(status='passed', format=fmt, architecture=arch, downloaded=actual,
                  wrong_hash_control='rejected', plan_sha256=sha(Path('/work/plan.json')),
                  client_version=command(['apt-get' if fmt == 'deb' else 'dnf', '--version'], env=env).splitlines()[0],
                  scope='native metadata and exact package download; no installation or target-architecture execution')
    (output / 'result.json').write_text(json.dumps(result, indent=2) + '\n')
    # Packages can be hundreds of MB; their exact receipt is retained, not another
    # published copy. Only delete after successful independent byte comparison.
    for path in downloads.iterdir():
        path.unlink()
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='action', required=True)
    inv = sub.add_parser('inventory'); inv.add_argument('format', choices=['deb', 'rpm'])
    inv.add_argument('directory', type=Path); inv.add_argument('output', type=Path)
    cons = sub.add_parser('consume'); cons.add_argument('format', choices=['deb', 'rpm'])
    cons.add_argument('architecture', choices=['amd64', 'arm64']); cons.add_argument('output', type=Path)
    args = parser.parse_args()
    if args.action == 'inventory':
        args.output.write_text(json.dumps(inventory(args.directory, args.format), indent=2) + '\n')
    else:
        consume(json.loads(Path('/work/plan.json').read_text()), args.format, args.architecture, args.output)


if __name__ == '__main__':
    main()
