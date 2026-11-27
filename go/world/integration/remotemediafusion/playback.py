"""Replay exact B camera/audio originals, A fetched media and B hazard reads.

This checks retained receipt correspondence, not signatures a second time.
Presentation holds original simulation steps; it never interpolates evidence.
"""
import argparse
import base64
from copy import deepcopy
from pathlib import Path
from types import SimpleNamespace

from integration.remotemediafusion import model, proof
from integration.hazard.playback import fusion_view as hazard_view
from integration.people.playback import timestamp_ns, _decoded, _vector
from sim.native_replica_bridge import hazard_native_evidence

MAX_FILE = 8 << 20
NAMES = ('multimodal-hazard', 'original-expiry', 'fresh-restoration')
SERIES = 'RECORDED SIMULATION · REMOTE CAMERA + AUDIO'


def receiver_hazard(step, runner):
    ns, reader = step['simulation_time_ns'], step['reader']
    present = bool(reader.get('hazards'))
    model.require(proof.reader_matches(reader, runner, ns, present) and
                  type(reader.get('tick')) is int and reader['tick'] > 0,
                  'original ready receiver observation required')
    if not present:
        model.require(not step['origins'] and not step['remote_origins'] and step['correspondence'] is None,
                      'empty observation cannot have selected originals')
        return None, {}, None
    checked = proof.correspondence(reader, step['origins'], step['remote_origins'], runner, ns)
    model.require(checked == step['correspondence'], 'recorded correspondence differs from original bytes')
    row = reader['hazards'][0]
    hazard = row['payload']
    model.require(_vector(hazard.get('position_m'), 3, 10000) and
                  _vector([hazard.get('uncertainty_m')], 1, 100) and hazard['uncertainty_m'] >= 0 and
                  _vector([hazard.get('confidence')], 1, 1) and hazard['confidence'] >= 0,
                  'bounded original hazard geometry required')
    original = _decoded(row['envelope_json'], 384 << 10)
    return hazard, {row['envelope_sha256']: original}, checked


def check_captures(step, runner, hazard):
    """Join source files to signed inputs and the exact media fetched by A."""
    ns, scene = step['simulation_time_ns'], runner.scene
    enabled = model.SENSORS if hazard else model.LOCAL_SENSORS
    captures, media = step['captures'], step['media']
    model.require(set(captures) == set(enabled) and
                  set(media) == set(enabled) & {*model.CAMERAS, 'audio-1'}, 'source evidence set differs')
    inputs, fetched = {}, {}
    if hazard:
        publication = next(iter(step['origins'].values()))['publication']
        for item in publication['inputs']:
            signed = model.strict_json(_decoded(item['envelope_json'], 384 << 10), 384 << 10)
            record = model.strict_json(_decoded(signed['record'], 64 << 10), 64 << 10)
            inputs[record['publication']['stream']] = model.strict_json(
                _decoded(record['publication']['payload'], 32 << 10), 32 << 10)
        fetched = {row['source']['service']: _decoded(row['data'], MAX_FILE) for row in publication['media']}
    images = {}
    for sensor, value in captures.items():
        capture = value['capture']
        sample = model.strict_json(_decoded(capture['sample_json'], 32 << 10), 32 << 10)
        model.require(capture.get('scene_boot_id') == scene['boot_id'] and
                      capture.get('simulation_anchor_unix_ns') == model.ANCHOR and
                      sample.get('sample_ns') == ns and sample.get('sensor_id') == sensor and
                      sample.get('run_id') == scene['run_id'] and sample.get('site_id') == scene['site'] and
                      sample.get('frame') == scene['frame'] and sample.get('package_sha256') == scene['package_sha256'],
                      'retained source capture differs from the recorded step')
        if hazard:
            model.require(capture == inputs[sensor], 'source capture differs from A original input')
        raw = base64.b64decode(value.get('media', ''), validate=True)
        ref = sample.get('evidence')
        if ref:
            model.require(raw == media[sensor] and len(raw) == ref['size'] and model.digest(raw) == ref['sha256'] and
                          ref['content_type'] == ('audio/wav' if sensor == 'audio-1' else 'image/png'),
                          'retained source media differs')
            if hazard:
                model.require(raw == fetched[sensor], 'A fetched media differs from original source bytes')
                if sensor in model.CAMERAS:
                    model.require(raw.startswith(b'\x89PNG\r\n\x1a\n'), 'original PNG required')
                    images[sensor] = raw
        else:
            model.require(not raw, 'inline capture unexpectedly has media')
    return images


