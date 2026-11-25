"""Offline correspondence checks for a recorded live native walkthrough.

Reads original public receipts only. It does not verify signatures, acquire
time, run inference, control a session or turn a failed source run into a pass.
"""
from __future__ import annotations

import argparse
from copy import deepcopy
import hashlib
import json
from pathlib import Path

from sim.native_replica_bridge import NativeReplicaBridge, MAX_DISPLAY_AGE_NS, timestamp

PLAN = [('floor-plan', 'camera'), ('camera-off', 'command'), ('camera-on', 'command'),
        ('overview', 'camera'), ('observations-off', 'toggle_layer'), ('observations-on', 'toggle_layer')]


def require(condition, message):
    if not condition:
        raise ValueError(message)


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def unique(pairs):
    value = {}
    for key, item in pairs:
        require(key not in value, 'duplicate JSON field')
        value[key] = item
    return value


class Files:
    def __init__(self, root):
        self.root = Path(root).resolve(strict=True)
        self.hashes = {}

    def read(self, name, limit=8 << 20):
        path = self.root / name
        require(not Path(name).is_absolute() and '..' not in Path(name).parts and not path.is_symlink(), 'invalid artifact path')
        require(path.resolve(strict=True).is_relative_to(self.root) and path.is_file() and path.stat().st_size <= limit,
                'missing or oversized artifact')
        raw = path.read_bytes()
        require(len(raw) <= limit, 'artifact exceeds bound')
        self.hashes[name] = {'sha256': digest(raw), 'size': len(raw)}
        return raw

    def json(self, name, limit=8 << 20):
        return json.loads(self.read(name, limit), object_pairs_hook=unique,
                          parse_constant=lambda _: (_ for _ in ()).throw(ValueError('nonfinite JSON')))

    def names(self, pattern, maximum):
        names = sorted(str(p.relative_to(self.root)) for p in self.root.glob(pattern))
        require(len(names) <= maximum, 'artifact count exceeds bound')
        return names


def _sources(rows):
    require(isinstance(rows, list) and len(rows) <= 8 and len({r['id'] for r in rows}) == len(rows), 'invalid source switches')
    require(all(type(r['enabled']) is bool for r in rows), 'invalid source switch state')
    return {r['id']: r['enabled'] for r in rows}


def _layer(rows, name, enabled):
    matches = [r for r in rows if r['name'] == name]
    return len(matches) == 1 and matches[0]['enabled'] is enabled


def _controls(files, config, window):
    run_id = config['run_id']
    actions = window['actions']
    require([(a['id'], a['action']) for a in actions] == PLAN, 'all six walkthrough actions required')
    for action in actions:
        require(action == files.json('session/qa-result-' + action['id'] + '.json', 32 << 10), 'saved QA action differs')
        require(action['schema'] == 'wendy.macos-demo.app-qa.v1' and action['run_id'] == run_id and
                action['kind'] == 'application_model_check_not_OS_input' and action['accepted'] is True and
                action.get('error') is None, 'walkthrough action was not accepted')
    require(actions[0]['camera'] == 'Floor plan' and actions[3]['camera'] == 'Overview', 'camera presets not observed')
    require(_layer(actions[4]['layers'], 'Observations', False) and _layer(actions[5]['layers'], 'Observations', True),
            'observation layer changes not observed')
    command_names = files.names('session/commands/*.json', 2)
    receipt_names = files.names('session/receipts/*.json', 2)
    require(len(command_names) == len(receipt_names) == 2, 'exactly two source commands required')
    receipts = []
    for index in (1, 2):
        name = '%06d.json' % index
        command = files.json('session/commands/' + name, 4096)
        require(command == {'schema': 'wendy.macos-demo.command.v1', 'run_id': run_id, 'sequence': index,
                            'action': 'toggle_sensor', 'target': 'camera-b'}, 'camera-b command differs')
        receipt = files.json('session/receipts/' + name, 32 << 10)
        require(receipt['schema'] == 'wendy.macos-demo.command-result.v1' and receipt['run_id'] == run_id and
                receipt['sequence'] == index and receipt['action'] == 'toggle_sensor' and receipt['accepted'] is True and
                receipt.get('error') is None and receipt['no_mutation_retry'] is True, 'source command not acknowledged')
        request, response = receipt['source_request'], receipt['source_response']
        require(response == {'sequence': response['sequence'], **request}, 'source response differs')
        source = files.json('live/source-%04d.json' % response['sequence'], 32 << 10)
        require(source == {'request': request, 'response': response}, 'original source control differs')
        sensors = request['sensors']
        expected = set(config['input_sources']) - ({'camera-b'} if index == 1 else set())
        require(len(sensors) == len(expected) and set(sensors) == expected, 'source toggle changed other sensors')
        require(actions[index]['worker_command_sequence'] == index, 'QA command acknowledgment differs')
        receipts.append(receipt)
    off = receipts[0]['source_request']['simulation_time_ns']
    restored = receipts[1]['source_request']['simulation_time_ns']
    require(0 < off < restored, 'source toggle times differ')
    return off, restored


