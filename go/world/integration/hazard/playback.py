"""Replay successful native receiver hazards, preserving their original bytes.

This is receipt correspondence, not a second cryptographic verifier. Source
images are labeled origin evidence; they were not fetched by the B reader.
"""
import argparse
import base64
from copy import deepcopy
from pathlib import Path

from integration.hazard import model
from integration.people.playback import timestamp_ns, _decoded, _vector

MAX_FILE = 8 << 20
NAMES = ('multimodal-hazard', 'original-expiry', 'fresh-restoration')


def receiver_hazard(receipt, origins, pair, scene, ns, generation):
    """Accept only the recorded, successfully verified B observation."""
    model.require(isinstance(receipt, dict) and receipt.get('version') == 1 and
                  receipt.get('status') == 'ready' and receipt.get('run_id') == scene['run_id'] and
                  receipt.get('reader_generation') == generation and
                  receipt.get('origin_attested_inference') is True and
                  receipt.get('raw_inputs_independently_verified') is False,
                  'ready scoped receiver observation required')
    now = timestamp_ns(receipt.get('sample_time'))
    model.require(now == model.ANCHOR + ns, 'receiver and presentation sample times differ')
    rows = receipt.get('hazards')
    model.require(isinstance(rows, list) and len(rows) <= 1 and isinstance(origins, dict),
                  'single fixed-source hazard observation required')
    if not rows:
        model.require(not origins, 'empty read cannot have a selected origin')
        return None, {}
    row = rows[0]
    raw = _decoded(row.get('envelope_json'), 384 << 10)
    digest = model.digest(raw)
    model.require(digest == row.get('envelope_sha256') and set(origins) == {digest},
                  'receiver original digest differs')
    acknowledged = origins[digest].get('publication', {})
    model.require(acknowledged.get('status') == 'complete' and acknowledged.get('locally_verified') is True and
                  acknowledged.get('proof_sha256') == digest and
                  _decoded(acknowledged.get('signed_record_json'), 384 << 10) == raw,
                  'receiver bytes differ from acknowledged origin')
    signed = model.strict_json(raw, 384 << 10)
    record_raw = _decoded(signed.get('record'), 64 << 10)
    model.require(model.digest(record_raw) == row.get('record_sha256') == acknowledged.get('record_sha256'),
                  'receiver record hash differs')
    record = model.strict_json(record_raw, 64 << 10)
    model.require(row.get('version') == {k: record.get(k) for k in ('epoch', 'sequence')} and
                  all(type(record.get(k)) is int and record[k] > 0 for k in ('epoch', 'sequence')),
                  'receiver source version differs')
    origin_device = pair['nodes'][0]['public']['principal']
    scope = record.get('scope', {})
    model.require(all(scope.get(k) == v for k, v in {
        'tenant': pair['tenant'], 'site': scene['site'], 'device': origin_device,
        'app': model.APP, 'service': 'fusion', 'execution_domain': 'simulation'}.items()),
        'hazard origin differs')
    publication = record.get('publication', {})
    model.require(publication.get('component_type') == model.RESULT and publication.get('stream') == model.STREAM and
                  publication.get('entity') == scene['run_id'] + '/possible-gas-hazard' and
                  publication.get('frame') == scene['frame'] and publication.get('tombstone') is False,
                  'hazard publication differs')
    result = model.strict_json(_decoded(publication.get('payload'), 32 << 10), 32 << 10)
    model.require(result.get('version') == 1 and result.get('result_schema') == 'wendy.hazard.v1',
                  'known typed hazard required')
    hazard = model.strict_json(_decoded(result.get('result_json'), 32 << 10), 32 << 10)
    model.require(hazard == row.get('payload') and hazard.get('label') == 'possible-gas-hazard',
                  'receiver hazard differs from exact original payload')
    timing = publication.get('timing', {})
    sample = timestamp_ns(timing.get('sample_time'))
    ttl, error, age = publication.get('ttl_ns'), timing.get('error_bound_ns'), timing.get('age_hint_ns')
    model.require(timing.get('clock_domain') == scene['clock_domain'] and timing.get('error_known') is True and
                  all(type(n) is int and n >= 0 for n in (ttl, error, age)) and
                  sample <= now < sample + ttl - max(error, age) and now < timestamp_ns(hazard.get('valid_until')),
                  'hazard is not live at the recorded receiver time')
    model.require(timestamp_ns(hazard.get('physical_valid_until')) > 0 and
                  _vector(hazard.get('position_m'), 3, 10000) and
                  _vector([hazard.get('uncertainty_m')], 1, 100) and hazard['uncertainty_m'] >= 0 and
                  _vector([hazard.get('confidence')], 1, 1) and hazard['confidence'] >= 0,
                  'bounded original geometry and physical deadline required')
    inputs = hazard.get('inputs')
    model.require(isinstance(inputs, list) and len(inputs) == len(model.SENSORS) and
                  {i.get('sensor_id') for i in inputs} == set(model.SENSORS), 'five original inputs required')
    for item in inputs:
        sensor = item['sensor_id']
        source = item.get('source', {})
        capture = item.get('capture', {})
        model.require(source == {'device': origin_device, 'app': model.APP, 'service': sensor, 'stream': sensor} and
                      timestamp_ns(item.get('sample_time')) == now and now < timestamp_ns(item.get('valid_until')) and
                      capture.get('scene_boot_id') == scene['boot_id'] and
                      timestamp_ns(capture.get('simulation_anchor')) == model.ANCHOR,
                      'input original scope or source step differs')
        if item.get('kind') != 'lidar':
            s = item.get('sample', {})
            model.require(s.get('id') == item.get('sample_id') and s.get('sensor_id') == sensor and
                          s.get('run_id') == scene['run_id'] and s.get('frame') == scene['frame'] and
                          s.get('sample_ns') == ns and s.get('site_id') == scene['site'] and
                          s.get('package_sha256') == scene['package_sha256'] and
                          _vector(s.get('position_m'), 3, 10000), 'embedded original sample differs')
    return hazard, {digest: raw}


