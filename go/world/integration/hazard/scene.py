#!/usr/bin/env python3
"""Stepped office observations for ordinary multimodal capture publishers.

Camera pixels and ray returns come from MuJoCo. Gas and audio reuse the labeled
analytic/generated office fixtures. No derived hazard enters a capture reply.
"""
from __future__ import annotations

import argparse
import base64
from concurrent.futures import ThreadPoolExecutor
import hashlib
import hmac
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import math
from pathlib import Path
import random
import re
import threading
import time
import uuid

from sim.office_fusion import to_world
from sim.run import gas_value
from sim.sensors import HEIGHT, WIDTH, audio_wav, calibration, png_rgb

SENSORS = {'camera-a': 'camera', 'camera-b': 'camera', 'lidar-atlas': 'lidar',
           'gas-1': 'gas', 'audio-1': 'audio'}
SCENARIO = 'furnished-office-fusion-v1'
SCENE = Path(__file__).resolve().parents[2] / 'examples/collaborative-office/scene.json'


def encoded(value):
    return json.dumps(value, allow_nan=False, separators=(',', ':')).encode()


class OfficeScene:
    """The existing office sensors, stationary and without a motion coordinator."""
    sensor_kinds = SENSORS
    scenario_version = SCENARIO
    calibration_revision = 'office-sensors-v1'

    def __init__(self, run_id, capture_resolution_ns=1):
        import mujoco
        import numpy as np
        if mujoco.__version__ != '4.0.0':
            raise ValueError('MuJoCo 4.0.0 required')
        if capture_resolution_ns not in (1, 1000):
            raise ValueError('nanosecond or SensorLink microsecond capture resolution required')
        self.capture_resolution_ns = capture_resolution_ns
        self.mj, self.np, self.run_id = mujoco, np, run_id
        raw = SCENE.read_bytes()
        self.config = json.loads(raw)
        self.package_sha256 = hashlib.sha256(raw).hexdigest()
        xml = (SCENE.parent / self.config['model']['path']).read_bytes()
        if hashlib.sha256(xml).hexdigest() != self.config['model']['sha256']:
            raise ValueError('office model digest differs')
        self.frame = {'id': 'office-floor', 'revision': 'office-' + self.config['model']['sha256'][:16]}
        self.model = mujoco.MjModel.from_xml_string(xml.decode())
        self.data = mujoco.MjData(self.model)
        self.bindings = {r['id']: {'body': mujoco.mj_name2id(self.model, mujoco.mjtObj.mjOBJ_BODY, r['body'])}
                         for r in self.config['robots']}
        if self.bindings['atlas']['body'] <= 0:
            raise ValueError('Atlas body missing')
        self.cameras = {'camera-a': 'camera-a', 'camera-b': 'corridor-view'}
        self.camera_ids = {n: mujoco.mj_name2id(self.model, mujoco.mjtObj.mjOBJ_CAMERA, n)
                           for n in self.cameras.values()}
        if min(self.camera_ids.values()) < 0:
            raise ValueError('office cameras missing')
        mujoco.mj_forward(self.model, self.data)
        self.renderer = mujoco.Renderer(self.model, height=HEIGHT, width=WIDTH)
        self.geometry = None

    def advance(self, ns):
        target = ns / 1e9
        original = self.model.opt.timestep
        try:
            while self.data.time < target - 1e-10:
                self.model.opt.timestep = min(original, target - self.data.time)
                self.mj.mj_step(self.model, self.data)
            self.data.time = target
        finally:
            self.model.opt.timestep = original
        self.mj.mj_forward(self.model, self.data)

    def capture(self, sensor, ns, sequence):
        # Select precision at original acquisition. SensorLink carries whole
        # microseconds; a bridge must never rewrite an already issued capture.
        captured = time.time_ns() // self.capture_resolution_ns * self.capture_resolution_ns
        sample = dict(schema_version='wendy.sensor.sample.v1', id=f'{self.run_id}/{sensor}/{sequence}',
            site_id='studio-01', frame=self.frame, sensor_id=sensor, kind=self.sensor_kinds[sensor],
            sequence=sequence, sample_ns=ns, run_id=self.run_id, scenario_version=self.scenario_version,
            seed=7, package_sha256=self.package_sha256, position_m=[0., 0., 0.],
            uncertainty_m=.02, calibration_revision=self.calibration_revision)
        media = b''
        if sensor in self.cameras:
            position, camera = self.calibration(self.cameras[sensor])
            sample.update(position_m=position, camera=camera, sensor_model='mujoco-4.0.0-rgb-pinhole')
            media = self.render(self.cameras[sensor])
        elif sensor == 'lidar-atlas':
            body = self.bindings['atlas']['body']
            position = self.data.xpos[body]
            points = []
            # Same three heights, 48 directions and six-metre ray limit as
            # SceneRuntime._lidar. Only actual ray hits become observations.
            for height in (.25, .65, 1.15):
                origin = self.np.array([position[0], position[1], height], dtype=float)
                for i in range(48):
                    angle = 2 * math.pi * i / 48
                    direction = self.np.array([math.cos(angle), math.sin(angle), 0.], dtype=float)
                    geom = self.np.zeros(1, dtype=self.np.int32)
                    distance = self.mj.mj_ray(self.model, self.data, origin, direction, None, True, body, geom)
                    if .04 < distance <= 6.:
                        points.append(to_world(origin + distance * direction))
            sample.update(position_m=to_world([position[0], position[1], .65]),
                uncertainty_m=.03, sensor_model='mujoco-4.0.0-raycast',
                lidar={'points_m': points, 'max_range_m': 6.1})
        elif sensor == 'gas-1':
            position = to_world([-7.1, .2, .7])
            field = {'source_local_m': to_world([-7.3, .9, .65]), 'sigma_m': 1.6,
                     'background_ppm': 5, 'peak_ppm': 95, 'noise_stddev_ppm': 1.5}
            sample.update(position_m=position, sensor_model='analytic-gaussian-fixture-v1',
                gas={'value_ppm': gas_value(field, position, random.Random(7 + sequence)),
                     'uncertainty_ppm': 1.5, 'species': 'synthetic-test-gas'})
        else:
            sample.update(position_m=to_world([-6.6, -.1, 1.2]), sensor_model='generated-880hz-tone-v1',
                audio={'sample_rate_hz': 8000, 'channels': 1, 'duration_ns': 250_000_000})
            media = audio_wav(7, sequence, True)
        if media:
            sample['evidence'] = {'sha256': hashlib.sha256(media).hexdigest(),
                'content_type': 'audio/wav' if sensor == 'audio-1' else 'image/png', 'size': len(media)}
        return sample, media, captured

    def calibration(self, camera_name):
        return calibration(self.model, self.data, self.camera_ids[camera_name])

    def render(self, camera_name):
        self.renderer.update_scene(self.data, camera=camera_name)
        return png_rgb(self.renderer.render())

    def presentation(self):
        from sim.macos_demo_bridge import geometry, _transform
        if self.geometry is None:
            self.geometry, self.dynamic_ids = geometry(self)
            self.geometry['semantics'] = 'Reference simulation geometry; hazard inference arrives from a remote replica'
        return {'geometry': self.geometry,
                'dynamic_geoms': [_transform(self, i) for i in self.dynamic_ids]}

    def close(self):
        self.renderer.close()


