"""Replay B gas to A fusion to B results, preserving original recorded bytes.

This is receipt correspondence, not a second cryptographic verifier. Source
images are labeled origin evidence; they were not fetched by the B reader.
"""
import argparse
import base64
from copy import deepcopy
from pathlib import Path

from integration.distributedfusion import model
from integration.hazard.playback import fusion_view as _hazard_view
from integration.people.playback import timestamp_ns, _decoded, _vector

MAX_FILE = 8 << 20
NAMES = ('multimodal-hazard', 'original-expiry', 'fresh-restoration')


def receiver_hazard(receipt, origins, pair, scene, ns, generation, gas_origin=None, correspondence=None):
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
        model.require(not origins and gas_origin is None and correspondence is None, 'empty read cannot have a selected origin')
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
        model.require(source == {'device': pair['nodes'][1 if sensor == 'gas-1' else 0]['public']['principal'], 'app': model.APP, 'service': sensor, 'stream': sensor} and
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
    gas_raw = input_correspondence(hazard, result, acknowledged, gas_origin, correspondence, pair, scene, ns, digest)
    return hazard, {digest: raw, model.digest(gas_raw): gas_raw}


def input_correspondence(hazard, result, acknowledged, gas_origin, correspondence, pair, scene, ns, hazard_digest):
    """Match recorded bytes and source identities; do not authenticate them anew."""
    refs, proofs = result.get('inputs'), acknowledged.get('inputs')
    model.require(isinstance(refs, list) and isinstance(proofs, list) and
                  len(refs) == len(proofs) == len(hazard['inputs']), 'complete original input receipts required')
    gas_raw, gas_record, gas_capture, gas_sample, gas_ref, gas_until = None, None, None, None, None, None
    for item, ref, proof in zip(hazard['inputs'], refs, proofs):
        sensor, source = item['sensor_id'], item['source']
        model.require(isinstance(ref, dict) and proof.get('reference') == ref and
                      ref.get('key') == {'tenant': pair['tenant'], 'site': scene['site'],
                          'entity': 'sensor/' + sensor, 'type': model.CAPTURE, 'source': source} and
                      ref.get('version') == item.get('version') and
                      ref.get('envelope_sha256') == item['capture'].get('proof_sha256'),
                      'typed and generic original input identities differ')
        raw = _decoded(proof.get('envelope_json'), 256 << 10)
        signed = model.strict_json(raw, 256 << 10)
        record_raw = _decoded(signed.get('record'), 64 << 10)
        model.require(model.digest(raw) == ref.get('envelope_sha256') and
                      model.digest(record_raw) == ref.get('record_sha256'), 'original input digest differs')
        record = model.strict_json(record_raw, 64 << 10)
        scope, publication = record.get('scope', {}), record.get('publication', {})
        model.require(all(scope.get(k) == v for k, v in {
            'tenant': pair['tenant'], 'site': scene['site'], 'device': source['device'],
            'app': model.APP, 'service': sensor, 'execution_domain': 'simulation'}.items()) and
            ref['version'] == {k: record.get(k) for k in ('epoch', 'sequence')} and
            all(type(record.get(k)) is int and record[k] > 0 for k in ('epoch', 'sequence')) and
            publication.get('component_type') == model.CAPTURE and publication.get('entity') == 'sensor/' + sensor and
            publication.get('stream') == sensor and publication.get('frame') == scene['frame'] and
            publication.get('tombstone') is False and
            publication.get('timing', {}).get('clock_domain') == scene['clock_domain'] and
            timestamp_ns(publication.get('timing', {}).get('sample_time')) == model.ANCHOR + ns,
            'original capture source, version or time differs')
        capture = model.strict_json(_decoded(publication.get('payload'), 32 << 10), 32 << 10)
        sample = model.strict_json(_decoded(capture.get('sample_json'), 64 << 10), 64 << 10)
        model.require(capture.get('version') == 1 and capture.get('scene_boot_id') == scene['boot_id'] and
                      capture.get('simulation_anchor_unix_ns') == model.ANCHOR and
                      type(capture.get('captured_unix_ns')) is int and capture['captured_unix_ns'] > 0 and
                      sample.get('id') == item['sample_id'] and sample.get('sensor_id') == sensor and
                      sample.get('sample_ns') == ns and sample.get('kind') == item['kind'] and
                      sample.get('run_id') == scene['run_id'] and sample.get('site_id') == scene['site'] and
                      sample.get('frame') == scene['frame'] and sample.get('package_sha256') == scene['package_sha256'],
                      'original capture metadata differs')
        if sensor == 'gas-1':
            gas_raw, gas_record, gas_capture, gas_sample, gas_ref = raw, record_raw, capture, sample, ref
            timing = publication['timing']
            ttl, error, age = publication.get('ttl_ns'), timing.get('error_bound_ns'), timing.get('age_hint_ns')
            model.require(timing.get('error_known') is True and all(type(n) is int and n >= 0 for n in (ttl, error, age)) and
                          ttl > max(error, age), 'original gas sample deadline required')
            gas_until = timestamp_ns(timing['sample_time']) + ttl - max(error, age)
    model.require(isinstance(gas_origin, dict) and gas_raw is not None and
                  _decoded(gas_origin.get('signed_record_json'), 256 << 10) == gas_raw and
                  gas_origin.get('proof_sha256') == model.digest(gas_raw) and
                  gas_origin.get('record_sha256') == model.digest(gas_record) and
                  gas_origin.get('capture_id') == gas_sample['id'] and
                  gas_origin.get('native_sequence') == gas_sample['sequence'] and
                  gas_origin.get('scene_boot_id') == gas_capture['scene_boot_id'] and
                  gas_origin.get('captured_unix_ns') == gas_capture['captured_unix_ns'] and
                  gas_origin.get('sample_ns') == ns and not gas_origin.get('evidence_sha256'),
                  'B gas acknowledgment differs from A original input')
    # Check the runner's correspondence receipt against the same reconstructed
    # byte chain. This metadata is not a separate source of trust.
    check_correspondence(correspondence, gas_ref, gas_until, hazard_digest, pair)
    return gas_raw


def check_correspondence(value, gas_ref, gas_until, hazard_digest, pair):
    a, b = (n['public']['principal'] for n in pair['nodes'])
    expected = {'version': 1, 'source_devices': {s: b if s == 'gas-1' else a for s in model.SENSORS},
                'gas_reference': gas_ref, 'gas_envelope_sha256': gas_ref['envelope_sha256'],
                'a_input_envelope_sha256': gas_ref['envelope_sha256'],
                'gas_sample_valid_until_unix_ns': gas_until,
                'hazard_envelope_sha256': hazard_digest, 'reader_envelope_sha256': hazard_digest,
                'fuser_device': a, 'reader_device': b, 'exact_bytes_matched': True,
                'independent_crypto_reverification': False}
    model.require(value == expected, 'recorded gas-to-fusion-to-reader correspondence differs')


def fusion_view(hazard, ns, images):
    view = _hazard_view(hazard, ns, images)
    if hazard is None:
        view['basis'] = 'Gateway B gas is disabled in this recorded step. Gateway A camera, audio and LiDAR sources remain enabled.'
        return view
    for row in view['observations']:
        row['label'] = {'gas-1': 'Gas · Gateway B · analytic sample',
                        'camera-a': 'Camera 1 · Gateway A · origin evidence',
                        'camera-b': 'Camera 2 · Gateway A · origin evidence',
                        'audio-1': 'Audio · Gateway A · generated sample'}[row['id']]
    view['reason'] = 'Gateway A fused its sensors with Gateway B gas. This is the exact A-signed result read by B.'
    view['basis'] += ' Fusion runs on Gateway A; Gateway B did not fetch the camera/audio inputs.'
    return view


def _chapter(name):
    title, detail = {
        NAMES[0]: ('Two Agents. One shared inference.', 'B sends its gas observation to A. A combines local camera pixels, LiDAR and audio; B reads the exact A-signed hazard.'),
        NAMES[1]: ('Stop remote gas. The result expires.', 'Only B gas stops in this recorded synthetic step. A sensors stay enabled. B reports no live hazard result, not clearance.'),
        NAMES[2]: ('Fresh remote gas restores the result.', 'A new B gas observation joins fresh A captures. B receives a new hazard with the original times and deadlines.'),
    }[name]
    return {'series': 'RECORDED SYNTHETIC FUSION · TWO AGENTS', 'title': title, 'detail': detail}


def build_timeline(pair, scene, generation, steps, duration=40, *, step_count=3):
    model.validate_pair(pair)
    scene = model.validate_scene_identity(scene['run_id'], scene)
    model.require(type(step_count) is int and step_count in (2, 3) and
                  type(generation) is int and generation > 0 and 24 <= duration <= 60 and
                  len(steps) == step_count and tuple(s['name'] for s in steps) == NAMES[:step_count],
                  'exact completed recorded steps required')
    frames, originals, assets, geometry = [], {}, {}, None
    previous, first_until, first_gas_until, first_originals = -1, 0, 0, set()
    for index, step in enumerate(steps):
        ns, presentation, control = step['simulation_time_ns'], step['presentation'], step['control']
        model.require(type(ns) is int and previous < ns <= 120_000_000_000 and
                      presentation.get('simulation_time_ns') == ns, 'presentation sample time differs')
        sensors = list(model.SENSORS if index != 1 else model.LOCAL_SENSORS)
        request = {'simulation_time_ns': ns, 'sensors': sensors}
        model.require(control.get('request') == request and control.get('response') == {'sequence': index + 1, **request} and
                      control.get('no_mutation_retry') is True, 'successful original source step required')
        model.require(control.get('source_clock_shared') is True and control.get('clock_domain') == scene['clock_domain'] and
                      control.get('anchor_unix_ns') == model.ANCHOR, 'recorded shared source clock differs')
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
        hazard, raw = receiver_hazard(step['reader'], step.get('origins', {}), pair, scene, ns, generation,
                                      step.get('gas_origin'), step.get('correspondence'))
        model.require(bool(hazard) == (index != 1), 'required recorded hazard observation missing')
        if index == 0:
            first_until, first_originals = timestamp_ns(hazard['valid_until']), set(raw)
            first_gas_until = step['correspondence']['gas_sample_valid_until_unix_ns']
        elif index == 1:
            model.require(model.ANCHOR + ns >= max(first_until, first_gas_until),
                          'absence step precedes original hazard or gas deadline')
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
        frame = {'at_s': index * duration / 3 if step_count == 3 else index * duration * 2 / 3, 'state': state, 'assets': mapping,
                 'actions': [{'kind': 'camera', 'target': 'Floor plan' if index == 1 else 'Overview'},
                             {'kind': 'layer', 'target': 'Hazards', 'enabled': True},
                             {'kind': 'layer', 'target': 'Observations', 'enabled': True}],
                 'chapter': _chapter(step['name']), 'source_step': {'name': step['name'],
                    'reader_tick': step['reader']['tick'],
                    'original_envelope_sha256': [h['envelope_sha256'] for h in step['reader']['hazards']],
                    'gas_original_envelope_sha256': step['gas_origin']['proof_sha256'] if hazard else None}}
        if index:
            frame['pointer'] = {'x': 1685, 'y': 250, 'label': 'Recorded B gas: ' + ('disabled' if index == 1 else 'restored')}
        frames.append(frame)
        if index == 0:
            for at, enabled in ((duration / 8, False), (duration / 8 + 1.5, True)):
                toggle = deepcopy(frame)
                toggle.update(at_s=at, actions=[{'kind': 'layer', 'target': 'Hazards', 'enabled': enabled}])
                toggle['chapter'] = {'series': 'RECORDED SYNTHETIC FUSION · TWO AGENTS', 'title': 'Separate the twin from the inference.',
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
        for key in ('gas_origin', 'correspondence'):
            step[key] = read(row[key]) if row.get(key) else None
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
        if not hazards:
            step['images'] = {}  # Retain source evidence in inventory, but do not invent B inference.
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
              'gas_source_node': 'b', 'fusion_node': 'a', 'reader_node': 'b',
              'gas_original_bytes_matched_to_fusion_inputs': True,
              'origin_attested_inference': True, 'receiver_raw_inputs_independently_verified': False,
              'new_crypto_verification': False, 'sensor_interpolation': False, 'sample_deadlines_rebased': False,
              'origin_camera_images': len(assets), 'reader_camera_fetches': 0, 'live_commands': 0,
              'scope': 'Native UI replay of recorded synthetic B gas, A fusion and B replica results',
              'originals': {digest: {'bytes': len(raw), 'sha256': digest} for digest, raw in originals.items()},
              'assets': {name: {'bytes': len(raw), 'sha256': model.digest(raw)} for name, raw in assets.items()}}
    for name in ('geometry.json', 'frames.jsonl'):
        raw = (timeline / name).read_bytes()
        report[name] = {'bytes': len(raw), 'sha256': model.digest(raw)}
    (out / 'playback.json').write_bytes(model.json_bytes(report))
    (out / 'README.md').write_text(
        '# Recorded distributed fusion playback\n\n'
        'This private timeline holds three actual stepped MuJoCo states for presentation. '
        'B published gas, A fused that exact original with its local camera/audio/LiDAR observations, and B read the A-signed result. '
        'Only that recorded result supplies hazard geometry, readings and original deadlines. '
        'The adapter checks receipt correspondence and does not perform new signature verification.\n\n'
        'Camera images are separately retained origin evidence, matched to the signed image references. '
        'B did not fetch or independently interpret raw camera, audio or LiDAR inputs. The compact LiDAR '
        'summary supplies a count, not a source pose. The office and robot geometry is reference presentation.\n\n'
        'Video time holds sample time; nothing interpolates sensor values or extends a result deadline. '
        'Hazards layer toggles and camera presets are presentation actions. Source-control annotations '
        'refer to recorded successful HTTP steps, not live UI commands. The middle step disables B gas while A sources remain enabled. An empty result means no live hazard result, not clearance.\n\n'
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
