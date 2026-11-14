#!/usr/bin/env python3
"""Require real first-boot driver RPC reconnects and identity/kernel controls."""
import argparse
import importlib.util
import json
from pathlib import Path
import tempfile

ROOT = Path(__file__).resolve().parents[2]
TEST = 'TestDriverBootReconnectRPC'
SPEC = importlib.util.spec_from_file_location('driver_boot_guardian', Path(__file__).with_name('run-owned-media-test.py'))
GUARDIAN = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GUARDIAN)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    source = ROOT / 'go/internal/cli/commands/os_install_drivers.go'
    original = source.read_text()
    changes = {
        'ignore-identity': ('if normalizeMDNSHost(v.GetHostname()) != host || v.GetDeviceType() != p.deviceType {', 'if normalizeMDNSHost(v.GetHostname()) == "" && v.GetDeviceType() == "" {'),
        'ignore-kernel': ('if dl.GetKernelVersion() != p.kernel {', 'if false {'),
    }
    # The full race job runs normal too; retain a standalone reproducible gate.
    with tempfile.TemporaryDirectory(prefix='wendy-driver-boot-controls-') as tmp:
        for name in ['normal', *changes]:
            command = ['go', 'test', '-gcflags=all=-c=1', '-vet=off', '-race']
            if name != 'normal':
                old, new = changes[name]
                if original.count(old) != 1:
                    raise RuntimeError('production guard changed: ' + name)
                replacement = Path(tmp) / (name + '.go')
                replacement.write_text(original.replace(old, new))
                overlay = Path(tmp) / (name + '.json')
                overlay.write_text(json.dumps({'Replace': {str(source): str(replacement)}}))
                command += ['-overlay', str(overlay)]
            command += ['./go/internal/cli/commands', '-run', '^' + TEST + '$', '-v', '-count=1', '-timeout=60s']
            log = output / (name + '.log')
            with log.open('w') as stream:
                result = GUARDIAN.run(command, 300, cwd=ROOT, stdout=stream)
            text = log.read_text()
            if name == 'normal':
                ok = result == 0 and '--- PASS: ' + TEST + ' ' in text and 'FIRST_BOOT_DRIVER_TRANSPORT_VERIFIED' in text
            else:
                ok = result == 1 and '--- FAIL: ' + TEST + ' ' in text and 'driver upload reached a rejected boot identity:' in text
            if not ok or '--- SKIP:' in text:
                raise RuntimeError('unexpected first-boot driver result: ' + str(log))
            print(name + ': expected outcome', flush=True)


if __name__ == '__main__':
    main()
