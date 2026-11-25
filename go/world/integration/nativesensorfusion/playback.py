"""Replay native SensorLink camera ingestion and exact remote hazard receipts.

Receipt correspondence only. The displayed camera-b JPEG is the original
native wire image, not the source PNG or a re-encoded presentation asset.
"""
import argparse
import base64
from copy import deepcopy
from pathlib import Path

from integration.nativesensorfusion import libraries, model, proof
from integration.people.playback import timestamp_ns, _decoded

MAX_FILE = 8 << 20
NAMES = ('native-camera-hazard', 'original-expiry', 'native-camera-restoration')
# A separate module object keeps the existing hazard playback and its model
# unchanged, including its PNG-only legacy behavior.
_hazard = libraries.load('native_camera_hazard_playback', libraries.HERE.parent / 'hazard/playback.py')
_hazard.model = model
receiver_hazard = _hazard.receiver_hazard


def fusion_view(hazard, ns, images):
    value = _hazard.fusion_view(hazard, ns, images)
    if hazard:
        camera = next(row for row in value['observations'] if row['id'] == model.NATIVE_CAMERA)
        camera['label'] = 'Camera B · native SensorLink origin evidence'
        camera['evidence_path'] = 'fusion/origin/camera-b.jpg'
        value['basis'] += ' Camera B is the original native SensorLink JPEG; its source clock basis is unknown.'
    return value


def _chapter(name):
    title, detail = {
        NAMES[0]: ('A native camera feeds the shared hazard.',
                   'Camera B arrives through SensorLink and the ordinary World socket. A fuses it; B reads the signed result.'),
        NAMES[1]: ('The observations stop. The result expires.',
                   'Sources stop in this recorded simulation step. B reports no live hazard result, not clearance.'),
        NAMES[2]: ('A fresh native frame restores the result.',
                   'A new SensorLink JPEG joins fresh camera, LiDAR, gas and audio observations in the result read by B.'),
    }[name]
    return {'series': 'RECORDED SIMULATION · NATIVE SENSORLINK', 'title': title, 'detail': detail}