def scene_point(world):
    """World Y-up to the existing MuJoCo presentation frame, exactly once."""
    return [world[0], -world[2], world[1]]


def fusion_view(hazard, ns, images):
    if hazard is None:
        return {'state': 'no_live_result', 'label': 'No live hazard result',
                'reason': 'The recorded B read is empty after the original result expired. This is not clearance.',
                'basis': 'Source observations are disabled at this recorded simulation step.',
                'sample_time_s': ns / 1e9, 'observations': []}
    target = scene_point(hazard['position_m'])
    cameras = {c['sensor_id']: c for c in hazard['cameras']}
    observations = []
    lidar_returns = 0
    for item in hazard['inputs']:
        sensor, kind = item['sensor_id'], item['kind']
        if kind == 'lidar':
            # The signed compact summary has no source pose. Do not borrow a
            # ground-truth pose to invent a receiver-side LiDAR marker.
            lidar_returns += item['lidar_summary']['point_count']
            continue
        sample = item['sample']
        row = {'id': sensor, 'kind': kind, 'position': scene_point(sample['position_m']), 'active': True,
               'label': sensor}
        if kind == 'camera':
            row.update(label=('Camera A' if sensor == 'camera-a' else 'Camera B') + ' · origin evidence',
                       target=target, value=100 * cameras[sensor]['red_fraction'], unit='% red',
                       evidence_path='fusion/origin/' + sensor + '.png')
            model.require(sensor in images, 'retained origin camera image required')
        elif kind == 'gas':
            row.update(label='Gas · analytic sample', value=hazard['gas_value_ppm'], unit='ppm')
        else:
            row.update(label='Audio · generated sample', value=100 * hazard['audio']['tone_fraction'], unit='% tone')
        observations.append(row)
    return {'state': 'hazard', 'label': 'Possible gas hazard',
            'reason': 'Origin-attested inference read by B through the native World replica.',
            'sample_time_s': ns / 1e9, 'position': target, 'uncertainty_m': hazard['uncertainty_m'],
            'confidence': hazard['confidence'], 'valid_until_s': (timestamp_ns(hazard['valid_until']) - model.ANCHOR) / 1e9,
            'basis': f'{lidar_returns} LiDAR returns in the original scans. Images are retained origin evidence; B did not fetch the raw inputs.',
            'observations': observations}


