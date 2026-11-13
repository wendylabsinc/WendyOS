#!/usr/bin/env python3
"""Actual process/flock/socket interruption cleanup with no DDS or motor access."""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import sys
import tempfile
import xml.etree.ElementTree as ET

ROOT = Path(__file__).resolve().parents[2]
EXAMPLE = ROOT / 'Examples/G1CokeDemo'
SPEC = importlib.util.spec_from_file_location('coke_cleanup_owner', ROOT / '.github/scripts/run-owned-media-test.py')
OWNER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(OWNER)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True)
    output = parser.parse_args().output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    source = EXAMPLE / 'physical_io/unitree_io.py'
    original = source.read_text()
    before = '''            except (KeyboardInterrupt, SystemExit) as exc:
                release_error = str(exc) or type(exc).__name__
                interruption = exc
            except Exception as exc:
                release_error = str(exc)'''
    after = '''            except BaseException as exc:
                release_error = str(exc)'''
    if original.count(before) != 1:
        raise RuntimeError('original disarm interruption boundary changed')
    results = {}
    with tempfile.TemporaryDirectory(prefix='coke-interruption-') as temporary:
        work = Path(temporary)
        package = work / 'physical_io'
        package.mkdir()
        shutil.copyfile(EXAMPLE / 'physical_io/__init__.py', package / '__init__.py')
        (package / 'unitree_io.py').write_text(original.replace(before, after))
        for mode, path in (('normal', EXAMPLE), ('swallowed-interruption', work)):
            environment = dict(os.environ, PYTHONPATH=str(path), PYTEST_DISABLE_PLUGIN_AUTOLOAD='1', PYTHONDONTWRITEBYTECODE='1')
            report = output / (mode + '.xml')
            with (output / (mode + '.log')).open('w') as log:
                code = OWNER.run([sys.executable, '-m', 'pytest', '-q', '-s', '-p', 'no:cacheprovider', '--junitxml', str(report), str(EXAMPLE / 'tests/test_physical_interruption.py')], 30, cwd=path, env=environment, stdout=log)
            text = (output / (mode + '.log')).read_text()
            print(text)
            cases = ET.parse(report).findall('.//testcase')
            if len(cases) != 2 or any(case.find(tag) is not None for case in cases for tag in ('skipped', 'error')):
                raise RuntimeError('both interruption cases must run without skips/errors')
            if mode == 'normal':
                accepted = code == 0 and all(case.find('failure') is None for case in cases) and text.count('PHYSICAL_INTERRUPTION_CLEANUP_VERIFIED ') == 2
            else:
                accepted = code == 1 and all(case.find('failure') is not None and case.find('failure').get('message', '').startswith('AssertionError: PHYSICAL_INTERRUPTION_SWALLOWED') for case in cases) and 'PHYSICAL_INTERRUPTION_CLEANUP_VERIFIED' not in text
            if not accepted:
                raise RuntimeError(mode + ' did not reach required cleanup boundary')
            results[mode] = {'exit_code': code, 'expected': True}
    (output / 'evidence.json').write_text(json.dumps({'audit': '2022', 'results': results, 'sources': {str(p.relative_to(ROOT)): hashlib.sha256(p.read_bytes()).hexdigest() for p in (source, Path(__file__).resolve(), EXAMPLE / 'tests/test_physical_interruption.py', ROOT / '.github/scripts/run-owned-media-test.py')}, 'scope': 'actual process flock, socket and watchdog cleanup; deliberately supplied private backend, no DDS/physical command publisher'}, indent=2) + '\n')

    print('COKE_PHYSICAL_INTERRUPTION_MATRIX_VERIFIED real SIGINT/SystemExit, owned flock/socket/watchdog and historical control')


if __name__ == '__main__':
    main()