def fusion_view(hazard, ns, images):
    value = hazard_view(hazard, ns, images)
    if hazard is None:
        value['basis'] = 'Only B camera and audio are disabled. A camera, gas and LiDAR remain enabled in this recorded step.'
        return value
    labels = {'camera-a': 'Camera A · Gateway A · original evidence',
              'camera-b': 'Camera B · Gateway B · fetched by A',
              'audio-1': 'Audio · Gateway B · generated sample fetched by A',
              'gas-1': 'Gas · Gateway A · analytic sample'}
    for row in value['observations']:
        row['label'] = labels[row['id']]
    value['reason'] = 'A fused B camera pixels and audio with its local camera, gas and LiDAR. B read this exact A-signed result.'
    count = sum(row['lidar_summary']['point_count'] for row in hazard['inputs'] if row['kind'] == 'lidar')
    value['basis'] = f'{count} original LiDAR returns. The thumbnails match media fetched by A. B read the result and did not fetch the raw inputs.'
    return value


def chapter(index):
    title, detail = (
        ("B's camera and audio feed A.", 'A fetches the original PNG and WAV through native World evidence calls, then publishes the hazard read by B.'),
        ('Remote inputs expire.', 'Only B camera and audio stop. A camera, gas and LiDAR stay enabled. B reports no live hazard result.'),
        ('Fresh inputs restore the shared hazard.', 'New B camera and audio originals reach A. B reads a new result with its original times and deadlines.'),
    )[index]
    return {'series': SERIES, 'title': title, 'detail': detail}


