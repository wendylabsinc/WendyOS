#!/usr/bin/env python3
"""Native macOS source-cache publication with real permission failure and locks."""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import platform
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location('g1_cache_owner', Path(__file__).with_name('run-owned-media-test.py'))
OWNER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(OWNER)
CONTROLS = (
    ('readonly-publication-root', 'os.Chmod(temporary, 0o700)', 'error(nil)', 'G1_PUBLICATION_ROOT_NOT_WRITABLE'),
    ('lost-published-ownership', '\ttemporary = target\n', '\t// regression: retain the old temporary name\n', 'G1_FAILED_PUBLICATION_LEAKED'),
    ('unprotected-published-root', 'os.Chmod(target, 0o555)', 'error(nil)', 'G1_PUBLISHED_ROOT_NOT_PROTECTED'),
    ('early-cache-unlock', '\tdefer unlock()\n', '\tunlock() // regression: release before publication\n', 'G1_CACHE_LOCK_RELEASED_BEFORE_PROTECTION'),
)


def observed_source(source):
    for before, after in [('os.Rename(temporary, target)', 'cachePublicationRename(temporary, target)'),
                          ('removeTemporary(temporary)', 'cachePublicationCleanup(temporary)')]:
        if source.count(before) != 1:
            raise RuntimeError('publication observer boundary changed: ' + before)
        source = source.replace(before, after)
    return source


def historical_source(source):
    # Restore the actual original publication sequence, without observers or
    # an artificial cross-parent move. Keep current payload bytes unchanged.
    blocks = (
        '\tif err := os.Chmod(temporary, 0o700); err != nil {\n'
        '\t\treturn "", fmt.Errorf("preparing G1 source cache publication: %w", err)\n\t}\n',
        '\ttemporary = target\n',
        '\tif err := os.Chmod(target, 0o555); err != nil {\n'
        '\t\treturn "", fmt.Errorf("protecting published G1 source cache: %w", err)\n\t}\n',
    )
    for block in blocks:
        if source.count(block) != 1:
            raise RuntimeError('original publication sequence changed')
        source = source.replace(block, '')
    return source


def historical_result(code, raw):
    events = [json.loads(line) for line in raw.splitlines() if line.strip()]
    output = ''.join(event.get('Output', '') for event in events)
    actions = {(event.get('Test'), event.get('Action')) for event in events}
    if any(event.get('Action') == 'skip' for event in events) or any(value in output for value in ('WARNING: DATA RACE', 'panic:', 'test timed out')):
        raise RuntimeError('historical observation skipped, raced or timed out')
    if code == 0 and ('TestMaterializeIdentity', 'pass') in actions:
        return False
    if (code == 1 and ('TestMaterializeIdentity', 'fail') in actions and
            'publishing G1 source cache:' in output and 'permission denied' in output):
        return True
    raise RuntimeError('historical observation failed outside native rename permission')


