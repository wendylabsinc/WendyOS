"""Audit original native captures of shared hazards and anonymous people tracks.

This is offline receipt correspondence, not new cryptographic verification,
inference or time authority. Each layer retains its own reader clock/deadlines.
"""
from __future__ import annotations

import argparse
import json
import math
from pathlib import Path

from integration.livetwin.proof import Files, _layer, _sources, _updates, digest, require
from sim.native_replica_bridge import NativeReplicaBridge, MAX_DISPLAY_AGE_NS

HAZARD_SOURCES = {'camera-a', 'camera-b', 'lidar-atlas', 'gas-1', 'audio-1'}
PEOPLE_SOURCES = {'people-front', 'people-oblique'}
SOURCES = HAZARD_SOURCES | PEOPLE_SOURCES
PLAN = [('floor-plan', 'camera'), ('camera-off', 'command'), ('camera-on', 'command'),
        ('people-inspector', 'inspector'), ('overview', 'camera'),
        ('tracks-off', 'toggle_layer'), ('tracks-on', 'toggle_layer'),
        ('hazard-inspector', 'inspector'), ('observations-off', 'toggle_layer'),
        ('observations-on', 'toggle_layer')]


def switches(rows):
    result = _sources(rows)
    require(set(result) == SOURCES, 'all seven shared source switches required')
    return result


def controls(files, config, window):
    actions = window['actions']
    require([(a['id'], a['action']) for a in actions] == PLAN, 'all ten walkthrough actions required')
    for action in actions:
        require(action == files.json('session/qa-result-' + action['id'] + '.json', 32 << 10),
                'saved QA action differs')
        require(action['schema'] == 'wendy.macos-demo.app-qa.v1' and action['run_id'] == config['run_id'] and
                action['kind'] == 'application_model_check_not_OS_input' and action['accepted'] is True and
                action.get('error') is None, 'walkthrough action was not accepted')
    require(actions[0]['camera'] == 'Floor plan' and actions[4]['camera'] == 'Overview', 'camera presets not observed')
    require(actions[3]['inspector_panel'] == 'People' and actions[7]['inspector_panel'] == 'Hazard',
            'both inspectors must be observed')
    for index, name, enabled in ((5, 'Tracks', False), (6, 'Tracks', True),
                                 (8, 'Observations', False), (9, 'Observations', True)):
        require(_layer(actions[index]['layers'], name, enabled), 'layer change not observed')
    require(len(files.names('session/commands/*.json', 2)) ==
            len(files.names('session/receipts/*.json', 2)) == 2, 'exactly two source commands required')
    times = []
    for index in (1, 2):
        filename = '%06d.json' % index
        require(files.json('session/commands/' + filename, 4096) == {
            'schema': 'wendy.macos-demo.command.v1', 'run_id': config['run_id'], 'sequence': index,
            'action': 'toggle_sensor', 'target': 'camera-b'}, 'camera-b command differs')
        receipt = files.json('session/receipts/' + filename, 32 << 10)
        require(receipt['schema'] == 'wendy.macos-demo.command-result.v1' and receipt['run_id'] == config['run_id'] and
                receipt['sequence'] == index and receipt['action'] == 'toggle_sensor' and receipt['accepted'] is True and
                receipt.get('error') is None and receipt['no_mutation_retry'] is True, 'source command not acknowledged')
        request, response = receipt['source_request'], receipt['source_response']
        require(response == {'sequence': response['sequence'], **request} and
                files.json('live/source-%04d.json' % response['sequence'], 32 << 10) ==
                {'request': request, 'response': response}, 'original source control differs')
        expected = SOURCES - ({'camera-b'} if index == 1 else set())
        require(len(request['sensors']) == len(expected) and set(request['sensors']) == expected,
                'camera toggle changed other shared sources')
        require(actions[index]['worker_command_sequence'] == index, 'QA command acknowledgment differs')
        times.append(request['simulation_time_ns'])
    require(all(type(t) is int for t in times) and 0 < times[0] < times[1], 'source toggle times differ')
    return times