def _updates(files, config):
    rows = []
    names = files.names('live/update-*.json', 512)
    require(names, 'original reader updates required')
    for name in names:
        update = files.json(name)
        reader = update['reader']
        if reader['status'] != 'ready':
            continue
        started = update['collection_started_monotonic_ns']
        bridge = NativeReplicaBridge(config, update['presentation'], monotonic_ns=lambda: started)
        # Same byte/scope/lineage validation as the live adapter, with a supplied
        # historical clock. This asserts correspondence, never present liveness.
        require(bridge.ingest(update), 'original reader update not accepted')
        current = bridge.latest
        if not current or not current.get('clock_current'):
            continue
        parsed = current['parsed']
        rows.append({'path': name, 'started': started, 'observation_until': started + MAX_DISPLAY_AGE_NS,
                     'sample_ns': current['sample'] - config['anchor_unix_ns'], 'switches': _sources(update['presentation']['sensor_sources']),
                     'sha256': parsed['sha256'] if parsed else None,
                     'display_until': min(started + MAX_DISPLAY_AGE_NS, started + parsed['physical_until'] - current['upper']) if parsed else 0,
                     'physical_until': parsed['physical_until'] if parsed else None,
                     'physical_upper': current['upper'], 'sample_until': parsed['sample_until'] if parsed else None})
    return rows


def _frames(files, config, window, updates):
    frames = window['frames']
    require(isinstance(frames, list) and 1 <= len(frames) <= 2000, 'native captured frames required')
    current, stale_empty = [], 0
    for frame in frames:
        receipt = files.json(frame['receipt'], 32 << 10)
        require({k: v for k, v in frame.items() if k not in ('file', 'receipt', 'elapsed_s')} == receipt,
                'native window ledger differs from capture sidecar')
        png = files.read(frame['file'], 16 << 20)
        require(png.startswith(b'\x89PNG\r\n\x1a\n') and receipt['png_sha256'] == digest(png) and receipt['png_bytes'] == len(png),
                'native PNG differs from capture sidecar')
        require(receipt['schema'] == 'wendy.macos-demo.app-capture.v1' and receipt['run_id'] == config['run_id'] and
                receipt['mode'] == 'live-native-replica' and receipt['display_clock'] == 'mach_absolute_time_ns' and
                receipt['state_sequence'] == receipt['renderer_sequence'] > 0, 'native capture scope/render differs')
        start, end = receipt['capture_started_monotonic_ns'], receipt['capture_finished_monotonic_ns']
        require(type(start) is int and type(end) is int and 0 <= start <= end, 'capture interval differs')
        is_hazard = receipt['current_native_hazard_captured'] is True
        is_empty = receipt['fusion_state'] == 'no_live_result'
        observation_fresh = (receipt['native_observation_fresh_at_start'] is True and
                             receipt['native_observation_fresh_at_end'] is True)
        if is_empty and not is_hazard and not observation_fresh:
            # A retained empty display may outlive its observation. Preserve its
            # verified PNG/sidecar, but it cannot prove a current absence.
            stale_empty += 1
            continue
        if not (is_hazard or is_empty):
            continue  # Unknown, waiting and expired frames cannot establish absence.
        require(observation_fresh, 'claimed native observation was stale during capture')
        sample_ns = round(receipt['sample_time_s'] * 1_000_000_000)
        switches = _sources(receipt['sensor_sources'])
        matches = [row for row in updates if row['sample_ns'] == sample_ns and row['switches'] == switches and
                   row['observation_until'] == receipt['observation_deadline_monotonic_ns'] and
                   row['display_until'] == receipt['display_deadline_monotonic_ns'] and
                   row['started'] <= start <= end < row['observation_until'] and
                   row['sha256'] == receipt.get('original_envelope_sha256')]
        if is_hazard:
            require(receipt['fusion_state'] == 'hazard' and receipt['localized_hazard'] is True and
                    receipt['hazard_visible'] is True and receipt['fresh_at_start'] is True and receipt['fresh_at_end'] is True,
                    'current hazard flag lacks fresh visible hazard')
            require(_layer(receipt['layers'], 'Hazards', True) and any(r['name'] == 'Hazards' and
                    r['hidden'] is False and r['child_nodes'] > 0 for r in receipt['layers']), 'hazard layer was not rendered')
            matches = [row for row in matches if row['sha256'] and end < row['display_until'] and
                       config['anchor_unix_ns'] + sample_ns < row['sample_until'] and
                       row['physical_upper'] + end - row['started'] < row['physical_until']]
        else:
            require(receipt['localized_hazard'] is False and receipt['hazard_visible'] is False and
                    receipt.get('original_envelope_sha256') is None, 'empty observation retains a hazard')
            matches = [row for row in matches if row['sha256'] is None]
        require(matches, 'native capture has no matching current original reader update')
        current.append({'frame': frame['file'], 'sidecar': frame['receipt'], 'update': matches[-1]['path'],
                        'sample_ns': sample_ns, 'captured_at': end, 'hazard': is_hazard,
                        'camera_b_enabled': switches['camera-b'], 'envelope_sha256': matches[-1]['sha256']})
    return current, stale_empty