def parse_result(code, raw, marker=None):
    events = [json.loads(line) for line in raw.splitlines() if line.strip()]
    output = ''.join(event.get('Output', '') for event in events)
    failures = ('WARNING: DATA RACE', 'G1_CACHE_CHILD_DRAIN_UNKNOWN',
                'G1_CACHE_RETAINED_UNKNOWN_DRAIN', 'G1_CLEANUP_OBSERVER_ERROR',
                'panic:', 'test timed out')
    if any(event.get('Action') == 'skip' for event in events) or any(value in output for value in failures):
        return False
    actions = {(event.get('Test'), event.get('Action')) for event in events}
    main = 'TestG1CachePublicationLifecycle'
    if marker is not None:
        return code == 1 and (main, 'fail') in actions and marker in output
    return (code == 0 and (main, 'pass') in actions and ('TestG1DarwinRenamePermissions', 'pass') in actions
            and all((main + '/deny-final-protection-' + mode, 'pass') in actions for mode in ('true', 'false'))
            and 'G1_CACHE_PUBLICATION_VERIFIED deny=true' in output
            and 'G1_CACHE_PUBLICATION_VERIFIED deny=false' in output
            and 'G1_DARWIN_RENAME cross_parent=false' in output
            and 'G1_DARWIN_RENAME cross_parent=true' in output)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    if sys.platform != 'darwin' or os.geteuid() == 0:
        raise RuntimeError('requires a native ordinary macOS user, never sudo')
    if not Path('/usr/bin/sandbox-exec').is_file():
        raise RuntimeError('native per-process sandbox-exec required; no skipped permission failure')
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    production = ROOT/'go/simulator/g1/bundle.go'
    original = production.read_text()
    versions = {'platform': platform.platform(), 'machine': platform.machine()}
    evidence = {'status': 'failed', 'versions': versions, 'cases': {},
                'original_commit': '44be244bc42ffa1f8a1f55349d5dc6c9da4d4d54',
                'source_sha256': {str(p.relative_to(ROOT)): hashlib.sha256(p.read_bytes()).hexdigest()
                    for p in (production, ROOT/'go/simulator/g1/publication_darwin_test.go', Path(__file__).resolve(), ROOT/'.github/scripts/run-owned-media-test.py')},
                'scope': 'real filesystem publication/cleanup/flock; same-parent historical EACCES is not inferred from the writable-root control'}
    try:
        with tempfile.TemporaryDirectory(prefix='wendy-g1-cache-overlay-') as temporary:
            work = Path(temporary)
            cases = [('normal', None, None, None)] + list(CONTROLS)
            for name, before, after, marker in cases:
                source = original
                if before is not None:
                    if source.count(before) != 1:
                        raise RuntimeError('historical boundary changed: ' + name)
                    source = source.replace(before, after)
                patched = work/(name+'.go')
                patched.write_text(observed_source(source))
                overlay = work/(name+'.json')
                overlay.write_text(json.dumps({'Replace': {str(production): str(patched)}}))
                command = ['go', 'test', '-json', '-race', '-tags=g1_cache_lifecycle', '-overlay='+str(overlay),
                           './go/simulator/g1', '-run', '^(TestG1CachePublicationLifecycle|TestG1DarwinRenamePermissions)$',
                           '-count=1', '-timeout=60s']
                environment = dict(os.environ, GOOS='darwin', GOARCH={'arm64':'arm64','x86_64':'amd64'}[platform.machine()],
                                   CGO_ENABLED='1')
                for key in list(environment):
                    if key.startswith('WENDY_G1_CACHE_'):
                        del environment[key]
                log = output/(name+'.jsonl')
                with log.open('w') as stream:
                    code = OWNER.run(command, 150, cwd=ROOT, env=environment, stdout=stream)
                raw = log.read_text()
                # Compilation diagnostics are not accepted as a historical
                # control: every control must execute and fail the real test.
                if not parse_result(code, raw, marker):
                    raise RuntimeError(name + ' failed outside its required filesystem boundary')
                evidence['cases'][name] = {'exit_code': code, 'expected': True}
            native = work/'historical-native.go'
            native.write_text(historical_source(original))
            overlay = work/'historical-native.json'
            overlay.write_text(json.dumps({'Replace': {str(production): str(native)}}))
            command = ['go', 'test', '-json', '-race', '-overlay='+str(overlay),
                       './go/simulator/g1', '-run', '^TestMaterializeIdentity$', '-count=1', '-timeout=30s']
            with (output/'historical-native.jsonl').open('w') as stream:
                code = OWNER.run(command, 150, cwd=ROOT, env=environment, stdout=stream)
            reproduced = historical_result(code, (output/'historical-native.jsonl').read_text())
            evidence['historical_native_observation'] = {'exit_code': code,
                'same_parent_rename_EACCES_reproduced': reproduced,
                'scope': 'actual original sequence with current embedded payload, no observers or synthetic rename policy'}
        evidence['status'] = 'passed'
    finally:
        (output/'summary.json').write_text(json.dumps(evidence, indent=2)+'\n')
    print('G1_CACHE_PUBLICATION_MATRIX_VERIFIED; actual kernel permission, locks and four controls')


if __name__ == '__main__':
    main()
