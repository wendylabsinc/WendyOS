"""Opt-in Atlas inspection in the existing shared MuJoCo office.

One thread advances the authoritative scene. Rendering uses copied MjData and
never steps it. Commands are simulated planar velocities with an ideal brake;
this module neither authorizes missions nor talks to physical hardware.
"""
from __future__ import annotations

import base64
import copy
import hashlib
import json
import math
import random
import threading
import time
import uuid

from sim.actuator import Rejected
from sim.run import gas_value
from sim.sensors import audio_wav
from sim.shared_perception_scene import INSPECTION_CAMERAS

PREFIX = '/v1/inspection/'
STATE_SCHEMA = 'wendy.sim.actuator.planar.v1'
INSPECTION_SEQUENCE_BASE = 1 << 32
CALIBRATION = 'shared-office-atlas-cameras-v1'
MAX_SNAPSHOT_BYTES = 1 << 20
MAX_SNAPSHOT_AGE = .5
GAS_FIELD = {'source_local_m': [-7.3, .65, -.9], 'sigma_m': 1.6,
             'background_ppm': 5, 'peak_ppm': 95, 'noise_stddev_ppm': 1.5}


def encoded(value):
    return json.dumps(value, allow_nan=False, separators=(',', ':')).encode()


class SharedInspection:
    """Own physics for an explicitly inspection-enabled SharedPerceptionScene.

    The caller owns GL construction/destruction on its render worker. Call
    render_copy once there, then copy_state before each capture. Every returned
    state and all four inspection observations describe that single copy.
    """
    def __init__(self, scene, *, boot_id=None):
        if not set(INSPECTION_CAMERAS).issubset(scene.camera_ids):
            raise ValueError('inspection cameras must be compiled into the shared scene')
        self.scene, self.mj, self.model, self.data = scene, scene.mj, scene.model, scene.data
        self.body = scene.bindings['atlas']['body']
        self.actuators = [self.mj.mj_name2id(self.model, self.mj.mjtObj.mjOBJ_ACTUATOR,
                                          'robot-atlas-' + axis) for axis in ('x', 'y')]
        joints = [self.mj.mj_name2id(self.model, self.mj.mjtObj.mjOBJ_JOINT,
                                   'robot-atlas-' + axis) for axis in ('x', 'y')]
        if min(*self.actuators, *joints) < 0 or not 0 < self.model.opt.timestep <= .02:
            raise ValueError('Atlas planar actuators and bounded timestep required')
        self.dofs = [int(self.model.jnt_dofadr[j]) for j in joints]
        self.people_dofs = [int(self.model.jnt_dofadr[j]) for j in range(self.model.njnt)
                            if (self.mj.mj_id2name(self.model, self.mj.mjtObj.mjOBJ_JOINT, j) or '').startswith('person-')]
        self.lock = threading.Lock()
        self.done = threading.Event()
        self.thread = None
        self.error = None
        self.boot_id = boot_id or str(uuid.uuid4())
        self.session = str(uuid.uuid4())
        self.sequence = 0
        self.velocity = [0., 0., 0.]
        self.deadline = 0.
        self.watchdog_stopped = True
        self.sample_ms = time.time_ns() // 1_000_000

    def start(self):
        if self.thread is not None or self.done.is_set():
            raise ValueError('inspection physics starts once')
        self.thread = threading.Thread(target=self._run, name='shared-inspection-physics', daemon=True)
        self.thread.start()

    def close(self):
        self.done.set()
        if self.thread is not None:
            self.thread.join(timeout=3)
            if self.thread.is_alive():
                raise RuntimeError('inspection physics did not stop')
        with self.lock:
            self._brake()

    def _brake(self):
        self.velocity = [0., 0., 0.]
        self.deadline = 0.
        self.watchdog_stopped = True
        self.data.ctrl[self.actuators] = 0
        self.data.qvel[self.dofs] = 0
        self.mj.mj_forward(self.model, self.data)

    def _obstacle_clear(self):
        # Actual short-range geometry rays around the .24m collision sphere.
        # Visual mannequin geometry may also block a ray. This is a synthetic
        # local sensor, not a safety proof or a person-tracking output.
        np = self.scene.np
        origin = self.data.xpos[self.body].copy()
        mask = np.array([1, 1, 0, 0, 0, 0], dtype=np.uint8)
        for i in range(16):
            angle = i * 2 * math.pi / 16
            direction = np.array([math.cos(angle), math.sin(angle), 0.])
            geom = np.zeros(1, dtype=np.int32)
            distance = self.mj.mj_ray(self.model, self.data, origin, direction,
                                      mask, True, self.body, geom)
            if 0 <= distance < .29:
                return False
        return True

    def _tick(self, mono):
        # Called only by the physics owner, under lock. No catch-up motion.
        if mono >= self.deadline or not self._obstacle_clear():
            self._brake()
        self.data.ctrl[self.actuators] = [self.velocity[0], -self.velocity[2]]
        self.mj.mj_step(self.model, self.data)
        # Mannequin articulation is a kinematic image fixture. Do not carry
        # integrated joint momentum into the next prescribed pose.
        self.data.qvel[self.people_dofs] = 0
        self.data.qacc_warmstart[self.people_dofs] = 0
        self.scene._pose_people(float(self.data.time))
        if not all(math.isfinite(float(v)) for v in (*self.data.qpos, *self.data.qvel)):
            raise ValueError('nonfinite shared physics')
        self.sample_ms = time.time_ns() // 1_000_000

    def _run(self):
        try:
            while not self.done.is_set():
                started = time.monotonic()
                with self.lock:
                    if self.data.time + self.model.opt.timestep > 120. + 1e-9:
                        self._brake()
                        self.done.set()
                        break
                    self._tick(started)
                self.done.wait(max(0, float(self.model.opt.timestep) - (time.monotonic() - started)))
        except Exception as exc:
            self.error = str(exc)
            self.done.set()
            with self.lock:
                self._brake()

    def _state(self):
        x, y, z = map(float, self.data.xpos[self.body])
        position = [x, z, -y]
        return {'schema_version': STATE_SCHEMA, 'synthetic': True, 'robot_id': 'atlas',
                'run_id': self.scene.run_id, 'boot_id': self.boot_id, 'session': self.session, 'sequence': self.sequence,
                'site_id': 'studio-01', 'frame': dict(self.scene.frame),
                'position_m': position,
                'velocity_mps': [float(self.data.qvel[self.dofs[0]]), 0., -float(self.data.qvel[self.dofs[1]])],
                'sample_unix_ms': self.sample_ms, 'simulation_time_s': float(self.data.time),
                'sensors': {'gas': True, 'local-obstacles': True},
                'obstacle_clear': self._obstacle_clear(),
                'gas_ppm': gas_value(GAS_FIELD, position, random.Random(0)), 'energy': 1.,
                'commanded_velocity_mps': list(self.velocity),
                'lease_remaining_ms': max(0, int((self.deadline - time.monotonic()) * 1000)),
                'watchdog_stopped': self.watchdog_stopped,
                'provenance': {'mode': 'simulated', 'package_sha256': self.scene.package_sha256,
                               'robot_model': 'shared-office-planar-puck-ideal-brake',
                               'gas_model': 'analytic-gaussian-fixture'}}

    def state(self):
        with self.lock:
            if self.done.is_set():
                raise Rejected('inspection physics unavailable', 503)
            return self._state()

    def request(self, path, body):
        fields = {PREFIX + 'session': set(), PREFIX + 'stop': {'session', 'sequence'},
                  PREFIX + 'command': {'session', 'sequence', 'velocity_mps', 'lease_ms'}}
        if path not in fields:
            raise Rejected('unknown inspection endpoint', 404)
        if not isinstance(body, dict) or set(body) != fields[path]:
            raise Rejected('exact inspection command fields required')
        with self.lock:
            if self.done.is_set():
                raise Rejected('inspection physics unavailable', 503)
            if path == PREFIX + 'session':
                self._brake()
                self.session, self.sequence = str(uuid.uuid4()), 0
            else:
                if body['session'] != self.session:
                    raise Rejected('stale writer session', 409)
                sequence = body['sequence']
                if type(sequence) is not int or not self.sequence < sequence <= 2**53-1:
                    raise Rejected('sequence must strictly increase', 409)
                if path == PREFIX + 'stop':
                    self._brake()
                else:
                    velocity, lease = body['velocity_mps'], body['lease_ms']
                    if (not isinstance(velocity, list) or len(velocity) != 3
                            or any(type(v) not in (int, float) or not math.isfinite(v) for v in velocity)
                            or velocity[1] != 0 or math.hypot(velocity[0], velocity[2]) > .5):
                        raise Rejected('finite planar velocity of at most .5m/s required')
                    if type(lease) is not int or not 1 <= lease <= 500:
                        raise Rejected('lease_ms must be 1..500')
                    if not self._obstacle_clear():
                        raise Rejected('local obstacle blocks motion', 409)
                    self.velocity = [float(v) for v in velocity]
                    self.deadline = time.monotonic() + lease / 1000
                    self.watchdog_stopped = False
                self.sequence = sequence
            return self._state()

    def render_copy(self):
        """Create an independent render view, sharing only immutable model/config."""
        view = copy.copy(self.scene)
        view.data = self.mj.MjData(self.model)
        view.renderers, view.geometry = {}, None
        return view

    def copy_state(self, view):
        if view.model is not self.model or view.data is self.data:
            raise ValueError('independent render data for this model required')
        with self.lock:
            if self.done.is_set():
                raise Rejected('inspection physics unavailable', 503)
            for key in ('qpos', 'qvel', 'act', 'ctrl', 'mocap_pos', 'mocap_quat'):
                getattr(view.data, key)[:] = getattr(self.data, key)
            view.data.time = self.data.time
            state = self._state()
            captured_ns, captured_mono = time.time_ns(), time.monotonic()
            state['sample_unix_ms'] = captured_ns // 1_000_000
        self.mj.mj_forward(self.model, view.data)
        return state, captured_ns, captured_mono

    def snapshot(self, view, state, sequence):
        """Build four new originals from a previously copied robot-local state."""
        ns = round(state['simulation_time_s'] * 1e9)
        if (type(sequence) is not int or not INSPECTION_SEQUENCE_BASE <= sequence <= 2**53-1 or ns < 250_000_000
                or round(view.data.time * 1e9) != ns or state['boot_id'] != self.boot_id):
            raise ValueError('coherent inspection state, time and sequence required')
        observations, media = [], []
        for sensor in ('camera-a', 'camera-b', 'gas-1', 'audio-1'):
            kind = 'camera' if sensor.startswith('camera') else 'gas' if sensor == 'gas-1' else 'audio'
            sample = {'schema_version': 'wendy.sensor.sample.v1',
                      'id': f'{view.run_id}/{sensor}/{sequence}', 'site_id': 'studio-01',
                      'frame': dict(view.frame), 'sensor_id': sensor, 'kind': kind, 'sequence': sequence,
                      'sample_ns': ns, 'run_id': view.run_id, 'scenario_version': 'shared-office-inspection-v1',
                      'seed': 7, 'package_sha256': view.package_sha256, 'uncertainty_m': .02,
                      'calibration_revision': CALIBRATION}
            blob = None
            if kind == 'camera':
                camera = 'inspection-' + sensor
                position, calibration = view.calibration(camera)
                sample.update(position_m=position, camera=calibration, sensor_model='mujoco-4.0.0-rgb-pinhole')
                blob, mime = view.render(camera), 'image/png'
            elif kind == 'gas':
                sample.update(position_m=list(state['position_m']), sensor_model='analytic-gaussian-fixture-v1',
                              gas={'value_ppm': gas_value(GAS_FIELD, state['position_m'], random.Random(7 + sequence)),
                                   'uncertainty_ppm': 1.5, 'species': 'synthetic-test-gas'})
            else:
                sample.update(position_m=list(state['position_m']), sensor_model='generated-880hz-tone-v1',
                              audio={'sample_rate_hz': 8000, 'channels': 1, 'duration_ns': 250_000_000})
                blob, mime = audio_wav(7, sequence, True), 'audio/wav'
            if blob is not None:
                digest = hashlib.sha256(blob).hexdigest()
                sample['evidence'] = {'sha256': digest, 'size': len(blob), 'content_type': mime}
                media.append({'sha256': digest, 'data': base64.b64encode(blob).decode()})
            observations.append(sample)
        result = {'schema_version': 'wendy.sim.live.v1', 'run_id': view.run_id, 'boot_id': self.boot_id,
                  'sequence': sequence, 'sample_unix_ms': state['sample_unix_ms'],
                  'simulation_time_ns': ns, 'state': copy.deepcopy(state),
                  'observations': observations, 'media': media}
        if len(encoded(result)) > MAX_SNAPSHOT_BYTES:
            raise ValueError('inspection snapshot exceeds 1MiB')
        return result


