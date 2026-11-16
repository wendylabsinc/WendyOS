#!/usr/bin/env python3
"""Generate the physical Lite linear-mapping replacement procedure for #2573.

Reads local inventory/artifacts only; no USB, firmware or application changes.
Usage: python3 lite-memory-rehearsal.py --inventory inventory.json --output NEWDIR

Inventory fields: cli, hardware_id, serial, usb_selector, firmware_version,
firmware_commit, firmware, sdkconfig, setup_recipe, fixture_recipe,
heap_loss_budget_bytes, fixtures. Recipe paths name local text files.
Fixtures are exactly A, B, smaller, rejected; each has project, wasm,
initial_pages, maximum_pages, marker. Projects require Package.swift,
Package.resolved and wendy.json. A/B must declare equal memory dimensions;
smaller must differ; rejected must exceed the configured firmware memory cap.
The fixture recipe pins SDK/toolchain, exact build commands/output paths and
WASM memory inspection. Setup describes independent power, serial logging,
USB data interruption, backup and restoration for the exact board.
"""
import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import re
import shlex
import subprocess

ROOT = Path(__file__).resolve().parents[3]


def line(value, field):
    if not isinstance(value, str) or not value.strip() or any(ord(c) < 32 for c in value) or '<' in value or '>' in value:
        raise ValueError(f'{field} must be a nonempty single line without placeholders')
    return value


def artifact(value):
    p = Path(line(value, 'artifact')).expanduser().resolve(strict=True)
    if not p.is_file() or not p.stat().st_size:
        raise ValueError('artifact must be a nonempty file')
    return {'path': str(p), 'sha256': hashlib.sha256(p.read_bytes()).hexdigest()}


def write(path, value):
    with path.open('x', encoding='utf-8') as output:
        os.chmod(path, 0o600)
        output.write(value)


