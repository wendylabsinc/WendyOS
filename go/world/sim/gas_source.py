"""MuJoCo sensor placements + synthetic analytic gas -> independent Go estimator.

MuJoCo supplies geometry and images, NOT fluid dynamics. Source truth and the
generator's wind remain outside estimator requests. The directional Gaussian
is an explicitly designed fixture, not a validated physical dispersion model.
"""
from __future__ import annotations

import argparse
import copy
import hashlib
import json
import math
import os
from pathlib import Path
import random
import select
import signal
import subprocess
import struct
import time
import uuid
import zlib
import xml.etree.ElementTree as ET

def png(rgb):
    height, width, channels = rgb.shape
    if channels != 3 or width != 960 or height != 600:
        raise ValueError('invalid rendered dimensions')
    def chunk(kind, raw):
        return struct.pack('>I', len(raw))+kind+raw+struct.pack('>I', zlib.crc32(kind+raw)&0xffffffff)
    raw = b''.join(b'\0'+row.tobytes() for row in rgb)
    return b'\x89PNG\r\n\x1a\n'+chunk(b'IHDR', struct.pack('>IIBBBBB', width, height, 8, 2, 0, 0, 0))+chunk(b'IDAT', zlib.compress(raw))+chunk(b'IEND', b'')


def atomic(path, value):
    raw = json.dumps(value, allow_nan=False, separators=(',', ':')).encode()
    temp = path.with_name(path.name + '.tmp')
    with temp.open('wb') as stream:
        stream.write(raw)
    os.replace(temp, path)


def scene_xml():
    root = ET.Element('mujoco', model='gas-source-laboratory-v1')
    ET.SubElement(root, 'option', timestep='.01', gravity='0 0 -9.81')
    visual = ET.SubElement(root, 'visual')
    ET.SubElement(visual, 'global', offwidth='960', offheight='600')
    ET.SubElement(visual, 'headlight', ambient='.35 .35 .35', diffuse='.6 .6 .6')
    ET.SubElement(visual, 'quality', offsamples='4', shadowsize='2048')
    assets = ET.SubElement(root, 'asset')
    for name, rgba in [('floor', '.86 .87 .84 1'), ('wall', '.94 .94 .9 1'),
                       ('metal', '.19 .27 .3 1'), ('teal', '.07 .58 .51 1')]:
        ET.SubElement(assets, 'material', name=name, rgba=rgba)
    world = ET.SubElement(root, 'worldbody')
    ET.SubElement(world, 'light', pos='1 -3 7', dir='0 0 -1', diffuse='.9 .87 .82')
    ET.SubElement(world, 'geom', type='plane', size='4 3 .1', material='floor')
    ET.SubElement(world, 'geom', type='box', pos='0 2.8 1.2', size='3.4 .08 1.2', material='wall')
    ET.SubElement(world, 'geom', type='box', pos='-3.3 .3 .7', size='.08 2.4 .7', material='wall')
    center = ET.SubElement(world, 'body', name='center')
    ET.SubElement(center, 'geom', type='sphere', size='.001', rgba='0 0 0 0', contype='0', conaffinity='0')
    ET.SubElement(world, 'camera', name='overview', pos='6 -7 7', mode='targetbody', target='center', fovy='45')
    for row in range(2):
        for col in range(4):
            name = chr(ord('a') + row*4+col)
            x, z = -2 + col*4/3, -1.6+row*3.2
            body = ET.SubElement(world, 'body', name='gas-'+name, pos=f'{x} {-z} 0')
            ET.SubElement(body, 'geom', type='cylinder', size='.10 .32', pos='0 0 .32', material='metal')
            ET.SubElement(body, 'geom', name='lamp-'+name, type='box', size='.16 .13 .09', pos='0 0 .72', material='teal')
            ET.SubElement(body, 'site', name=name, pos='0 0 .75', size='.03', rgba='0 0 0 0')
    source = ET.SubElement(world, 'body', name='source', mocap='true', pos='-.8 .4 .16')
    ET.SubElement(source, 'geom', name='source-marker', type='sphere', size='.16', rgba='.95 .4 .1 0', contype='0', conaffinity='0')
    return ET.tostring(root, encoding='unicode')


