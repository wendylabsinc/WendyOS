"""Live sensor acquisition for the furnished office's ordinary fusion app.

Camera pixels and LiDAR returns come from MuJoCo. Gas is an analytic fixture
and audio is a generated tone. Only sensor observations enter the Go fuser;
the fixture's known location is used exclusively to generate its gas field.
"""
from __future__ import annotations

import base64
import copy
from datetime import datetime, timezone
import hashlib
import json
import math
import os
from pathlib import Path
import queue
import random
import select
import subprocess
import threading

from sim.run import gas_value
from sim.sensors import WIDTH, HEIGHT, audio_wav, calibration, png_rgb


def to_world(point):
    return [float(point[0]), float(point[2]), -float(point[1])]


def to_scene(point):
    return [float(point[0]), -float(point[2]), float(point[1])]


def replace_bytes(path, value):
    temporary = path.with_name('.' + path.name + '.next')
    with temporary.open('wb') as stream:
        stream.write(value)
    os.chmod(temporary, 0o600)
    os.replace(temporary, path)


class FusionPipe:
    """One pending capture; physics never waits for the inference subprocess."""
    def __init__(self, binary, *, config=None, state_dir=None):
        command = [str(Path(binary).resolve())]
        self.log = None
        if config is not None:
            command += ['--config', str(config), '--state-dir', str(state_dir)]
            Path(state_dir).mkdir(mode=0o700, exist_ok=True)
            self.log = (Path(state_dir) / 'worker.log').open('a')
        try:
            self.process = subprocess.Popen(command, stdin=subprocess.PIPE,
                stdout=subprocess.PIPE, stderr=self.log or subprocess.DEVNULL, text=True, bufsize=1)
        except BaseException:
            if self.log:
                self.log.close()
            raise
        self.requests, self.responses = queue.Queue(1), queue.Queue(2)
        self.done = threading.Event()
        self.error = None
        self.thread = threading.Thread(target=self._run, name='office-fusion', daemon=True)
        self.thread.start()

    def submit(self, request):
        if self.error or self.done.is_set():
            return False
        try:
            self.requests.put_nowait(request)
            return True
        except queue.Full:
            return False

    def poll(self):
        result = []
        while True:
            try:
                result.append(self.responses.get_nowait())
            except queue.Empty:
                return result

    def _run(self):
        try:
            while not self.done.is_set():
                try:
                    request = self.requests.get(timeout=.1)
                except queue.Empty:
                    continue
                self.process.stdin.write(json.dumps(request, allow_nan=False, separators=(',', ':')) + '\n')
                self.process.stdin.flush()
                if not select.select([self.process.stdout], [], [], 5)[0]:
                    raise TimeoutError('fusion response timed out')
                line = self.process.stdout.readline((2 << 20) + 1)
                if not line.endswith('\n') or len(line) > 2 << 20:
                    raise ValueError('fusion process ended or returned an oversized response')
                self.responses.put((request, json.loads(line)), timeout=1)
        except Exception as exc:
            if not self.done.is_set():
                self.error = str(exc)

    def close(self):
        self.done.set()
        if self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=2)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=2)
        self.thread.join(timeout=1)
        for stream in (self.process.stdin, self.process.stdout):
            try:
                stream.close()
            except BrokenPipeError:
                pass
        if self.log:
            self.log.close()


