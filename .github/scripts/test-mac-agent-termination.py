#!/usr/bin/env python3
"""Real macOS process termination through production AgentRelauncher; no app/launchd."""
import argparse
import hashlib
import importlib.util
import json
from pathlib import Path
import signal
import subprocess
import sys
import time

ROOT = Path(__file__).resolve().parents[2]
SOURCE = ROOT / 'swift/WendyAgentCore/Sources/WendyAgent/Services/Platform/AgentRelauncher.swift'
FIXTURE = ROOT / '.github/scripts/fixtures/mac-agent-termination.swift'
spec = importlib.util.spec_from_file_location('owned_termination_build', Path(__file__).with_name('run-owned-media-test.py'))
OWNED = importlib.util.module_from_spec(spec)
spec.loader.exec_module(OWNED)


def observe(binary, mode, output):
    output.mkdir(mode=0o700)
    start = time.monotonic()
    # The compiled fixture calls only scheduleTermination, which cannot spawn
    # descendants. subprocess.run retains and reaps this exact child on timeout.
    try:
        result = subprocess.run([str(binary), mode, str(output)], stdin=subprocess.DEVNULL,
                                stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, timeout=14)
    except subprocess.TimeoutExpired as error:
        raise RuntimeError('watchdog did not terminate the real hung process within 14 seconds') from error
    elapsed = time.monotonic() - start
    if result.returncode != 0:
        raise RuntimeError(f'{mode} child failed: exit={result.returncode} stderr={result.stderr[-4096:]!r}')
    values = {name: float((output/name).read_text()) for name in ('armed', 'ack')}
    if not 0 <= values['armed'] < values['ack'] or values['ack'] < .18:
        raise RuntimeError('scheduled termination did not preserve the actual ack-delay write')
    if mode != 'nil':
        values['callback'] = float((output/'callback').read_text())
        if not .4 <= values['callback'] < 4 or values['ack'] >= values['callback']:
            raise RuntimeError('graceful callback did not run after the ack-flush interval')
    elif (output/'callback').exists():
        raise RuntimeError('nil handler unexpectedly invoked a callback')
    if mode == 'hung':
        if not 9 <= elapsed < 14:
            raise RuntimeError(f'watchdog exited outside its real 10-second interval: {elapsed}')
    elif not .4 <= elapsed < 4:
        raise RuntimeError(f'graceful/nil termination timing was not bounded: {elapsed}')
    return {'mode': mode, 'exit': result.returncode, 'elapsed_seconds': elapsed, 'events': values}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    if sys.platform != 'darwin':
        parser.error('requires native macOS; does not skip on another platform')
    output = args.output.resolve()
    output.mkdir(parents=True, mode=0o700, exist_ok=False)
    evidence = {'status': 'failed', 'audit_entries': ['1786'], 'packaged_execution': 'not_run',
                'cases': [], 'sources': {str(p.relative_to(ROOT)): hashlib.sha256(p.read_bytes()).hexdigest()
                                        for p in (SOURCE, FIXTURE)}}
    previous = {}
    def stop(signum, frame):
        raise InterruptedError(f'termination fixture interrupted by signal {signum}')
    try:
        previous = {sig: signal.signal(sig, stop) for sig in (signal.SIGINT, signal.SIGTERM)}
        original = SOURCE.read_text()
        before = 'static let hardExitDelay: Duration = .seconds(10)'
        if original.count(before) != 1:
            raise ValueError('historical watchdog mutation no longer has one exact source site')
        mutant = output/'AgentRelauncher.swift'
        mutant.write_text(original.replace(before, 'static let hardExitDelay: Duration = .seconds(45)'))
        for name, source in (('normal', SOURCE), ('historical-delay', mutant)):
            binary = output/name
            with (output/(name+'-compile.log')).open('w') as log:
                code = OWNED.run(['xcrun', 'swiftc', '-swift-version', '6', '-parse-as-library',
                                  str(source), str(FIXTURE), '-o', str(binary)], 60, stdout=log)
            if code:
                raise RuntimeError(f'{name} compile failed: {code}')
        for mode in ('graceful', 'nil', 'hung'):
            evidence['cases'].append(observe(output/'normal', mode, output/mode))
        try:
            observe(output/'historical-delay', 'hung', output/'control')
        except RuntimeError as error:
            if str(error) != 'watchdog did not terminate the real hung process within 14 seconds':
                raise
            # A startup failure cannot satisfy this control. It must have
            # completed the same ack and entered its real hung callback.
            ack = float((output/'control/ack').read_text())
            callback = float((output/'control/callback').read_text())
            if not .18 <= ack < callback < 4:
                raise RuntimeError('historical control never reached its hung callback')
            evidence['control'] = {'status': 'rejected', 'boundary': str(error)}
        else:
            raise RuntimeError('historical 45-second watchdog unexpectedly satisfied the current deadline')
        for name, digest in evidence['sources'].items():
            if hashlib.sha256((ROOT/name).read_bytes()).hexdigest() != digest:
                raise RuntimeError('source changed while testing: '+name)
        evidence['status'] = 'passed'
    except BaseException as error:
        evidence['error'] = str(error)
    finally:
        try:
            (output/'evidence.json').write_text(json.dumps(evidence, indent=2)+'\n')
        finally:
            for sig, handler in previous.items():
                signal.signal(sig, handler)
    print(json.dumps(evidence, indent=2))
    return 0 if evidence['status'] == 'passed' else 1


if __name__ == '__main__':
    sys.exit(main())