# Reuse the existing seven-source capture/clock encoding; only this opt-in
# owner supplies continuous physics and render copies.
from integration.hazard.scene import Source as HazardSource, SENSORS as HAZARD_SENSORS

class ContinuousSource(HazardSource):
    """Opt-in source sharing one continuously stepped model with Atlas control.

    The inherited worker owns all render contexts. Its copy never steps physics.
    /v1/step selects enabled fixed sources and waits for their next capture at or
    beyond the requested minimum; it does not set this mode's running clock.
    """
    def __init__(self, run_id, anchor, renderer=None, capture_resolution_ns=1, *, scene_factory, defer_start=False, fixed_capture_hz=5):
        import threading
        if renderer is not None or type(defer_start) is not bool:
            raise ValueError('continuous inspection uses its shared model renderer')
        if type(fixed_capture_hz) is not int or fixed_capture_hz not in (1, 5):
            raise ValueError('fixed capture cadence must be 1 or 5 Hz')
        HazardSource.__init__(self, run_id, anchor, capture_resolution_ns=capture_resolution_ns,
                              scene_factory=scene_factory)
        self.inspection = SharedInspection(self.scene.shared, boot_id=self.identity['boot_id'])
        self.render_scene = self.worker.submit(self._render_copy).result()
        self.enabled = tuple(self.sensor_kinds)
        self.selection = 0
        self.captured_selection = -1
        self.inspection_sequence = INSPECTION_SEQUENCE_BASE - 1
        self.inspection_latest = None
        self.inspection_captured = 0.
        self.latest_presentation = None
        self.render_error = None
        self.done = threading.Event()
        self.changed = threading.Condition(self.lock)
        self.physics_started = False
        self.render_thread = None
        # Default mode captures on every existing 5 Hz render iteration, even
        # when physics scheduling advanced by slightly less than 200 ms.
        self.fixed_capture_interval_ns = 1_000_000_000 if fixed_capture_hz == 1 else 0
        self.last_fixed_capture_ns = -self.fixed_capture_interval_ns
        if defer_start:
            # Commissioning needs the actual compiled geometry, not simulated
            # elapsed time or a fabricated sensor capture. The first authorized
            # /step starts this same data/clock once, after ordinary app setup.
            self.latest_presentation = self.worker.submit(self._initial_presentation).result()
        else:
            with self.changed:
                self._start_locked()

    def _initial_presentation(self):
        self.inspection.copy_state(self.render_scene.shared)
        return {**self.render_scene.presentation(), 'simulation_time_ns': 0,
            'sensor_sources': [{'id': s, 'kind': kind, 'enabled': s in self.enabled} for s, kind in self.sensor_kinds.items()],
            'camera_sources': [{'id': s, 'enabled': s in self.enabled} for s, kind in self.sensor_kinds.items() if kind == 'camera']}

    def _start_locked(self):
        if self.physics_started or self.done.is_set():
            raise ValueError('continuous source starts once')
        self.inspection.start()
        self.physics_started = True
        self.render_thread = threading.Thread(target=self._render_loop, name='shared-inspection-render', daemon=True)
        self.render_thread.start()

    def capture_batches(self, sensors):
        hazard = [s for s in sensors if s in HAZARD_SENSORS]
        people = [s for s in sensors if s not in HAZARD_SENSORS]
        return (hazard, people) if people else (hazard,)

    def _render_copy(self):
        import copy
        view = copy.copy(self.scene)
        view.shared = self.inspection.render_copy()
        view.data = view.shared.data
        return view

    def _render_loop(self):
        import time
        try:
            while not self.done.is_set() and not self.inspection.done.is_set():
                started = time.monotonic()
                self.worker.submit(self._capture_continuous).result()
                self.done.wait(max(0, .2 - (time.monotonic() - started)))
        except Exception as exc:
            with self.changed:
                self.render_error = str(exc)
                self.inspection_latest = None
                self.changed.notify_all()

    def _capture_continuous(self):
        import base64
        import time
        state, captured_ns, captured_mono = self.inspection.copy_state(self.render_scene.shared)
        ns = round(state['simulation_time_s'] * 1e9)
        with self.lock:
            if ns < 250_000_000 or ns <= self.simulation_ns:
                return
            enabled, selection, sequences = self.enabled, self.selection, dict(self.sequences)
            fixed_due = selection != self.captured_selection or ns - self.last_fixed_capture_ns >= self.fixed_capture_interval_ns
        snapshot = self.inspection.snapshot(self.render_scene.shared, state, self.inspection_sequence + 1)
        # Expose the small robot-local capture without waiting for four fixed
        # camera renders. This is a different source namespace, never aliases.
        with self.lock:
            if time.monotonic() - captured_mono <= MAX_SNAPSHOT_AGE:
                self.inspection_latest = encoded(snapshot)
                self.inspection_captured = captured_mono
                self.inspection_sequence = snapshot['sequence']
        if fixed_due:
            captures = {}
            captured_ns = captured_ns // self.scene.capture_resolution_ns * self.scene.capture_resolution_ns
            # Every fixed original in this batch comes from the same immutable
            # copied data and original timestamp. Faster presentation/robot-local
            # snapshots do not restamp or replace this cache.
            for batch in self.capture_batches(enabled):
                for sensor in batch:
                    sequences[sensor] += 1
                    sample, media, _ = self.render_scene.capture(sensor, ns, sequences[sensor])
                    capture = dict(version=1, scene_boot_id=self.identity['boot_id'], captured_unix_ns=captured_ns,
                        simulation_anchor_unix_ns=self.identity['anchor_unix_ns'],
                        sample_json=base64.b64encode(encoded(sample)).decode())
                    captures[sensor] = encoded({'capture': capture, 'media': base64.b64encode(media).decode()})
                with self.lock:
                    if selection == self.selection:
                        self.sequences, self.captures = dict(sequences), dict(captures)
        presentation = {**self.render_scene.presentation(), 'simulation_time_ns': ns,
            'sensor_sources': [{'id': s, 'kind': kind, 'enabled': s in enabled} for s, kind in self.sensor_kinds.items()],
            'camera_sources': [{'id': s, 'enabled': s in enabled} for s, kind in self.sensor_kinds.items() if kind == 'camera']}
        with self.changed:
            if selection == self.selection:
                self.simulation_ns = ns
                self.latest_presentation = presentation
                self.captured_selection = selection
                if fixed_due:
                    self.last_fixed_capture_ns = ns
                self.sequence += 1
                self.changed.notify_all()

    def clock(self, nonce):
        import time
        if not isinstance(nonce, str) or len(nonce) != 64 or any(c not in '0123456789abcdef' for c in nonce):
            raise ValueError('a 32-byte hexadecimal clock nonce is required')
        state = self.inspection.state()
        with self.lock:
            self.clock_sequence += 1
            return {**self.identity, 'simulation_time_ns': round(state['simulation_time_s'] * 1e9),
                    'observed_unix_ns': time.time_ns(), 'observation_sequence': self.clock_sequence, 'nonce': nonce}

    def step(self, value):
        import time
        if not isinstance(value, dict) or set(value) != {'simulation_time_ns', 'sensors'}:
            raise ValueError('exact step fields required')
        ns, sensors = value['simulation_time_ns'], value['sensors']
        if (type(ns) is not int or not 0 <= ns <= 120_000_000_000 or not isinstance(sensors, list)
                or any(not isinstance(s, str) or s not in self.sensor_kinds for s in sensors)
                or len(set(sensors)) != len(sensors)):
            raise ValueError('invalid continuous source selection')
        # A request may wait for a nearby minimum but cannot jump the physics clock.
        current = self.inspection.state()['simulation_time_s'] * 1e9
        if ns > current + 2_000_000_000:
            raise ValueError('continuous minimum may be at most two seconds ahead')
        deadline = time.monotonic() + 5
        with self.changed:
            self.enabled, self.selection = tuple(sensors), self.selection + 1
            selection = self.selection
            if not self.physics_started:
                self._start_locked()
            while self.captured_selection != selection or self.last_fixed_capture_ns < ns:
                if self.selection != selection:
                    raise ValueError('source selection superseded')
                if self.done.is_set() or self.inspection.done.is_set() or self.render_error:
                    raise ValueError('continuous source unavailable')
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise ValueError('continuous capture did not complete in time')
                self.changed.wait(remaining)
            return {'sequence': self.sequence, 'simulation_time_ns': self.simulation_ns, 'sensors': list(sensors)}

    def presentation(self):
        import copy
        from sim.actuator import Rejected
        with self.lock:
            if self.latest_presentation is None or self.render_error or self.inspection.done.is_set():
                raise Rejected('continuous presentation unavailable', 503)
            return copy.deepcopy(self.latest_presentation)

    def inspection_samples(self):
        import json
        import time
        from sim.actuator import Rejected
        if not self.physics_started:
            raise Rejected('inspection source has not started', 503)
        state = self.inspection.state()
        with self.lock:
            raw = self.inspection_latest
            if (raw is None or time.monotonic() - self.inspection_captured > MAX_SNAPSHOT_AGE
                    or json.loads(raw)['state']['session'] != state['session']):
                raise Rejected('inspection samples unavailable or stale', 503)
            return raw

    def close(self):
        self.done.set()
        self.inspection.close()
        with self.changed:
            self.changed.notify_all()
        if self.render_thread is not None:
            self.render_thread.join(timeout=5)
            if self.render_thread.is_alive():
                raise RuntimeError('continuous render worker did not finish')
        try:
            self.worker.submit(self.render_scene.close).result()
        finally:
            super().close()


