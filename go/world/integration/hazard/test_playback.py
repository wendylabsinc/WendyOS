"""Receipt-correspondence fixtures, not cryptographic or runtime evidence."""
import base64
from copy import deepcopy
from datetime import datetime, timezone
import importlib.util
import json
from pathlib import Path
import tempfile
import struct
import zlib
import unittest

from integration.hazard import model, playback
from integration.hazard.test_model import identity_fixture, pair_fixture

def pixel_png():
    # A generated one-pixel image keeps the fixture inspectable without an
    # opaque encoded asset. It is not a recorded simulator image.
    def chunk(kind, data):
        return struct.pack('>I', len(data)) + kind + data + struct.pack('>I', zlib.crc32(kind + data))
    return (b'\x89PNG\r\n\x1a\n' + chunk(b'IHDR', struct.pack('>IIBBBBB', 1, 1, 8, 2, 0, 0, 0)) +
            chunk(b'IDAT', zlib.compress(bytes([0, 192, 32, 32]))) + chunk(b'IEND', b''))


PNG = pixel_png()


def instant(ns):
    seconds, nanos = divmod(model.ANCHOR + ns, 1_000_000_000)
    return datetime.fromtimestamp(seconds, timezone.utc).strftime('%Y-%m-%dT%H:%M:%S') + '.%09dZ' % nanos


def encoded(raw):
    return base64.b64encode(raw).decode()


def publication(pair, scene, ns, sequence):
    inputs, captures, media = [], {}, {}
    for sensor in sorted(model.SENSORS):
        kind = sensor.split('-')[0]
        sample = {'id': f'{scene["run_id"]}/{sensor}/{sequence}', 'sensor_id': sensor, 'kind': kind,
                  'run_id': scene['run_id'], 'frame': scene['frame'], 'sample_ns': ns,
                  'site_id': scene['site'], 'package_sha256': scene['package_sha256'],
                  'position_m': [1, 2, 3]}
        content = PNG if kind == 'camera' else b'RIFF-test-wave' if kind == 'audio' else b''
        if content:
            sample['evidence'] = {'sha256': model.digest(content), 'size': len(content),
                                  'content_type': 'image/png' if kind == 'camera' else 'audio/wav'}
            media[sensor] = content
        origin = {'scene_boot_id': scene['boot_id'], 'simulation_anchor': instant(0), 'proof_sha256': 'c' * 64}
        item = {'sensor_id': sensor, 'kind': kind, 'sample_id': sample['id'], 'sample_time': instant(ns),
                'valid_until': instant(ns + 2_000_000_000), 'capture': origin,
                'source': {'device': pair['nodes'][0]['public']['principal'], 'app': model.APP,
                           'service': sensor, 'stream': sensor}, 'version': {'epoch': 1, 'sequence': sequence}}
        if kind == 'lidar':
            item['lidar_summary'] = {'point_count': 144, 'max_range_m': 6.1, 'normalized_sample_sha256': 'd' * 64}
        else:
            item['sample'] = sample
            if content:
                item['evidence'] = sample['evidence']
        inputs.append(item)
        captures[sensor] = {'capture': {'version': 1, 'scene_boot_id': scene['boot_id'],
            'simulation_anchor_unix_ns': model.ANCHOR, 'captured_unix_ns': 1794484800000000000,
            'sample_json': encoded(model.json_bytes(sample))}, 'media': encoded(content)}
    hazard = {'label': 'possible-gas-hazard', 'confidence': .8, 'uncertainty_m': .4,
              'position_m': [4, .8, -2], 'valid_until': instant(ns + 2_000_000_000),
              'physical_valid_until': '2026-11-12T12:00:05Z', 'inputs': inputs,
              'gas_value_ppm': 80, 'audio': {'tone_fraction': .9},
              'cameras': [{'sensor_id': c, 'red_fraction': .1} for c in model.CAMERAS]}
    generic = {'version': 1, 'result_schema': 'wendy.hazard.v1', 'result_json': encoded(model.json_bytes(hazard))}
    record = {'epoch': 2, 'sequence': sequence,
              'scope': {'tenant': pair['tenant'], 'site': scene['site'],
                        'device': pair['nodes'][0]['public']['principal'], 'app': model.APP,
                        'service': 'fusion', 'execution_domain': 'simulation'},
              'publication': {'component_type': model.RESULT, 'stream': model.STREAM,
                  'entity': scene['run_id'] + '/possible-gas-hazard', 'frame': scene['frame'],
                  'tombstone': False, 'payload': encoded(model.json_bytes(generic)), 'ttl_ns': 2_000_000_000,
                  'timing': {'sample_time': instant(ns), 'clock_domain': scene['clock_domain'],
                             'error_known': True, 'error_bound_ns': 0, 'age_hint_ns': 0}}}
    raw_record = json.dumps(record, indent=3).encode()
    raw = b' \n' + json.dumps({'record': encoded(raw_record), 'signature': encoded(b'fixture-only')}, indent=2).encode() + b'\n'
    digest = model.digest(raw)
    row = {'envelope_json': encoded(raw), 'envelope_sha256': digest, 'record_sha256': model.digest(raw_record),
           'version': {'epoch': 2, 'sequence': sequence}, 'payload': hazard}
    origin = {digest: {'publication': {'status': 'complete', 'locally_verified': True,
        'proof_sha256': digest, 'record_sha256': row['record_sha256'], 'signed_record_json': encoded(raw)}}}
    return row, origin, captures, media