def _chapter(name):
    title, detail = {
        NAMES[0]: ('Five sensors. One shared inference.', 'A fuses camera pixels, LiDAR, gas and audio. B reads the exact origin-signed hazard.'),
        NAMES[1]: ('Stop observing. The result expires.', 'The recorded source step disables all sensors. B reports no live hazard result, not clearance.'),
        NAMES[2]: ('Fresh observations restore the result.', 'New captures produce a new origin-signed hazard on B, with their original times and deadlines.'),
    }[name]
    return {'series': 'NATIVE MULTIMODAL FUSION', 'title': title, 'detail': detail}


def build_timeline(pair, scene, generation, steps, duration=40):
    model.validate_pair(pair)
    scene = model.validate_scene_identity(scene['run_id'], scene)
    model.require(type(generation) is int and generation > 0 and 24 <= duration <= 60 and
                  len(steps) == 3 and tuple(s['name'] for s in steps) == NAMES, 'three completed recorded steps required')
    frames, originals, assets, geometry = [], {}, {}, None
    previous, first_until, first_originals = -1, 0, set()
    for index, step in enumerate(steps):
        ns, presentation, control = step['simulation_time_ns'], step['presentation'], step['control']
        model.require(type(ns) is int and previous < ns <= 120_000_000_000 and
                      presentation.get('simulation_time_ns') == ns, 'presentation sample time differs')
        sensors = list(model.SENSORS) if index != 1 else []
        request = {'simulation_time_ns': ns, 'sensors': sensors}
        model.require(control.get('request') == request and control.get('response') == {'sequence': index + 1, **request} and
                      control.get('no_mutation_retry') is True, 'successful original source step required')
        clock = control.get('receiver_clock', {})
        model.require(clock.get('domain') == scene['clock_domain'] and timestamp_ns(clock.get('now')) == model.ANCHOR + ns,
                      'recorded receiver clock differs')
        current_geometry = presentation.get('geometry', {})
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
        hazard, raw = receiver_hazard(step['reader'], step.get('origins', {}), pair, scene, ns, generation)
        model.require(bool(hazard) == bool(sensors), 'required recorded hazard observation missing')
        if index == 0:
            first_until, first_originals = timestamp_ns(hazard['valid_until']), set(raw)
        elif index == 1:
            model.require(model.ANCHOR + ns >= first_until, 'absence step precedes original deadline')
        else:
            model.require(first_originals.isdisjoint(raw), 'restoration reused the original result')
        images = step.get('images', {})
        model.require(set(images) == (set(model.CAMERAS) if hazard else set()), 'origin image set differs')
        if hazard:
            refs = {i['sensor_id']: i['evidence'] for i in hazard['inputs'] if i['kind'] == 'camera'}
            for sensor, image in images.items():
                ref = refs[sensor]
                model.require(image.startswith(b'\x89PNG\r\n\x1a\n') and len(image) == ref['size'] and
                              model.digest(image) == ref['sha256'], 'origin image differs from signed reference')
                assets['assets/' + ref['sha256'] + '.png'] = image
        view = fusion_view(hazard, ns, images)
        snapshot = {'run_id': scene['run_id'], 'time_s': ns / 1e9, 'phase': 'fusion', 'authorized': False,
                    'stopped': False, 'robots': [], 'jobs': [], 'completed': 0, 'total': 0, 'events': [],
                    'voxels': [], 'voxel_size': .2, 'map_node': 'receiver-b', 'fusion': view,
                    'network': {'edges': [], 'internet': {'id': 'internet', 'a': 'receiver-b', 'b': 'internet',
                        'enabled': False, 'up': False, 'packets': 0}, 'gossip_rounds': 0, 'peer_voxels': {}}}
        state = {'schema': 'wendy.macos-demo.state.v1', 'run_id': scene['run_id'], 'sequence': index + 1,
                 'worker_status': 'running', 'last_command_sequence': index + 1, 'snapshot': snapshot,
                 'dynamic_geoms': deepcopy(presentation['dynamic_geoms'])}
        mapping = {'fusion/origin/' + sensor + '.png': 'assets/' + model.digest(image) + '.png'
                   for sensor, image in images.items()}
        frame = {'at_s': index * duration / 3, 'state': state, 'assets': mapping,
                 'actions': [{'kind': 'camera', 'target': 'Floor plan' if index == 1 else 'Overview'},
                             {'kind': 'layer', 'target': 'Hazards', 'enabled': True},
                             {'kind': 'layer', 'target': 'Observations', 'enabled': True}],
                 'chapter': _chapter(step['name']), 'source_step': {'name': step['name'],
                    'reader_tick': step['reader']['tick'], 'original_envelope_sha256': sorted(raw)}}
        if index:
            frame['pointer'] = {'x': 1685, 'y': 250, 'label': 'Recorded source step: ' + ('disabled' if index == 1 else 'restored')}
        frames.append(frame)
        if index == 0:
            for at, enabled in ((duration / 8, False), (duration / 8 + 1.5, True)):
                toggle = deepcopy(frame)
                toggle.update(at_s=at, actions=[{'kind': 'layer', 'target': 'Hazards', 'enabled': enabled}])
                toggle['chapter'] = {'series': 'NATIVE MULTIMODAL FUSION', 'title': 'Separate the twin from the inference.',
                                     'detail': 'The Hazards layer comes only from B’s recorded result. Source images are origin evidence.'}
                frames.append(toggle)
        originals.update(raw)
        previous = ns
    return geometry, sorted(frames, key=lambda f: f['at_s']), originals, assets