class OfficeSensors:
    """Samples the live model without taking over robot or mission controls."""
    def __init__(self, runtime):
        self.runtime = runtime
        self.sequence = 0
        self.last_capture = -math.inf
        self.frame = {'id': 'office-floor', 'revision': 'office-' + runtime.config['model']['sha256'][:16]}
        self.digest = hashlib.sha256(runtime.scene_path.read_bytes()).hexdigest()
        self.cameras = {'camera-a': 'camera-a', 'camera-b': 'corridor-view'}
        self.camera_ids = {key: runtime.mj.mj_name2id(runtime.model,
            runtime.mj.mjtObj.mjOBJ_CAMERA, name) for key, name in self.cameras.items()}
        if min(self.camera_ids.values()) < 0:
            raise ValueError('office fusion cameras are missing')
        self.renderer = runtime.mj.Renderer(runtime.model, height=HEIGHT, width=WIDTH)

    def ready(self):
        now = float(self.runtime.data.time)
        return (now >= .25 and now - self.last_capture >= .9
                and math.isfinite(self.runtime.last_scan_s)
                and 0 <= now - self.runtime.last_scan_s < .05)

    def capture(self, *, alarm=True):
        rt = self.runtime
        now = float(rt.data.time)
        if now < .25 or not math.isfinite(rt.last_scan_s):
            raise ValueError('camera/audio capture needs a completed scan and audio interval')
        self.sequence += 1
        self.last_capture = now
        sample_ns = round(now * 1_000_000_000)
        samples, media = [], {}

        def sample(sensor_id, kind, position, model, uncertainty=.02):
            return {'schema_version': 'wendy.sensor.sample.v1',
                'id': f'{rt.run_id}/{sensor_id}/{self.sequence}', 'site_id': 'studio-01',
                'frame': self.frame, 'sensor_id': sensor_id, 'kind': kind,
                'sequence': self.sequence, 'sample_ns': sample_ns, 'run_id': rt.run_id,
                'scenario_version': 'furnished-office-fusion-v1', 'seed': 7,
                'package_sha256': self.digest, 'sensor_model': model,
                'position_m': position, 'uncertainty_m': uncertainty,
                'calibration_revision': 'office-sensors-v1'}

        def attach(item, data, content_type):
            digest = hashlib.sha256(data).hexdigest()
            item['evidence'] = {'sha256': digest, 'content_type': content_type, 'size': len(data)}
            media[digest] = base64.b64encode(data).decode('ascii')

        for sensor_id, name in self.cameras.items():
            position, camera = calibration(rt.model, rt.data, self.camera_ids[sensor_id])
            item = sample(sensor_id, 'camera', position, 'mujoco-4.0.0-rgb-pinhole')
            item['camera'] = camera
            self.renderer.update_scene(rt.data, camera=name)
            attach(item, png_rgb(self.renderer.render()), 'image/png')
            samples.append(item)

        for robot_id, link in rt.links.items():
            position, _ = rt._pose(robot_id)
            if link.lidar_rays:
                position = link.lidar_rays[0][0]
            # Origins span three scan heights; their enclosing range is 6.1m.
            item = sample('lidar-' + robot_id, 'lidar',
                          to_world([position[0], position[1], .65]), 'mujoco-4.0.0-raycast', .03)
            item['sample_ns'] = round(rt.last_scan_s * 1_000_000_000)
            item['lidar'] = {'points_m': [to_world(p) for p in link.lidar_points], 'max_range_m': 6.1}
            samples.append(item)

        gas = sample('gas-1', 'gas', to_world([-7.1, .2, .7]), 'analytic-gaussian-fixture-v1')
        # Scene-only source placement never appears in the inference request.
        field = {'source_local_m': to_world([-7.3, .9, .65]), 'sigma_m': 1.6,
                 'background_ppm': 5, 'peak_ppm': 95 if alarm else 0, 'noise_stddev_ppm': 1.5}
        gas['gas'] = {'value_ppm': gas_value(field, gas['position_m'], random.Random(7 + self.sequence)),
                      'uncertainty_ppm': 1.5, 'species': 'synthetic-test-gas'}
        samples.append(gas)
        audio = sample('audio-1', 'audio', to_world([-6.6, -.1, 1.2]), 'generated-880hz-tone-v1')
        audio['audio'] = {'sample_rate_hz': 8000, 'channels': 1, 'duration_ns': 250_000_000}
        attach(audio, audio_wav(7, self.sequence, alarm), 'audio/wav')
        samples.append(audio)
        return {'run_id': rt.run_id, 'now_ns': sample_ns, 'frame': self.frame,
                'samples': samples, 'media': media}

    def close(self):
        self.renderer.close()


