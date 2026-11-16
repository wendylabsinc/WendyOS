#!/usr/bin/env python3
"""Generate the physical LiDAR memory procedure for audit #2684.

Only reads local files and writes a new directory. No device is accessed.
Usage: python3 lidar-memory-rehearsal.py --inventory inventory.json --output NEWDIR

Inventory fields: device_udid, device_model, ios_build, app_source_commit,
configuration, app_bundle (signed WendyMobile.app), room_recipe (local text),
measurement_recipe (local text), budget_approval (local text), capture_peak_mib,
reconstruction_peak_mib, idle_growth_mib.
The recipes identify the repeatable room/path, physical-footprint measurement
method, sample interval/export, reviewer and rationale for the numerical limits.
"""
import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import plistlib
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
    info = {k: line(raw[k], k) for k in ('device_udid', 'device_model', 'ios_build', 'app_source_commit', 'configuration')}
    if not re.fullmatch(r'[0-9a-f]{40}', info['app_source_commit']):
        raise ValueError('app_source_commit must be the full source SHA of the installed build')
    for k in ('capture_peak_mib', 'reconstruction_peak_mib', 'idle_growth_mib'):
        if type(raw[k]) not in (int, float) or not math.isfinite(raw[k]) or raw[k] <= 0:
            raise ValueError(f'{k} must be a finite positive number approved before execution')
        info[k] = raw[k]
    recipes = {}
    for key in ('room_recipe', 'measurement_recipe', 'budget_approval'):
        info[key] = artifact(raw[key])
        recipes[key] = Path(info[key]['path']).read_text()
        if len(recipes[key].strip()) < 80:
            raise ValueError(f'{key} needs concrete instructions, not a label or placeholder')
    bundle = Path(line(raw['app_bundle'], 'app_bundle')).expanduser().resolve(strict=True)
    plist = plistlib.loads((bundle / 'Info.plist').read_bytes())
    if plist.get('CFBundleIdentifier') != 'dev.wendy.mobile':
        raise ValueError('expected WendyMobile.app with bundle ID dev.wendy.mobile')
    executable = plist.get('CFBundleExecutable', '')
    if not executable or Path(executable).name != executable:
        raise ValueError('invalid app executable name')
    info['app_executable'] = artifact(str(bundle / executable))
    records = []
    for p in sorted(bundle.rglob('*')):
        # Include resources/extensions, but never follow symlinks outside the app.
        if p.is_symlink():
            records.append({'path': str(p.relative_to(bundle)), 'symlink': os.readlink(p)})
        elif p.is_file():
            records.append({'path': str(p.relative_to(bundle)), 'sha256': hashlib.sha256(p.read_bytes()).hexdigest()})
    info['app_bundle'] = str(bundle)
    info['bundle_manifest_sha256'] = hashlib.sha256(json.dumps(records, sort_keys=True).encode()).hexdigest()
    commit = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT, text=True).strip()
    dirty = bool(subprocess.check_output(['git', 'status', '--porcelain'], cwd=ROOT, text=True).strip())
    out = args.output.resolve()
    out.mkdir(mode=0o700, parents=True, exist_ok=False)
    q = shlex.quote
    cases = []
    for kind, repeats in [('finish', 10), ('cancel', 5), ('warning', 3), ('cancel-reconstruction', 3)]:
        for index in range(1, repeats + 1):
            cases.append({'id': f'{kind}-{index:02}', 'kind': kind, 'status': 'not_run',
                          'start_utc': None, 'end_utc': None, 'baseline_mib': None,
                          'capture_peak_mib': None, 'reconstruction_peak_mib': None,
                          'idle_after_60s_mib': None, 'trace_path': None, 'ui_video_path': None,
                          'sample_interval_seconds': None, 'outcome': None, 'notes': None})
    procedure = f'''Physical LiDAR memory rehearsal, audit #2684
Procedure source: {commit}; dirty checkout: {dirty}
App source: {info['app_source_commit']}; configuration: {info['configuration']}
Device: {info['device_udid']}; {info['device_model']}; iOS build {info['ios_build']}
Executable SHA-256: {info['app_executable']['sha256']}
Bundle manifest SHA-256: {info['bundle_manifest_sha256']}

All 21 cases start as not_run. Regenerate from a clean committed checkout before
execution. Verify the signed app's source/build record separately; entering a SHA
is not provenance. A simulator or PhysicalRoomCaptureTests startup probe cannot
satisfy this procedure. Exercise the actual SiteRoomCapture UI and room export.

Preparation
1. Reserve this exact LiDAR iPhone/iPad and a private repeatable room. Match UDID
   and iOS build in Xcode Devices and Simulators. Open Scan Room to confirm support.
   Absence of the action means unavailable, not pass. Record device thermal state,
   power arrangement, background apps, room dimensions, lighting and walking path.
2. Obtain owner approval to install this development-signed app. Back up existing
   sites/settings and record how to reinstall the previous version. Do not erase
   the device or use production captures. Verify the app and extension signatures:
     codesign --verify --deep --strict --verbose=2 {q(str(bundle))}
     codesign -dv --verbose=4 {q(str(bundle))}
   Record xcodebuild -version. Check signing team, UDID and entitlements. Install:
     xcrun devicectl device install app --device {q(info['device_udid'])} --timeout 120 --json-output {q(str(out / 'install.json'))} {q(str(bundle))}
   Retain each install attempt separately; never overwrite failed evidence.
3. Run xcodegen generate in go/macos, open WendyDesktop.xcodeproj, choose WendyMobile
   and this physical device. Launch the installed app and attach Xcode to its
   process without rebuilding/replacing it. Resolve signing/debug permissions if
   attaching fails. Verify the installed artifact matches the recorded build.
4. Read room_recipe.txt and measurement_recipe.txt. Use Xcode's memory gauge with
   a continuous recording, or a verified Instruments physical-footprint timeline.
   Record actual metric, units, sample interval and export procedure. Heap live
   allocations, virtual address size and resident size are not physical footprint.
   Keep samples no more than one second apart throughout capture AND reconstruction,
   and UI video with phase timestamps. Missing intervals cannot establish a peak.
   Keep method/configuration constant across cases; pause if thermal state changes.
   Apple reference:
   https://developer.apple.com/documentation/xcode/gathering-information-about-memory-use
   https://developer.apple.com/videos/play/wwdc2022/10106/
5. Before observing results, obtain the named reviewer's approval in budget_approval.txt:
     capture peak <= {info['capture_peak_mib']} MiB
     reconstruction/export peak <= {info['reconstruction_peak_mib']} MiB
     post-idle growth from INITIAL series baseline <= {info['idle_growth_mib']} MiB
   One MiB = 1048576 bytes; convert decimal MB. These project limits are not Apple
   jetsam thresholds. Do not raise limits after failure to relabel that run a pass.

Run evidence.json cases in order without relaunching between them. Start the series
with 60 seconds idle in the Site workspace and record its baseline. Record process
ID; an unexpected PID change, crash, jetsam or OS termination fails the case. A
restart after failure begins a new series and must preserve earlier results.

For every case
1. Record ID, UTC and baseline. Open Site Actions > Scan Room, or Scan Room on an
   empty Site workspace. Verify live camera and room geometry, not a stale frame.
2. Walk the recorded path for 90 seconds for finish/cancel-reconstruction and at
   least 30 seconds for cancel/warning. Mark the capture interval in the recording.
3. Perform the case action below. Keep recording through export/dismissal and for
   60 seconds idle afterward. No action may hang longer than 120 seconds: interrupt,
   mark failure and preserve evidence if it does. Do not retry over that evidence.
4. Record interval maxima, final idle footprint, sample interval, trace/video paths
   and outcome. Include transient peaks, not just a screenshot after memory falls.
   Compare capture and reconstruction/export against their respective budgets.
5. Verify scanning ends after exit, UI remains usable and reopening Scan Room gives
   fresh frames. Retained sites must remain readable. Export evidence each cycle.

finish-01 through finish-10
Tap Finish once; observe Building Room and wait for a newly saved room. Open it,
inspect nonempty geometry, export/open its .wendysite archive. Measure from Finish
through completion, including model/archive allocation. Reopen/close the saved site
before the idle interval. All ten scans must fit both peak and idle-growth limits.

cancel-01 through cancel-05
Tap Cancel during scanning. No room may be saved and Building Room must not start.
Navigate away/back. Observe 60 seconds for a late result or restarted session.

warning-01 through warning-03
During scanning, pause in Xcode, select the main thread and execute in LLDB:
  expression -l objc -- @import UIKit
  expression -l objc -- (void)[[NSNotificationCenter defaultCenter] postNotificationName:UIApplicationDidReceiveMemoryWarningNotification object:nil]
  continue
This is a SYNTHETIC app notification, not actual OS memory pressure. Record that
qualification. Expect scanning to stop, the low-memory error and no new saved room.
Observe 60 seconds for queued reconstruction or a late saved result, then reopen and
confirm fresh frames. Do not induce a system-wide allocation storm. Record any real
OS warning or jetsam separately. Missing symbol/import is an invalid attempt.

cancel-reconstruction-01 through cancel-reconstruction-03
Tap Finish then Cancel while Building Room is visible. Record the action timestamp.
If export finished first, retain the invalid attempt and retry with a new ID.
Expect dismissal and no late saved result; reopen Scan Room and verify fresh frames.
Measure in-flight reconstruction until it stops, including its memory peak. Do not
assert that a warning during reconstruction cancels it: the current warning handler
only acts while scanning. These cases specifically exercise cancellation.

Review and recovery
1. Calculate each idle_after_60s_mib minus the INITIAL series baseline. Using a later
   inflated baseline could hide accumulation. Every difference must fit the idle
   budget. Plot per-cycle idle values and retain sampled evidence.
2. Pass requires every observation, complete samples and all numerical limits.
   Missing instrumentation, unobserved phases or unsupported hardware are incomplete.
   Generator execution and simulator results do not establish a physical pass.
3. Before relaunching after failure, export device crash/jetsam logs with timestamps.
   Preserve private room imagery securely, redact personal data for review, and do
   not attach credentials or unrelated device logs. Hash evidence files in the report.
4. Stop tracing and detach debugger. After exporting evidence, delete only disposable
   captures created here, restore the prior app/settings with owner approval and
   verify original sites still open.
5. Report source/build/signing provenance, device/OS/Xcode, all 21 outcomes, phase
   peaks, idle-growth plot and budget approval. This proves only the measured device
   and configuration. Repeat for other supported LiDAR memory classes selected by
   the app maintainer; never infer their results from this device.
'''
    write(out / 'procedure.txt', procedure)
    for key, value in recipes.items():
        write(out / (key + '.txt'), value)
    write(out / 'bundle-files.json', json.dumps(records, indent=2) + '\n')
    write(out / 'evidence.json', json.dumps({'audit_ids': ['2684'], 'procedure_commit': commit,
          'dirty_preview': dirty, 'inventory': info, 'series_baseline_mib': None,
          'hardware_execution': 'not_run', 'cases': cases}, indent=2) + '\n')
    print(f'Generated {len(cases)} physical cases in {out}; no hardware tests executed.')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--inventory', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    try:
        generate(args)
    except (ValueError, KeyError, OSError, plistlib.InvalidFileException) as exc:
        parser.exit(2, f'error: {exc}\n')
