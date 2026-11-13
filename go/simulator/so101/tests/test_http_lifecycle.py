"""Actual loopback HTTP/physics/render lifecycle. No browser or physical arm."""
import hashlib
import http.client
import io
import json
import math
import os
from pathlib import Path
import socket
import subprocess
import sys
import threading
import time
import uuid

from PIL import Image
import pytest
import uvicorn
from so101_sim import server

pytestmark = pytest.mark.skipif(os.getenv('SO101_RENDER_TEST') != '1', reason='requires the shipped real renderer')


def _exercise_http_lifecycle():
    root = Path(server.__file__).resolve().parents[1]
    lock = json.loads((root / 'assets.lock.json').read_text())
    for name, digest in lock['sha256'].items():
        assert hashlib.sha256((root / 'model' / name).read_bytes()).hexdigest() == digest
    name = 'http-arm-' + uuid.uuid4().hex
    digest = 'sha256:' + hashlib.sha256(Path(server.__file__).read_bytes()).hexdigest()
    os.environ['SO101_VM_NAME'] = name
    os.environ['SO101_SOURCE_DIGEST'] = digest
    active = []

    def request(port, path, body=None, expected=200):
        connection = http.client.HTTPConnection('127.0.0.1', port, timeout=2)
        try:
            connection.request('GET' if body is None else 'POST', path,
                               body=None if body is None else json.dumps(body),
                               headers={} if body is None else {'Content-Type': 'application/json'})
            response = connection.getresponse()
            data = response.read(2 * 1024 * 1024 + 1)
            assert len(data) <= 2 * 1024 * 1024
            assert response.status == expected, f'SO101_HTTP_STATUS {path}: {response.status} {data[:200]!r}'
            return data if path == '/camera.jpg' else json.loads(data)
        finally:
            connection.close()

    def stop(entry):
        instance, worker, listener, simulator = entry
        instance.should_exit = True
        worker.join(12)
        listener.close()
        assert not worker.is_alive(), 'actual ASGI/lifespan worker did not stop'
        assert simulator.stop.is_set(), 'production simulator shutdown not signalled'
        assert not any(thread.is_alive() and getattr(getattr(thread, '_target', None), '__self__', None) is simulator for thread in threading.enumerate()), 'production simulator worker still running'
        if entry in active:
            active.remove(entry)

    def start(port=0):
        listener = socket.socket()
        try:
            listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            listener.bind(('127.0.0.1', port))
            listener.listen(16)
            port = listener.getsockname()[1]
            instance = uvicorn.Server(uvicorn.Config(server.app, log_level='error', lifespan='on', timeout_graceful_shutdown=3))
        except BaseException:
            listener.close()
            raise
        previous_simulator = server.sim
        errors = []
        def serve():
            try:
                instance.run(sockets=[listener])
            except BaseException as error:
                errors.append(error)
        worker = threading.Thread(target=serve, daemon=True)
        deadline = time.monotonic() + 65
        try:
            worker.start()
            while not instance.started:
                assert worker.is_alive() and not errors, f'actual ASGI startup failed: {errors}'
                assert time.monotonic() < deadline, 'actual simulator startup deadline'
                time.sleep(.02)
            simulator = server.sim
            entry = (instance, worker, listener, simulator)
            active.append(entry)
            status = request(port, '/api/status')
            assert status['vm_name'] == name and status['source_digest'] == digest
            assert status['healthy'] and status['ready'] and status['virtual_only']
            assert status['backend'] == 'mujoco' and status['robot_kind'] == 'so101'
            return port, entry, status
        except BaseException:
            instance.should_exit = True
            if worker.ident is not None:
                worker.join(12)
            listener.close()
            assert not worker.is_alive(), 'failed startup left ASGI worker running; owned process must exit'
            created = server.sim
            if created is not None and created is not previous_simulator:
                assert created.stop.is_set(), 'failed startup did not stop its simulator'
                assert not any(thread.is_alive() and getattr(getattr(thread, '_target', None), '__self__', None) is created for thread in threading.enumerate()), 'failed startup left simulator worker running; owned process must exit'
            raise

    try:
        port, first, initial = start()
        image = Image.open(io.BytesIO(request(port, '/camera.jpg')))
        image.load()
        assert image.size == (640, 480) and image.format == 'JPEG'
        joints = initial['joints']
        for bound in ('min_deg', 'max_deg'):
            command = {joint['name']: joint[bound] for joint in joints}
            # This is the JSON value emitted by the real status endpoint, not
            # recomputed or rounded test input. Original b3c475393 rejects it.
            try:
                accepted = request(port, '/api/pose', {'joints_deg': command})
            except AssertionError as error:
                if not str(error).startswith('SO101_HTTP_STATUS /api/pose: 422 ') or 'Out of range:' not in str(error):
                    raise
                raise AssertionError('SO101_REPORTED_BOUND_REJECTED ' + bound) from error
            assert all(math.isclose(joint['target_deg'], command[joint['name']], abs_tol=1e-10) for joint in accepted['joints'])
        invalid = {'shoulder_pan': 0, 'wrist_roll': 9999}
        request(port, '/api/pose', {'joints_deg': invalid}, expected=422)
        assert [j['target_deg'] for j in request(port, '/api/status')['joints']] == [j['target_deg'] for j in accepted['joints']]
        reset = request(port, '/api/reset', {})
        request(port, '/api/pose', {'joints_deg': {'shoulder_pan': 25}})
        deadline = time.monotonic() + 4
        while True:
            moved = request(port, '/api/status')
            if moved['joints'][0]['position_deg'] > 5 and moved['frames'] > reset['frames']:
                break
            assert time.monotonic() < deadline, 'real physics/render did not advance after HTTP pose'
            time.sleep(.03)
        paused = request(port, '/api/pause', {'paused': True})
        time.sleep(.1)
        assert request(port, '/api/status')['simulation_seconds'] == paused['simulation_seconds']
        old_simulator = first[3]
        stop(first)
        with pytest.raises((OSError, http.client.HTTPException)):
            request(port, '/api/status')
        # Rebind the same loopback endpoint only after the old owner has joined.
        # An unexpected bind owner fails the test; it is never terminated.
        again_port, second, recovered = start(port)
        assert again_port == port and second[3] is not old_simulator
        assert recovered['error'] is None and not recovered['paused']
        assert recovered['epoch'] == 1 and recovered['simulation_seconds'] >= 0
        assert all(math.isclose(j['target_deg'], math.degrees(server.HOME[i]), abs_tol=1e-10) for i, j in enumerate(recovered['joints']))
        request(port, '/api/pose', {'joints_deg': {'shoulder_pan': 10}})
        assert request(port, '/api/status')['joints'][0]['target_deg'] == 10
        stop(second)
        print('SO101_HTTP_LIFECYCLE_VERIFIED real status bounds, physics, render, shutdown, refused poll and same-endpoint restart')
    finally:
        for entry in active[:]:
            stop(entry)


def test_real_http_joint_bounds_and_restart():
    # The dedicated process owns all native renderer/physics and ASGI threads.
    # A failed startup or uncertain thread join cannot leak into other tests.
    # No subprocess is launched inside _exercise_http_lifecycle.
    result = subprocess.run(
        [sys.executable, str(Path(__file__).resolve())],
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, timeout=180,
    )
    print(result.stdout)
    assert result.returncode == 0, result.stdout
    assert 'SO101_HTTP_LIFECYCLE_VERIFIED' in result.stdout


if __name__ == '__main__':
    if os.getenv('SO101_RENDER_TEST') != '1':
        raise RuntimeError('real shipped renderer must be required')
    _exercise_http_lifecycle()