def display_result(request, result, *, node=None, catalog=(), records=()):
    """Convert inferred World coordinates once at the presentation boundary."""
    hazard = result.get('hazard') if result.get('state') == 'hazard' else None
    spatial = result.get('spatial') or {}
    camera_features = {f['sensor_id']: f for f in spatial.get('cameras', [])}
    target = to_scene(hazard['position_m']) if hazard else None
    observations = []
    for item in request['samples']:
        kind, sensor_id = item['kind'], item['sensor_id']
        observation = {'id': sensor_id, 'kind': kind, 'label': sensor_id,
                       'position': to_scene(item['position_m']), 'active': True}
        if kind == 'camera':
            feature = camera_features.get(sensor_id, {})
            observation.update(label='Camera A' if sensor_id == 'camera-a' else 'Camera B',
                evidence_path='fusion/' + sensor_id + '.png')
            if 'red_fraction' in feature:
                observation.update(value=100 * feature['red_fraction'], unit='% red')
            if target is not None:
                observation['target'] = target
        elif kind == 'lidar':
            observation.update(label=sensor_id.removeprefix('lidar-').title() + ' LiDAR',
                               value=len(item['lidar']['points_m']), unit='returns')
        elif kind == 'gas':
            observation.update(label='Gas · simulated', value=item['gas']['value_ppm'], unit='ppm')
        else:
            observation.update(label='Audio · generated, 880 Hz probe')
            if hazard:
                observation.update(value=100 * hazard['audio']['tone_fraction'], unit='% tone')
        observations.append(observation)
    view = {'state': result.get('state', 'insufficient_evidence'),
            'label': 'Localized warning' if hazard else 'Observing the office',
            'reason': result.get('reason') or result.get('error') or 'Waiting for sensor evidence',
            'sample_time_s': request['now_ns'] / 1_000_000_000,
            'observations': observations}
    if hazard:
        view.update(position=target, uncertainty_m=hazard['uncertainty_m'],
                    confidence=hazard['confidence'],
                    basis='Camera + LiDAR location; simulated gas and audio corroboration')
        if hazard.get('valid_until'):
            deadline = datetime.fromisoformat(hazard['valid_until'].replace('Z', '+00:00'))
            view['valid_until_s'] = (deadline - datetime(2026, 1, 1, tzinfo=timezone.utc)).total_seconds()
    if node is not None:
        by_id = {s['sensor_id']: s for s in catalog}
        samples = {s['sensor_id']: s for s in request['samples']}
        retained = {r['key']['source']['service'] for r in records}
        for observation in observations:
            sensor_id = observation['id']
            owner = by_id[sensor_id]['owner']
            observation.update(source_node=owner, delivery='local' if owner == node else 'received',
                               age_s=max(0, (request['now_ns'] - samples[sensor_id]['sample_ns']) / 1e9))
            if observation.get('evidence_path'):
                observation['evidence_path'] = f'fusion/{node}/{sensor_id}.png'
        view['replica_node'] = node
        view['missing_observations'] = [
            {'id': s['sensor_id'], 'kind': s['kind'], 'label': sensor_label(s['sensor_id']),
             'source_node': s['owner'], 'reason': 'expired' if s['sensor_id'] in retained else 'not_received'}
            for s in catalog if s['sensor_id'] not in samples]
    return view


def sensor_label(sensor_id):
    if sensor_id.startswith('lidar-'):
        return sensor_id.removeprefix('lidar-').title() + ' LiDAR'
    return {'camera-a': 'Camera A', 'camera-b': 'Camera B',
            'gas-1': 'Gas · simulated', 'audio-1': 'Audio · generated, 880 Hz probe'}[sensor_id]


def sensor_catalog(nodes):
    return [{'sensor_id': sid, 'kind': kind, 'owner': 'hub'} for sid, kind in
            [('camera-a', 'camera'), ('camera-b', 'camera'), ('gas-1', 'gas'), ('audio-1', 'audio')]] + [
            {'sensor_id': 'lidar-' + node, 'kind': 'lidar', 'owner': node} for node in nodes if node != 'hub']