def generate(args):
    raw = json.loads(args.inventory.read_text())
    info = {k: line(raw[k], k) for k in ('hardware_id', 'serial', 'usb_selector', 'firmware_version', 'firmware_commit')}
    if not info['usb_selector'].startswith('wendy-lite:'):
        raise ValueError('use the exact Wendy Lite USB selector, not automatic discovery or Cloud')
    if not re.fullmatch(r'[0-9a-f]{40}', info['firmware_commit']):
        raise ValueError('firmware_commit must be a full source SHA')
    budget = raw['heap_loss_budget_bytes']
    if type(budget) not in (int, float) or not math.isfinite(budget) or budget < 0:
        raise ValueError('heap loss budget must be a finite nonnegative byte count')
    info['heap_loss_budget_bytes'] = budget
    for key in ('cli', 'firmware', 'sdkconfig', 'setup_recipe', 'fixture_recipe'):
        info[key] = artifact(raw[key])
    config = Path(info['sdkconfig']['path']).read_text()
    for option in ('CONFIG_SPIRAM=y', 'CONFIG_WENDY_WASM_USE_PSRAM=y'):
        if option not in config.splitlines():
            raise ValueError(f'physical mapping reuse needs {option}')
    limit = re.search(r'^CONFIG_WENDY_WASM_MAX_MEMORY_PAGES=(\d+)$', config, re.M)
    if not limit:
        raise ValueError('sdkconfig must record the firmware memory page cap')
    info['firmware_max_pages'] = int(limit[1])
    recipes = {}
    for key in ('setup_recipe', 'fixture_recipe'):
        recipes[key] = Path(info[key]['path']).read_text()
        if len(recipes[key].strip()) < 100:
            raise ValueError(f'{key} must include concrete commands, target checks and recovery')
    if set(raw['fixtures']) != {'A', 'B', 'smaller', 'rejected'}:
        raise ValueError('supply A, B, smaller and rejected fixture projects')
    fixtures = {}
    for name, source in raw['fixtures'].items():
        project = Path(line(source['project'], 'project')).expanduser().resolve(strict=True)
        for filename in ('Package.swift', 'Package.resolved', 'wendy.json'):
            if not (project / filename).is_file():
                raise ValueError(f'{name} needs {filename}')
        app = json.loads((project / 'wendy.json').read_text())
        if app.get('language') != 'swift':
            raise ValueError('only Swift WASM fixtures are allowed; do not flash native firmware')
        item = {'project': str(project), 'app_id': line(app.get('appId'), 'appId'),
                'wasm': artifact(source['wasm']), 'marker': line(source['marker'], 'marker')}
        if Path(item['wasm']['path']).read_bytes()[:8] != b'\x00asm\x01\x00\x00\x00':
            raise ValueError('fixture artifact must be a version-1 WASM module')
        for key in ('initial_pages', 'maximum_pages'):
            if type(source[key]) is not int or source[key] <= 0:
                raise ValueError(f'{key} must be a positive page count')
            item[key] = source[key]
        if item['initial_pages'] > item['maximum_pages']:
            raise ValueError('initial pages cannot exceed maximum pages')
        records = []
        for p in sorted(project.rglob('*')):
            relative = p.relative_to(project)
            if any(part in ('.git', '.build', '.swiftpm') for part in relative.parts):
                continue
            if p.is_symlink():
                raise ValueError('fixture source must not depend on unrecorded symlinks')
            if p.is_file():
                records.append({'path': str(relative), 'sha256': hashlib.sha256(p.read_bytes()).hexdigest()})
        item['source_files'] = records
        fixtures[name] = item
    dimensions = lambda item: (item['initial_pages'], item['maximum_pages'])
    if dimensions(fixtures['A']) != dimensions(fixtures['B']):
        raise ValueError('A/B need equal memory dimensions to prove reuse')
    if fixtures['A']['wasm']['sha256'] == fixtures['B']['wasm']['sha256']:
        raise ValueError('A/B must be different guest binaries')
    if fixtures['smaller']['maximum_pages'] >= fixtures['A']['maximum_pages']:
        raise ValueError('smaller fixture must shrink the maximum memory mapping')
    if any(fixtures[k]['maximum_pages'] > info['firmware_max_pages'] for k in ('A', 'B', 'smaller')):
        raise ValueError('successful fixtures must fit the firmware page cap')
    if fixtures['rejected']['initial_pages'] <= info['firmware_max_pages']:
        raise ValueError('rejected fixture must exceed the firmware page cap at admission')
    if len({v['marker'] for v in fixtures.values()}) != 4:
        raise ValueError('fixture startup markers must be distinct')
    info['fixtures'] = fixtures
    if len({v['app_id'] for v in fixtures.values()}) != 1:
        raise ValueError('all fixtures must replace the same app identity')
    commit = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT, text=True).strip()
    dirty = bool(subprocess.check_output(['git', 'status', '--porcelain'], cwd=ROOT, text=True).strip())
    out = args.output.resolve(); out.mkdir(mode=0o700, parents=True, exist_ok=False)
    cmd = lambda *xs: ' '.join(shlex.quote(str(x)) for x in (info['cli']['path'], *xs))
    deploy = lambda name: cmd('run', '--device', info['usb_selector'], '--prefix', fixtures[name]['project'], '--detach')
    cases = [{'id': 'cold-A', 'status': 'not_run'}]
    cases += [{'id': f'replace-{i:02}', 'status': 'not_run'} for i in range(1, 21)]
    cases += [{'id': name, 'status': 'not_run'} for name in ('shrink', 'grow-back', 'interrupted-push', 'recover-after-push', 'rejected-start', 'recover-after-start')]
    procedure = f'''Physical Lite mapping replacement, audit #2573
Procedure commit: {commit}; dirty checkout: {dirty}
Board: {info['hardware_id']}; serial {info['serial']}; USB {info['usb_selector']}
Firmware: {info['firmware_version']}, source {info['firmware_commit']}
Firmware SHA-256: {info['firmware']['sha256']}
CLI SHA-256: {info['cli']['sha256']}

The generator has not accessed hardware. All 27 cases begin as not_run. Regenerate
from a clean committed checkout before execution. Verify the RUNNING firmware's
build provenance and sdkconfig, not just a local filename. This requires supported
physical PSRAM hardware; a VM, host allocator test or non-PSRAM board is insufficient.
Retained memory means allocation/mapping reuse, NOT preservation of guest contents.
The firmware zeroes reused memory. Never use an app counter surviving as the oracle.

Preparation
1. Reserve the exact disposable board. Record owner authorization for app replacement
   and any firmware changes. Back up its apps, configuration and enrollment recovery.
   Follow setup_recipe.txt for board-specific flash commands only after explicit
   approval of that target and erase scope. This procedure does not authorize erase.
2. Verify CONFIG_SPIRAM and CONFIG_WENDY_WASM_USE_PSRAM are enabled in the running
   build, firmware cap is {info['firmware_max_pages']} pages, and PSRAM initializes.
   Retain boot logs, compiler/IDF/WAMR versions, firmware hash and source revision.
3. Keep the board independently powered. Capture firmware INFO logs on a separate
   serial/UART interface that survives interruption of the USB data connection.
   Record the exact interfaces and confirm unplugging data does NOT reset the board.
   Boot-counter/uptime continuity is mandatory for the interruption case. An ordinary
   cable pull that removes power cannot prove mapping retention.
4. Read fixture_recipe.txt and inspect all four pinned Swift projects. A/B have
   equal initial/maximum pages, different startup markers, and the same app identity.
   They must stay alive and periodically report their marker while yielding to the
   host, e.g. Console.print plus System.sleepMs(1000). They must not use networking,
   unrelated allocations or hardware peripherals. Pin the WendyLite SDK revision and
   toolchain. Include the exact build commands and WASM memory-section inspection.
   A/B use {fixtures['A']['initial_pages']}/{fixtures['A']['maximum_pages']} pages;
   smaller uses {fixtures['smaller']['initial_pages']}/{fixtures['smaller']['maximum_pages']};
   rejected requests {fixtures['rejected']['initial_pages']} initial pages, above cap.
   Build rejected as valid WASM that the FIRMWARE refuses at admission. A compiler or
   CLI validation failure does not exercise firmware replacement failure cleanup.
5. Verify the declared dimensions against built WASM and runtime admission logs.
   User-entered page counts alone are insufficient. CLI run rebuilds each project;
   hash its actual output and compare the corresponding inventory WASM hash before
   accepting a case. If a rebuild differs, stop and regenerate the inventory.
6. Approve the heap-loss budget BEFORE measuring: {budget} bytes. Record rationale
   and reviewer in fixture_recipe.txt. Hold other board work constant. For each
   replacement retain 'free heap before instantiate' internal/total/largest values
   at the same phase. Compare like-for-like A/B cycles to the first warm cycle;
   loss beyond budget, allocation failure, panic or reset fails the case.
7. Record UTC, CLI exit code, firmware logs and startup marker for each case. Builds
   have ten minutes, push/start two minutes and marker observation thirty seconds.
   Interrupt on deadline and retain the failure. Detached exit zero is not readiness.
   Each case needs firmware and guest evidence, not just CLI output.

Commands, in a POSIX shell
  {cmd('--version')}
  A: {deploy('A')}
  B: {deploy('B')}
  smaller: {deploy('smaller')}
  rejected: {deploy('rejected')}
The A/B labels above are labels, not shell syntax; execute the command after them.
Keep the selected USB port constant; if re-enumerated, verify hardware ID/serial
again and record the new selector before continuing. Never use auto-selection.

cold-A
Boot once, deploy A and observe marker {fixtures['A']['marker']}. Confirm the actual
running module and memory dimensions. This is the only planned reboot in the series.
The first cold start need not log reuse. Preserve its boot and allocation logs.

replace-01 through replace-20
Without StopApp, reboot or unloading, alternate B then A for twenty replacements.
Observe the new marker within thirty seconds and verify the old guest stops. Each
same-dimension replacement must log 'reusing replacement linear mapping: bytes='.
That production log is emitted when the retained mapping is consumed. Retain its
byte count and verify it matches the runtime's committed allocation, accounting for
WAMR host-managed heap insertion; do not assume only initial_pages times 65536.
Missing reuse is a failed case even if the new app starts. Compare heap values at
the same pre-instantiate phase and verify the series stays inside the loss budget.

shrink, then grow-back
Deploy smaller, observe its distinct marker {fixtures['smaller']['marker']}, then
A again. Different dimensions must use the correct newly sized allocation rather
than pretending to reuse an incompatible mapping. Confirm runtime sizes, successful
markers, no panic and recovered heap. No same-dimension reuse log is required here.

interrupted-push, then recover-after-push
Begin replacing running A with B. After firmware observes PushBegin and at least
one data chunk, interrupt USB DATA ONLY using the recorded setup. Preserve evidence
that PushEnd/Start did not succeed. If transfer finished first, retain as invalid
and repeat with a larger fixture/custom data section recorded in a new inventory.
The CLI must fail within two minutes. Verify board uptime is unchanged. Reconnect
data, verify exact board identity, and deploy A without StopApp or reboot. Require
its marker, no allocation error and valid heap values. Preserve reuse/mapping logs;
if the firmware cannot expose this boundary, the retention observation is incomplete.
Do not require automatic restart of the old guest after an interrupted upload.

rejected-start, then recover-after-start
While A runs, deploy rejected. Require the upload reaches firmware and AppStart
fails with a memory-admission error, not a compiler/transport error. Its guest marker
{fixtures['rejected']['marker']} must never appear. Keep logs through cleanup, then
deploy B without reboot or StopApp. Require marker {fixtures['B']['marker']}, no heap
leak beyond budget and no stuck replacement transaction. A new allocation after
failure may be legitimate; the oracle is ownership cleanup and successful recovery,
not mandatory reuse after every failed transaction.

Review and recovery
1. Keep every attempt, including invalid and failed attempts, with logs, hashes,
   stage timestamps, heap samples, mapping byte counts and observed guest marker.
   Enter per-case outcomes in evidence.json. A missing signal is incomplete, not pass.
2. No case passes without the matching physical board, running firmware provenance,
   actual WASM dimensions and firmware/guest observations. Portable protocol-ordering
   and allocator tests remain separate evidence. Repeat this procedure on each
   supported PSRAM firmware/board combination selected by the firmware maintainer.
3. After preserving evidence, stop the disposable app through its recorded management
   command, restore the original application and settings using setup_recipe.txt and
   verify normal boot/operation. Reflash only with new explicit target authorization.
   If firmware is stuck, capture console/heap evidence before the approved recovery
   reboot; that reboot cannot turn the failed retention case into a pass.
'''
    write(out / 'procedure.txt', procedure)
    for name, text in recipes.items(): write(out / (name + '.txt'), text)
    write(out / 'evidence.json', json.dumps({'audit_ids':['2573'], 'procedure_commit':commit,
          'dirty_preview':dirty, 'inventory':info, 'hardware_execution':'not_run', 'cases':cases}, indent=2)+'\n')
    print(f'Generated {len(cases)} physical cases in {out}; no hardware tests executed.')


if __name__ == '__main__':
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument('--inventory', type=Path, required=True);p.add_argument('--output', type=Path, required=True)
    args = p.parse_args()
    try: generate(args)
    except (ValueError, KeyError, OSError) as exc: p.exit(2, f'error: {exc}\n')