def build_timeline(runner, steps, duration=40):
    pair, scene = runner.pair, runner.scene
    model.validate_pair(pair)
    model.validate_scene_identity(scene['run_id'], scene)
    model.require(24 <= duration <= 60 and len(steps) == 3 and tuple(s['name'] for s in steps) == NAMES,
                  'three completed recorded steps required')
    frames, originals, assets, geometry = [], {}, {}, None
    previous, first_until, first_originals = -1, 0, set()
    for index, step in enumerate(steps):
        ns, presentation, control = step['simulation_time_ns'], step['presentation'], step['control']
        model.require(type(ns) is int and previous < ns <= 120_000_000_000 and
                      presentation.get('simulation_time_ns') == ns, 'presentation sample time differs')
        sensors = list(model.SENSORS if index != 1 else model.LOCAL_SENSORS)
        request = {'simulation_time_ns': ns, 'sensors': sensors}
        model.require(control.get('request') == request and control.get('response') == {'sequence': index + 1, **request} and
                      control.get('no_mutation_retry') is True and control.get('source_clock_shared') is True and
                      control.get('clock_domain') == scene['clock_domain'] and control.get('anchor_unix_ns') == model.ANCHOR,
                      'original shared-clock source step required')
        current_geometry = presentation['geometry']
        digest = current_geometry.get('model_sha256')
        model.require(current_geometry.get('run_id') == scene['run_id'] and model.digest_shape(digest) and
                      scene['frame']['revision'] == 'office-' + digest[:16], 'reference geometry differs')
        if geometry is None:
            geometry = deepcopy(current_geometry)
        model.require(current_geometry == geometry, 'reference geometry changed between steps')
        for key, expected in (('sensor_sources', model.SENSORS), ('camera_sources', model.CAMERAS)):
            rows = presentation.get(key, [])
            model.require(len(rows) == len(expected) and {r['id']: r['enabled'] for r in rows} ==
                          {sensor: sensor in sensors for sensor in expected}, 'presented source controls differ')
        hazard, raw, checked = receiver_hazard(step, runner)
        model.require(bool(hazard) == (index != 1), 'required recorded receiver observation missing')
        if index == 0:
            first_until = max(timestamp_ns(hazard['valid_until']), *checked['remote_sample_valid_until_unix_ns'].values())
            first_originals = set(raw)
        elif index == 1:
            model.require(model.ANCHOR + ns > first_until, 'absence precedes original camera, audio or hazard expiry')
        else:
            model.require(first_originals.isdisjoint(raw), 'restoration reused the original result')
        images = check_captures(step, runner, hazard)
        for image in images.values():
            assets['assets/' + model.digest(image) + '.png'] = image
        snapshot = {'run_id': scene['run_id'], 'time_s': ns / 1e9, 'phase': 'fusion', 'authorized': False,
                    'stopped': False, 'robots': [], 'jobs': [], 'completed': 0, 'total': 0, 'events': [],
                    'voxels': [], 'voxel_size': .2, 'map_node': 'receiver-b', 'fusion': fusion_view(hazard, ns, images),
                    'network': {'edges': [], 'internet': {'id': 'internet', 'a': 'receiver-b', 'b': 'internet',
                        'enabled': False, 'up': False, 'packets': 0}, 'gossip_rounds': 0, 'peer_voxels': {}}}
        if hazard:
            # receiver_hazard checked this envelope against the source inputs.
            # Keep its exact references for recorded selection/details, without
            # rebasing expiry or claiming a new viewer download.
            digest, original = next(iter(raw.items()))
            signed = model.strict_json(original, 384 << 10)
            record = model.strict_json(_decoded(signed['record'], 64 << 10), 64 << 10)
            result = model.strict_json(_decoded(record['publication']['payload'], 32 << 10), 32 << 10)
            snapshot['fusion'].update(original_envelope_sha256=digest,
                native_evidence=hazard_native_evidence(hazard, result['inputs'],
                    tenant=pair['tenant'], site=scene['site']))
        state = {'schema': 'wendy.macos-demo.state.v1', 'run_id': scene['run_id'], 'sequence': index + 1,
                 'worker_status': 'running', 'last_command_sequence': index + 1, 'snapshot': snapshot,
                 'dynamic_geoms': deepcopy(presentation['dynamic_geoms'])}
        frame = {'at_s': index * duration / 3, 'state': state,
                 'assets': {'fusion/origin/' + sensor + '.png': 'assets/' + model.digest(image) + '.png' for sensor, image in images.items()},
                 'actions': [{'kind': 'camera', 'target': 'Floor plan' if index == 1 else 'Overview'},
                             {'kind': 'layer', 'target': 'Hazards', 'enabled': True},
                             {'kind': 'layer', 'target': 'Observations', 'enabled': True}],
                 'chapter': chapter(index), 'source_step': {'name': step['name'], 'reader_tick': step['reader']['tick'],
                    'original_envelope_sha256': sorted(raw),
                    'remote_original_envelope_sha256': {key: value['proof_sha256'] for key, value in step['remote_origins'].items()}}}
        frames.append(frame)
        if index == 0:
            for at, layer, enabled in ((duration / 8, 'Hazards', False), (duration / 8 + 1.5, 'Hazards', True),
                                       (duration / 5, 'Observations', False), (duration / 5 + 1.5, 'Observations', True)):
                toggle = deepcopy(frame)
                toggle.update(at_s=at, actions=[{'kind': 'layer', 'target': layer, 'enabled': enabled}])
                toggle['chapter'] = {'series': SERIES, 'title': 'Inspect the original camera evidence.',
                    'detail': 'These thumbnails match the bytes A used. Native layer controls separate the reference office from the recorded inference.'}
                frames.append(toggle)
        originals.update(raw)
        previous = ns
    return geometry, sorted(frames, key=lambda f: f['at_s']), originals, assets


