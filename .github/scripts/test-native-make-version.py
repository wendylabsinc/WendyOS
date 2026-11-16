#!/usr/bin/env python3
"""Build the real native Make target twice and inspect its executable version.

Run only in an isolated checkout. Refuses to overwrite an existing go/bin/wendy.
"""
import json
import os
from pathlib import Path
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[2]


def run(args, timeout=600, **kwargs):
    return subprocess.run(args, check=True, text=True, timeout=timeout, **kwargs)


def main():
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
            binary.unlink()
            failure = subprocess.run(
                ['make', '-C', str(ROOT/'go'), 'build-cli', 'LDFLAGS=-definitely-invalid-linker-flag'],
                env=env, text=True, capture_output=True, timeout=600)
            if failure.returncode == 0 or binary.exists():
                raise RuntimeError('native Make swallowed a linker failure: '+failure.stdout+failure.stderr)
            if 'definitely-invalid-linker-flag' not in failure.stderr:
                raise RuntimeError('negative build failed before the intended linker check: '+failure.stderr)
            print('PASS native Make propagates a real linker failure', flush=True)
        finally:
            binary.unlink(missing_ok=True)


if __name__ == '__main__':
    main()
