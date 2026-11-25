import base64
from concurrent.futures import ThreadPoolExecutor
import hashlib
import http.client
import json
import os
from pathlib import Path
import struct
import threading
from types import SimpleNamespace
import unittest
from unittest.mock import patch
import zlib

from integration.hazard.scene import SENSORS as HAZARD_SENSORS
from integration.sharedperception.scene import CAMERAS, SCENARIO, SENSORS, Source, encoded, server

ANCHOR = 1_767_225_600_000_000_000


def observation(raw):
    reply = json.loads(raw)
    return reply, json.loads(base64.b64decode(reply['capture']['sample_json']))


def png(width, height, color):
    def chunk(name, data):
        return struct.pack('>I', len(data)) + name + data + struct.pack('>I', zlib.crc32(name + data) & 0xffffffff)
    pixels = (b'\0' + bytes([color, 0, 0]) * width) * height
    return (b'\x89PNG\r\n\x1a\n' + chunk(b'IHDR', struct.pack('>IIBBBBB', width, height, 8, 2, 0, 0, 0)) +
            chunk(b'IDAT', zlib.compress(pixels)) + chunk(b'IEND', b''))


class Vector(list):
    def __add__(self, other):
        return Vector(a+b for a, b in zip(self, other))

    def __rmul__(self, scalar):
        return Vector(scalar*a for a in self)


class SharedFixture:
    """Distinctive camera/ray outputs exercise the real observation encoder."""
    package_sha256 = 'b' * 64
    model_sha256 = 'c' * 64
    frame = {'id': 'shared-office', 'revision': 'fixture'}
    config = {'model': {'sha256': model_sha256}}
    bindings = {'atlas': {'body': 1}}
    xml = '<fixture/>'

    def __init__(self, run_id):
        self.run_id = run_id
        self.model = object()
        self.data = SimpleNamespace(time=0., xpos=[[0., 0., 0.], [-5., .5, .2]])
        self.np = SimpleNamespace(array=lambda values, **_: Vector(values), zeros=lambda *_args, **_kwargs: [0], int32=int)
        self.mj = SimpleNamespace(mj_ray=self.ray)
        self.threads = {threading.get_ident()}
        self.renders, self.calibrations, self.rays = [], [], []
        self.images = {}
        self.closed = False

    def advance(self, ns):
        self.threads.add(threading.get_ident())
        self.data.time = ns / 1e9

    def calibration(self, name):
        self.threads.add(threading.get_ident())
        self.calibrations.append((name, self.data.time))
        width, height = (640, 480) if name.startswith('people-') else (320, 240)
        position = [1., 2., 3.]
        return position, {'width': width, 'height': height, 'fx': 200., 'fy': 200.,
            'cx': width/2, 'cy': height/2, 'camera_to_site': [1.,0.,0.,1.,0.,1.,0.,2.,0.,0.,1.,3.,0.,0.,0.,1.],
            'calibration_valid_for_ns': 60_000_000_000}

    def render(self, name):
        self.threads.add(threading.get_ident())
        self.renders.append((name, self.data.time))
        width, height = (640, 480) if name.startswith('people-') else (320, 240)
        raw = png(width, height, len(self.renders) % 256)
        self.images[(name, self.data.time)] = raw
        return raw

    def ray(self, model, data, origin, direction, *_):
        self.threads.add(threading.get_ident())
        self.rays.append((model, data, data.time, list(origin), list(direction)))
        return 1.

    def presentation(self):
        self.threads.add(threading.get_ident())
        return {'geometry': {'model_sha256': self.model_sha256, 'geoms': [{'id': 1, 'name': 'person-1-reference'}]},
                'dynamic_geoms': [{'id': 1, 'position': [self.data.time, 0., 0.]}]}

    def close(self):
        self.threads.add(threading.get_ident())
        self.closed = True