class Source:
    def __init__(self, run_id, anchor, renderer=None, capture_resolution_ns=1, *, scene_factory=None):
        if not isinstance(run_id, str) or re.fullmatch(r'[A-Za-z0-9_-]{1,80}', run_id) is None or type(anchor) is not int or not 0 < anchor < 2**63-120_000_000_000:
            raise ValueError('bounded run identity and anchor required')
        self.worker = ThreadPoolExecutor(max_workers=1, thread_name_prefix='hazard-render')
        try:
            factory = scene_factory or OfficeScene
            self.scene = self.worker.submit(lambda: renderer if renderer is not None else factory(run_id, capture_resolution_ns)).result()
        except BaseException:
            self.worker.shutdown()
            raise
        self.identity = dict(schema_version='wendy.sim.clock.v1', run_id=run_id, boot_id=uuid.uuid4().hex,
            package_sha256=self.scene.package_sha256, frame=self.scene.frame,
            clock_domain='simulation:'+run_id, anchor_unix_ns=anchor)
        self.lock = threading.Lock()
        self.simulation_ns = self.sequence = self.clock_sequence = 0
        self.sensor_kinds = dict(getattr(self.scene, 'sensor_kinds', SENSORS))
        self.scenario_version = getattr(self.scene, 'scenario_version', SCENARIO)
        self.sequences = dict.fromkeys(self.sensor_kinds, 0)
        self.captures = {}

    def clock(self, nonce):
        if not isinstance(nonce, str) or re.fullmatch(r'[0-9a-f]{64}', nonce) is None:
            raise ValueError('clock nonce required')
        with self.lock:
            self.clock_sequence += 1
            return {**self.identity, 'simulation_time_ns': self.simulation_ns,
                'observed_unix_ns': time.time_ns(), 'observation_sequence': self.clock_sequence, 'nonce': nonce}

    def step(self, value):
        return self.worker.submit(self._step, value).result()

    def capture_batches(self, sensors):
        """The default source exposes the complete requested instant atomically."""
        return (sensors,)

    def _step(self, value):
        if not isinstance(value, dict) or set(value) != {'simulation_time_ns', 'sensors'}:
            raise ValueError('exact step fields required')
        ns, sensors = value['simulation_time_ns'], value['sensors']
        if type(ns) is not int or not 0 < ns <= 120_000_000_000 or not isinstance(sensors, list) or len(sensors) > len(self.sensor_kinds) or any(not isinstance(s, str) or s not in self.sensor_kinds for s in sensors) or len(set(sensors)) != len(sensors) or sensors and ns < 250_000_000:
            raise ValueError('invalid step')
        with self.lock:
            if ns <= self.simulation_ns:
                raise ValueError('simulation time must increase')
            sequences = dict(self.sequences)
        self.scene.advance(ns)
        captures = {}
        for batch in self.capture_batches(sensors):
            for sensor in batch:
                sequences[sensor] += 1
                sample, media, captured = self.scene.capture(sensor, ns, sequences[sensor])
                capture = dict(version=1, scene_boot_id=self.identity['boot_id'], captured_unix_ns=captured,
                    simulation_anchor_unix_ns=self.identity['anchor_unix_ns'],
                    sample_json=base64.b64encode(encoded(sample)).decode())
                captures[sensor] = encoded({'capture': capture, 'media': base64.b64encode(media).decode()})
            with self.lock:
                # A source may opt into exposing completed groups from this
                # one scene instant. The first commit replaces all old inputs;
                # later commits add only this instant's exact captured bytes.
                self.simulation_ns = ns
                self.sequences, self.captures = dict(sequences), dict(captures)
        with self.lock:
            self.sequence += 1
            return {'sequence': self.sequence, 'simulation_time_ns': ns, 'sensors': sensors}

    def capture(self, sensor):
        with self.lock:
            return self.captures.get(sensor)

    def presentation(self):
        return self.worker.submit(self._presentation).result()

    def _presentation(self):
        with self.lock:
            ns, enabled = self.simulation_ns, set(self.captures)
        return {**self.scene.presentation(), 'simulation_time_ns': ns,
            'sensor_sources': [{'id': s, 'kind': kind, 'enabled': s in enabled} for s, kind in self.sensor_kinds.items()],
            'camera_sources': [{'id': s, 'enabled': s in enabled} for s, kind in self.sensor_kinds.items() if kind == 'camera']}

    def close(self):
        try:
            self.worker.submit(self.scene.close).result()
        finally:
            self.worker.shutdown()


