#!/usr/bin/env python3
"""Camera-only MuJoCo source for ordinary-app tracking on a stepped clock."""
from __future__ import annotations

import argparse
import base64
from concurrent.futures import ThreadPoolExecutor
import hashlib
import hmac
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import threading
import time
from types import SimpleNamespace
import uuid

from sim.people_scene import PeopleScene

CAMERAS = {'camera-a': 'people-front', 'camera-b': 'people-oblique'}


def encoded(value):
    return json.dumps(value, allow_nan=False, separators=(',', ':')).encode()


class Source:
    def __init__(self, run_id, anchor, renderer=None):
        if not run_id or len(run_id) > 128 or type(anchor) is not int or not 0 < anchor < 2**63-120_000_000_000:
            raise ValueError('bounded run identity and anchor required')
        self.scene = renderer if renderer is not None else PeopleScene()
        self.worker = ThreadPoolExecutor(max_workers=1, thread_name_prefix='people-render')
        digest = hashlib.sha256(self.scene.xml.encode()).hexdigest()
        self.identity = dict(schema_version='wendy.sim.clock.v1', run_id=run_id,
            boot_id=uuid.uuid4().hex, package_sha256=digest,
            frame={'id': 'people-annex-floor', 'revision': digest[:16]},
            clock_domain='simulation:'+run_id, anchor_unix_ns=anchor)
        self.lock = threading.Lock()
        self.step_lock = threading.Lock()
        self.simulation_ns = self.sequence = self.clock_sequence = 0
        self.captures = {}
        self.reference_geometry = None

    def clock(self, nonce):
        if not isinstance(nonce, str) or len(nonce) != 64 or any(c not in '0123456789abcdef' for c in nonce):
            raise ValueError('clock nonce required')
        with self.lock:
            self.clock_sequence += 1
            return {**self.identity, 'simulation_time_ns': self.simulation_ns,
                'observed_unix_ns': time.time_ns(), 'observation_sequence': self.clock_sequence, 'nonce': nonce}

    def step(self, value):
        # Keep the renderer's graphics context on the same thread while clock
        # observations remain responsive during a capture.
        return self.worker.submit(self._step, value).result()

    def _step(self, value):
        if not isinstance(value, dict) or set(value) != {'simulation_time_ns', 'cameras'}:
            raise ValueError('exact step fields required')
        ns, cameras = value['simulation_time_ns'], value['cameras']
        if type(ns) is not int or not 0 < ns <= 120_000_000_000 or not isinstance(cameras, list) or len(cameras) > 2 or any(c not in CAMERAS for c in cameras) or len(set(cameras)) != len(cameras):
            raise ValueError('invalid step')
        with self.step_lock:
            with self.lock:
                if ns <= self.simulation_ns:
                    raise ValueError('simulation time must increase')
                sequence = self.sequence + 1
            self.scene.pose(ns/1e9)
            captures = {}
            for sensor in cameras:
                raw = self.scene.render(CAMERAS[sensor])
                captured = time.time_ns()
                cal = self.scene.calibration(CAMERAS[sensor])
                m = cal['camera_to_scene']
                # MuJoCo Z-up to the shared right-handed Y-up site frame.
                world = m[:4]+m[8:12]+[-x for x in m[4:8]]+m[12:16]
                camera = {key: cal[key] for key in ('width','height','fx','fy','cx','cy')}
                camera.update(camera_to_site=world, calibration_valid_for_ns=60_000_000_000)
                i = self.identity
                sample = dict(schema_version='wendy.sensor.sample.v1',
                    id=f'{i["run_id"]}/{sensor}/{sequence}', site_id='studio-01', frame=i['frame'],
                    sensor_id=sensor, kind='camera', sequence=sequence, sample_ns=ns, run_id=i['run_id'],
                    scenario_version='people-annex-v1', seed=7, package_sha256=i['package_sha256'],
                    sensor_model='mujoco-4.0.0-rgb-pinhole', position_m=[world[3],world[7],world[11]],
                    uncertainty_m=.02, calibration_revision='annex-camera-v1', camera=camera,
                    evidence={'sha256':hashlib.sha256(raw).hexdigest(),'content_type':'image/png','size':len(raw)})
                capture = dict(version=1, scene_boot_id=i['boot_id'], captured_unix_ns=captured,
                    simulation_anchor_unix_ns=i['anchor_unix_ns'], sample_json=base64.b64encode(encoded(sample)).decode())
                captures[sensor] = encoded({'capture':capture,'media':base64.b64encode(raw).decode()})
            with self.lock:
                self.simulation_ns, self.sequence, self.captures = ns, sequence, captures
                return {'sequence':sequence,'simulation_time_ns':ns,'cameras':cameras}

    def camera(self, sensor):
        with self.lock:
            return self.captures.get(sensor)

    def presentation(self):
        # Reference geometry is for the twin renderer only. Camera publishers
        # use /v1/cameras; the inference subprocess receives only image bytes.
        return self.worker.submit(self._presentation).result()

    def _presentation(self):
        from sim.macos_demo_bridge import geometry, _transform
        runtime = SimpleNamespace(model=self.scene.model, data=self.scene.data,
            mj=self.scene.mj, bindings={}, run_id=self.identity['run_id'],
            config={'model':{'sha256':self.identity['package_sha256']},
                'presentation':{'name':'Studio 01 / Shared perception',
                    'subtitle':'Image-derived tracks across Wendy agents','labels':[]}})
        if self.reference_geometry is None:
            self.reference_geometry, _ = geometry(runtime)
            for geom in self.reference_geometry['geoms']:
                if geom['name'].startswith('person-'):
                    geom['dynamic'] = True
            self.reference_geometry['semantics'] = 'Reference simulation geometry; inferred tracks arrive from a remote replica'
        return {'geometry':self.reference_geometry, 'simulation_time_ns':self.simulation_ns,
            'dynamic_geoms':[_transform(runtime,g['id']) for g in self.reference_geometry['geoms'] if g.get('dynamic')],
            'camera_sources':[{'id':name,'enabled':name in self.captures} for name in CAMERAS]}

    def close(self):
        self.worker.submit(self.scene.close).result()
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
            self.send_header('Content-Type','application/json')
            self.send_header('Content-Length',str(len(raw)))
            self.send_header('Cache-Control','no-store')
            self.end_headers()
            self.wfile.write(raw)

        def authorized(self, token):
            return hmac.compare_digest(self.headers.get('Authorization','').encode(), ('Bearer '+token).encode())

        def do_GET(self):
            if not self.authorized(read_token):
                return self.reply(401, {'error':'unauthorized'})
            try:
                if self.path == '/v1/identity':
                    return self.reply(200, {**source.identity,'site':'studio-01','scenario_version':'people-annex-v1','seed':7})
                if self.path == '/v1/clock':
                    return self.reply(200, source.clock(self.headers.get('X-Wendy-Clock-Nonce')))
                if self.path == '/v1/presentation':
                    return self.reply(200, source.presentation())
                if self.path.startswith('/v1/cameras/') and self.path.removeprefix('/v1/cameras/') in CAMERAS:
                    raw = source.camera(self.path.removeprefix('/v1/cameras/'))
                    return self.reply(200 if raw else 204, raw)
                return self.reply(404, {'error':'unknown endpoint'})
            except ValueError as exc:
                return self.reply(400, {'error':str(exc)})

        def do_POST(self):
            if not self.authorized(control_token):
                return self.reply(401, {'error':'unauthorized'})
            if self.path != '/v1/step':
                return self.reply(404, {'error':'unknown endpoint'})
            try:
                length = int(self.headers.get('Content-Length','0'))
                if not 0 < length < 1024 or self.headers.get('Transfer-Encoding'):
                    raise ValueError('bounded step body required')
                value = json.loads(self.rfile.read(length))
                return self.reply(200, source.step(value))
            except (ValueError, TypeError) as exc:
                return self.reply(400, {'error':str(exc)})
    return ThreadingHTTPServer(address, Handler)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--bind', default='127.0.0.1')
    parser.add_argument('--port', type=int, default=8091)
    parser.add_argument('--run-id', required=True)
    parser.add_argument('--anchor-unix-ns', required=True, type=int)
    parser.add_argument('--read-token-file', required=True, type=Path)
    parser.add_argument('--control-token-file', required=True, type=Path)
    args = parser.parse_args()
    tokens = [p.read_text().strip() for p in (args.read_token_file,args.control_token_file)]
    if any(len(t) < 32 or len(t) > 256 or any(c.isspace() for c in t) for t in tokens) or tokens[0] == tokens[1]:
        parser.error('distinct bounded source read and step tokens required')
    source = Source(args.run_id,args.anchor_unix_ns)
    http = server(source,(args.bind,args.port),*tokens)
    try:
        http.serve_forever()
    finally:
        http.server_close()
        source.close()


if __name__ == '__main__':
    main()