class SharedSourceTests(unittest.TestCase):
    def setUp(self):
        self.created = []
        def factory(run_id):
            value = SharedFixture(run_id)
            self.created.append(value)
            return value
        self.patch = patch('integration.sharedperception.scene._scene', factory)
        self.patch.start()
        self.source = Source('shared-source-test', ANCHOR)
        self.scene = self.created[0]
        self.addCleanup(self.patch.stop)
        self.addCleanup(self.source.close)

    def step(self, ns, sensors=None):
        return self.source.step({'simulation_time_ns': ns, 'sensors': list(SENSORS) if sensors is None else sensors})

    def test_one_scene_clock_frame_and_original_calibrated_camera_bytes(self):
        self.step(1_000_000_000)
        self.assertEqual(len(self.created), 1)
        self.assertIs(self.source.scene.model, self.scene.model)
        self.assertIs(self.source.scene.data, self.scene.data)
        self.assertEqual(self.source.clock('a'*64)['simulation_time_ns'], 1_000_000_000)
        self.assertEqual(self.source.scenario_version, SCENARIO)
        for sensor, kind in SENSORS.items():
            raw = self.source.capture(sensor)
            reply, sample = observation(raw)
            self.assertEqual((sample['kind'], sample['sample_ns'], sample['sequence']), (kind, 1_000_000_000, 1))
            self.assertEqual(sample['frame'], self.source.identity['frame'])
            self.assertEqual(sample['package_sha256'], self.source.identity['package_sha256'])
            self.assertEqual((sample['run_id'], sample['scenario_version'], sample['seed']), ('shared-source-test', SCENARIO, 7))
            self.assertEqual(reply['capture']['scene_boot_id'], self.source.identity['boot_id'])
            self.assertEqual(reply['capture']['simulation_anchor_unix_ns'], ANCHOR)
            self.assertNotIn(b'person-1', raw)
            self.assertNotIn('hazard', sample)
            self.assertNotIn('tracks', sample)
            media = base64.b64decode(reply['media'])
            if kind == 'camera':
                name = CAMERAS[sensor]
                self.assertEqual(media, self.scene.images[(name, 1.)])
                self.assertEqual(struct.unpack('>II', media[16:24]), (sample['camera']['width'], sample['camera']['height']))
                self.assertEqual(sample['position_m'], [1., 2., 3.])
                self.assertEqual(sample['camera']['camera_to_site'][3:12:4], sample['position_m'])
            if media:
                self.assertEqual(sample['evidence'], {'sha256': hashlib.sha256(media).hexdigest(),
                    'size': len(media), 'content_type': 'audio/wav' if kind == 'audio' else 'image/png'})
            else:
                self.assertNotIn('evidence', sample)
        self.assertEqual(self.scene.calibrations, [(name, 1.) for name in CAMERAS.values()])
        self.assertEqual(self.scene.renders, [(name, 1.) for name in CAMERAS.values()])
        self.assertEqual(len(self.scene.rays), 144)
        self.assertTrue(all(m is self.scene.model and d is self.scene.data and t == 1. for m, d, t, *_ in self.scene.rays))
        self.assertEqual(len(self.scene.threads), 1)
        self.assertNotIn(threading.get_ident(), self.scene.threads)

    def test_held_originals_disable_and_per_sensor_sequences(self):
        self.step(1_000_000_000)
        originals = {s: self.source.capture(s) for s in SENSORS}
        for _ in range(3):
            self.source.clock('b'*64)
            view = self.source.presentation()
            self.assertEqual(view['simulation_time_ns'], 1_000_000_000)
            self.assertEqual({s['id'] for s in view['camera_sources']}, set(CAMERAS))
            self.assertEqual({s: self.source.capture(s) for s in SENSORS}, originals)
        self.assertEqual(len(self.scene.renders), 4)
        self.step(2_000_000_000, ['people-front'])
        self.assertIsNone(self.source.capture('camera-a'))
        self.assertIsNone(self.source.capture('people-oblique'))
        self.assertEqual(observation(self.source.capture('people-front'))[1]['sequence'], 2)
        self.step(3_000_000_000)
        self.assertEqual(observation(self.source.capture('people-front'))[1]['sequence'], 3)
        self.assertEqual(observation(self.source.capture('camera-a'))[1]['sequence'], 2)
        self.assertNotEqual(self.source.capture('people-front'), originals['people-front'])

    def test_original_microsecond_acquisition_precision(self):
        other = Source('shared-precision', ANCHOR, capture_resolution_ns=1000)
        try:
            with patch('integration.hazard.scene.time.time_ns', return_value=1_790_000_000_123_456_789):
                other.step({'simulation_time_ns': 1_000_000_000, 'sensors': ['people-front']})
            reply, sample = observation(other.capture('people-front'))
            self.assertEqual(reply['capture']['captured_unix_ns'], 1_790_000_000_123_456_000)
            self.assertEqual(sample['sample_ns'], 1_000_000_000)
        finally:
            other.close()

    def test_hazard_group_is_readable_while_people_rendering_is_blocked(self):
        self.step(1_000_000_000)
        started, release = threading.Event(), threading.Event()
        render = self.scene.render
        def blocked(name):
            if name == 'people-front':
                started.set()
                if not release.wait(3):
                    raise RuntimeError('test render barrier timed out')
            return render(name)
        self.scene.render = blocked
        # Exclude a previously enabled hazard camera to prove that the first
        # group replaces old/disabled observations, rather than merging them.
        enabled = [s for s in SENSORS if s != 'camera-b']
        physical = 1_790_000_000_123_456_789
        with patch('integration.hazard.scene.time.time_ns', return_value=physical), ThreadPoolExecutor(max_workers=2) as calls:
            step = calls.submit(self.step, 2_000_000_000, enabled)
            try:
                self.assertTrue(started.wait(2))
                self.assertFalse(step.done())
                self.assertEqual(self.source.sequence, 1)
                self.assertEqual(self.source.clock('c'*64)['simulation_time_ns'], 2_000_000_000)
                self.assertIsNone(self.source.capture('camera-b'))
                self.assertIsNone(self.source.capture('people-front'))
                self.assertIsNone(self.source.capture('people-oblique'))
                early = {s: self.source.capture(s) for s in HAZARD_SENSORS if s != 'camera-b'}
                for raw in early.values():
                    reply, sample = observation(raw)
                    self.assertEqual(sample['sample_ns'], 2_000_000_000)
                    self.assertEqual(sample['sequence'], 2)
                    self.assertEqual(reply['capture']['captured_unix_ns'], physical)
                presentation = calls.submit(self.source.presentation)
                self.assertFalse(presentation.done())
            finally:
                release.set()
            self.assertEqual(step.result(2), {'sequence': 2, 'simulation_time_ns': 2_000_000_000, 'sensors': enabled})
            self.assertEqual(presentation.result(2)['simulation_time_ns'], 2_000_000_000)
        self.assertEqual({s: self.source.capture(s) for s in early}, early)
        for s in ('people-front', 'people-oblique'):
            reply, sample = observation(self.source.capture(s))
            self.assertEqual((sample['sample_ns'], sample['sequence']), (2_000_000_000, 2))
            self.assertEqual(reply['capture']['captured_unix_ns'], physical)
        self.assertEqual(len(self.scene.threads), 1)

    def test_http_camera_alias_is_exact_cached_capture_without_render(self):
        listener = server(self.source, ('127.0.0.1', 0), 'r'*32, 'c'*32)
        thread = threading.Thread(target=listener.serve_forever)
        thread.start()
        def get(path):
            conn = http.client.HTTPConnection(*listener.server_address, timeout=2)
            conn.request('GET', path, headers={'Authorization': 'Bearer '+'r'*32})
            response = conn.getresponse(); result = response.status, response.read(); conn.close()
            return result
        try:
            self.assertEqual(get('/v1/cameras/people-front')[0], 204)
            self.step(1_000_000_000)
            for name in CAMERAS:
                self.assertEqual(get('/v1/cameras/'+name), get('/v1/captures/'+name))
                self.assertEqual(get('/v1/cameras/'+name), (200, self.source.capture(name)))
            self.assertEqual(get('/v1/cameras/gas-1')[0], 404)
            self.assertEqual(get('/v1/cameras/unknown')[0], 404)
            identity = json.loads(get('/v1/identity')[1])
            self.assertEqual(identity['scenario_version'], SCENARIO)
            self.assertEqual(identity['package_sha256'], self.scene.package_sha256)
            self.assertEqual(len(self.scene.renders), 4)
            self.step(2_000_000_000, list(HAZARD_SENSORS))
            self.assertEqual(get('/v1/cameras/people-front')[0], 204)
            self.assertEqual(get('/v1/captures/people-front')[0], 204)
        finally:
            listener.shutdown(); thread.join(2); listener.server_close()


