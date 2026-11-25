import base64
from concurrent.futures import ThreadPoolExecutor
import hashlib
import http.client
import json
import os
from pathlib import Path
import threading
import unittest

from integration.hazard.scene import SENSORS, Source, encoded, server


class Renderer:
    package_sha256 = 'a' * 64
    frame = {'id': 'office-floor', 'revision': 'test'}

    def __init__(self):
        self.threads = set()
        self.acquired = 0
        self.started = self.release = None

    def advance(self, ns):
        self.threads.add(threading.get_ident())
        if self.started is not None:
            self.started.set()
            self.release.wait(2)

    def capture(self, sensor, ns, sequence):
        self.threads.add(threading.get_ident())
        self.acquired += 1
        return {'sensor_id': sensor, 'sample_ns': ns, 'sequence': sequence}, b'', 1_790_000_000_000_000_000 + self.acquired

    def presentation(self):
        self.threads.add(threading.get_ident())
        return {'geometry': {'geoms': []}, 'dynamic_geoms': []}

    def close(self):
        self.threads.add(threading.get_ident())


def sample(raw):
    return json.loads(base64.b64decode(json.loads(raw)['capture']['sample_json']))


class SourceTests(unittest.TestCase):
    def setUp(self):
        self.renderer = Renderer()
        self.source = Source('hazard-test', 1_767_225_600_000_000_000, self.renderer)
        self.addCleanup(self.source.close)

    def step(self, ns, sensors):
        return self.source.step({'simulation_time_ns': ns, 'sensors': sensors})

    def test_held_originals_have_independent_sensor_sequences_and_capture_times(self):
        self.assertTrue(all(self.source.capture(s) is None for s in SENSORS))
        self.step(1_000_000_000, list(SENSORS))
        originals = {s: self.source.capture(s) for s in SENSORS}
        times = {json.loads(raw)['capture']['captured_unix_ns'] for raw in originals.values()}
        self.assertEqual(len(times), 5)
        for s, raw in originals.items():
            self.assertEqual(sample(raw), {'sensor_id': s, 'sample_ns': 1_000_000_000, 'sequence': 1})
            self.assertEqual(self.source.capture(s), raw)
        self.step(1_100_000_000, ['gas-1'])
        self.assertIsNone(self.source.capture('camera-a'))
        self.assertEqual(sample(self.source.capture('gas-1'))['sequence'], 2)
        self.step(1_200_000_000, list(SENSORS))
        self.assertEqual(sample(self.source.capture('camera-a'))['sequence'], 2)
        self.assertEqual(sample(self.source.capture('gas-1'))['sequence'], 3)
        self.assertEqual(len(self.renderer.threads), 1)

    def test_silence_and_presentation_do_not_generate_observations(self):
        self.step(1_000_000_000, list(SENSORS))
        self.step(4_000_000_000, [])
        self.assertTrue(all(self.source.capture(s) is None for s in SENSORS))
        view = self.source.presentation()
        self.assertEqual(view['simulation_time_ns'], 4_000_000_000)
        self.assertTrue(all(not s['enabled'] for s in view['sensor_sources']))
        self.assertEqual(self.renderer.acquired, 5)
        self.assertNotIn('hazard', view)

    def test_clock_is_responsive_and_step_is_atomic_during_acquisition(self):
        self.renderer.started, self.renderer.release = threading.Event(), threading.Event()
        thread = threading.Thread(target=self.step, args=(1_000_000_000, ['gas-1']))
        thread.start()
        self.assertTrue(self.renderer.started.wait(1))
        first = self.source.clock('a' * 64)
        second = self.source.clock('b' * 64)
        self.assertEqual(first['simulation_time_ns'], 0)
        self.assertEqual(second['simulation_time_ns'], 0)
        self.assertGreater(second['observation_sequence'], first['observation_sequence'])
        self.assertIsNone(self.source.capture('gas-1'))
        self.renderer.release.set()
        thread.join(2)
        self.assertFalse(thread.is_alive())
        self.assertEqual(self.source.clock('c' * 64)['simulation_time_ns'], 1_000_000_000)

    def test_invalid_control_does_not_advance_source(self):
        self.step(1_000_000_000, ['gas-1'])
        before = self.source.capture('gas-1')
        for value in [
            {'simulation_time_ns': 1_000_000_000, 'sensors': []},
            {'simulation_time_ns': True, 'sensors': []},
            {'simulation_time_ns': 2_000_000_000, 'sensors': ['gas-1', 'gas-1']},
            {'simulation_time_ns': 2_000_000_000, 'sensors': ['lidar-iris']},
            {'simulation_time_ns': 2_000_000_000, 'sensors': ['people-front']},
            {'simulation_time_ns': 2_000_000_000, 'sensors': [], 'hazard': True},
        ]:
            with self.subTest(value=value), self.assertRaises(ValueError):
                self.source.step(value)
        self.assertEqual(self.source.capture('gas-1'), before)
        other = Source('early-audio', 1_767_225_600_000_000_000, Renderer())
        try:
            with self.assertRaises(ValueError):
                other.step({'simulation_time_ns': 100_000_000, 'sensors': ['audio-1']})
        finally:
            other.close()

    def test_default_keeps_complete_previous_group_until_last_capture_finishes(self):
        self.step(1_000_000_000, list(SENSORS))
        originals = {s: self.source.capture(s) for s in SENSORS}
        started, release = threading.Event(), threading.Event()
        capture = self.renderer.capture
        def blocked(sensor, ns, sequence):
            if sensor == 'audio-1':
                started.set()
                if not release.wait(3):
                    raise RuntimeError('test capture barrier timed out')
            return capture(sensor, ns, sequence)
        self.renderer.capture = blocked
        with ThreadPoolExecutor(max_workers=1) as calls:
            step = calls.submit(self.step, 2_000_000_000, list(SENSORS))
            try:
                self.assertTrue(started.wait(2))
                self.assertFalse(step.done())
                self.assertEqual(self.source.clock('e'*64)['simulation_time_ns'], 1_000_000_000)
                self.assertEqual({s: self.source.capture(s) for s in SENSORS}, originals)
            finally:
                release.set()
            self.assertEqual(step.result(2)['sequence'], 2)
        self.assertEqual(self.source.clock('f'*64)['simulation_time_ns'], 2_000_000_000)
        self.assertTrue(all(sample(self.source.capture(s))['sample_ns'] == 2_000_000_000 for s in SENSORS))

    def test_scene_factory_constructs_and_closes_on_existing_render_worker(self):
        created = []
        def factory(run_id, resolution):
            created.append((run_id, resolution, threading.get_ident(), Renderer()))
            return created[-1][-1]
        source = Source('factory-test', 1_767_225_600_000_000_000,
                        capture_resolution_ns=1000, scene_factory=factory)
        try:
            self.assertEqual(source.sensor_kinds, SENSORS)
            self.assertEqual(created[0][:2], ('factory-test', 1000))
            source.step({'simulation_time_ns': 1_000_000_000, 'sensors': ['gas-1']})
            self.assertEqual(created[0][3].threads, {created[0][2]})
            self.assertNotEqual(created[0][2], threading.get_ident())
        finally:
            source.close()
        self.assertEqual(created[0][3].threads, {created[0][2]})

    def test_http_controls_and_capture_availability(self):
        read, control = 'r' * 32, 'c' * 32
        listener = server(self.source, ('127.0.0.1', 0), read, control)
        thread = threading.Thread(target=listener.serve_forever)
        thread.start()
        try:
            def call(method, path, token, body=None, extra=None):
                conn = http.client.HTTPConnection(*listener.server_address, timeout=2)
                conn.request(method, path, body=None if body is None else encoded(body),
                             headers={'Authorization': 'Bearer '+token, **(extra or {})})
                response = conn.getresponse()
                result = response.status, response.read()
                conn.close()
                return result
            self.assertEqual(call('GET', '/v1/captures/gas-1', read)[0], 204)
            step = {'simulation_time_ns': 1_000_000_000, 'sensors': ['gas-1']}
            self.assertEqual(call('POST', '/v1/step', read, step)[0], 401)
            self.assertEqual(call('POST', '/v1/step', control, step)[0], 200)
            status, raw = call('GET', '/v1/captures/gas-1', read)
            self.assertEqual(status, 200)
            self.assertEqual(set(json.loads(raw)), {'capture', 'media'})
            self.assertEqual(call('GET', '/v1/captures/gas-1', control)[0], 401)
            self.assertEqual(call('GET', '/v1/clock', read, extra={'X-Wendy-Clock-Nonce': 'd'*64})[0], 200)
            self.assertEqual(call('GET', '/v1/captures/unknown', read)[0], 404)
        finally:
            listener.shutdown()
            thread.join(2)
            listener.server_close()