class OfficeFusion:
    """Independent durable workers exchange observations over the modeled links."""
    def __init__(self, runtime, binary, directory):
        from sim.observation_mesh import ObservationMesh
        self.runtime, self.directory = runtime, Path(directory)
        self.evidence = self.directory / 'fusion'
        self.evidence.mkdir(mode=0o700)
        self.sensors = OfficeSensors(runtime)
        self.nodes = list(runtime.mesh.nodes)
        self.catalog = sensor_catalog(self.nodes)
        self.owners = {s['sensor_id']: s['owner'] for s in self.catalog}
        self.transport = ObservationMesh(self.nodes, self.catalog)
        self.pipes, self.latest = {}, {}
        self.pending_local = {n: {} for n in self.nodes}
        self.pending_received = {n: {} for n in self.nodes}
        self.pending_media = {n: {} for n in self.nodes}
        self.last_exchange = -math.inf
        try:
            participants = self.directory / 'participants'
            participants.mkdir(mode=0o700)
            for node in self.nodes:
                (self.evidence / node).mkdir(mode=0o700)
                state_dir = participants / node
                state_dir.mkdir(mode=0o700)
                config = state_dir / 'config.json'
                config.write_text(json.dumps({'version': 1, 'participant_id': node,
                    'run_id': runtime.run_id, 'frame': self.sensors.frame, 'sensors': self.catalog}))
                self.pipes[node] = FusionPipe(binary, config=config, state_dir=state_dir)
        except BaseException:
            for pipe in self.pipes.values():
                pipe.close()
            self.sensors.close()
            raise

    def tick(self):
        for node, pipe in self.pipes.items():
            for request, response in pipe.poll():
                if response.get('participant_id') != node or response.get('now_ns') != request['now_ns']:
                    pipe.error = 'Participant response identity or simulation time differs'
                    continue
                records, media = response.get('replica_records', []), response.get('media', {})
                self.transport.update_export(node, records, media)
                available = {'now_ns': response['now_ns'], 'samples': response.get('observations', [])}
                result = response['result']
                view = display_result(available, result, node=node, catalog=self.catalog, records=records)
                directory = self.evidence / node
                for observation in view['observations']:
                    if observation['kind'] != 'camera':
                        continue
                    sample = next(s for s in available['samples'] if s['sensor_id'] == observation['id'])
                    digest = sample['evidence']['sha256']
                    if digest in media:
                        replace_bytes(directory / (observation['id'] + '.png'),
                                      base64.b64decode(media[digest], validate=True))
                    else:
                        observation.pop('evidence_path', None)
                for filename, value in [('latest-input.json', request), ('latest-result.json', response)]:
                    replace_bytes(directory / filename, json.dumps(value, allow_nan=False).encode())
                self.latest[node] = view
        if self.sensors.ready():
            capture = self.sensors.capture()
            for sample in capture['samples']:
                owner = self.owners[sample['sensor_id']]
                self.pending_local[owner][sample['sensor_id']] = sample
                if sample.get('evidence'):
                    digest = sample['evidence']['sha256']
                    self.pending_media[owner][digest] = capture['media'][digest]
        now = float(self.runtime.data.time)
        if now - self.last_exchange >= .2:
            self.last_exchange = now
            for node, chunk in self.transport.round(self.runtime.mesh).items():
                for record in chunk['received']:
                    sensor = record['key']['source']['service']
                    old = self.pending_received[node].get(sensor)
                    version = lambda r: (r['version']['epoch'], r['version']['sequence'], r['timing'].get('age_hint_ns', 0))
                    if old is None or version(record) >= version(old):
                        self.pending_received[node][sensor] = record
                self.pending_media[node].update(chunk['media'])
            for node, pipe in self.pipes.items():
                local = list(self.pending_local[node].values())
                received = list(self.pending_received[node].values())
                references = {s['evidence']['sha256'] for s in local if s.get('evidence')}
                references.update(r['payload']['evidence']['sha256'] for r in received if r.get('payload', {}).get('evidence'))
                media = {d: b for d, b in self.pending_media[node].items() if d in references}
                request = {'now_ns': round(now * 1e9), 'local_samples': local,
                           'received': received, 'media': media}
                if pipe.submit(request):
                    self.pending_local[node].clear()
                    self.pending_received[node].clear()
                    self.pending_media[node].clear()
                else:
                    self.pending_media[node] = media

    def snapshot(self, node='hub'):
        if node not in self.pipes:
            raise ValueError('Unknown fusion participant')
        view = copy.deepcopy(self.latest[node]) if node in self.latest else {
            'state': 'insufficient_evidence', 'label': 'Observing the office',
            'reason': 'Waiting for this participant\'s sensor observations',
            'sample_time_s': float(self.runtime.data.time), 'observations': [], 'replica_node': node,
            'missing_observations': [{'id': s['sensor_id'], 'kind': s['kind'],
                'label': sensor_label(s['sensor_id']), 'source_node': s['owner'], 'reason': 'not_received'}
                for s in self.catalog]}
        now = float(self.runtime.data.time)
        elapsed = max(0, now - view['sample_time_s'])
        for observation in view['observations']:
            observation['age_s'] += elapsed
        expired = [o for o in view['observations'] if o['age_s'] >= 2]
        if expired:
            view['observations'] = [o for o in view['observations'] if o['age_s'] < 2]
            missing = {o['id']: o for o in view['missing_observations']}
            for observation in expired:
                missing[observation['id']] = {'id': observation['id'], 'kind': observation['kind'],
                    'label': observation['label'], 'source_node': observation['source_node'], 'reason': 'expired'}
            view['missing_observations'] = [missing[s['sensor_id']] for s in self.catalog if s['sensor_id'] in missing]
        inference_expired = view.get('valid_until_s') is not None and now >= view['valid_until_s']
        if self.pipes[node].error or elapsed > 2 or inference_expired:
            view.update(state='insufficient_evidence', label='Observations unavailable',
                        reason=self.pipes[node].error or ('Waiting for fresh evidence for this location'
                            if inference_expired else 'Waiting for this participant\'s next snapshot'))
            for name in ('position', 'uncertainty_m', 'confidence', 'basis', 'valid_until_s'):
                view.pop(name, None)
            for observation in view['observations']:
                observation.pop('target', None)
                if self.pipes[node].error or elapsed > 2:
                    observation['active'] = False
                    observation.pop('evidence_path', None)
        return view

    def close(self):
        for pipe in self.pipes.values():
            pipe.close()
        self.sensors.close()