@unittest.skipUnless(os.environ.get('WORLD_SHARED_RENDER_TESTS'), 'set WORLD_SHARED_RENDER_TESTS for actual shared MuJoCo captures')
class SharedRenderTests(unittest.TestCase):
    def test_actual_shared_office_seven_captures_and_dynamic_presentation(self):
        source = Source('shared-render-test', ANCHOR)
        try:
            source.step({'simulation_time_ns': 1_000_000_000, 'sensors': list(SENSORS)})
            before = source.presentation()
            self.assertGreater(len(before['geometry']['geoms']), 900)
            actors = [g['id'] for g in before['geometry']['geoms'] if g['name'].startswith('person-') and g.get('dynamic')]
            self.assertTrue(actors)
            self.assertTrue(set(actors) <= {g['id'] for g in before['dynamic_geoms']})
            self.assertEqual(len(before['sensor_sources']), 7)
            self.assertEqual(len(before['camera_sources']), 4)
            directory = os.environ.get('WORLD_SHARED_CAPTURE_DIR')
            if directory:
                Path(directory).mkdir(parents=True, exist_ok=True)
            originals = {}
            for sensor, kind in SENSORS.items():
                raw = source.capture(sensor); originals[sensor] = raw
                reply, sample = observation(raw); media = base64.b64decode(reply['media'])
                self.assertEqual((sample['frame'], sample['package_sha256']), (source.identity['frame'], source.identity['package_sha256']))
                self.assertEqual((sample['sample_ns'], sample['kind']), (1_000_000_000, kind))
                self.assertNotIn('hazard', sample); self.assertNotIn('tracks', sample)
                if kind == 'camera':
                    expected = (640, 480) if sensor.startswith('people-') else (320, 240)
                    self.assertEqual(struct.unpack('>II', media[16:24]), expected)
                    self.assertEqual((sample['camera']['width'], sample['camera']['height']), expected)
                    self.assertEqual(sample['camera']['camera_to_site'][3:12:4], sample['position_m'])
                if media:
                    self.assertEqual(hashlib.sha256(media).hexdigest(), sample['evidence']['sha256'])
                if kind == 'lidar':
                    self.assertGreater(len(sample['lidar']['points_m']), 0)
                    self.assertLessEqual(len(sample['lidar']['points_m']), 144)
                if directory:
                    (Path(directory)/(sensor+'.json')).write_bytes(raw)
                    if kind == 'camera': (Path(directory)/(sensor+'.png')).write_bytes(media)
            source.step({'simulation_time_ns': 2_000_000_000, 'sensors': list(SENSORS)})
            after = source.presentation()
            prior = {g['id']: g for g in before['dynamic_geoms']}
            current = {g['id']: g for g in after['dynamic_geoms']}
            self.assertTrue(any(prior[i] != current[i] for i in actors))
            for sensor in SENSORS:
                self.assertEqual(observation(source.capture(sensor))[1]['sequence'], 2)
                self.assertNotEqual(source.capture(sensor), originals[sensor])
        finally:
            source.close()


if __name__ == '__main__':
    unittest.main()