def fixture():
    pair, _ = pair_fixture()
    scene = identity_fixture()
    geometry = {'schema': 'wendy.macos-demo.geometry.v1', 'run_id': scene['run_id'],
                'model_sha256': 'b' * 64, 'geoms': []}
    steps = []
    for index, ns in enumerate((1_000_000_000, 3_100_000_000, 3_200_000_000)):
        sensors = list(model.SENSORS) if index != 1 else []
        hazards, origins, captures, media = [], {}, {}, {}
        if sensors:
            row, origins, captures, media = publication(pair, scene, ns, index + 1)
            hazards.append(row)
        reader = {'version': 1, 'tick': index + 3, 'status': 'ready', 'run_id': scene['run_id'],
                  'reader_generation': 9, 'sample_time': instant(ns), 'hazards': hazards,
                  'origin_attested_inference': True, 'raw_inputs_independently_verified': False}
        request = {'simulation_time_ns': ns, 'sensors': sensors}
        steps.append({'name': playback.NAMES[index], 'simulation_time_ns': ns, 'reader': reader,
                      'origins': origins, 'images': {s: media[s] for s in model.CAMERAS if s in media},
                      'capture_values': captures, 'media_values': media,
                      'control': {'request': request, 'response': {'sequence': index + 1, **request},
                          'receiver_clock': {'domain': scene['clock_domain'], 'now': instant(ns)}, 'no_mutation_retry': True},
                      'presentation': {'geometry': geometry, 'simulation_time_ns': ns, 'dynamic_geoms': [],
                          'sensor_sources': [{'id': s, 'enabled': s in sensors} for s in model.SENSORS],
                          'camera_sources': [{'id': s, 'enabled': s in sensors} for s in model.CAMERAS]}})
    return pair, scene, steps


def write_run(directory, pair, scene, steps):
    def write(name, value, raw=False):
        path = directory / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(value if raw else model.json_bytes(value))
    write('result.json', {'passed': True, 'gate_completed': True})
    write('pair-public.json', pair)
    write('scene-identity.json', scene)
    write('task-b-reader.json', {'node': 'b', 'app': model.APP, 'service': 'reader',
                               'device': pair['nodes'][1]['public']['principal'], 'generation': 9})
    rows = []
    for index, step in enumerate(steps, 1):
        prefix = 'steps/%04d' % index
        row = {k: step[k] for k in ('name', 'simulation_time_ns')}
        for key in ('presentation', 'control', 'reader'):
            row[key] = prefix + '-' + key + '.json'
            write(row[key], step[key])
        row['origin'] = prefix + '-origin.json' if step['origins'] else None
        if row['origin']:
            write(row['origin'], step['origins'])
        row['captures'], row['media'] = {}, {}
        for sensor, capture in step['capture_values'].items():
            row['captures'][sensor] = prefix + '-' + sensor + '.json'
            write(row['captures'][sensor], capture)
        for sensor, content in step['media_values'].items():
            row['media'][sensor] = prefix + '-' + sensor + ('.png' if sensor in model.CAMERAS else '.wav')
            write(row['media'][sensor], content, raw=True)
        row['original_envelope_sha256'] = [h['envelope_sha256'] for h in step['reader']['hazards']]
        row['hazards'] = len(step['reader']['hazards'])
        rows.append(row)
    write('playback-steps.json', {'version': 1, 'steps': rows})