def audit(root):
    files = Files(root)
    report = {'version': 1, 'status': 'incomplete', 'passed': False, 'source_run_passed': False,
              'live_nonreplay': False, 'independent_crypto_verification': False,
              'new_time_authority': False, 'issues': []}
    try:
        result = files.json('result.json', 64 << 10)
        report['source_run_passed'] = result.get('passed') is True
        require(report['source_run_passed'] and result.get('gate_completed') is True, 'source run failed or incomplete')
        require(result.get('simulation_only') is True and result['observations']['replay'] is False and
                result['observations']['physical_devices_used'] is False, 'live simulation result required')
        require(result['cleanup']['complete'] is True and all(r['after'] == 'absent' for r in result['cleanup']['resources']),
                'owned resource cleanup incomplete')
        config = files.json('live/bridge-config.json', 64 << 10)
        require(config.get('display_clock') == 'mach_absolute_time_ns', 'shared capture clock mapping required')
        window = files.json('live/native-window.json', 8 << 20)
        off, restored = _controls(files, config, window)
        updates = _updates(files, config)
        frames, stale_empty = _frames(files, config, window, updates)
        report['excluded_stale_empty_frames'] = stale_empty
        before = [f for f in frames if f['hazard'] and f['camera_b_enabled'] and f['sample_ns'] < off]
        empty = [f for f in frames if not f['hazard'] and not f['camera_b_enabled'] and off <= f['sample_ns'] < restored]
        after = [f for f in frames if f['hazard'] and f['camera_b_enabled'] and f['sample_ns'] >= restored]
        require(before and empty and after, 'current hazard, observed source-loss absence and restored hazard frames required')
        chain = next(((a, b, c) for a in before for b in empty for c in after
                      if a['captured_at'] < b['captured_at'] < c['captured_at'] and a['envelope_sha256'] != c['envelope_sha256']), None)
        require(chain is not None, 'distinct original results and ordered native captures required')
        report.update(status='complete', passed=True, live_nonreplay=True,
                      run_id=config['run_id'], actions=6, source_commands=2,
                      original_updates=len(updates), native_frames=len(window['frames']),
                      current_native_hazard_frames=sum(f['hazard'] for f in frames),
                      observed_empty_frames=sum(not f['hazard'] for f in frames),
                      selected={'before_camera_off': chain[0], 'camera_off_empty': chain[1], 'after_camera_on': chain[2]},
                      absence_is_clearance=False)
    except (ValueError, KeyError, TypeError, OSError, OverflowError) as exc:
        report['issues'].append(str(exc) if isinstance(exc, ValueError) else 'required walkthrough artifact missing or malformed')
    report['artifacts'] = files.hashes
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--run-dir', type=Path, required=True)
    parser.add_argument('--output', type=Path)
    args = parser.parse_args()
    report = audit(args.run_dir)
    raw = json.dumps(report, indent=2, sort_keys=True) + '\n'
    if args.output:
        with args.output.open('x') as out:
            out.write(raw)
    else:
        print(raw, end='')
    return 0 if report['passed'] else 1


if __name__ == '__main__':
    raise SystemExit(main())
