"""Build native UI playback from recorded, successful receiver observations.

This checks receipt/byte correspondence; it does not run a new signature verifier.
No sample timestamp, inferred position or inference deadline is interpolated.
"""
import argparse
import base64
from copy import deepcopy
from datetime import datetime, timezone
import math
from pathlib import Path
import re

from integration.people import model

MAX_FILE = 8 << 20
NAMES = ('two-camera-original', 'new-observation', 'original-expiry', 'fresh-restoration')


def timestamp_ns(value):
    """Parse Go's UTC RFC3339Nano without rounding away the original nanoseconds."""
    model.require(isinstance(value, str), 'recorded UTC sample timestamp required')
    match = re.fullmatch(r'(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d{1,9}))?Z', value)
    model.require(match is not None, 'recorded UTC sample timestamp required')
    try:
        seconds = int(datetime.strptime(match[1], '%Y-%m-%dT%H:%M:%S').replace(tzinfo=timezone.utc).timestamp())
    except ValueError as error:
        raise model.Refused('invalid recorded timestamp') from error
    return seconds * 1_000_000_000 + int((match[2] or '').ljust(9, '0'))


def _decoded(value, limit):
    model.require(isinstance(value, str) and len(value) <= 4 * ((limit + 2) // 3), 'bounded original bytes required')
    try:
        raw = base64.b64decode(value, validate=True)
    except ValueError as error:
        raise model.Refused('invalid original byte encoding') from error
    model.require(0 < len(raw) <= limit, 'bounded original bytes required')
    return raw


def _vector(value, length, maximum):
    return (isinstance(value, list) and len(value) == length and
            all(type(n) in (int, float) and math.isfinite(n) and abs(n) <= maximum for n in value))


def receiver_tracks(receipt, origins, pair, scene, simulation_ns, generation):
    """Map a ready observation, binding it back to exact acknowledged A bytes."""
    model.require(isinstance(receipt, dict) and receipt.get('version') == 1 and
                  receipt.get('status') == 'ready' and receipt.get('run_id') == scene['run_id'] and
                  receipt.get('reader_generation') == generation and
                  receipt.get('origin_attested_inference') is True and
                  receipt.get('camera_inputs_independently_verified') is False,
                  'successful scoped receiver observation required')
    now = timestamp_ns(receipt.get('sample_time'))
    model.require(now == scene['anchor_unix_ns'] + simulation_ns, 'receiver and scene sample times differ')
    rows = receipt.get('tracks')
    model.require(isinstance(rows, list) and len(rows) <= 8 and isinstance(origins, dict), 'bounded receiver tracks required')
    tracks, originals, ids = [], {}, set()
    origin_device = pair['nodes'][0]['public']['principal']
    for row in rows:
        raw = _decoded(row.get('envelope_json'), 384 << 10)
        digest = model.digest(raw)
        model.require(digest == row.get('envelope_sha256') and digest not in originals,
                      'receiver envelope digest differs')
        acknowledged = origins.get(digest, {}).get('publication', {})
        model.require(acknowledged.get('locally_verified') is True and
                      _decoded(acknowledged.get('signed_record_json'), 384 << 10) == raw,
                      'receiver bytes differ from acknowledged origin')
        envelope = model.strict_json(raw, 384 << 10)
        record_raw = _decoded(envelope.get('record'), 64 << 10)
        model.require(model.digest(record_raw) == row.get('record_sha256'), 'receiver record digest differs')
        record = model.strict_json(record_raw, 64 << 10)
        scope, publication = record.get('scope', {}), record.get('publication', {})
        model.require(all(scope.get(k) == v for k, v in {
            'tenant': pair['tenant'], 'site': scene['site'], 'device': origin_device,
            'app': model.APP, 'service': 'people', 'execution_domain': 'simulation'}.items()), 'track origin differs')
        model.require(row.get('version') == {'epoch': record.get('epoch'), 'sequence': record.get('sequence')} and
                      all(type(record.get(k)) is int and record[k] > 0 for k in ('epoch', 'sequence')),
                      'recorded source version differs')
        model.require(publication.get('component_type') == model.RESULT and publication.get('stream') == model.STREAM and
                      publication.get('frame') == scene['frame'] and publication.get('tombstone') is False,
                      'track result publication differs')
        result = model.strict_json(_decoded(publication.get('payload'), 32 << 10), 32 << 10)
        model.require(result.get('version') == 1 and result.get('result_schema') == 'wendy.person.track.v1', 'typed track required')
        payload = model.strict_json(_decoded(result.get('result_json'), 24 << 10), 24 << 10)
        model.require(payload == row.get('payload') and payload.get('schema') == 'wendy.person.track.v1' and
                      payload.get('run_id') == scene['run_id'] and payload.get('clock_domain') == scene['clock_domain'] and
                      payload.get('frame') == scene['frame'], 'decoded track payload differs')
        observed, until = timestamp_ns(payload.get('observed_at')), timestamp_ns(payload.get('valid_until'))
        model.require(observed <= now < until, 'recorded track is not live at receiver observation')
        timing = publication.get('timing', {})
        sample = timestamp_ns(timing.get('sample_time'))
        ttl, error, age = publication.get('ttl_ns'), timing.get('error_bound_ns'), timing.get('age_hint_ns')
        model.require(timing.get('clock_domain') == scene['clock_domain'] and timing.get('error_known') is True and
                      all(type(n) is int and n >= 0 for n in (ttl, error, age)) and
                      sample <= now < sample + ttl - max(error, age), 'outer result is not live at receiver observation')
        identity = payload.get('track_id')
        model.require(isinstance(identity, str) and 0 < len(identity) <= 160 and identity not in ids and
                      publication.get('entity') == scene['run_id'] + '/track/' + identity, 'track identity differs')
        model.require(_vector(payload.get('position_xz'), 2, 10000) and _vector(payload.get('covariance'), 4, 100) and
                      type(payload.get('ambiguous')) is bool and type(payload.get('motion_known')) is bool,
                      'native track geometry differs')
        covariance = payload['covariance']
        model.require(covariance[0] >= 0 and covariance[3] >= 0 and abs(covariance[1] - covariance[2]) < 1e-6 and
                      covariance[0] * covariance[3] - covariance[1] * covariance[2] >= 0, 'invalid track covariance')
        members = payload.get('current')
        model.require(isinstance(members, list) and 1 <= len(members) <= 8, 'current input members required')
        native_members = []
        for member in members:
            image = member.get('image', {})
            model.require(member.get('sensor_id') in model.CAMERAS and isinstance(member.get('sample_id'), str) and
                          0 < len(member['sample_id']) <= 512 and model.digest_shape(image.get('sha256')),
                          'original camera lineage required')
            native_members.append({'source': member['sensor_id'], 'sample_id': member['sample_id'],
                                   'evidence': [{'sha256': image['sha256']}]})
        track = {'id': identity, 'state': 'observed', 'position_xz': deepcopy(payload['position_xz']),
                 'covariance': deepcopy(covariance), 'last_observed': payload['observed_at'],
                 'valid_until': payload['valid_until'], 'ambiguous': payload['ambiguous'],
                 'motion_known': payload['motion_known'], 'members': native_members}
        if payload['motion_known']:
            model.require(_vector(payload.get('velocity_xz'), 2, 50), 'known motion needs its recorded velocity')
            track['velocity_xz'] = deepcopy(payload['velocity_xz'])
        else:
            model.require('velocity_xz' not in payload, 'unknown motion cannot display a velocity')
        tracks.append(track)
        originals[digest] = raw
        ids.add(identity)
    model.require(set(origins) == set(originals), 'origin acknowledgment set differs from selected remote read')
    return tracks, originals


def _chapter(name):
    title, detail = {
        'two-camera-original': ('Camera inference reaches another agent.', 'A publishes anonymous tracks. B reads the exact origin-signed results through its World API.'),
        'new-observation': ('New pixels. New measurements.', 'Position and available motion come from recorded camera observations, with visible uncertainty.'),
        'original-expiry': ('Switch off the sources. Estimates expire.', 'B reports no live tracks at this recorded step. Missing observations do not establish an empty room.'),
        'fresh-restoration': ('Restore cameras. Observe again.', 'Fresh image-derived results arrive on the receiver replica. Original sample times and deadlines remain intact.'),
    }[name]
    return {'series': 'NATIVE REPLICA PERCEPTION', 'title': title, 'detail': detail}


def build_timeline(pair, scene, generation, steps, duration=40):
    model.validate_pair(pair)
    scene = model.validate_scene_identity(scene['run_id'], scene)
    model.require(type(generation) is int and generation > 0 and 24 <= duration <= 60 and
                  len(steps) == 4 and tuple(s['name'] for s in steps) == NAMES, 'complete four-step playback required')
    frames, originals, geometry, prior_ns, prior_cameras = [], {}, None, -1, None
    for index, step in enumerate(steps):
        ns, presentation, control = step['simulation_time_ns'], step['presentation'], step['control']
        model.require(type(ns) is int and prior_ns < ns <= 120_000_000_000 and
                      presentation.get('simulation_time_ns') == ns, 'presentation step differs')
        request, response = control.get('request', {}), control.get('response', {})
        cameras = request.get('cameras')
        model.require(request.get('simulation_time_ns') == ns and isinstance(cameras, list) and
                      len(set(cameras)) == len(cameras) and all(c in model.CAMERAS for c in cameras) and
                      response == {'sequence': index + 1, 'simulation_time_ns': ns, 'cameras': cameras} and
                      control.get('no_mutation_retry') is True, 'successful original source control required')
        clock = control.get('receiver_clock', {})
        model.require(clock.get('domain') == scene['clock_domain'] and timestamp_ns(clock.get('now')) == model.ANCHOR + ns,
                      'recorded receiver clock differs')
        current_geometry = presentation.get('geometry')
        model.require(isinstance(current_geometry, dict) and current_geometry.get('run_id') == scene['run_id'] and
                      current_geometry.get('model_sha256') == scene['package_sha256'], 'reference geometry differs')
        if geometry is None:
            geometry = deepcopy(current_geometry)
        model.require(current_geometry == geometry, 'reference geometry changed between steps')
        camera_sources = presentation.get('camera_sources')
        model.require(isinstance(camera_sources, list) and len(camera_sources) == 2 and
                      {row['id']: row['enabled'] for row in camera_sources} == {c: c in cameras for c in model.CAMERAS},
                      'presented source controls differ from actual step')
        tracks, raw = receiver_tracks(step['reader'], step.get('origins', {}), pair, scene, ns, generation)
        model.require(bool(tracks) == bool(cameras), 'selected step does not show the required recorded observation')
        if cameras:
            model.require(any(timestamp_ns(track['last_observed']) == model.ANCHOR + ns for track in tracks),
                          'new source step has no received observation from that step')
        originals.update(raw)
        native_sources = [{'id': c, 'label': 'Front camera' if c == 'camera-a' else 'Oblique camera',
                           'enabled': c in cameras} for c in model.CAMERAS]
        tracking = {'participant_id': 'receiver-b', 'now_ns': ns, 'tracks': tracks, 'cameras': [], 'camera_sources': native_sources}
        snapshot = {'run_id': scene['run_id'], 'time_s': ns / 1e9, 'phase': 'tracking', 'authorized': False,
                    'stopped': False, 'robots': [], 'jobs': [], 'completed': 0, 'total': 0, 'events': [],
                    'voxels': [], 'voxel_size': .2, 'map_node': 'receiver-b', 'tracking': tracking,
                    'network': {'edges': [], 'internet': {'id': 'internet', 'a': 'receiver-b', 'b': 'internet',
                        'enabled': False, 'up': False, 'packets': 0}, 'gossip_rounds': 0, 'peer_voxels': {}}}
        state = {'schema': 'wendy.macos-demo.state.v1', 'run_id': scene['run_id'], 'sequence': index + 1,
                 'worker_status': 'running', 'last_command_sequence': response['sequence'],
                 'snapshot': snapshot, 'dynamic_geoms': deepcopy(presentation['dynamic_geoms'])}
        at = index * duration / 4
        actions = [{'kind': 'camera', 'target': 'Floor plan' if index == 1 else 'Overview'}]
        if index == 0:
            actions += [{'kind': 'layer', 'target': 'Tracks', 'enabled': True}]
        frame = {'at_s': at, 'state': state, 'assets': {}, 'actions': actions, 'chapter': _chapter(step['name']),
                 'source_step': {'name': step['name'], 'reader_tick': step['reader']['tick'],
                                 'original_envelope_sha256': sorted(raw)}}
        if prior_cameras is not None and set(prior_cameras) != set(cameras):
            frame['pointer'] = {'x': 1785, 'y': 269, 'label': 'Restore camera sources' if cameras else 'Disable camera sources'}
        frames.append(frame)
        if index == 0:
            for offset, enabled in ((duration / 8, False), (duration / 8 + 1.5, True)):
                toggle = deepcopy(frame)
                toggle.update(at_s=offset, actions=[{'kind': 'layer', 'target': 'Tracks', 'enabled': enabled}])
                toggle['chapter'] = {'series': 'NATIVE REPLICA PERCEPTION', 'title': 'Separate the scene from its estimates.',
                                     'detail': 'The Tracks layer shows only the receiver’s recorded, origin-attested inference.'}
                frames.append(toggle)
        prior_ns, prior_cameras = ns, cameras
    frames.sort(key=lambda frame: frame['at_s'])
    return geometry, frames, originals


def assemble(run_dir, out, duration=40):
    run_dir, out = Path(run_dir).resolve(), Path(out).absolute()
    model.require(not out.exists(), 'fresh playback output required')
    inventory = {}

    def read(name):
        path = Path(name)
        model.require(isinstance(name, str) and not path.is_absolute() and path.parts and
                      all(part not in ('', '.', '..') for part in name.split('/')), 'run-relative receipt required')
        path = run_dir / path
        model.require(not path.is_symlink() and path.is_file() and 0 < path.stat().st_size <= MAX_FILE, 'bounded public receipt required')
        raw = path.read_bytes()
        model.require(0 < len(raw) <= MAX_FILE, 'bounded public receipt required')
        inventory[name] = {'bytes': len(raw), 'sha256': model.digest(raw)}
        return model.strict_json(raw, MAX_FILE)

    result = read('result.json')
    model.require(result.get('passed') is True and result.get('gate_completed') is True, 'successful recorded run required')
    pair, scene = read('pair-public.json'), read('scene-identity.json')
    task = read('task-b-reader.json')
    model.require(all(task.get(k) == v for k, v in {'node': 'b', 'app': model.APP, 'service': 'reader',
                  'device': pair['nodes'][1]['public']['principal']}.items()), 'recorded reader task differs')
    manifest = read('playback-steps.json')
    model.require(manifest.get('version') == 1 and isinstance(manifest.get('steps'), list), 'recorded step manifest required')
    steps = []
    for row in manifest['steps']:
        step = {k: row[k] for k in ('name', 'simulation_time_ns')}
        step.update({k: read(row[k]) for k in ('presentation', 'control', 'reader')})
        step['origins'] = read(row['origin']) if row.get('origin') else {}
        model.require(row.get('original_envelope_sha256') == [track['envelope_sha256'] for track in step['reader']['tracks']],
                      'step manifest original set differs')
        steps.append(step)
    geometry, frames, originals = build_timeline(pair, scene, task['generation'], steps, duration)
    out.mkdir(parents=True, mode=0o700)
    timeline = out / 'timeline'
    timeline.mkdir(mode=0o700)
    (timeline / 'geometry.json').write_bytes(model.json_bytes(geometry))
    (timeline / 'frames.jsonl').write_bytes(b''.join(model.json_bytes(frame) for frame in frames))
    (out / 'originals').mkdir(mode=0o700)
    for digest, raw in originals.items():
        (out / 'originals' / (digest + '.json')).write_bytes(raw)
    report = {'version': 1, 'status': 'complete', 'run_id': scene['run_id'], 'duration_s': duration,
              'recorded_steps': len(steps), 'presentation_frames': len(frames), 'inputs': inventory,
              'origin_attested_inference': True, 'receiver_camera_inputs_independently_verified': False,
              'new_crypto_verification': False, 'sensor_interpolation': False, 'sample_deadlines_rebased': False,
              'camera_thumbnails': 0, 'live_commands': 0,
              'scope': 'Native UI replay of actual stepped MuJoCo simulation and native replica output',
              'originals': {digest: {'bytes': len(raw), 'sha256': digest} for digest, raw in originals.items()}}
    for name in ('geometry.json', 'frames.jsonl'):
        raw = (timeline / name).read_bytes()
        report[name] = {'bytes': len(raw), 'sha256': model.digest(raw)}
    (out / 'playback.json').write_bytes(model.json_bytes(report))
    (out / 'README.md').write_text(
        '# Recorded native replica playback\n\n'
        'This private timeline replays four successful, explicit MuJoCo steps. '
        'Its track results were published on A and read by the ordinary reader on B. '
        'The reader verified the origin-attested inference; it did not independently fetch or re-run camera inputs.\n\n'
        'Video time holds each recorded step for presentation. Simulation timestamps, track positions, '
        'motion and deadlines are unchanged, with no interpolation. Building and Tracks layer toggles '
        'and camera presets are presentation actions. Source-control cues correspond to recorded successful source steps. '
        'There are no live commands, invented detections, thumbnails or inferred network statistics.\n\n'
        'Exact receiver envelope bytes are retained in `originals/`. The adapter checks them against '
        'the acknowledged origin and recorded payload; it does not perform new cryptographic verification. '
        'The geometry and actor transforms are reference simulation presentation, not tracker input.\n\n'
        'Render only after reviewing this recorded timeline, using '
        '`python3 -m integration.nativeapp.render --timeline /absolute/output/timeline '
        '--out /absolute/fresh-promo --duration 40 --fps 12 --poster-at 0`.\n')
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--run-dir', type=Path, required=True)
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--duration', type=float, default=40)
    args = parser.parse_args()
    report = assemble(args.run_dir, args.out, args.duration)
    print(model.json_bytes({'status': report['status'], 'timeline': str(args.out.absolute() / 'timeline'),
                            'recorded_steps': report['recorded_steps']}).decode(), end='')


if __name__ == '__main__':
    main()
