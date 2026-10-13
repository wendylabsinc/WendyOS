"""Real MuJoCo model/API contract tests, with optional OSMesa render smoke."""
import hashlib
import json
import math
import os
from pathlib import Path
import threading
import time

import numpy as np
import pytest
from fastapi.testclient import TestClient
from so101_sim import server


@pytest.fixture
def client(monkeypatch):
    monkeypatch.setenv('SO101_VM_NAME', 'test-arm')
    monkeypatch.setenv('SO101_SOURCE_DIGEST', 'sha256:' + 'a' * 64)
    # Physics/model/scene remain real. Unit API tests do not require OpenGL.
    def worker(self):
        self.last_frame = self.last_physics = time.monotonic()
        self.ready.set()
        while not self.stop.wait(.02):
            self.last_frame = self.last_physics = time.monotonic()
    monkeypatch.setattr(server.Simulator, 'run', worker)
    with TestClient(server.app) as result:
        yield result


def test_pinned_model_and_visual_scene(client):
    root = Path(server.__file__).resolve().parents[1]
    lock = json.loads((root / 'assets.lock.json').read_text())
    assert lock['revision'] == 'a758567c3978dfeefe282ede0500085a48fe8f78'
    for name, digest in lock['sha256'].items():
        assert hashlib.sha256((root / 'model' / name).read_bytes()).hexdigest() == digest
    viewer_lock = json.loads((root / 'viewer-assets.lock.json').read_text())
    for name, entry in viewer_lock['files'].items():
        data = (root / 'so101_sim' / 'vendor' / name).read_bytes()
        if name == 'OrbitControls.js':
            assert b"from './three.module.js';" in data
            data = data.replace(b"from './three.module.js';", b"from 'three';")
        assert hashlib.sha256(data).hexdigest() == entry['sha256']
        assert client.get('/vendor/' + name).status_code == 200
    status = client.get('/api/status').json()
    assert status['healthy'] and status['ready'] and status['simulation']
    assert status['robot_kind'] == 'so101' and status['vm_name'] == 'test-arm'
    assert status['source_digest'] == 'sha256:' + 'a' * 64
    assert status['policy_bundle'] == 'so101-follower-a758567-v1'
    assert status['dds_isolation'] == 'disabled'
    assert [j['name'] for j in status['joints']] == server.NAMES
    scene = client.get('/api/scene').json()
    state = client.get('/api/scene/state').json()
    assert scene['robot_body'] >= 0 and len(scene['meshes']) >= 10
    assert state['scene_id'] == scene['id'] and state['valid']
    assert len(state['positions']) == len(scene['bodies']) * 3
    assert len(state['quaternions']) == len(scene['bodies']) * 4
    assert all(math.isfinite(x) for x in state['positions'] + state['quaternions'])
    assert 'Joint targets' in client.get('/').text
    assert client.get('/viewer.js').status_code == 200
    assert client.get('/assets.lock.json').status_code == 404


def test_bounded_atomic_joint_commands_and_reset(client):
    before = client.get('/api/status').json()
    target = {'shoulder_pan': 25, 'wrist_roll': -20}
    assert client.post('/api/pose', json={'joints_deg': target}).status_code == 200
    for _ in range(100):
        # Step the same rate-limited controller as the production loop.
        with server.sim.lock:
            server.sim.step()
    after = client.get('/api/status').json()
    assert after['joints'][0]['position_deg'] > before['joints'][0]['position_deg'] + 5
    assert client.get('/api/scene/state').json()['time'] > 0
    for invalid in [
        {'shoulder_pan': 9999}, {'serial_motor': 0}, {'gripper': -999},
        {'shoulder_pan': 10, 'wrist_roll': 9999}, {'elbow_flex': float('inf')},
    ]:
        # Raw JSON permits testing non-finite input that httpx disallows.
        result = client.post('/api/pose', content=json.dumps({'joints_deg': invalid}), headers={'Content-Type': 'application/json'})
        assert result.status_code == 422
        assert [j['target_deg'] for j in client.get('/api/status').json()['joints']] == [j['target_deg'] for j in after['joints']]
    assert client.post('/api/pose', json={'joints_deg': {}}).status_code == 422
    assert client.post('/api/pose', json={'joints_deg': {'gripper': 0}, 'device': '/dev/ttyUSB0'}).status_code == 422
    assert client.post('/api/command', json={'velocity': [1, 0, 0]}).status_code == 405
    client.post('/api/demo')
    client.post('/api/pause', json={'paused': True})
    reset = client.post('/api/reset', json={}).json()
    assert reset['epoch'] == after['epoch'] + 1 and not reset['paused'] and not reset['demo']
    assert np.allclose([j['target_deg'] for j in reset['joints']], np.rad2deg(server.HOME))
    assert client.get('/api/scene/state').json()['epoch'] == reset['epoch']


def test_control_origin_bounds_and_health(client):
    for headers in [{'Origin': 'https://foreign.example'}, {'Sec-Fetch-Site': 'cross-site'}, {'Host': 'rebound.example'}]:
        assert client.post('/api/reset', json={}, headers=headers).status_code == 403
    assert client.post('/api/reset', content='x' * 4097).status_code == 413
    assert client.post('/api/pause', json={'paused': True, 'extra': 0}).status_code == 422
    with server.sim.lock:
        server.sim.error = 'test physics fault'
    assert client.get('/api/status').json()['healthy'] is False
    assert client.get('/api/health').status_code == 503
    assert client.get('/camera.jpg').status_code == 503
    assert client.post('/api/pose', json={'joints_deg': {'gripper': 0}}).status_code == 503


@pytest.mark.skipif(os.getenv('SO101_RENDER_TEST') != '1', reason='requires OSMesa (Linux) or a Mac render context')
def test_real_render_loop():
    sim = server.Simulator()
    worker = threading.Thread(target=sim.run)
    worker.start()
    try:
        assert sim.ready.wait(60), 'renderer startup timeout'
        assert not sim.error
        assert sim.status()['healthy'] and sim.frame.startswith(b'\xff\xd8')
        initial = sim.frame_id
        sim.targets[0] = .3
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline and sim.frame_id < initial + 3:
            time.sleep(.05)
        assert sim.frame_id > initial
        assert sim.data.time > 0 and sim.data.qpos[sim.qids[0]] > 0
    finally:
        sim.stop.set()
        worker.join(10)
        assert not worker.is_alive()


def test_slow_render_does_not_block_physics_or_status(monkeypatch):
    entered, release = threading.Event(), threading.Event()

    class SlowRenderer:
        def __init__(self, *args, **kwargs):
            pass

        def update_scene(self, *args, **kwargs):
            pass

        def render(self):
            entered.set()
            assert release.wait(5)
            return np.zeros((480, 640, 3), dtype=np.uint8)

        def close(self):
            pass

    monkeypatch.setattr(server.mujoco, 'Renderer', SlowRenderer)
    monkeypatch.setattr(server, 'export_scene', lambda model: {'id': 'test'})
    sim = server.Simulator()
    worker = threading.Thread(target=sim.run)
    worker.start()
    try:
        assert entered.wait(5)
        with sim.lock:
            sim.targets[0] = .3
        start = time.monotonic()
        status = sim.status()
        assert time.monotonic() - start < .3
        time.sleep(.3)
        after = sim.status()
        assert after['simulation_seconds'] > status['simulation_seconds'] + .1
        assert after['joints'][0]['position_deg'] > 1
        assert after['physics_age_ms'] < 200
    finally:
        release.set()
        sim.stop.set()
        worker.join(10)
        assert not worker.is_alive()