def inspection_server(source, http, read_token, control_token):
    """Extend only an explicitly continuous source with prefixed Atlas routes."""
    import json
    from sim.actuator import Rejected
    if not isinstance(source, ContinuousSource):
        return http
    base = http.RequestHandlerClass

    class Handler(base):
        def do_GET(self):
            if not self.path.startswith(PREFIX):
                return super().do_GET()
            if not self.authorized(read_token):
                return self.reply(401, {'error': 'unauthorized'})
            try:
                if not source.physics_started:
                    raise Rejected('inspection source has not started', 503)
                if self.path == PREFIX + 'state':
                    return self.reply(200, source.inspection.state())
                if self.path == PREFIX + 'samples':
                    return self.reply(200, source.inspection_samples())
                return self.reply(404, {'error': 'unknown endpoint'})
            except Rejected as exc:
                return self.reply(exc.status, {'error': str(exc)})

        def do_POST(self):
            if not self.path.startswith(PREFIX):
                return super().do_POST()
            if not self.authorized(control_token):
                return self.reply(401, {'error': 'unauthorized'})
            try:
                if not source.physics_started:
                    raise Rejected('inspection source has not started', 503)
                length = int(self.headers.get('Content-Length', '0'))
                if not 0 < length < 1024 or self.headers.get('Transfer-Encoding'):
                    raise ValueError('bounded inspection body required')
                body = json.loads(self.rfile.read(length))
                return self.reply(200, source.inspection.request(self.path, body))
            except Rejected as exc:
                return self.reply(exc.status, {'error': str(exc)})
            except (ValueError, TypeError) as exc:
                return self.reply(400, {'error': str(exc)})
    http.RequestHandlerClass = Handler
    return http