def assemble(run_dir, out, duration=40):
    run_dir, out = Path(run_dir).resolve(), Path(out).absolute()
    model.require(not out.exists(), 'fresh playback output required')
    inventory = {}

    def read_bytes(name):
        model.require(isinstance(name, str) and not Path(name).is_absolute() and
                      all(p not in ('', '.', '..') for p in name.split('/')), 'run-relative receipt required')
        path = run_dir
        for part in Path(name).parts:
            path /= part
            model.require(not path.is_symlink(), 'public receipt cannot follow a symlink')
        model.require(path.is_file() and 0 < path.stat().st_size <= MAX_FILE, 'bounded public receipt required')
        raw = path.read_bytes()
        model.require(0 < len(raw) <= MAX_FILE, 'bounded public receipt required')
        inventory[name] = {'bytes': len(raw), 'sha256': model.digest(raw)}
        return raw

    def read(name):
        return model.strict_json(read_bytes(name), MAX_FILE)

    result = read('result.json')
    model.require(result.get('passed') is True and result.get('gate_completed') is True and
                  result.get('cleanup', {}).get('complete') is True, 'passed run with completed cleanup required')
    pair, scene = read('pair-public.json'), read('scene-identity.json')
    tasks = {}
    for node, services in model.SERVICES.items():
        for service in services:
            if service == 'scene':
                continue
            task = read('task-' + node + '-' + service + '.json')
            model.require(all(task.get(k) == v for k, v in {'node': node, 'app': model.APP, 'service': service,
                          'device': pair['nodes'][0 if node == 'a' else 1]['public']['principal']}.items()) and
                          type(task.get('generation')) is int and task['generation'] > 0, 'recorded app task differs')
            tasks[node + '/' + service] = task
    runner = SimpleNamespace(pair=pair, scene=scene, tasks=tasks,
                             inputs={'run_id': scene['run_id'], 'anchor_unix_ns': model.ANCHOR})
    manifest = read('playback-steps.json')
    model.require(manifest.get('version') == 1 and isinstance(manifest.get('steps'), list) and
                  len(manifest['steps']) == 3, 'three-step manifest required')
    steps = []
    for row in manifest['steps']:
        step = {k: row[k] for k in ('name', 'simulation_time_ns')}
        step.update({k: read(row[k]) for k in ('presentation', 'control', 'reader')})
        step['origins'] = read(row['origin']) if row.get('origin') else {}
        step['remote_origins'] = {key: read(name) for key, name in row['remote_origins'].items()}
        step['correspondence'] = read(row['correspondence']) if row.get('correspondence') else None
        step['captures'] = {key: read(name) for key, name in row['captures'].items()}
        step['media'] = {key: read_bytes(name) for key, name in row['media'].items()}
        hazards = step['reader']['hazards']
        model.require(row.get('hazards') == len(hazards) and row.get('original_envelope_sha256') ==
                      [h['envelope_sha256'] for h in hazards] and row.get('reader_observation_semantics') ==
                      'live_at_original_tick_not_at_host_collection', 'manifest receiver observation differs')
        steps.append(step)
    geometry, frames, originals, assets = build_timeline(runner, steps, duration)
    out.mkdir(parents=True, mode=0o700)
    timeline = out / 'timeline'
    (timeline / 'assets').mkdir(parents=True, mode=0o700)
    (timeline / 'geometry.json').write_bytes(model.json_bytes(geometry))
    (timeline / 'frames.jsonl').write_bytes(b''.join(model.json_bytes(frame) for frame in frames))
    for name, raw in assets.items():
        (timeline / name).write_bytes(raw)
    (out / 'originals').mkdir(mode=0o700)
    for digest, raw in originals.items():
        (out / 'originals' / (digest + '.json')).write_bytes(raw)
    report = {'version': 1, 'status': 'complete', 'run_id': scene['run_id'], 'duration_s': duration,
              'recorded_steps': 3, 'presentation_frames': len(frames), 'inputs': inventory,
              'remote_media_source_node': 'b', 'fusion_node': 'a', 'reader_node': 'b',
              'b_originals_and_media_match_a_inputs': True, 'origin_attested_inference': True,
              'receiver_raw_inputs_independently_verified': False, 'new_crypto_verification': False,
              'sensor_interpolation': False, 'sample_deadlines_rebased': False, 'reader_media_fetches': 0,
              'live_commands': 0, 'scope': 'Native UI replay of recorded synthetic B camera/audio, A fusion and B results',
              'originals': {digest: {'bytes': len(raw), 'sha256': digest} for digest, raw in originals.items()},
              'assets': {name: {'bytes': len(raw), 'sha256': model.digest(raw)} for name, raw in assets.items()}}
    for name in ('geometry.json', 'frames.jsonl'):
        raw = (timeline / name).read_bytes()
        report[name] = {'bytes': len(raw), 'sha256': model.digest(raw)}
    (out / 'playback.json').write_bytes(model.json_bytes(report))
    (out / 'README.md').write_text(
        '# Recorded remote-media fusion playback\n\n'
        'Three actual simulation steps are held for presentation. B published camera-b and audio-1; '
        'A fetched those exact media bytes and fused them with its camera-a, gas and LiDAR. '
        'The hazard overlay comes only from the A-signed result read by B. This adapter checks '
        'receipt correspondence, without new cryptographic verification.\n\n'
        'Only B camera and audio are disabled in the middle step. A camera, gas and LiDAR remain enabled. '
        'The empty B read follows both remote input deadlines and the result deadline. It means no live '
        'hazard result, not clearance. Fresh originals restore the result in the final step.\n\n'
        'Thumbnails match the media A fetched. B read the result and did not fetch its raw inputs. '
        'Reference office geometry and native layer/camera actions are presentation only. Video time '
        'holds original sample time, without interpolation, rebasing or live commands.\n')
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
