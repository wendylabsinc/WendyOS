#!/usr/bin/env python3
"""Audit2022: extract the real pinned DLA package; never install or load it."""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import time
import urllib.request

ROOT = Path(__file__).resolve().parents[2]
SOURCE = ROOT / 'Examples/G1CokeDemo/runtime/bootstrap.py'
URL = 'https://repo.download.nvidia.com/jetson/common/pool/main/n/nvidia-l4t-dla-compiler/nvidia-l4t-dla-compiler_36.4.3-20250107174145_arm64.deb'
SIZE = 2791444
SHA = '928170c9e743257c063c235a610473cfec4be917a36f91a61a890ba2bfc47c94'
LIB = Path('usr/lib/aarch64-linux-gnu/nvidia/libnvdla_compiler.so')
LIB_SIZE = 8159168
LIB_SHA = 'c21986bed8d48a5ef11928aa54a500cd14a997d1d0bed99edf4d161d1c778bec'


def sha(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        raise RuntimeError('pinned package redirect refused')


def download(path):
    started = time.monotonic()
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    received = 0
    with opener.open(URL, timeout=10) as response, path.open('xb') as output:
        require(response.status == 200, 'package HTTP failure')
        while chunk := response.read(65536):
            received += len(chunk)
            require(received <= SIZE and time.monotonic() - started < 60, 'package exceeds byte/time limit')
            output.write(chunk)
    require(received == SIZE and sha(path) == SHA, 'package identity mismatch')


def inside(output, mode, parent):
    require(sys.platform == 'linux' and os.getpid() == 1 and os.geteuid() == 0,
            'requires owned namespace PID1')
    require(os.readlink('/proc/self/ns/mnt') != parent, 'refusing host mount namespace')
    require(mode in ('normal', 'trusted-existing'), 'unknown case')
    package = output / 'package.deb'
    require(package.stat().st_size == SIZE and sha(package) == SHA, 'package changed')
    # This mount exists only in the admitted private namespace. The host /opt
    # and its package/driver state remain untouched; no maintainer script runs.
    subprocess.run(['mount', '-t', 'tmpfs', '-o', 'size=16m,nosuid,nodev,noexec', 'tmpfs', '/opt'], check=True, timeout=10)
    vendor = Path('/opt/vendor/nvidia-l4t-dla-compiler.deb')
    vendor.parent.mkdir()
    shutil.copyfile(package, vendor)
    source = SOURCE if mode == 'normal' else output / 'trusted-existing.py'
    spec = importlib.util.spec_from_file_location('actual_dla_bootstrap', source)
    bootstrap = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(bootstrap)
    require(bootstrap.DLA_SHA256 == SHA, 'production package pin changed')
    work = output / mode
    work.mkdir()
    cases = ('fresh', 'existing', 'corrupt', 'missing') if mode == 'normal' else ('existing',)
    for case in cases:
        target = work / case
        library = target / LIB
        if case != 'fresh':
            library.parent.mkdir(parents=True)
            library.write_bytes(b'untrusted existing host library\n' * 40000)
        before = sha(library) if library.exists() else None
        if case == 'corrupt':
            vendor.write_bytes(package.read_bytes() + b'corrupt-package')
        elif case == 'missing':
            vendor.unlink()
        else:
            shutil.copyfile(package, vendor)
        if case in ('corrupt', 'missing'):
            try:
                bootstrap.ensure_dla_compiler(target)
            except RuntimeError as error:
                require(('checksum' if case == 'corrupt' else 'missing') in str(error), 'wrong package rejection')
            else:
                raise RuntimeError('DLA_PACKAGE_REFUSAL missing or corrupt package accepted')
            require(sha(library) == before, 'rejected package modified existing library')
        else:
            actual = bootstrap.ensure_dla_compiler(target)
            require(actual == library and library.stat().st_size == LIB_SIZE and sha(library) == LIB_SHA,
                    'DLA_PINNED_LIBRARY existing library bypassed verified extraction')
            header = library.read_bytes()[:20]
            require(header[:6] == b'\x7fELF\x02\x01' and int.from_bytes(header[18:20], 'little') == 183,
                    'package did not yield its real AArch64 ELF library')
        print('DLA_PACKAGE_CASE_VERIFIED ' + case, flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--inside', choices=('normal', 'trusted-existing'))
    parser.add_argument('--parent')
    args = parser.parse_args()
    if args.inside:
        inside(args.output.resolve(), args.inside, args.parent)
        return
    require(sys.platform == 'linux' and os.environ.get('GITHUB_ACTIONS') == 'true'
            and os.environ.get('RUNNER_ENVIRONMENT') == 'github-hosted', 'requires disposable hosted Linux job')
    output = args.output.resolve()
    require(output.is_relative_to(Path(os.environ['RUNNER_TEMP']).resolve()), 'output must be private runner scratch')
    output.mkdir(parents=True, exist_ok=False)
    spec = importlib.util.spec_from_file_location('dla_owner', ROOT / '.github/scripts/run-owned-media-test.py')
    owner = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(owner)
    package = output / 'package.deb'
    download(package)
    fields = {field: subprocess.check_output(['dpkg-deb', '-f', str(package), field], text=True, timeout=10).strip()
              for field in ('Package', 'Version', 'Architecture')}
    require(fields == {'Package': 'nvidia-l4t-dla-compiler', 'Version': '36.4.3-20250107174145', 'Architecture': 'arm64'},
            'unexpected package metadata')
    text = SOURCE.read_text()
    boundary = '    # Always extract from the verified package; an existing host file is untrusted.'
    require(text.count(boundary) == 1, 'historical extraction boundary changed')
    (output / 'trusted-existing.py').write_text(text.replace(boundary,
        '    if library.is_file() and library.stat().st_size > 1_000_000:\n        return library'))
    parent = os.readlink('/proc/self/ns/mnt')
    results = {}
    for mode in ('normal', 'trusted-existing'):
        command = ['sudo', '-n', 'unshare', '--mount', '--net', '--pid', '--fork', '--kill-child=KILL',
                   '--propagation', 'private', '--mount-proc', '/usr/bin/python3', str(Path(__file__).resolve()),
                   '--output', str(output), '--inside', mode, '--parent', parent]
        with (output / (mode + '.log')).open('w') as log:
            status = owner.run(command, 60, cwd=ROOT, stdout=log)
        log = (output / (mode + '.log')).read_text()
        print(log)
        if mode == 'normal':
            require(status == 0 and log.count('DLA_PACKAGE_CASE_VERIFIED ') == 4, 'normal extraction cases incomplete')
        else:
            require(status == 1 and 'DLA_PINNED_LIBRARY existing library bypassed verified extraction' in log
                    and 'DLA_PACKAGE_CASE_VERIFIED' not in log, 'historical bypass did not fail its byte-identity boundary')
        results[mode] = {'exit_code': status, 'expected': True}
    (output / 'evidence.json').write_text(json.dumps({'audit': '2022', 'package_url': URL, 'package': fields,
        'package_sha256': SHA, 'library_sha256': LIB_SHA, 'source_sha256': sha(SOURCE), 'results': results,
        'scope': 'real dpkg extraction, corrupt/missing package refusal and existing-file replacement; no library loading, package installation or Jetson/GPU execution'}, indent=2) + '\n')


if __name__ == '__main__':
    main()
