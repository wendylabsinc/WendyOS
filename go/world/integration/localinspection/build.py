#!/usr/bin/env python3
"""Build the local inspection driver and its simulation services from this checkout."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[2]


def build(output):
    go = shutil.which('go')
    if not go:
        raise RuntimeError('Install Go before building the local inspection tools')
    output = Path(output).absolute()
    output.mkdir(mode=0o700)
    source = ROOT / 'integration/localinspection/driver.go'
    temporary = ROOT / 'production/.local'
    temporary.mkdir(mode=0o700, parents=True, exist_ok=True)
    receipt = {'version': 1, 'working_directory': str(ROOT), 'source': str(source),
               'source_sha256': hashlib.sha256(source.read_bytes()).hexdigest(), 'builds': []}
    # Keep the driver inside production's Go internal import boundary. This
    # temporary package uses Wendy's parent module and is removed on every exit.
    with tempfile.TemporaryDirectory(prefix='localinspection-driver-', dir=temporary) as name:
        package = Path(name)
        shutil.copyfile(source, package / 'main.go')
        targets = {'localinspection-driver': './' + str(package.relative_to(ROOT)),
                   'mission-gateway': './cmd/worldd', 'atlas-executor': './cmd/world-executor',
                   'mission-commissioner': './cmd/world-sharedinspection'}
        for target, path in targets.items():
            binary = output / target
            command = [go, 'build', '-trimpath', '-o', str(binary)]
            if target == 'localinspection-driver':
                command += ['-tags=integration']
            command += [path]
            with (output / (target + '.stdout')).open('xb') as stdout, (output / (target + '.stderr')).open('xb') as stderr:
                result = subprocess.run(command, cwd=ROOT, env=os.environ.copy(), stdout=stdout, stderr=stderr, timeout=300)
            row = {'command': command, 'exit_code': result.returncode}
            if result.returncode == 0:
                row['binary'] = {'path': str(binary), 'size': binary.stat().st_size,
                                 'sha256': hashlib.sha256(binary.read_bytes()).hexdigest()}
            receipt['builds'].append(row)
            (output / 'BUILD.json').write_text(json.dumps(receipt, indent=2) + '\n')
            if result.returncode:
                raise RuntimeError(target + ' build failed; see retained stderr')
    return receipt


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True)
    print(json.dumps(build(parser.parse_args().output)))
