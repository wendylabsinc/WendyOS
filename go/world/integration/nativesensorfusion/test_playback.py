"""Unsigned receipt fixtures; no deployment, cryptographic or image-decoder claim."""
import base64
from copy import deepcopy
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

from integration.nativesensorfusion import libraries, model, playback, proof
from integration.hazard.test_playback import encoded, instant
from integration.distributedfusion.test_playback import signed_bytes


def decode(value):
    return json.loads(base64.b64decode(value))


def fixture():
    # Isolated existing fixture generators preserve their original module state.
    hazard = libraries.load('native_playback_hazard_fixture', libraries.HERE.parent / 'hazard/test_playback.py')
    hazard.model = model
    pair, scene, steps = hazard.fixture()
    native_rows, bridge = [], None
    for index, step in enumerate(steps):
        step['name'] = playback.NAMES[index]
        if index == 1:
            continue
        ns, seq = step['simulation_time_ns'], 1 if index == 0 else 2
        distributed = libraries.load('native_playback_capture_fixture', libraries.HERE.parent / 'distributedfusion/test_playback.py')
        distributed.model = model
        structural = libraries.load('native_playback_proof_fixture', libraries.HERE / 'test_proof.py')
        captured = {}

        def publication(pair, scene, ignored_ns, ignored_seq, device):
            value = distributed.publication(pair, scene, ns, seq, device)
            captured['captures'], captured['media'] = value[4:]
            return value

        structural.publication = publication
        values = structural.fixture()
        values['expected']['sample_ns'] = ns
        values['reader'].update(version=1, tick=index + 3, sample_time=instant(ns))
        # Only the byte ancestry is exercised here. Real JPEG decoding belongs
        # to the native client/source tests, not this receipt fixture.
        jpeg = b'\xff\xd8unsigned-structural-jpeg-' + str(seq).encode() + b'\xff\xd9'
        jpeg_ref = {'sha256': model.digest(jpeg), 'size': len(jpeg), 'content_type': 'image/jpeg'}
        values['native_jpeg'] = jpeg
        metadata = values['source_metadata']
        metadata.update(frame_sequence=seq, jpeg_sha256=jpeg_ref['sha256'], jpeg_size=len(jpeg))
        bridge_row = values['bridge']['native_frames'][0]
        bridge_row['metadata'].update(frame_sequence=seq, jpeg_sha256=jpeg_ref['sha256'], jpeg_size=len(jpeg))
        published = next(iter(values['origins'].values()))['publication']
        camera_proof = next(row for row in published['inputs'] if row['reference']['key']['source']['stream'] == 'camera-b')
        camera = decode(decode(camera_proof['envelope_json'])['record'])
        bound = decode(camera['publication']['payload'])
        bound['sensorlink'].update(frame_sequence=seq, evidence=jpeg_ref)
        camera['publication']['payload'] = encoded(model.json_bytes(bound))
        envelope, record = signed_bytes(camera)
        ref = camera_proof['reference']
        ref.update(envelope_sha256=model.digest(envelope), record_sha256=model.digest(record))
        camera_proof['envelope_json'] = encoded(envelope)
        row = values['reader']['hazards'][0]
        item = next(i for i in row['payload']['inputs'] if i['sensor_id'] == 'camera-b')
        item['sample']['evidence'] = jpeg_ref
        item['evidence'] = jpeg_ref
        item['capture']['proof_sha256'] = ref['envelope_sha256']
        ack = values['bridge']['receipts'][0]
        ack.update(signed_record_json=encoded(envelope), record_sha256=ref['record_sha256'],
                   proof_sha256=ref['envelope_sha256'], evidence_sha256=jpeg_ref['sha256'],
                   request_json=encoded(model.json_bytes({'payload_json': camera['publication']['payload']})))
        bridge_row['proof_sha256'] = ref['envelope_sha256']
        result = decode(decode(row['envelope_json'])['record'])
        generic = decode(result['publication']['payload'])
        generic.update(inputs=[p['reference'] for p in published['inputs']], result_json=encoded(model.json_bytes(row['payload'])))
        result['publication']['payload'] = encoded(model.json_bytes(generic))
        raw, inner = signed_bytes(result)
        row.update(envelope_json=encoded(raw), envelope_sha256=model.digest(raw), record_sha256=model.digest(inner))
        published.update(signed_record_json=encoded(raw), proof_sha256=model.digest(raw), record_sha256=model.digest(inner))
        values['origins'] = {model.digest(raw): {'publication': published}}
        checked = proof.check_native_correspondence(**values)
        if bridge is None:
            bridge = deepcopy(values['bridge'])
        else:
            bridge['receipts'] += values['bridge']['receipts']
            bridge['native_frames'] += values['bridge']['native_frames']
        captured['captures']['camera-b'] = values['source_capture']
        step.update(reader=values['reader'], origins=values['origins'],
                    capture_values=captured['captures'], media_values=captured['media'],
                    images={'camera-a': captured['media']['camera-a'], 'camera-b': jpeg})
        native_rows.append({'metadata': metadata, 'jpeg': jpeg, 'checked': checked})
    return pair, scene, steps, bridge, native_rows


