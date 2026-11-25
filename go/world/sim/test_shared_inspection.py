"""Focused actual MuJoCo checks for the opt-in planar source contract."""
import base64
import hashlib
from http.client import HTTPConnection
import json
import math
import threading
import time
import unittest
import xml.etree.ElementTree as ET

from sim.actuator import Rejected
from sim.shared_perception_scene import SharedPerceptionScene, assemble_office_xml
from sim.shared_inspection import (SharedInspection, INSPECTION_SEQUENCE_BASE,
                                  PREFIX, STATE_SCHEMA)
from integration.sharedperception.scene import ContinuousSource, SENSORS, server

ANCHOR = 1767225600000000000


def warm_capture_renderers(source):
    # Initialize both actual GL context sizes before a timed capture. Cold
    # software-renderer setup may exceed the unchanged 500 ms freshness bound.
    # Warming renders no observation and does not advance the physics clock.
    def warm():
        for camera in ('inspection-camera-a', 'people-front'):
            source.render_scene.shared.render(camera)
    source.worker.submit(warm).result()


def current_inspection_samples(source):
    # /step completes the fixed-source batch. Robot-local capture is a separate
    # stream, so wait for its next accepted original rather than treating the
    # fixed-batch receipt as a freshness guarantee for that stream.
    deadline = time.monotonic() + 5
    while True:
        try:
            return json.loads(source.inspection_samples())
        except Rejected:
            if time.monotonic() >= deadline:
                raise
            time.sleep(.01)