def serve_inspection(source_factory, server_factory):
    import argparse
    from pathlib import Path
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--inspection', action='store_true')
    parser.add_argument('--inspection-defer-start', action='store_true',
                        help='start physics once on the first authenticated source step')
    parser.add_argument('--inspection-fixed-capture-hz', type=int, choices=(1, 5), default=5,
                        help='fixed sensor cadence; robot snapshots and presentation remain 5 Hz')
    parser.add_argument('--bind', default='127.0.0.1')
    parser.add_argument('--port', type=int, default=8091)
    parser.add_argument('--run-id', required=True)
    parser.add_argument('--anchor-unix-ns', required=True, type=int)
    parser.add_argument('--capture-resolution-ns', type=int, choices=(1, 1000), default=1)
    parser.add_argument('--read-token-file', required=True, type=Path)
    parser.add_argument('--control-token-file', required=True, type=Path)
    args = parser.parse_args()
    tokens = [p.read_text().strip() for p in (args.read_token_file, args.control_token_file)]
    if any(len(t) < 32 or len(t) > 256 or any(c.isspace() for c in t) for t in tokens) or tokens[0] == tokens[1]:
        parser.error('distinct bounded source read and control tokens required')
    source = source_factory(args.run_id, args.anchor_unix_ns, capture_resolution_ns=args.capture_resolution_ns,
                            defer_start=args.inspection_defer_start, fixed_capture_hz=args.inspection_fixed_capture_hz)
    try:
        http = server_factory(source, (args.bind, args.port), *tokens)
        try:
            http.serve_forever()
        finally:
            http.server_close()
    finally:
        source.close()