class GasDemo:
    def __init__(self, out, executable, seed=17):
        import mujoco
        self.mj = mujoco
        self.out = Path(out)
        if self.out.exists() or self.out.is_symlink():
            raise ValueError('output must be a fresh private directory')
        os.umask(0o077)
        self.out.mkdir(parents=True, mode=0o700)
        for name in ('commands', 'receipts', 'captures', 'estimator-input', 'ground-truth'):
            (self.out/name).mkdir(mode=0o700)
        self.xml = scene_xml()
        (self.out/'scene.xml').write_text(self.xml)
        self.model = mujoco.MjModel.from_xml_string(self.xml)
        self.data = mujoco.MjData(self.model)
        mujoco.mj_forward(self.model, self.data)
        self.renderer = mujoco.Renderer(self.model, height=600, width=960)
        self.run = 'gas-'+uuid.uuid4().hex[:12]
        digest = hashlib.sha256(self.xml.encode()).hexdigest()
        simulation = dict(run_id=self.run, scenario_version='gas-laboratory-v1', seed=seed, clock_domain='simulation:'+self.run)
        sensors = []
        for name in 'abcdefgh':
            sid = mujoco.mj_name2id(self.model, mujoco.mjtObj.mjOBJ_SITE, name)
            p = self.data.site_xpos[sid]
            sensors.append(dict(id=name, source=dict(device='gas-gateway-'+name, app='gas', service=name, stream='ppm'),
                                position_xz=[float(p[0]), float(-p[1])], position_uncertainty_m=0, calibration='calibration-1', sensor_type='analytic-scalar-v1'))
        self.config = dict(site='synthetic-gas-laboratory', frame=dict(id='laboratory-floor', revision=digest[:16]),
                           simulation=simulation, sensors=sensors, min_xz=[-2, -2], max_xz=[2, 2], step_m=.1, background_ppm=2)
        atomic(self.out/'catalog.json', self.config)
        self.assumptions = dict(revision='declared-1', wind_radians=.25, downwind_m=3, upwind_m=1, crosswind_m=1.3)
        self.true_wind = .25
        self.strength = 120
        self.source = [-.8, -.4]
        self.enabled = {name: True for name in 'abcdefgh'}
        self.bias = self.partition = self.duplicates = self.reveal = False
        self.rng = random.Random(seed)
        self.sequence = self.command_sequence = 0
        self.last = []
        self.traces = {name: [] for name in 'abcdefgh'}
        self.events = []
        self.log = (self.out/'estimator.log').open('w')
        self.process = subprocess.Popen([str(executable)], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                        stderr=self.log, text=True, bufsize=1)
        self.result = None
        self.request = None
        self.started = time.monotonic()

    def call(self, req):
        self.process.stdin.write(json.dumps(req, allow_nan=False, separators=(',', ':'))+'\n')
        self.process.stdin.flush()
        if not select.select([self.process.stdout], [], [], 5)[0]:
            raise RuntimeError('gas estimator did not respond within five seconds')
        result = json.loads(self.process.stdout.readline())
        if 'error' in result:
            raise RuntimeError(result['error'])
        return result

    def sample(self, sensor):
        # An independent implementation of the declared fixture, using actual
        # MuJoCo site/source positions. Neither truth coordinate enters Go.
        bid = self.mj.mj_name2id(self.model, self.mj.mjtObj.mjOBJ_BODY, 'source')
        truth = self.data.xpos[bid]
        dx, dz = sensor['position_xz'][0]-float(truth[0]), sensor['position_xz'][1]+float(truth[1])
        along = dx*math.cos(self.true_wind)+dz*math.sin(self.true_wind)
        cross = -dx*math.sin(self.true_wind)+dz*math.cos(self.true_wind)
        length = 3 if along >= 0 else 1
        value = 2+self.strength*math.exp(-.5*((along/length)**2+(cross/1.3)**2))+self.rng.gauss(0, .25)
        if self.bias and sensor['id'] == 'c':
            value += 35
        now = round(float(self.data.time), 6)
        return dict(id=f"{sensor['id']}/{self.sequence}", sensor=sensor['id'], source=sensor['source'],
                    version=dict(epoch=1, sequence=self.sequence), site=self.config['site'], frame=self.config['frame'],
                    provenance=dict(origin='simulated', app_version='gas-generator-v1', simulation=self.config['simulation']),
                    calibration=sensor['calibration'], sensor_type=sensor['sensor_type'], units='ppm', species='synthetic-tracer',
                    captured_s=now, valid_until_s=now+1.5, value_ppm=max(0, value), uncertainty_ppm=1)

    def step(self, archive=False):
        self.sequence += 1
        self.data.mocap_pos[0] = [self.source[0], -self.source[1], .16]
        for _ in range(20):
            self.mj.mj_step(self.model, self.data)
        self.mj.mj_forward(self.model, self.data)
        generated = [self.sample(sensor) for sensor in self.config['sensors']]
        if self.partition:
            delivered = []
        elif self.duplicates:
            delivered = copy.deepcopy(self.last)
        else:
            delivered = [o for o in generated if self.enabled[o['sensor']]]
            self.last = copy.deepcopy(delivered)
        req = dict(now_s=round(float(self.data.time), 6), assumptions=self.assumptions, observations=delivered)
        if self.sequence == 1:
            req['config'] = self.config
        self.request = copy.deepcopy(req)
        self.result = self.call(req)
        for reading in self.result['readings']:
            observation = reading.get('observation')
            if reading['state'] == 'live' and observation:
                series = self.traces[reading['sensor']['id']]
                if not series or series[-1][0] != observation['captured_s']:
                    series.append([observation['captured_s'], observation['value_ppm']])
                del series[:-60]
        for name in 'abcdefgh':
            gid = self.mj.mj_name2id(self.model, self.mj.mjtObj.mjOBJ_GEOM, 'lamp-'+name)
            self.model.geom_rgba[gid] = [.07, .58, .51, 1] if self.enabled[name] and not self.partition else [.4, .43, .45, 1]
        gid = self.mj.mj_name2id(self.model, self.mj.mjtObj.mjOBJ_GEOM, 'source-marker')
        self.model.geom_rgba[gid, 3] = 1 if self.reveal else 0
        self.renderer.update_scene(self.data, camera='overview')
        raw = png(self.renderer.render())
        image_name = 'scene-'+str(self.sequence % 2)+'.png'
        image_path = self.out/image_name
        temp = image_path.with_suffix('.tmp')
        temp.write_bytes(raw)
        os.replace(temp, image_path)
        snapshot = dict(schema='wendy.gas-demo.v1', run_id=self.run, sequence=self.sequence,
                        last_command_sequence=self.command_sequence, result=self.result, traces=self.traces,
                        image=image_name, image_sha256=hashlib.sha256(raw).hexdigest(), enabled=self.enabled,
                        partition=self.partition, duplicates=self.duplicates, bias=self.bias, reveal=self.reveal, releasing=self.strength > 0,
                        events=self.events[-12:], model_assumption='Designed directional Gaussian; no fluid simulation or physical accuracy claim')
        if self.reveal:
            snapshot['evaluation_truth'] = dict(position_xz=self.source, wind_radians=self.true_wind)
        atomic(self.out/'snapshot.json', snapshot)
        if archive:
            self.archive(snapshot, raw)
        return snapshot

    def archive(self, snapshot=None, raw=None):
        key = f'{self.sequence:05d}'
        atomic(self.out/'estimator-input'/f'{key}.json', self.request)
        atomic(self.out/'ground-truth'/f'{key}.json', dict(evaluation_only=True, position_xz=self.source, wind_radians=self.true_wind))
        atomic(self.out/'captures'/f'{key}.json', snapshot or json.loads((self.out/'snapshot.json').read_text()))
        if raw is not None:
            (self.out/'captures'/f'{key}.png').write_bytes(raw)

    def command(self, action, target=''):
        if action == 'move_source':
            self.source = [.6, .4] if self.source[0] < 0 else [-.8, -.4]
        elif action == 'wind_change':
            self.true_wind = 1.5 if self.true_wind < 1 else .25
        elif action == 'release':
            self.strength = 0 if self.strength else 120
        elif action == 'adopt_wind':
            self.assumptions = dict(self.assumptions, revision=f'declared-{self.sequence+1}', wind_radians=self.true_wind)
        elif action == 'toggle_sensor' and target in self.enabled:
            self.enabled[target] = not self.enabled[target]
        elif action == 'poor_geometry':
            self.enabled = {name: name in 'abcd' for name in self.enabled}
        elif action == 'partition':
            self.partition = not self.partition
        elif action == 'duplicates':
            self.duplicates = not self.duplicates
        elif action == 'bias':
            self.bias = not self.bias
        elif action == 'reveal':
            self.reveal = not self.reveal
        elif action == 'reset':
            self.strength = 120
            self.source = [-.8, -.4]
            self.true_wind = .25
            self.assumptions = dict(self.assumptions, revision=f'declared-{self.sequence+1}', wind_radians=.25)
            self.enabled = {name: True for name in self.enabled}
            self.bias = self.partition = self.duplicates = self.reveal = False
        else:
            raise ValueError('unknown control')
        self.events.append(dict(time_s=round(float(self.data.time), 2), action=action, target=target))
        del self.events[:-12]

    def poll_commands(self):
        # Exact monotonically increasing names avoid arbitrary paths and backlog.
        if self.command_sequence >= 4096:
            return False
        seq = self.command_sequence+1
        path = self.out/'commands'/f'{seq:06d}.json'
        if not path.is_file() or path.is_symlink():
            return False
        accepted, error = False, ''
        try:
            if path.stat().st_size > 4096:
                raise ValueError('command too large')
            req = json.loads(path.read_text())
            if set(req) != {'run_id', 'sequence', 'action', 'target'} or req['run_id'] != self.run or req['sequence'] != seq:
                raise ValueError('command scope mismatch')
            self.command(req['action'], req['target'])
            accepted = True
        except (ValueError, TypeError, KeyError) as exc:
            error = str(exc)
        self.command_sequence = seq
        atomic(self.out/'receipts'/f'{seq:06d}.json', dict(sequence=seq, accepted=accepted, error=error))
        return True

    def close(self):
        if self.process.poll() is None:
            self.process.stdin.close()
            try:
                self.process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                self.process.terminate()
                self.process.wait(timeout=3)
        self.renderer.close()
        self.log.close()