class SharedInspectionTests(unittest.TestCase):
    def setUp(self):
        self.scene = SharedPerceptionScene('inspection-test', inspection=True)
        self.control = SharedInspection(self.scene)
        self.addCleanup(self.scene.close)
        self.addCleanup(self.control.close)

    def command(self, velocity, sequence=1, lease=300):
        return self.control.request(PREFIX + 'command', {
            'session': self.control.session, 'sequence': sequence,
            'velocity_mps': velocity, 'lease_ms': lease})

    def ticks(self, count):
        with self.control.lock:
            for _ in range(count):
                self.control._tick(time.monotonic())

    def test_opt_in_adds_only_robot_cameras_to_default_scene(self):
        old = ET.fromstring(assemble_office_xml())
        new = ET.fromstring(self.scene.xml)
        atlas = new.find("./worldbody/body[@name='robot-atlas']")
        cameras = atlas.findall('camera')
        self.assertEqual([c.get('name') for c in cameras], ['inspection-camera-a', 'inspection-camera-b'])
        for camera in cameras:
            atlas.remove(camera)
        self.assertEqual(ET.tostring(old), ET.tostring(new))
        self.assertEqual(self.scene.model.opt.timestep, .01)
        with self.assertRaises(ValueError):
            default = SharedPerceptionScene('default-scene')
            try:
                SharedInspection(default)
            finally:
                default.close()

    def test_actual_diagonal_motion_stop_and_no_command_replay(self):
        before = self.control.state()
        state = self.command([.2, 0, .2])
        self.assertEqual(state['schema_version'], STATE_SCHEMA)
        self.assertEqual(state['robot_id'], 'atlas')
        self.assertEqual(state['run_id'], 'inspection-test')
        self.ticks(35)
        moved = self.control.state()
        self.assertGreater(moved['position_m'][0] - before['position_m'][0], .04)
        self.assertGreater(moved['position_m'][2] - before['position_m'][2], .04)
        self.assertAlmostEqual(moved['position_m'][1], .24)
        stopped = self.control.request(PREFIX+'stop', {'session': self.control.session, 'sequence': 2})
        self.assertEqual(stopped['commanded_velocity_mps'], [0., 0., 0.])
        self.assertEqual(stopped['velocity_mps'], [0., 0., 0.])
        with self.assertRaises(Rejected):
            self.command([.2, 0, .2], sequence=2)
        for invalid in ([.5, 0, .5], [0, .1, 0], [math.nan, 0, 0], .2):
            with self.subTest(invalid=invalid), self.assertRaises(Rejected):
                self.command(invalid, sequence=3)
        previous_session = self.control.session
        self.control.request(PREFIX+'session', {})
        with self.assertRaises(Rejected):
            self.control.request(PREFIX+'stop', {'session': previous_session, 'sequence': 3})

    def test_continuous_actor_pose_does_not_reset_the_physics_clock(self):
        self.ticks(500)
        self.assertAlmostEqual(self.control.data.time, 5.)
        self.assertEqual(sum(w.number for w in self.control.data.warning), 0)

    def test_watchdog_is_independent_of_rendering(self):
        self.control.start()
        self.command([.2, 0, 0], lease=50)
        # No render call is made; command expiry must still stop the actuator.
        deadline = time.monotonic() + 1
        while time.monotonic() < deadline:
            state = self.control.state()
            if state['simulation_time_s'] >= .12 and state['watchdog_stopped']:
                break
            time.sleep(.01)
        self.assertGreaterEqual(state['simulation_time_s'], .12)
        self.assertTrue(state['watchdog_stopped'])
        self.assertEqual(state['velocity_mps'], [0., 0., 0.])
        self.assertEqual(state['commanded_velocity_mps'], [0., 0., 0.])

    def test_immutable_copy_produces_four_coherent_robot_local_originals(self):
        self.ticks(30)
        view = self.control.render_copy()
        self.addCleanup(view.close)
        state, captured, _ = self.control.copy_state(view)
        qpos = view.data.qpos.copy()
        self.command([.2, 0, .2])
        self.ticks(20)
        self.scene.np.testing.assert_array_equal(qpos, view.data.qpos)
        self.assertGreater(self.scene.data.time, view.data.time)
        snapshot = self.control.snapshot(view, state, INSPECTION_SEQUENCE_BASE)
        self.assertEqual(snapshot['sample_unix_ms'], captured // 1_000_000)
        self.assertEqual(snapshot['state'], state)
        self.assertEqual(snapshot['simulation_time_ns'], 300_000_000)
        self.assertEqual({s['sensor_id'] for s in snapshot['observations']}, {'camera-a', 'camera-b', 'gas-1', 'audio-1'})
        blobs = {m['sha256']: base64.b64decode(m['data']) for m in snapshot['media']}
        for sample in snapshot['observations']:
            self.assertEqual(sample['id'], f"inspection-test/{sample['sensor_id']}/{INSPECTION_SEQUENCE_BASE}")
            self.assertEqual(sample['sequence'], snapshot['sequence'])
            self.assertEqual(sample['sample_ns'], snapshot['simulation_time_ns'])
            self.assertEqual(sample['frame'], self.scene.frame)
            self.assertEqual(sample['package_sha256'], self.scene.package_sha256)
            self.assertEqual(sample['calibration_revision'], 'shared-office-atlas-cameras-v1')
            if sample['kind'] == 'camera':
                self.assertEqual((sample['camera']['width'], sample['camera']['height']), (320, 240))
                self.scene.np.testing.assert_allclose(self.scene.np.array(sample['camera']['camera_to_site']).reshape(4,4)[:3,3], sample['position_m'])
            else:
                self.assertEqual(sample['position_m'], state['position_m'])
            if 'evidence' in sample:
                ref = sample['evidence']; raw = blobs[ref['sha256']]
                self.assertEqual(hashlib.sha256(raw).hexdigest(), ref['sha256'])
                self.assertEqual(len(raw), ref['size'])
        camera_b = snapshot['observations'][1]
        self.scene.np.testing.assert_allclose(camera_b['position_m'], self.scene.np.array(state['position_m']) + [0,.45,0])
        self.assertNotIn('person-', json.dumps(snapshot))
        self.assertLess(len(json.dumps(snapshot)), 1 << 20)
        with self.assertRaises(ValueError):
            self.control.snapshot(view, state, 1)


class ContinuousSourceTests(unittest.TestCase):
    def setUp(self):
        self.source = ContinuousSource('continuous-test', ANCHOR, capture_resolution_ns=1000, defer_start=True)
        self.addCleanup(self.source.close)
        warm_capture_renderers(self.source)

    def test_seven_fixed_sources_and_separate_inspection_share_original_clock(self):
        s = self.source
        ready = s.step({'simulation_time_ns': 300_000_000, 'sensors': list(SENSORS)})
        captures = {sensor: s.capture(sensor) for sensor in SENSORS}
        snapshot = current_inspection_samples(s)
        self.assertGreaterEqual(snapshot['sequence'], INSPECTION_SEQUENCE_BASE)
        self.assertEqual(snapshot['boot_id'], s.identity['boot_id'])
        self.assertIs(s.inspection.model, s.scene.model)
        self.assertIs(s.render_scene.model, s.scene.model)
        self.assertIsNot(s.render_scene.data, s.scene.data)
        for sensor in SENSORS:
            capture = json.loads(captures[sensor])['capture']
            sample = json.loads(base64.b64decode(capture['sample_json']))
            self.assertEqual(sample['sample_ns'], ready['simulation_time_ns'])
            self.assertLess(sample['sequence'], INSPECTION_SEQUENCE_BASE)
            self.assertEqual(sample['package_sha256'], s.identity['package_sha256'])
            self.assertEqual(sample['frame'], snapshot['state']['frame'])
            self.assertEqual(capture['captured_unix_ns'] % 1000, 0)
        self.assertGreaterEqual(s.clock('a'*64)['simulation_time_ns'], ready['simulation_time_ns'])
        active = [sensor for sensor in SENSORS if sensor != 'camera-b']
        switched = s.step({'simulation_time_ns': ready['simulation_time_ns'], 'sensors': active})
        self.assertGreater(switched['simulation_time_ns'], ready['simulation_time_ns'])
        self.assertIsNone(s.capture('camera-b'))
        # Disabling the fixed camera does not relabel or suppress Atlas camera-b.
        presentation = s.presentation()
        self.assertIn('camera-b', [v['sensor_id'] for v in current_inspection_samples(s)['observations']])
        self.assertGreaterEqual(presentation['simulation_time_ns'], switched['simulation_time_ns'])
        self.assertFalse(next(v for v in presentation['sensor_sources'] if v['id']=='camera-b')['enabled'])
        with self.assertRaises(ValueError):
            s.step({'simulation_time_ns': 100_000_000_000, 'sensors': active})

    def test_prefixed_http_control_and_same_cached_samples(self):
        s = self.source
        s.step({'simulation_time_ns': 300_000_000, 'sensors': []})
        http = server(s, ('127.0.0.1', 0), 'r'*32, 'c'*32)
        thread = threading.Thread(target=http.serve_forever, daemon=True)
        thread.start()
        def close():
            http.shutdown(); http.server_close(); thread.join(2)
        self.addCleanup(close)
        def request(method, path, token, body=None):
            connection = HTTPConnection(*http.server_address, timeout=2)
            try:
                raw = None if body is None else json.dumps(body)
                connection.request(method, path, raw, {'Authorization': 'Bearer '+token, 'Content-Type': 'application/json'})
                response = connection.getresponse()
                return response.status, json.loads(response.read())
            finally: connection.close()
        status, state = request('GET', PREFIX+'state', 'r'*32)
        self.assertEqual(status, 200)
        self.assertEqual(state['schema_version'], STATE_SCHEMA)
        self.assertEqual(request('POST', PREFIX+'session', 'r'*32, {})[0], 401)
        status, session = request('POST', PREFIX+'session', 'c'*32, {})
        self.assertEqual(status, 200)
        self.assertEqual(request('POST', PREFIX+'command', 'c'*32,
            {'session':session['session'],'sequence':1,'velocity_mps':[.1,0,0],'lease_ms':300})[0], 200)
        self.assertEqual(request('POST', PREFIX+'stop', 'c'*32,
            {'session':session['session'],'sequence':2})[0], 200)
        s.step({'simulation_time_ns': 0, 'sensors': []})
        status, snapshot = request('GET', PREFIX+'samples', 'r'*32)
        self.assertEqual(status, 200)
        self.assertEqual(snapshot['state']['session'], session['session'])
        self.assertEqual(snapshot['state']['schema_version'], STATE_SCHEMA)
        self.assertEqual(request('GET', '/v1/state', 'r'*32)[0], 404)
        self.assertIsNone(s.render_error)


class DeferredContinuousTests(unittest.TestCase):
    def source(self, fixed_capture_hz=1):
        value = ContinuousSource('deferred-test', ANCHOR, defer_start=True, fixed_capture_hz=fixed_capture_hz)
        self.addCleanup(value.close)
        return value

    def test_authenticated_first_step_starts_once_after_initial_geometry(self):
        s = self.source()
        self.assertEqual(s.presentation()['simulation_time_ns'], 0)
        self.assertEqual(s.presentation()['geometry']['run_id'], 'deferred-test')
        self.assertEqual(s.clock('a' * 64)['simulation_time_ns'], 0)
        self.assertTrue(all(s.capture(sensor) is None for sensor in SENSORS))
        with self.assertRaises(Rejected):
            s.inspection_samples()
        time.sleep(.06)
        self.assertEqual(s.inspection.data.time, 0)
        self.assertIsNone(s.inspection.thread)
        self.assertIsNone(s.render_thread)
        http = server(s, ('127.0.0.1', 0), 'r' * 32, 'c' * 32)
        thread = threading.Thread(target=http.serve_forever, daemon=True)
        thread.start()
        def close():
            http.shutdown(); http.server_close(); thread.join(2)
        self.addCleanup(close)
        def request(method, path, token, body=None):
            connection = HTTPConnection(*http.server_address, timeout=6)
            try:
                connection.request(method, path, None if body is None else json.dumps(body),
                    {'Authorization': 'Bearer ' + token, 'Content-Type': 'application/json'})
                response = connection.getresponse()
                return response.status, json.loads(response.read())
            finally:
                connection.close()
        first = {'simulation_time_ns': 1_000_000_000, 'sensors': list(SENSORS)}
        self.assertEqual(request('POST', '/v1/step', 'r' * 32, first)[0], 401)
        self.assertEqual(request('POST', PREFIX + 'session', 'c' * 32, {})[0], 503)
        self.assertEqual(request('GET', PREFIX + 'state', 'r' * 32)[0], 503)
        self.assertFalse(s.physics_started)
        status, ready = request('POST', '/v1/step', 'c' * 32, first)
        self.assertEqual(status, 200)
        self.assertGreaterEqual(ready['simulation_time_ns'], first['simulation_time_ns'])
        for sensor in SENSORS:
            original = json.loads(s.capture(sensor))['capture']
            sample = json.loads(base64.b64decode(original['sample_json']))
            self.assertGreaterEqual(sample['sample_ns'], first['simulation_time_ns'])
        physics, render = s.inspection.thread, s.render_thread
        session = s.inspection.session
        status, second = request('POST', '/v1/step', 'c' * 32,
            {'simulation_time_ns': 0, 'sensors': list(SENSORS)})
        self.assertEqual(status, 200)
        self.assertGreater(second['simulation_time_ns'], ready['simulation_time_ns'])
        self.assertIs(s.inspection.thread, physics)
        self.assertIs(s.render_thread, render)
        self.assertEqual(s.inspection.session, session)
        self.assertEqual(request('GET', PREFIX + 'state', 'r' * 32)[0], 200)
        with self.assertRaises(ValueError):
            s.step({'simulation_time_ns': 120_000_000_001, 'sensors': []})

    def test_fixed_batch_keeps_originals_while_robot_samples_and_geometry_advance(self):
        s = self.source()
        warm_capture_renderers(s)
        def capture_at(ticks):
            with s.inspection.lock:
                for _ in range(ticks):
                    s.inspection._tick(time.monotonic())
            s.worker.submit(s._capture_continuous).result()
        capture_at(30)
        first = {sensor: s.capture(sensor) for sensor in SENSORS}
        original = [json.loads(raw)['capture'] for raw in first.values()]
        self.assertEqual(len({v['captured_unix_ns'] for v in original}), 1)
        self.assertEqual({json.loads(base64.b64decode(v['sample_json']))['sample_ns'] for v in original}, {300_000_000})
        first_robot = json.loads(s.inspection_latest)
        first_geometry = s.presentation()['dynamic_geoms']
        s.inspection.request(PREFIX + 'command', {'session': s.inspection.session,
            'sequence': 1, 'velocity_mps': [.2, 0, 0], 'lease_ms': 500})
        capture_at(30)
        self.assertEqual(s.presentation()['simulation_time_ns'], 600_000_000)
        self.assertNotEqual(s.presentation()['dynamic_geoms'][0]['position'], first_geometry[0]['position'])
        self.assertEqual({sensor: s.capture(sensor) for sensor in SENSORS}, first)
        robot = json.loads(s.inspection_latest)
        self.assertGreater(robot['sequence'], first_robot['sequence'])
        self.assertEqual(robot['simulation_time_ns'], 600_000_000)
        self.assertEqual({v['sample_ns'] for v in robot['observations']}, {600_000_000})
        position = robot['state']['position_m']
        self.assertEqual(s.presentation()['dynamic_geoms'][0]['position'], [position[0], -position[2], position[1]])
        capture_at(71)
        fresh = [json.loads(s.capture(sensor))['capture'] for sensor in SENSORS]
        self.assertEqual(len({v['captured_unix_ns'] for v in fresh}), 1)
        self.assertEqual({json.loads(base64.b64decode(v['sample_json']))['sample_ns'] for v in fresh}, {1_310_000_000})
        self.assertEqual(s.presentation()['simulation_time_ns'], 1_310_000_000)
        self.assertEqual(s.last_fixed_capture_ns, 1_310_000_000)
        # No physics owner was started in this deterministic capture check.
        self.assertIsNone(s.inspection.thread)

    def test_default_cadence_keeps_each_render_original(self):
        s = self.source(fixed_capture_hz=5)
        s.enabled = ('gas-1',)
        for count, expected in ((30, 300_000_000), (10, 400_000_000)):
            with s.inspection.lock:
                for _ in range(count):
                    s.inspection._tick(time.monotonic())
            s.worker.submit(s._capture_continuous).result()
            raw = json.loads(s.capture('gas-1'))['capture']
            self.assertEqual(json.loads(base64.b64decode(raw['sample_json']))['sample_ns'], expected)
        self.assertEqual(s.sequences['gas-1'], 2)


if __name__ == '__main__':
    unittest.main()