def image_extension(sensor):
    return '.jpg' if sensor == model.NATIVE_CAMERA else '.png'


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
                model.require(((sensor == model.NATIVE_CAMERA and image.startswith(b'\xff\xd8') and image.endswith(b'\xff\xd9') and ref['content_type'] == 'image/jpeg') or
                               (sensor != model.NATIVE_CAMERA and image.startswith(b'\x89PNG\r\n\x1a\n') and ref['content_type'] == 'image/png')) and len(image) == ref['size'] and
                              model.digest(image) == ref['sha256'], 'origin image differs from signed reference')
                assets['assets/' + ref['sha256'] + image_extension(sensor)] = image
        view = fusion_view(hazard, ns, images)
        snapshot = {'run_id': scene['run_id'], 'time_s': ns / 1e9, 'phase': 'fusion', 'authorized': False,
                    'stopped': False, 'robots': [], 'jobs': [], 'completed': 0, 'total': 0, 'events': [],
                    'voxels': [], 'voxel_size': .2, 'map_node': 'receiver-b', 'fusion': view,
                    'network': {'edges': [], 'internet': {'id': 'internet', 'a': 'receiver-b', 'b': 'internet',
                        'enabled': False, 'up': False, 'packets': 0}, 'gossip_rounds': 0, 'peer_voxels': {}}}
        state = {'schema': 'wendy.macos-demo.state.v1', 'run_id': scene['run_id'], 'sequence': index + 1,
                 'worker_status': 'running', 'last_command_sequence': index + 1, 'snapshot': snapshot,
                 'dynamic_geoms': deepcopy(presentation['dynamic_geoms'])}
        mapping = {'fusion/origin/' + sensor + image_extension(sensor): 'assets/' + model.digest(image) + image_extension(sensor)
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
                toggle['chapter'] = {'series': 'RECORDED SIMULATION · NATIVE SENSORLINK', 'title': 'Separate the twin from the inference.',
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
    generations = {'reader': task['generation']}
    for service in (model.NATIVE_CAMERA, 'fusion'):
        observed = read('task-a-' + service + '.json')
        model.require(all(observed.get(k) == v for k, v in {
            'node': 'a', 'app': model.APP, 'service': service,
            'device': pair['nodes'][0]['public']['principal']}.items()) and
            type(observed.get('generation')) is int and observed['generation'] > 0,
            'recorded native source or fusion task differs')
        generations[service] = observed['generation']
    bridge, source = read('native-bridge.json'), read('native-source-result.json')
    model.require(bridge.get('status') == 'complete' and bridge.get('mode') == 'native-bridge' and
                  len(bridge.get('receipts', [])) == 2 and len(bridge.get('native_frames', [])) == 2 and
                  source.get('version') == 1 and source.get('status') == 'complete' and
                  source.get('mode') == 'native-source' and source.get('run_id') == scene['run_id'] and
                  source.get('sensor_id') == model.NATIVE_CAMERA and source.get('frames_sent') == 2 and
                  len(source.get('frames', [])) == 2, 'complete two-frame native session required')
    manifest = read('playback-steps.json')
    model.require(manifest.get('version') == 1 and isinstance(manifest.get('steps'), list) and
                  len(manifest['steps']) == 3, 'three-step manifest required')
    steps, native_checks, native_originals = [], [], {}
    for index, row in enumerate(manifest['steps']):
        step = {k: row[k] for k in ('name', 'simulation_time_ns')}
        step.update({k: read(row[k]) for k in ('presentation', 'control', 'reader')})
        step['origins'] = read(row['origin']) if row.get('origin') else {}
        hazards = step['reader']['hazards']
        model.require(row.get('hazards') == len(hazards) and row.get('original_envelope_sha256') ==
                      [h['envelope_sha256'] for h in hazards] and
                      row.get('reader_observation_semantics') == 'live_at_original_tick_not_at_host_collection',
                      'manifest original observation differs')
        sensors = step['control']['request']['sensors']
        model.require(set(row.get('captures', {})) == set(sensors) and
                      set(row.get('media', {})) == (set(model.CAMERAS) | {'audio-1'} if sensors else set()),
                      'retained source evidence set differs')
        step['images'], captures = {}, {}
        for sensor, name in row['captures'].items():
            capture = read(name)
            captures[sensor] = capture
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
        if index != 1:
            metadata, jpeg = read(row['native_metadata']), read_bytes(row['native_jpeg'])
            frame_index = 0 if index == 0 else 1
            model.require(source['frames'][frame_index]['metadata'] == metadata and
                          metadata.get('frame_sequence') == frame_index + 1,
                          'retained native source aggregate differs')
            expected = {'run_id': scene['run_id'], 'scene_boot_id': scene['boot_id'],
                'package_sha256': scene['package_sha256'], 'anchor_unix_ns': model.ANCHOR,
                'frame': scene['frame'], 'site': scene['site'], 'sample_ns': step['simulation_time_ns'],
                'source_id': model.native_source_id(pair['nodes'][0]['public']['principal']),
                'transport': 'tcp', 'channel_id': model.CHANNEL, 'tenant': pair['tenant'],
                'device': pair['nodes'][0]['public']['principal'], 'app': model.APP,
                'camera_service': model.NATIVE_CAMERA, 'camera_generation': generations[model.NATIVE_CAMERA],
                'fusion_generation': generations['fusion'], 'reader_generation': generations['reader']}
            checked = proof.check_native_correspondence(expected=expected,
                source_capture=captures[model.NATIVE_CAMERA], source_metadata=metadata,
                native_jpeg=jpeg, bridge=bridge, origins=step['origins'], reader=step['reader'])
            model.require(read(row['native_correspondence']) == checked, 'saved native correspondence differs')
            native_checks.append(checked)
            # The PNG was checked above and by proof.py as original lineage.
            # The signed normalized camera input references these exact JPEG bytes.
            step['images'][model.NATIVE_CAMERA] = jpeg
            camera_ack = next(ack for ack in bridge['receipts']
                              if ack['proof_sha256'] == checked['camera_envelope_sha256'])
            native_originals[checked['camera_envelope_sha256']] = _decoded(camera_ack['signed_record_json'], 384 << 10)
        else:
            model.require(not any(row.get(k) for k in ('native_metadata', 'native_jpeg', 'native_correspondence')),
                          'absence step cannot contain a selected native frame')
        steps.append(step)
    geometry, frames, originals, assets = build_timeline(pair, scene, task['generation'], steps, duration)
    originals.update(native_originals)
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
              'native_correspondence': native_checks, 'native_camera_images_reencoded': False,
              'native_timestamp_basis': 'unknown', 'physical_capture_time_established': False,
              'gateway_receipt_is_source_capture_time': False,
              'origin_camera_images': len(assets), 'reader_camera_fetches': 0, 'live_commands': 0,
              'scope': 'Recorded MuJoCo native SensorLink camera ingestion, ordinary app fusion and remote result reads',
              'originals': {digest: {'bytes': len(raw), 'sha256': digest} for digest, raw in originals.items()},
              'assets': {name: {'bytes': len(raw), 'sha256': model.digest(raw)} for name, raw in assets.items()}}
    for name in ('geometry.json', 'frames.jsonl'):
        raw = (timeline / name).read_bytes()
        report[name] = {'bytes': len(raw), 'sha256': model.digest(raw)}
    (out / 'playback.json').write_bytes(model.json_bytes(report))
    (out / 'README.md').write_text(
        '# Recorded native SensorLink fusion playback\n\n'
        'This private timeline holds three actual stepped MuJoCo states for presentation. '
        'A fused the raw observations and published the hazard. B read the exact origin-signed result. '
        'Only that recorded result supplies hazard geometry, readings and original deadlines. '
        'The adapter checks receipt correspondence and does not perform new signature verification.\n\n'
        'Camera B displays the exact JPEG from the native SensorLink stream and ordinary World frame RPC. '
        'Its original scene PNG remains separately hash-bound through the capture, native metadata and signed '
        'camera proof. The adapter does not convert images or label JPEG bytes as PNG. The two native '
        'correspondence checks also bind the camera acknowledgment, A fusion input and B result bytes.\n\n'
        'Native sequence and timestamp values are preserved. Their source-clock basis remains unknown. '
        'Gateway receipt time is separate and does not replace the original MuJoCo capture time or calibration. '
        'This simulation does not establish physical sensor timing or hardware operation.\n\n'
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