def write_run(directory):
    pair, scene, steps, bridge, native = fixture()
    helper = libraries.load('native_playback_writer_fixture', libraries.HERE.parent / 'hazard/test_playback.py')
    helper.model = model
    helper.write_run(directory, pair, scene, steps)

    def put(name, value, raw=False):
        path = directory / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(value if raw else model.json_bytes(value))

    for service, generation in (('camera-b', 3), ('fusion', 4)):
        put('task-a-' + service + '.json', {'node': 'a', 'app': model.APP, 'service': service,
            'device': pair['nodes'][0]['public']['principal'], 'generation': generation})
    put('native-bridge.json', bridge)
    put('native-source-result.json', {'version': 1, 'status': 'complete', 'mode': 'native-source',
        'run_id': scene['run_id'], 'sensor_id': 'camera-b', 'frames_sent': 2,
        'frames': [{'metadata': row['metadata']} for row in native]})
    manifest = json.loads((directory / 'playback-steps.json').read_bytes())
    for index, row in enumerate(manifest['steps']):
        row['reader_observation_semantics'] = 'live_at_original_tick_not_at_host_collection'
        if index == 1:
            continue
        frame = native[0 if index == 0 else 1]
        number = 1 if index == 0 else 2
        row.update(native_metadata=f'native-frames/frame-{number:06d}.json',
                   native_jpeg=f'native-frames/frame-{number:06d}.jpg',
                   native_correspondence=f'steps/{index+1:04d}-native-correspondence.json')
        put(row['native_metadata'], frame['metadata'])
        put(row['native_jpeg'], frame['jpeg'], raw=True)
        put(row['native_correspondence'], frame['checked'])
    put('playback-steps.json', manifest)
    return pair, scene, steps, native


