#!/usr/bin/env python3
"""Build the real native Make target twice and inspect its executable version.

Run only in an isolated checkout. Refuses to overwrite an existing go/bin/wendy.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import shutil
import tempfile

ROOT = Path(__file__).resolve().parents[2]


def run(args, timeout=600, **kwargs):
    return subprocess.run(args, check=True, text=True, timeout=timeout, **kwargs)


def retain_binary(source, destination):
    """Retain the exact tested bytes, refusing to replace an existing artifact."""
    with destination.open('xb') as output, source.open('rb') as input_file:
        shutil.copyfileobj(input_file, output)
    destination.chmod(source.stat().st_mode & 0o777)
    return hashlib.sha256(destination.read_bytes()).hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--retain-binary', type=Path,
                        help='exclusive output path for the second verified CLI, after every check passes')
    args = parser.parse_args()
    if args.retain_binary and (args.retain_binary.exists() or args.retain_binary.is_symlink()):
        raise RuntimeError('refusing to replace retained CLI: '+str(args.retain_binary))
    environment = json.loads(run(['go', 'env', '-json', 'GOOS', 'GOARCH', 'GOHOSTOS', 'GOHOSTARCH'], capture_output=True).stdout)
    if (environment['GOOS'], environment['GOARCH']) != (environment['GOHOSTOS'], environment['GOHOSTARCH']):
        raise RuntimeError('native VERSION regression must execute on the target host, without cross-compilation')
    windows = environment['GOHOSTOS'] == 'windows'
    binary = ROOT / 'go/bin' / ('wendy.exe' if windows else 'wendy')
    if binary.exists() or binary.is_symlink():
        raise RuntimeError(f'refusing to overwrite {binary}; use a clean isolated checkout')
    with tempfile.TemporaryDirectory(prefix='wendy-make-version-') as config:
        env = dict(os.environ, WENDY_CONFIG_DIR=config)
        try:
            # A second build catches a stale artifact or build rule that forgets
            # to apply a changed VERSION. Both execute the native Make recipe.
            for version in ['2099.01.02-regression-a', '2099.01.03-regression-b']:
                run(['make', '-C', str(ROOT/'go'), 'build-cli', 'VERSION='+version], env=env)
                actual = run([str(binary), '--version'], env=env, capture_output=True, timeout=15).stdout.strip()
                expected = 'wendy version '+version
                if actual != expected:
                    raise RuntimeError(f'native Make VERSION: got {actual!r}, expected {expected!r}')
                print(f'PASS native {environment["GOHOSTOS"]}/{environment["GOHOSTARCH"]}: {actual}', flush=True)
            # Save the second tested executable before the negative linker case
            # removes it. Publish it only after that case also passes.
            saved = Path(config) / binary.name
            if args.retain_binary:
                shutil.copy2(binary, saved)
            binary.unlink()
            failure = subprocess.run(
                ['make', '-C', str(ROOT/'go'), 'build-cli', 'LDFLAGS=-definitely-invalid-linker-flag'],
                env=env, text=True, capture_output=True, timeout=600)
            if failure.returncode == 0 or binary.exists():
                raise RuntimeError('native Make swallowed a linker failure: '+failure.stdout+failure.stderr)
            if 'definitely-invalid-linker-flag' not in failure.stderr:
                raise RuntimeError('negative build failed before the intended linker check: '+failure.stderr)
            print('PASS native Make propagates a real linker failure', flush=True)
            if args.retain_binary:
                digest = retain_binary(saved, args.retain_binary)
                print(f'Retained tested CLI {args.retain_binary}: sha256:{digest}', flush=True)
        finally:
            binary.unlink(missing_ok=True)


if __name__ == '__main__':
    main()