def server(source, address, read_token, control_token):
    class Handler(BaseHTTPRequestHandler):
        def setup(self):
            super().setup()
            self.connection.settimeout(5)

        def log_message(self, *_):
            pass

        def reply(self, status, value=None):
            raw = b'' if value is None else value if isinstance(value, bytes) else encoded(value)
            self.send_response(status)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(raw)))
            self.send_header('Cache-Control', 'no-store')
            self.end_headers()
            self.wfile.write(raw)

        def authorized(self, token):
            return hmac.compare_digest(self.headers.get('Authorization', '').encode(), ('Bearer '+token).encode())

        def do_GET(self):
            if not self.authorized(read_token):
                return self.reply(401, {'error': 'unauthorized'})
            try:
                if self.path == '/v1/identity':
                    return self.reply(200, {**source.identity, 'site': 'studio-01', 'scenario_version': source.scenario_version, 'seed': 7})
                if self.path == '/v1/clock':
                    return self.reply(200, source.clock(self.headers.get('X-Wendy-Clock-Nonce')))
                if self.path == '/v1/presentation':
                    return self.reply(200, source.presentation())
                if self.path.startswith('/v1/captures/') and self.path.removeprefix('/v1/captures/') in source.sensor_kinds:
                    raw = source.capture(self.path.removeprefix('/v1/captures/'))
                    return self.reply(200 if raw else 204, raw)
                if self.path.startswith('/v1/cameras/') and source.sensor_kinds.get(self.path.removeprefix('/v1/cameras/')) == 'camera':
                    raw = source.capture(self.path.removeprefix('/v1/cameras/'))
                    return self.reply(200 if raw else 204, raw)
                return self.reply(404, {'error': 'unknown endpoint'})
            except ValueError as exc:
                return self.reply(400, {'error': str(exc)})

        def do_POST(self):
            if not self.authorized(control_token):
                return self.reply(401, {'error': 'unauthorized'})
            if self.path != '/v1/step':
                return self.reply(404, {'error': 'unknown endpoint'})
            try:
                length = int(self.headers.get('Content-Length', '0'))
                if not 0 < length < 1024 or self.headers.get('Transfer-Encoding'):
                    raise ValueError('bounded step body required')
                return self.reply(200, source.step(json.loads(self.rfile.read(length))))
            except (ValueError, TypeError) as exc:
                return self.reply(400, {'error': str(exc)})
    return ThreadingHTTPServer(address, Handler)


def main(source_factory=Source):
    parser = argparse.ArgumentParser()
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
        parser.error('distinct bounded source read and step tokens required')
    source = source_factory(args.run_id, args.anchor_unix_ns, capture_resolution_ns=args.capture_resolution_ns)
    http = server(source, (args.bind, args.port), *tokens)
    try:
        http.serve_forever()
    finally:
        http.server_close()
        source.close()


if __name__ == '__main__':
    main()