@unittest.skipUnless(os.environ.get('WORLD_HAZARD_RENDER_TESTS'), 'set WORLD_HAZARD_RENDER_TESTS for actual MuJoCo images/rays')
class RenderTests(unittest.TestCase):
    def test_actual_office_captures(self):
        source = Source('hazard-render-test', 1_767_225_600_000_000_000)
        try:
            source.step({'simulation_time_ns': 1_000_000_000, 'sensors': list(SENSORS)})
            directory = os.environ.get('WORLD_HAZARD_CAPTURE_DIR')
            if directory:
                Path(directory).mkdir(parents=True, exist_ok=True)
            for sensor, kind in SENSORS.items():
                raw = source.capture(sensor)
                reply = json.loads(raw)
                observation = sample(raw)
                self.assertEqual(observation['kind'], kind)
                self.assertEqual(observation['sample_ns'], 1_000_000_000)
                self.assertEqual(observation['frame'], source.identity['frame'])
                self.assertNotIn('hazard', observation)
                self.assertNotIn('source_local_m', observation)
                media = base64.b64decode(reply['media'])
                if kind in ('gas', 'lidar'):
                    self.assertEqual(media, b'')
                    self.assertNotIn('evidence', observation)
                else:
                    self.assertEqual(hashlib.sha256(media).hexdigest(), observation['evidence']['sha256'])
                    self.assertTrue(media.startswith(b'RIFF' if kind == 'audio' else b'\x89PNG'))
                if kind == 'lidar':
                    self.assertGreater(len(observation['lidar']['points_m']), 0)
                    self.assertLessEqual(len(observation['lidar']['points_m']), 144)
                if directory:
                    (Path(directory)/(sensor+'.json')).write_bytes(raw)
            self.assertGreater(len(source.presentation()['geometry']['geoms']), 900)
        finally:
            source.close()


if __name__ == '__main__':
    unittest.main()