class PlaybackTests(unittest.TestCase):
    def test_receiver_result_geometry_and_deadlines_survive_without_restamping(self):
        pair, scene, steps = fixture()
        before = deepcopy(steps)
        geometry, frames, originals, assets = playback.build_timeline(pair, scene, 9, steps)
        self.assertEqual(steps, before)
        self.assertEqual(geometry['model_sha256'], 'b' * 64)
        self.assertNotEqual(geometry['model_sha256'], scene['package_sha256'])
        self.assertEqual(len(frames), 5)
        main = [f for f in frames if f['actions'][0]['kind'] == 'camera']
        self.assertEqual([f['actions'][0]['target'] for f in main], ['Overview', 'Floor plan', 'Overview'])
        view = frames[0]['state']['snapshot']['fusion']
        self.assertEqual(view['position'], [4, 2, .8])
        self.assertEqual(view['valid_until_s'], 3)
        self.assertEqual(view['confidence'], .8)
        self.assertEqual(len(view['observations']), 4)
        self.assertIn('144 LiDAR returns in the original scans', view['basis'])
        self.assertNotIn('replica_node', view)
        self.assertTrue(all('source_node' not in row for row in view['observations']))
        self.assertEqual(len(assets), 1)  # Same original image in this structural fixture.
        self.assertEqual(len(originals), 2)
        self.assertTrue(all(raw.startswith(b' \n') and raw.endswith(b'\n') for raw in originals.values()))

    def test_empty_ready_read_clears_overlay_and_images_without_clearance_claim(self):
        pair, scene, steps = fixture()
        _, frames, _, _ = playback.build_timeline(pair, scene, 9, steps)
        empty = next(f for f in frames if f['source_step']['name'] == playback.NAMES[1])
        view = empty['state']['snapshot']['fusion']
        self.assertEqual(view['label'], 'No live hazard result')
        self.assertIn('not clearance', view['reason'])
        self.assertEqual(view['observations'], [])
        self.assertNotIn('position', view)
        self.assertEqual(empty['assets'], {})

    def test_waiting_mismatched_scope_and_relabelled_verification_are_not_ready(self):
        for field, value in (('status', 'waiting_for_source'), ('reader_generation', 8),
                             ('run_id', 'other'), ('raw_inputs_independently_verified', True),
                             ('sample_time', instant(1_000_000_001))):
            with self.subTest(field=field):
                pair, scene, steps = fixture()
                steps[0]['reader'][field] = value
                with self.assertRaises(model.Refused):
                    playback.build_timeline(pair, scene, 9, steps)

    def test_original_bytes_and_decoded_payload_must_match(self):
        for mutation in ('ack', 'payload', 'hash', 'version'):
            with self.subTest(mutation=mutation):
                pair, scene, steps = fixture()
                row = steps[0]['reader']['hazards'][0]
                if mutation == 'ack':
                    next(iter(steps[0]['origins'].values()))['publication']['signed_record_json'] = encoded(b'changed')
                elif mutation == 'payload':
                    row['payload']['position_m'][0] += 1
                elif mutation == 'hash':
                    row['record_sha256'] = 'e' * 64
                else:
                    row['version']['sequence'] += 1
                with self.assertRaises(model.Refused):
                    playback.build_timeline(pair, scene, 9, steps)

    def test_expiry_and_current_source_step_cannot_be_relabelled(self):
        pair, scene, steps = fixture()
        row = steps[0]['reader']
        row['sample_time'] = instant(3_000_000_000)
        with self.assertRaises(model.Refused):
            playback.receiver_hazard(row, steps[0]['origins'], pair, scene, 3_000_000_000, 9)
        pair, scene, steps = fixture()
        row = steps[0]['reader']
        row['sample_time'] = instant(1_100_000_000)
        with self.assertRaises(model.Refused):
            playback.receiver_hazard(row, steps[0]['origins'], pair, scene, 1_100_000_000, 9)

    def test_origin_images_are_bound_to_the_received_signed_reference(self):
        for replacement in (None, PNG + b'changed'):
            pair, scene, steps = fixture()
            if replacement is None:
                del steps[0]['images']['camera-a']
            else:
                steps[0]['images']['camera-a'] = replacement
            with self.assertRaises(model.Refused):
                playback.build_timeline(pair, scene, 9, steps)

    def test_only_recorded_steps_and_actual_enabled_sources_are_used(self):
        for mutation in ('control', 'presentation', 'geometry', 'restore'):
            with self.subTest(mutation=mutation):
                pair, scene, steps = fixture()
                if mutation == 'control':
                    steps[1]['control']['response']['sensors'] = list(model.SENSORS)
                elif mutation == 'presentation':
                    steps[1]['presentation']['sensor_sources'][0]['enabled'] = True
                elif mutation == 'geometry':
                    steps[2]['presentation']['geometry'] = {**steps[2]['presentation']['geometry'], 'geoms': [1]}
                else:
                    steps[2]['reader'] = deepcopy(steps[0]['reader'])
                with self.assertRaises(model.Refused):
                    playback.build_timeline(pair, scene, 9, steps)

    def test_assemble_preserves_originals_and_passes_native_timeline_loader(self):
        pair, scene, steps = fixture()
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            source, out = root / 'run', root / 'playback'
            source.mkdir()
            write_run(source, pair, scene, steps)
            report = playback.assemble(source, out)
            self.assertEqual(report['recorded_steps'], 3)
            self.assertEqual(report['live_commands'], 0)
            self.assertFalse(report['receiver_raw_inputs_independently_verified'])
            self.assertEqual(report['reader_camera_fetches'], 0)
            for digest, metadata in report['originals'].items():
                raw = (out / 'originals' / (digest + '.json')).read_bytes()
                self.assertEqual(model.digest(raw), metadata['sha256'])
            path = Path(__file__).resolve().parents[2] / 'integration/nativeapp/render.py'
            spec = importlib.util.spec_from_file_location('hazard_promo_contract', path)
            module = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(module)
            _, _, frames, assets = module.timeline_inputs(out / 'timeline')
            self.assertEqual(len(frames), 5)
            self.assertTrue(assets)
            with self.assertRaises(model.Refused):
                playback.assemble(source, out)
            (source / 'result.json').write_bytes(model.json_bytes({'passed': False, 'gate_completed': True}))
            with self.assertRaises(model.Refused):
                playback.assemble(source, root / 'failed')
            self.assertFalse((root / 'failed').exists())


if __name__ == '__main__':
    unittest.main()