def check(out, executable):
    demo = GasDemo(out, executable, seed=811)
    checks = []
    try:
        def capture(label, expected, count=1):
            for _ in range(count):
                snap = demo.step(archive=True)
            result = snap['result']
            assert result['state'] in expected, (label, result['state'], result['reason'])
            error = None
            if result['state'] == 'localized':
                p = result['estimate']['position_xz']
                error = math.dist(p, demo.source)
                assert error <= .2, (label, error)
            # Truth must never appear in the exact executable request.
            assert not any(k in json.dumps(demo.request) for k in ('evaluation_truth', 'true_wind', 'source_position'))
            checks.append(dict(label=label, state=result['state'], coverage=result['coverage'], error_m=error, capture=demo.sequence))
        capture('eight-sensor source estimate', ['localized'])
        demo.command('move_source');capture('source moves', ['localized'], 2)
        demo.command('wind_change');capture('unmodelled wind change', ['unavailable'], 2)
        demo.command('adopt_wind');capture('declared airflow updated', ['localized'], 2)
        demo.command('bias');capture('sensor bias violates model', ['unavailable'], 2)
        demo.command('reset');demo.command('release');capture('background is not evidence of clearance', ['unavailable'], 2)
        demo.command('reset');capture('recovered samples', ['localized'], 2)
        demo.command('poor_geometry');capture('collinear remaining sensors', ['ambiguous', 'unavailable'], 3)
        demo.command('toggle_sensor', 'a');capture('three sensors insufficient', ['unavailable'], 3)
        demo.command('reset');capture('all sensors restored', ['localized'], 2)
        deadline = demo.result['estimate']['valid_until_s']
        demo.command('duplicates');capture('duplicates expire without renewal', ['unavailable'], 9)
        assert demo.result['now_s'] >= deadline and demo.result['coverage'] == 0
        demo.command('reset');capture('fresh input after duplicate stall', ['localized'], 2)
        demo.command('partition');capture('partition expires estimate', ['unavailable'], 9)
        demo.command('partition');capture('reconnected fresh readings', ['localized'], 2)
        heldout = []
        for seed in (31, 4099, 12289):
            trial = GasDemo(demo.out/f'heldout-{seed}', executable, seed=seed)
            try:
                for position in ((-.91, -.53), (.35, .65), (-.15, .23)):
                    trial.source = list(position)
                    snap = trial.step(archive=True)
                    result = snap['result']
                    assert result['state'] == 'localized', (seed, position, result['reason'])
                    error = math.dist(result['estimate']['position_xz'], position)
                    assert error <= .2, (seed, position, error)
                    heldout.append(dict(seed=seed, source_xz=position, error_m=error, residual=result['estimate']['normalized_rms']))
            finally:
                trial.close()
        report = dict(passed=True, mujoco_version=demo.mj.__version__, captures=demo.sequence, checks=checks, heldout=heldout,
                      limitations=['Synthetic concentration kernel, not fluid dynamics', 'No calibrated confidence or hardware accuracy claim'])
        atomic(demo.out/'checks.json', report)
        print(json.dumps(report, indent=2))
    finally:
        demo.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--estimator', required=True, type=Path)
    parser.add_argument('--out', required=True, type=Path)
    parser.add_argument('--duration', type=float, default=180)
    parser.add_argument('--check', action='store_true')
    args = parser.parse_args()
    if not 1 <= args.duration <= 3600 or not args.estimator.is_absolute() or not args.out.is_absolute():
        parser.error('absolute paths and a 1–3600 second duration are required')
    if args.check:
        check(args.out, args.estimator)
        return
    demo = GasDemo(args.out, args.estimator)
    stopped = [False]
    def stop(_signum, _frame):
        stopped[0] = True
    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    try:
        while not stopped[0] and time.monotonic()-demo.started < args.duration:
            started = time.monotonic()
            changed = demo.poll_commands()
            demo.step(archive=changed or demo.sequence == 0)
            time.sleep(max(0, .2-(time.monotonic()-started)))
    finally:
        demo.close()


if __name__ == '__main__':
    main()