class PlaybackTests(unittest.TestCase):
    def test_exact_jpeg_and_png_assets_original_deadlines_and_labels(self):
        pair, scene, steps, _, _ = fixture()
        before = deepcopy(steps)
        _, frames, originals, assets = playback.build_timeline(pair, scene, 9, steps)
        self.assertEqual(steps, before)
        self.assertEqual(len(originals), 2)
        self.assertEqual(len(frames), 5)
        view = frames[0]['state']['snapshot']['fusion']
        camera = next(row for row in view['observations'] if row['id'] == 'camera-b')
        self.assertEqual(camera['evidence_path'], 'fusion/origin/camera-b.jpg')
        self.assertIn('native SensorLink origin evidence', camera['label'])
        self.assertIn('source clock basis is unknown', view['basis'])
        self.assertEqual(view['valid_until_s'], 3)
        self.assertEqual(view['position'], [4, 2, .8])
        for sensor, image in steps[0]['images'].items():
            path = frames[0]['assets']['fusion/origin/' + sensor + playback.image_extension(sensor)]
            self.assertEqual(assets[path], image)
        empty = next(frame for frame in frames if frame['source_step']['name'] == playback.NAMES[1])
        self.assertEqual(empty['assets'], {})
        self.assertEqual(empty['state']['snapshot']['fusion']['observations'], [])
        self.assertIn('not clearance', empty['state']['snapshot']['fusion']['reason'])
        self.assertEqual(empty['actions'][0]['target'], 'Floor plan')

    def test_complete_assembly_rechecks_native_chain_and_loads_native_timeline(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            run, out = root / 'run', root / 'playback'
            run.mkdir()
            _, _, _, native = write_run(run)
            report = playback.assemble(run, out)
            self.assertEqual(report['recorded_steps'], 3)
            self.assertEqual(report['native_correspondence'], [row['checked'] for row in native])
            self.assertFalse(report['native_camera_images_reencoded'])
            self.assertFalse(report['physical_capture_time_established'])
            self.assertFalse(report['gateway_receipt_is_source_capture_time'])
            self.assertEqual(len(report['originals']), 4)
            for row in native:
                self.assertEqual((out / 'timeline/assets' / (model.digest(row['jpeg']) + '.jpg')).read_bytes(), row['jpeg'])
            render_path = libraries.ROOT / 'integration/nativeapp/render.py'
            spec = importlib.util.spec_from_file_location('native_sensor_promo_loader', render_path)
            render = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(render)
            _, _, frames, assets = render.timeline_inputs(out / 'timeline')
            self.assertEqual(len(frames), 5)
            self.assertTrue(any(name.endswith('.jpg') for name in assets))

    def test_changed_native_or_source_ancestry_refuses_before_output(self):
        for name in ('jpeg', 'png', 'metadata', 'correspondence', 'source', 'bridge', 'task', 'result'):
            with self.subTest(name=name), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                run, out = root / 'run', root / 'out'
                run.mkdir()
                write_run(run)
                if name in ('jpeg', 'png'):
                    path = run / ('native-frames/frame-000001.jpg' if name == 'jpeg' else 'steps/0001-camera-b.png')
                    path.write_bytes(path.read_bytes() + b'changed')
                else:
                    path, mutate = {
                        'metadata': ('native-frames/frame-000001.json', lambda v: v.update(timestamp_us=v['timestamp_us'] + 1)),
                        'correspondence': ('steps/0001-native-correspondence.json', lambda v: v.update(physical_capture_time_established=True)),
                        'source': ('native-source-result.json', lambda v: v.update(status='failed')),
                        'bridge': ('native-bridge.json', lambda v: v.update(status='unknown_outcome')),
                        'task': ('task-a-camera-b.json', lambda v: v.update(generation=4)),
                        'result': ('result.json', lambda v: v.update(passed=False)),
                    }[name]
                    target = run / path
                    value = json.loads(target.read_bytes()); mutate(value)
                    target.write_bytes(model.json_bytes(value))
                with self.assertRaises(model.Refused):
                    playback.assemble(run, out)
                self.assertFalse(out.exists())

    def test_png_cannot_replace_native_jpeg_or_expired_result_be_restored(self):
        for change in ('image', 'expiry', 'restoration'):
            with self.subTest(change=change):
                pair, scene, steps, _, _ = fixture()
                if change == 'image':
                    steps[0]['images']['camera-b'] = steps[0]['media_values']['camera-b']
                elif change == 'expiry':
                    steps[1]['reader']['hazards'] = steps[0]['reader']['hazards']
                    steps[1]['origins'] = steps[0]['origins']
                else:
                    steps[2]['reader'] = steps[0]['reader']
                with self.assertRaises(model.Refused):
                    playback.build_timeline(pair, scene, 9, steps)

    def test_import_does_not_change_legacy_png_playback(self):
        from integration.hazard import playback as legacy
        from integration.hazard.test_playback import fixture as old_fixture
        pair, scene, steps = old_fixture()
        _, frames, _, assets = legacy.build_timeline(pair, scene, 9, steps)
        self.assertEqual(legacy.model.APP, 'com.wendy.world.hazard')
        self.assertTrue(all(name.endswith('.png') for name in assets))
        self.assertIn('fusion/origin/camera-b.png', frames[0]['assets'])


if __name__ == '__main__':
    unittest.main()