def assemble(run_dir, out, duration=40):
    run_dir, out = Path(run_dir).resolve(), Path(out).absolute()
    model.require(not out.exists(), 'fresh playback output required')
    inventory = {}

    def read_bytes(name):
        model.require(isinstance(name, str), 'run-relative receipt required')
        relative = Path(name)
        model.require(not relative.is_absolute() and relative.parts and
                      all(p not in ('', '.', '..') for p in name.split('/')), 'run-relative receipt required')
        path = run_dir / relative
        model.require(not path.is_symlink() and path.is_file() and 0 < path.stat().st_size <= MAX_FILE,
                      'bounded public receipt required')
        raw = path.read_bytes()
        model.require(0 < len(raw) <= MAX_FILE, 'bounded public receipt required')
        inventory[name] = {'bytes': len(raw), 'sha256': model.digest(raw)}
        return raw

    def read(name):
        return model.strict_json(read_bytes(name), MAX_FILE)

    result = read('result.json')
    model.require(result.get('passed') is True and result.get('gate_completed') is True, 'successful recorded run required')
    pair, scene, task = read('pair-public.json'), read('scene-identity.json'), read('task-b-reader.json')
    model.require(all(task.get(k) == v for k, v in {'node': 'b', 'app': model.APP, 'service': 'reader',
                  'device': pair['nodes'][1]['public']['principal']}.items()), 'recorded reader task differs')
    manifest = read('playback-steps.json')
    model.require(manifest.get('version') == 1 and isinstance(manifest.get('steps'), list), 'step manifest required')
    steps = []
    for row in manifest['steps']:
        step = {k: row[k] for k in ('name', 'simulation_time_ns')}
        step.update({k: read(row[k]) for k in ('presentation', 'control', 'reader')})
        step['origins'] = read(row['origin']) if row.get('origin') else {}
        hazards = step['reader']['hazards']
        model.require(row.get('hazards') == len(hazards) and row.get('original_envelope_sha256') ==
                      [h['envelope_sha256'] for h in hazards], 'manifest original set differs')
        sensors = step['control']['request']['sensors']
        model.require(set(row.get('captures', {})) == set(sensors) and
                      set(row.get('media', {})) == (set(model.CAMERAS) | {'audio-1'} if sensors else set()),
                      'retained source evidence set differs')
        step['images'] = {}
        for sensor, name in row['captures'].items():
            capture = read(name)
            original = capture.get('capture', {})
            sample = model.strict_json(_decoded(original.get('sample_json'), 64 << 10), 64 << 10)
            model.require(original.get('scene_boot_id') == scene['boot_id'] and
                          sample.get('sensor_id') == sensor and sample.get('run_id') == scene['run_id'] and
                          sample.get('frame') == scene['frame'] and sample.get('sample_ns') == step['simulation_time_ns'],
                          'retained source sample differs')
            media = base64.b64decode(capture.get('media', ''), validate=True)
            ref = sample.get('evidence')
            if ref:
                raw = read_bytes(row['media'][sensor])
                model.require(raw == media and len(raw) == ref['size'] and model.digest(raw) == ref['sha256'],
                              'retained source media differs')
                if sensor in model.CAMERAS:
                    step['images'][sensor] = raw
            else:
                model.require(not media, 'inline sample has unexpected media')
        steps.append(step)
    geometry, frames, originals, assets = build_timeline(pair, scene, task['generation'], steps, duration)
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
              'recorded_steps': len(steps), 'presentation_frames': len(frames), 'inputs': inventory,
              'origin_attested_inference': True, 'receiver_raw_inputs_independently_verified': False,
              'new_crypto_verification': False, 'sensor_interpolation': False, 'sample_deadlines_rebased': False,
              'origin_camera_images': len(assets), 'reader_camera_fetches': 0, 'live_commands': 0,
              'scope': 'Native UI replay of actual stepped MuJoCo simulation and native replica output',
              'originals': {digest: {'bytes': len(raw), 'sha256': digest} for digest, raw in originals.items()},
              'assets': {name: {'bytes': len(raw), 'sha256': model.digest(raw)} for name, raw in assets.items()}}
    for name in ('geometry.json', 'frames.jsonl'):
        raw = (timeline / name).read_bytes()
        report[name] = {'bytes': len(raw), 'sha256': model.digest(raw)}
    (out / 'playback.json').write_bytes(model.json_bytes(report))
    (out / 'README.md').write_text(
        '# Recorded native hazard playback\n\n'
        'This private timeline holds three actual stepped MuJoCo states for presentation. '
        'A fused the raw observations and published the hazard. B read the exact origin-signed result. '
        'Only that recorded result supplies hazard geometry, readings and original deadlines. '
        'The adapter checks receipt correspondence and does not perform new signature verification.\n\n'
        'Camera images are separately retained origin evidence, matched to the signed image references. '
        'B did not fetch or independently interpret raw camera, audio or LiDAR inputs. The compact LiDAR '
        'summary supplies a count, not a source pose. The office and robot geometry is reference presentation.\n\n'
        'Video time holds sample time; nothing interpolates sensor values or extends a result deadline. '
        'Hazards layer toggles and camera presets are presentation actions. Source-control annotations '
        'refer to recorded successful HTTP steps, not live UI commands. An empty result means no live hazard result, not clearance.\n\n'
        'Render after reviewing actual receipts with `python3 -m integration.nativeapp.render '
        '--timeline /absolute/output/timeline --out /absolute/fresh-promo --duration 40 --fps 12 --poster-at 0`.\n')
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