def people_updates(files, config):
    rows, presentations = [], []
    names = files.names('live/people-update-*.json', 512)
    require(names, 'original people reader updates required')
    for name in names:
        update = files.json(name)
        if update['reader']['status'] != 'ready':
            continue
        started = update['collection_started_monotonic_ns']
        bridge = NativeReplicaBridge(config, update['presentation'], monotonic_ns=lambda: started)
        require(bridge.ingest_people(update), 'original people update not accepted')
        current = bridge.people.latest
        source = switches(update['presentation']['sensor_sources'])
        sample_ns = current['sample'] - config['anchor_unix_ns']
        presentations.append({'path': name, 'started': started, 'sample_ns': sample_ns, 'switches': source})
        for parsed in current['parsed']:
            rows.append({'path': name, 'id': parsed['track']['id'], 'sha256': parsed['sha256'],
                         'started': started, 'sample_ns': sample_ns, 'sample_until': parsed['sample_until'],
                         'physical_until': parsed['physical_until'], 'physical_from': parsed['physical_from'],
                         'physical_lower': current['lower'], 'physical_upper': current['upper'],
                         'display_until': min(started + MAX_DISPLAY_AGE_NS,
                                              started + parsed['physical_until'] - current['upper'])})
    return rows, presentations


def native_frames(files, config, window, hazards, people, presentations):
    frames = window['frames']
    require(isinstance(frames, list) and 1 <= len(frames) <= 2000, 'native captured frames required')
    accepted, stale_empty, track_frames, seen = [], 0, 0, set()
    for frame in frames:
        require(frame['file'] not in seen, 'duplicate captured frame')
        seen.add(frame['file'])
        receipt = files.json(frame['receipt'], 32 << 10)
        require({k: v for k, v in frame.items() if k not in ('file', 'receipt', 'elapsed_s')} == receipt,
                'native window ledger differs from capture sidecar')
        png = files.read(frame['file'], 16 << 20)
        require(png.startswith(b'\x89PNG\r\n\x1a\n') and receipt['png_sha256'] == digest(png) and
                receipt['png_bytes'] == len(png), 'native PNG differs from capture sidecar')
        require(receipt['schema'] == 'wendy.macos-demo.app-capture.v1' and receipt['run_id'] == config['run_id'] and
                receipt['mode'] == 'live-native-replica' and receipt['display_clock'] == 'mach_absolute_time_ns' and
                receipt['state_sequence'] == receipt['renderer_sequence'] > 0, 'native capture scope/render differs')
        start, end = receipt['capture_started_monotonic_ns'], receipt['capture_finished_monotonic_ns']
        require(type(start) is int and type(end) is int and 0 <= start <= end, 'capture interval differs')
        seconds = receipt['sample_time_s']
        require(type(seconds) in (int, float) and math.isfinite(seconds) and seconds >= 0, 'capture sample differs')
        sample_ns = round(seconds * 1_000_000_000)
        source = switches(receipt['sensor_sources'])
        # A newer ready observation from either reader may advance geometry.
        # It cannot refresh the other layer's original observation/deadlines.
        known = [row for row in presentations if row['sample_ns'] == sample_ns and
                 row['switches'] == source and row['started'] <= start]
        require(known, 'capture presentation lacks an original shared source observation')
        tracks = receipt['native_tracks']
        require(isinstance(tracks, list) and len(tracks) <= 8 and
                len({t['id'] for t in tracks}) == len(tracks), 'bounded distinct captured tracks required')
        current_tracks = []
        for track in tracks:
            require(all(type(track[k]) is bool for k in ('visible', 'fresh_at_start', 'fresh_at_end')),
                    'track visibility/freshness flags required')
            matches = [row for row in people if row['id'] == track['id'] and
                       row['sha256'] == track['original_envelope_sha256'] and
                       row['display_until'] == track['display_deadline_monotonic_ns'] and row['started'] <= start and
                       row['sample_ns'] <= sample_ns and config['anchor_unix_ns'] + sample_ns < row['sample_until'] and
                       row['physical_lower'] >= row['physical_from']]
            require(matches, 'captured track has no matching original people observation')
            row = matches[-1]
            require(track['fresh_at_start'] == (start < row['display_until']) and
                    track['fresh_at_end'] == (end < row['display_until']), 'track freshness differs from original deadline')
            if track['visible'] and track['fresh_at_start'] and track['fresh_at_end']:
                require(_layer(receipt['layers'], 'Tracks', True) and any(r['name'] == 'Tracks' and
                        r['hidden'] is False and r['child_nodes'] > 0 for r in receipt['layers']), 'track layer was not rendered')
                require(row['physical_upper'] + end - row['started'] < row['physical_until'], 'track physical bound expired')
                current_tracks.append({'id': row['id'], 'envelope_sha256': row['sha256'], 'update': row['path'],
                                       'display_deadline_monotonic_ns': row['display_until']})
        track_frames += bool(current_tracks)
        is_hazard = receipt['current_native_hazard_captured'] is True
        is_empty = receipt['fusion_state'] == 'no_live_result'
        observation_fresh = (receipt['native_observation_fresh_at_start'] is True and
                             receipt['native_observation_fresh_at_end'] is True)
        if is_empty and not is_hazard and not observation_fresh:
            stale_empty += 1
            continue
        if not (is_hazard or is_empty):
            continue
        require(observation_fresh, 'claimed native hazard observation was stale during capture')
        matches = [row for row in hazards if row['sample_ns'] <= sample_ns and
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
        require(matches, 'native hazard capture has no matching current original reader update')
        row = matches[-1]
        accepted.append({'frame': frame['file'], 'sidecar': frame['receipt'], 'update': row['path'],
                         'sample_ns': sample_ns, 'hazard_sample_ns': row['sample_ns'],
                         'captured_at': end, 'hazard': is_hazard, 'camera_b_enabled': source['camera-b'],
                         'envelope_sha256': row['sha256'], 'tracks': current_tracks})
    return accepted, stale_empty, track_frames


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
        require(config.get('display_clock') == 'mach_absolute_time_ns' and set(config['input_sources']) == HAZARD_SOURCES and
                set(config['people']['input_sources']) == PEOPLE_SOURCES, 'exact shared readers and capture clock required')
        window = files.json('live/native-window.json', 8 << 20)
        off, restored = controls(files, config, window)
        hazards = _updates(files, config)
        for row in hazards:
            require(set(row['switches']) == SOURCES, 'all seven hazard presentation switches required')
        people, presentations = people_updates(files, config)
        frames, stale_empty, track_frames = native_frames(files, config, window, hazards, people, hazards + presentations)
        report['excluded_stale_empty_frames'] = stale_empty
        before = [f for f in frames if f['hazard'] and f['camera_b_enabled'] and f['hazard_sample_ns'] < off]
        empty = [f for f in frames if not f['hazard'] and not f['camera_b_enabled'] and
                 off <= f['hazard_sample_ns'] < restored]
        after = [f for f in frames if f['hazard'] and f['camera_b_enabled'] and f['hazard_sample_ns'] >= restored]
        together = [f for f in frames if f['hazard'] and f['tracks']]
        require(before and empty and after, 'current hazard, observed source-loss absence and restored hazard frames required')
        require(together, 'one actual capture must show a current hazard and visible fresh tracks together')
        chain = next(((a, b, c) for a in before for b in empty for c in after
                      if a['captured_at'] < b['captured_at'] < c['captured_at'] and
                      a['envelope_sha256'] != c['envelope_sha256']), None)
        require(chain is not None, 'distinct original hazards and ordered native captures required')
        report.update(status='complete', passed=True, live_nonreplay=True, run_id=config['run_id'], actions=10,
                      source_commands=2, original_hazard_updates=len(hazards),
                      original_people_updates=len(presentations), native_frames=len(window['frames']),
                      current_native_hazard_frames=sum(f['hazard'] for f in frames), current_native_track_frames=track_frames,
                      current_combined_frames=len(together), observed_empty_frames=sum(not f['hazard'] for f in frames),
                      unique_hazard_originals=len({r['sha256'] for r in hazards if r['sha256']}),
                      unique_people_originals=len({r['sha256'] for r in people}),
                      selected={'before_camera_off': chain[0], 'camera_off_empty': chain[1],
                                'after_camera_on': chain[2], 'together': together[0]}, absence_is_clearance=False)
    except (ValueError, KeyError, TypeError, OSError, OverflowError) as exc:
        report['issues'].append(str(exc) if isinstance(exc, ValueError) else 'required shared walkthrough artifact missing or malformed')
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
        with args.output.open('x') as output:
            output.write(raw)
    else:
        print(raw, end='')
    raise SystemExit(0 if report['passed'] else 1)


if __name__ == '__main__':
    main()
