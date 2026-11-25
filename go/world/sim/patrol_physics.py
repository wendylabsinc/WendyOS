"""Private full-office Go2 physics service. Navigation never receives world pose.

MuJoCo and the pinned ONNX policy own all motion. Only initialization assigns a
floating-base pose. Leg-contact kinematics and gyro supply drifting odometry.
Gas and audio remain explicitly synthetic fixture models, not fluid/acoustics.
"""
from __future__ import annotations
import argparse
import base64
import copy
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from concurrent.futures import ThreadPoolExecutor
import importlib
import json
import math
from pathlib import Path
import random
import sys
import threading
import time
import uuid
import xml.etree.ElementTree as ET

import mujoco as mj
import numpy as np
from sim.sensors import audio_wav, calibration, png_rgb
from sim.run import gas_value
from sim.patrol_work import WorkMeter
from sim.patrol_environment import EnvironmentMeter
from sim.patrol_world_field import WorldField

SOURCE_PINS = {
    'simulation.py': '02ff85a2e26ca434efeeb09c3dba1c2207f53875dfe2dd816f3cf868ac8904bc',
    'policy.py': '8e5d80e63ffe5548b216f510ed3a39f713bce8b87fc11af442f5c106779c21c8',
}
LIDAR = [.28, 0., .08]
CAMERA = [.4, 0., .04]
INITIAL = [-6., 0., .34, 1., 0., 0., 0.]


def load_controller(source, assets):
    source, assets = Path(source).resolve(), Path(assets).resolve()
    if mj.__version__ != '4.0.0':
        raise ValueError('patrol requires MuJoCo 4.0.0')
    for name, digest in SOURCE_PINS.items():
        if hashlib.sha256((source / 'go2_sim' / name).read_bytes()).hexdigest() != digest:
            raise ValueError('pinned controller differs: ' + name)
    lock = json.loads((source / 'assets.lock.json').read_text())
    for item in lock['files']:
        raw = (assets / item['path']).read_bytes()
        if len(raw) != item['size'] or hashlib.sha256(raw).hexdigest() != item['sha256']:
            raise ValueError('pinned asset differs: ' + item['path'])
    import onnxruntime
    if onnxruntime.__version__ != '1.23.2':
        raise ValueError('patrol requires pinned ONNX Runtime 1.23.2')
    sys.path.insert(0, str(source))
    module = importlib.import_module('go2_sim.simulation')
    if Path(module.__file__).resolve() != source / 'go2_sim/simulation.py':
        raise ValueError('controller imported from wrong directory')
    return module.Simulation


def compose_office(office, assets):
    """Retain the entire office hierarchy and its contact geometry, minus car."""
    scene = ET.parse(office).getroot()
    robot = ET.parse(Path(assets) / 'robot/go2.xml').getroot()
    # Office uses degrees; the unchanged pinned robot uses radians.
    for node in scene.iter():
        for attr in ('euler', 'axisangle'):
            if attr in node.attrib:
                values = [float(v) for v in node.get(attr).split()]
                values = [math.radians(v) for v in values] if attr == 'euler' else values[:3] + [math.radians(values[3])]
                node.set(attr, ' '.join(map(str, values)))
    scene.find('compiler').set('angle', 'radian')
    scene.find('compiler').set('meshdir', str(Path(assets).resolve() / 'robot/assets'))
    scene.find('option').set('timestep', '.002')
    for name in ('inspection-puck', 'robot-atlas', 'robot-iris', 'robot-echo'):
        car = scene.find(f"worldbody/body[@name='{name}']")
        if car is not None:
            scene.find('worldbody').remove(car)
    scene.remove(scene.find('actuator'))
    # Named materials overlap, but their appearance and contacts must not change.
    for node in robot.iter():
        if node.tag == 'material' and 'name' in node.attrib:
            node.set('name', 'go2-' + node.get('name'))
        if 'material' in node.attrib:
            node.set('material', 'go2-' + node.get('material'))
    base = robot.find("worldbody/body[@name='base']")
    for geom in base.iter('geom'):
        geom.set('group', '2' if geom.get('class') == 'visual' else '3')
    for name, pos in (('lidar', LIDAR), ('imu', [0., 0., 0.]), ('gas', [0., 0., .12]), ('audio', [0., 0., .12])):
        ET.SubElement(base, 'site', name=name, pos=' '.join(map(str, pos)), size='.003', rgba='0 0 0 0')
    ET.SubElement(base, 'camera', name='camera-b', pos=' '.join(map(str, CAMERA)), xyaxes='0 -1 0 0 0 1', fovy='60')
    sensor = ET.SubElement(scene, 'sensor')
    ET.SubElement(sensor, 'gyro', name='patrol-gyro', site='imu')
    ET.SubElement(sensor, 'accelerometer', name='patrol-acceleration', site='imu')
    for tag in ('default', 'contact', 'actuator', 'keyframe'):
        node = robot.find(tag)
        if node is not None:
            scene.append(node)
    scene.find('asset').extend(list(robot.find('asset')))
    scene.find('worldbody').extend(list(robot.find('worldbody')))
    obstacle = ET.SubElement(scene.find('worldbody'), 'body', name='patrol-obstacle', mocap='true', pos='0 0 -5')
    ET.SubElement(obstacle, 'geom', name='patrol-obstacle-collision', type='box', size='.35 .35 .5', rgba='.85 .3 .12 1', group='0')
    # Non-colliding commissioned gas sites and an analytic-source marker. They
    # add no actuator, force, contact geometry or navigation observation.
    for i in range(8):
        x,z=-7+2*(i%4),-2+4*(i//4)
        ET.SubElement(scene.find('worldbody'),'site',name='world-gas-'+str(i),pos=f'{x} {-z} .75',size='.003',rgba='0 0 0 0')
    marker=ET.SubElement(scene.find('worldbody'),'body',name='patrol-world-source',mocap='true',pos='-6 -1.6 .5')
    ET.SubElement(marker,'site',name='patrol-world-source-marker',size='.003',rgba='0 0 0 0')
    return ET.tostring(scene, encoding='unicode')


def command_values(body):
    values = [body.get(k, 0.) for k in ('vx', 'vy', 'wz')]
    if any(isinstance(v, bool) or not isinstance(v, (int, float)) or not math.isfinite(v) for v in values):
        raise ValueError('command must be finite')
    vx, vy, wz = values
    if (vx != 0 and not .5 <= vx <= .55) or vy != 0 or abs(wz) > .7:
        raise ValueError('supported profile: vx=0 or .5..55 m/s; vy=0; |wz|<=.7 rad/s')
    return values


class LegOdometry:
    """Integrate stance-foot joint kinematics plus gyro, with no base pose input."""
    def __init__(self):
        self.x = self.y = self.yaw = 0.
        self.vx = self.vy = self.wz = 0.
        self.valid = False
        self.scale = 1.005
        self.gyro_bias = .0002

    def update(self, velocity, gyro, dt):
        self.valid = velocity is not None
        if velocity is None:
            self.vx = self.vy = self.wz = 0.
            return
        self.vx, self.vy = float(velocity[0]) * self.scale, float(velocity[1]) * self.scale
        self.wz = float(gyro[2]) + self.gyro_bias
        yaw = self.yaw + .5 * self.wz * dt
        self.x += (math.cos(yaw) * self.vx - math.sin(yaw) * self.vy) * dt
        self.y += (math.sin(yaw) * self.vx + math.cos(yaw) * self.vy) * dt
        self.yaw = math.atan2(math.sin(self.yaw + self.wz * dt), math.cos(self.yaw + self.wz * dt))

    def snapshot(self):
        return {k: getattr(self, k) for k in ('x', 'y', 'yaw', 'vx', 'vy', 'wz', 'valid')}


class IMUAttitude:
    """Roll/pitch from simulated accelerometer and gyro, never world pose.

    The declared level initial estimate is corrected by gravity. The slow
    gravity correction is enabled only while measured motion and the applied
    command are near zero, and rejects freefall/large acceleration magnitudes;
    gyro integration preserves the faster gait attitude changes.
    """
    def __init__(self):
        self.roll = self.pitch = 0.
        self.acceleration = [0., 0., 0.]
        self.gyro = [0., 0., 0.]

    def update(self, acceleration, gyro, dt, gravity_correction=True):
        ax, ay, az = map(float, acceleration)
        gx, gy, gz = map(float, gyro)
        sr, cr = math.sin(self.roll), math.cos(self.roll)
        tp = math.tan(max(-1.4, min(1.4, self.pitch)))
        self.roll += (gx + sr * tp * gy + cr * tp * gz) * dt
        self.pitch += (cr * gy - sr * gz) * dt
        magnitude = math.sqrt(ax*ax + ay*ay + az*az)
        if gravity_correction and 8.5 <= magnitude <= 11.:
            roll = math.atan2(ay, az)
            pitch = math.atan2(-ax, math.hypot(ay, az))
            alpha = dt / (.8 + dt)
            self.roll += alpha * math.atan2(math.sin(roll-self.roll), math.cos(roll-self.roll))
            self.pitch += alpha * (pitch-self.pitch)
        self.acceleration, self.gyro = [ax, ay, az], [gx, gy, gz]

    def snapshot(self):
        return {'roll': self.roll, 'pitch': self.pitch,
                'acceleration_mps2': list(self.acceleration),
                'gyro_rps': list(self.gyro),
                'model': 'gyro integration with complementary gravity correction'}


class PatrolPhysics:
    def __init__(self, source, assets, office, run_id=None):
        controller = load_controller(source, assets)
        self.xml = compose_office(office, assets)
        self.digest = hashlib.sha256(self.xml.encode()).hexdigest()
        self.identity = {'engine': 'MuJoCo 4.0.0', 'onnxruntime': '1.23.2', 'policy_bundle': 'go2-moe-cts-164k-0.6715-v1', 'controller_sha256': SOURCE_PINS, 'office_sha256': hashlib.sha256(Path(office).read_bytes()).hexdigest(), 'assets_manifest_sha256': hashlib.sha256((Path(source) / 'assets.lock.json').read_bytes()).hexdigest(), 'navigation_coordinates': 'MuJoCo and ROS: X east, Y north, Z up, metres', 'world_coordinates': 'World: X east, Y up, Z south; scene [x,y,z] maps to [x,z,-y]', 'virtual_mounts_base_m': {'lidar': LIDAR, 'camera': CAMERA}, 'odometry': 'stance-foot joint Jacobians and gyro; 0.5% scale error, 0.0002 rad/s gyro bias'}
        self.model = mj.MjModel.from_xml_string(self.xml)
        self.sim = controller(asset_dir=assets, model=self.model)
        self.data = self.sim.data
        # Initial placement is the sole direct floating-base pose assignment.
        self.data.qpos[:7] = INITIAL
        mj.mj_forward(self.model, self.data)
        self.token = self.sim.arm()
        self.run_id = run_id or 'patrol-' + uuid.uuid4().hex[:12]
        self.work_meter = WorkMeter(self.run_id, self.digest)
        self.work_loss_until = 0.
        self.work_frozen = None
        self.environment_meter = EnvironmentMeter(self.run_id, self.digest, self.work_meter.sample["boot_id"])
        self.world_field = WorldField(mj,self.model,self.data,self.run_id,self.digest,self.work_meter.sample["boot_id"])
        self.environment_loss_until = 0.
        self.environment_frozen = None
        self.environment_pulse_until = 0.
        self.environment_pulse_ppm = 0.
        self.lock = threading.RLock()
        self.done = threading.Event()
        self.odom = LegOdometry()
        self.imu = IMUAttitude()
        self.scan_odom = None
        self.scan_imu = None
        self.sequence = 0
        self.renderer = None
        self.capture_worker = ThreadPoolExecutor(max_workers=1, thread_name_prefix='camera')
        self.lidar_loss_until = 0.
        self.scan = None
        self.scan_points = []
        self.scan_origin = None
        self.last_scan_s = -1.
        self.last_command_wall = 0.
        self.foot_ids = [int(self.model.body_geomadr[self.model.body(leg + '_calf').id] + self.model.body_geomnum[self.model.body(leg + '_calf').id] - 1) for leg in ('FL', 'FR', 'RL', 'RR')]
        self.dynamic_ids = [i for i in range(self.model.ngeom) if self._robot_geom(i) or self.model.body_mocapid[self.model.geom_bodyid[i]] >= 0]
        self._map = self.occupancy_map()
        self._geometry = self.geometry()
        self.fault = None

    def _robot_geom(self, i):
        body = int(self.model.geom_bodyid[i])
        while body:
            if body == self.sim.body_id:
                return True
            body = int(self.model.body_parentid[body])
        return False

    def work(self):
        with self.lock:
            if self.data.time < self.work_loss_until:
                return dict(self.work_frozen)
            return self.work_meter.snapshot()

    def world_samples(self):
        with self.lock:
            return self.world_field.snapshot()

    def environment(self):
        with self.lock:
            if self.data.time < self.environment_loss_until:
                return copy.deepcopy(self.environment_frozen)
            return self.environment_meter.snapshot()

    def measure_environment(self, sequence):
        p = self.data.site('gas').xpos
        world_position = [float(p[0]), float(p[2]), -float(p[1])]
        field = {'source_local_m': [-7.3, .65, -.9], 'sigma_m': 1.6, 'background_ppm': 5., 'peak_ppm': 95., 'noise_stddev_ppm': 1.5}
        value = gas_value(field, world_position, random.Random(7 + sequence))
        # Acceptance fixture changes only the declared virtual gas field; no forces,
        # positions, navigation readings, clocks or controller thresholds change.
        if self.data.time < self.environment_pulse_until:
            value += self.environment_pulse_ppm
        return value, 1.5

    def command(self, body):
        values = command_values(body)
        with self.lock:
            if self.fault:
                raise ValueError('physics fault: ' + self.fault)
            self.sim.command_velocity(*values, token=self.token)
            self.last_command_wall = time.monotonic()
        return {'accepted': True, 'command': values}

    def _odometry(self):
        rotation = self.data.xmat[self.sim.body_id].reshape(3, 3)
        gyro = self.data.sensor('patrol-gyro').data.copy()
        contacts = set()
        for c in self.data.contact:
            contacts.update((int(c.geom1), int(c.geom2)))
        velocities = []
        for foot in self.foot_ids:
            if foot not in contacts:
                continue
            jp, jr = np.zeros((3, self.model.nv)), np.zeros((3, self.model.nv))
            mj.mj_jacGeom(self.model, self.data, jp, jr, foot)
            relative = rotation.T @ (self.data.geom_xpos[foot] - self.data.xpos[self.sim.body_id])
            joint_velocity = rotation.T @ (jp[:, self.sim.vaddr] @ self.data.qvel[self.sim.vaddr])
            velocities.append(-joint_velocity - np.cross(gyro, relative))
        velocity = np.median(velocities, axis=0) if velocities else None
        self.odom.update(velocity, gyro, float(self.model.opt.timestep))

    def step(self):
        self.sim.step()
        mj.mj_forward(self.model, self.data)
        self.work_meter.step(float(self.data.time), float(self.model.opt.timestep),
                             self.data.qfrc_actuator[self.sim.vaddr], self.data.qvel[self.sim.vaddr])
        self.environment_meter.step(float(self.data.time), self.measure_environment)
        self.world_field.step()
        gyro = self.data.sensor('patrol-gyro').data
        stationary = np.linalg.norm(self.sim.applied_command) < .01 and math.hypot(self.odom.vx, self.odom.vy) < .08 and np.linalg.norm(gyro) < .1
        self.imu.update(self.data.sensor('patrol-acceleration').data, gyro, float(self.model.opt.timestep), gravity_correction=stationary)
        self._odometry()
        if self.data.time - self.last_scan_s >= .099:
            self.raycast()

    def raycast(self):
        origin = self.data.site('lidar').xpos.copy()
        self.scan_origin = origin.tolist()
        rotation = self.data.site('lidar').xmat.reshape(3, 3)
        angles = np.linspace(-math.pi, math.pi, 360, endpoint=False)
        vectors = np.column_stack((np.cos(angles), np.sin(angles), np.zeros(360))) @ rotation.T
        distances, ids = np.empty(360), np.empty(360, dtype=np.int32)
        mj.mj_multiRay(self.model, self.data, origin, vectors.ravel(), np.array([1, 1, 0, 0, 0, 0], dtype=np.uint8), True, self.sim.body_id, ids, distances, None, 360, 12.)
        self.scan_points = [p.tolist() for p in origin + vectors[(distances >= .1) & (distances <= 12)] * distances[(distances >= .1) & (distances <= 12), None]]
        self.scan = {'ranges': np.where((distances >= .1) & (distances <= 12), distances, 12.).tolist(), 'angle_min': -math.pi, 'angle_increment': 2 * math.pi / 360, 'range_min': .1, 'range_max': 12., 'frame_id': 'laser'}
        self.last_scan_s = float(self.data.time)
        self.scan_odom = self.odom.snapshot()
        self.scan_imu = self.imu.snapshot()

    def sensors(self):
        with self.lock:
            available = self.data.time >= self.lidar_loss_until
            return {'stamp': self.last_scan_s, 'scan': copy.deepcopy(self.scan) if available else None, 'odom': copy.deepcopy(self.scan_odom), 'imu': copy.deepcopy(self.scan_imu), 'transforms': [{'parent': 'base_link', 'child': 'laser', 'translation': LIDAR, 'quaternion': [0., 0., 0., 1.]}], 'ready': self.data.time >= 3 and self.scan is not None and available and self.scan_odom is not None and self.scan_odom['valid'] and self.sim.mode in ('standing', 'moving') and not self.fault}

    def transform(self, i):
        return {'id': i, 'position': self.data.geom_xpos[i].tolist(), 'rotation': self.data.geom_xmat[i].tolist()}

    def state(self):
        with self.lock:
            value = self.sim.snapshot()
            value.update(actuator_work=self.work_meter.snapshot(), identity=self.identity, stamp=float(self.data.time), frame={'id': 'office-floor', 'revision': 'office-' + self.digest[:16]}, package_sha256=self.digest, run_id=self.run_id, fault=self.fault, simulation=True, dynamic_geoms=[self.transform(i) for i in self.dynamic_ids], lidar_points=copy.deepcopy(self.scan_points), foot_contacts=[any(i in (c.geom1, c.geom2) for c in self.data.contact) for i in self.foot_ids], model_sha256=self.digest, odometry=self.odom.snapshot())
            return value

    def geometry(self):
        geoms = []
        kinds = {int(getattr(mj.mjtGeom, 'mjGEOM_' + k.upper())): k for k in ('plane', 'sphere', 'capsule', 'ellipsoid', 'cylinder', 'box', 'mesh')}
        for i in range(self.model.ngeom):
            m = self.model
            mat = int(m.geom_matid[i])
            color = m.geom_rgba[i]
            if mat >= 0 and np.allclose(color, [.5, .5, .5, 1.]):
                color = m.mat_rgba[mat]
            g = dict(self.transform(i), name=m.geom(i).name or 'geom-' + str(i), type=kinds[int(m.geom_type[i])], size=m.geom_size[i].tolist(), rgba=color.tolist(), body_id=int(m.geom_bodyid[i]), body_name=m.body(int(m.geom_bodyid[i])).name or 'world', robot_id='go2' if self._robot_geom(i) else None, dynamic=i in self.dynamic_ids, group=int(m.geom_group[i]), material_name=m.material(mat).name if mat >= 0 else None, material={'emission': float(m.mat_emission[mat]) if mat >= 0 else 0., 'specular': .3, 'shininess': .3, 'reflectance': 0.})
            if g['type'] == 'mesh':
                mesh = int(m.geom_dataid[i]); va, vn = int(m.mesh_vertadr[mesh]), int(m.mesh_vertnum[mesh]); fa, fn = int(m.mesh_faceadr[mesh]), int(m.mesh_facenum[mesh])
                g.update(vertices=np.round(m.mesh_vert[va:va+vn].astype(float), 6).tolist(), faces=m.mesh_face[fa:fa+fn].tolist())
            geoms.append(g)
        lights = [{'id': i, 'type': int(self.model.light_type[i]), 'active': True, 'position': self.data.light_xpos[i].tolist(), 'direction': self.data.light_xdir[i].tolist(), 'ambient': self.model.light_ambient[i].tolist(), 'diffuse': self.model.light_diffuse[i].tolist(), 'specular': self.model.light_specular[i].tolist(), 'cast_shadow': bool(self.model.light_castshadow[i])} for i in range(self.model.nlight)]
        return {'schema': 'wendy.macos-demo.geometry.v1', 'run_id': self.run_id, 'model_sha256': self.digest, 'coordinate_frame': 'mujoco-z-up', 'engine': 'MuJoCo 4.0.0', 'synthetic': True, 'geoms': geoms, 'lights': lights, 'presentation': {'name': 'Autonomous Go2 patrol', 'subtitle': 'Simulated office · passive observations', 'labels': []}, 'semantics': 'Full collision office and articulated pinned Go2; rendered pose is physics ground truth'}

    def occupancy_map(self):
        """Conservative map from static collision bounding boxes, not robot pose."""
        res, width, height, origin = .05, 340, 260, [-8.5, -6.5, 0.]
        grid = np.zeros((height, width), dtype=np.int8)
        # Outside the office is occupied, even where its cutaway floor extends.
        yy, xx = np.mgrid[:height, :width]
        grid[(np.abs(origin[0] + (xx + .5) * res) >= 7.9) | (np.abs(origin[1] + (yy + .5) * res) >= 5.9)] = 100
        for i in range(self.model.ngeom):
            if i in self.dynamic_ids or not (self.model.geom_contype[i] or self.model.geom_conaffinity[i]) or self.model.geom_type[i] == mj.mjtGeom.mjGEOM_PLANE:
                continue
            center, half = self.model.geom_aabb[i, :3], self.model.geom_aabb[i, 3:]
            rotation = self.data.geom_xmat[i].reshape(3, 3)
            center = self.data.geom_xpos[i] + rotation @ center
            extent = np.abs(rotation) @ half
            if center[2] + extent[2] < .08 or center[2] - extent[2] > .65:
                continue
            lo = np.floor((center[:2] - extent[:2] - np.array(origin[:2])) / res).astype(int)
            hi = np.ceil((center[:2] + extent[:2] - np.array(origin[:2])) / res).astype(int)
            grid[max(0, lo[1]):min(height, hi[1]+1), max(0, lo[0]):min(width, hi[0]+1)] = 100
        return {'resolution': res, 'width': width, 'height': height, 'origin': origin, 'data': grid.ravel().tolist(), 'provenance': 'known synthetic office static collision AABB map'}

    def capture(self):
        return self.capture_worker.submit(self._capture).result(timeout=20)

    def _capture(self):
        with self.lock:
            if not self.sensors()['ready']:
                raise ValueError('fresh physics, LiDAR and leg odometry required')
            self.sequence += 1
            captured_wall = round(time.time()*1000)
            signature = mj.mjtState.mjSTATE_INTEGRATION
            state = np.empty(mj.mj_stateSize(self.model, signature))
            mj.mj_getState(self.model, self.data, state, signature)
            scan_points = copy.deepcopy(self.scan_points)
            scan_origin = list(self.scan_origin)
            scan_s = self.last_scan_s
        data = mj.MjData(self.model)
        mj.mj_setState(self.model, data, state, signature)
        mj.mj_forward(self.model, data)
        now = float(data.time)
        frame = {'id': 'office-floor', 'revision': 'office-' + self.digest[:16]}
        samples, media = [], {}
        def world(p): return [float(p[0]), float(p[2]), -float(p[1])]
        def sample(sensor, kind, position, model):
            return {'schema_version': 'wendy.sensor.sample.v1', 'id': f'{self.run_id}/{sensor}/{self.sequence}', 'site_id': 'studio-01', 'frame': frame, 'sensor_id': sensor, 'kind': kind, 'sequence': self.sequence, 'sample_ns': round(now * 1e9), 'run_id': self.run_id, 'scenario_version': 'go2-passive-patrol-v1', 'seed': 7, 'package_sha256': self.digest, 'sensor_model': model, 'position_m': position, 'uncertainty_m': .03, 'calibration_revision': 'go2-virtual-mounts-v1'}
        def attach(item, raw, content_type):
            digest = hashlib.sha256(raw).hexdigest()
            item['evidence'] = {'sha256': digest, 'content_type': content_type, 'size': len(raw)}
            media[digest] = base64.b64encode(raw).decode()
        if self.renderer is None:
            self.renderer = mj.Renderer(self.model, height=240, width=320)
        for name in ('camera-a', 'camera-b'):
            cam = self.model.camera(name).id
            position, metadata = calibration(self.model, data, cam)
            item = sample(name, 'camera', position, 'mujoco-4.0.0-rgb-pinhole')
            item['camera'] = metadata
            self.renderer.update_scene(data, camera=name)
            attach(item, png_rgb(self.renderer.render()), 'image/png')
            samples.append(item)
        item = sample('lidar-go2', 'lidar', world(scan_origin), 'mujoco-4.0.0-raycast')
        item['sample_ns'] = round(scan_s * 1e9)
        item['lidar'] = {'points_m': [world(p) for p in scan_points], 'max_range_m': 12.}
        samples.append(item)
        item = sample('gas-1', 'gas', world(data.site('gas').xpos), 'analytic-gaussian-fixture-v1')
        field = {'source_local_m': world([-7.3, .9, .65]), 'sigma_m': 1.6, 'background_ppm': 5., 'peak_ppm': 95., 'noise_stddev_ppm': 1.5}
        ppm = gas_value(field, item['position_m'], random.Random(7 + self.sequence))
        item['gas'] = {'value_ppm': ppm, 'uncertainty_ppm': 1.5, 'species': 'synthetic-test-gas'}
        samples.append(item)
        item = sample('audio-1', 'audio', world(data.site('audio').xpos), 'generated-880hz-tone-v1')
        item['audio'] = {'sample_rate_hz': 8000, 'channels': 1, 'duration_ns': 250000000}
        attach(item, audio_wav(7, self.sequence, ppm > 20), 'audio/wav')
        samples.append(item)
        return {'run_id': self.run_id, 'now_ns': round(now * 1e9), 'frame': frame, 'simulation_time_s': now, 'captured_unix_ms': captured_wall, 'samples': samples, 'media': media}

    def run(self):
        start = time.monotonic()
        try:
            while not self.done.is_set():
                with self.lock:
                    self.step()
                    deadline = start + self.data.time
                self.done.wait(max(0., deadline - time.monotonic()))
        except Exception as exc:
            self.fault = str(exc)
            self.done.set()


def serve(rt, host, port):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *args): pass
        def respond(self, status, value):
            data = json.dumps(value, separators=(',', ':'), allow_nan=False).encode()
            self.send_response(status); self.send_header('Content-Type', 'application/json'); self.send_header('Content-Length', str(len(data))); self.end_headers(); self.wfile.write(data)
        def do_GET(self):
            try:
                actions = {'/world-samples': rt.world_samples, '/environment': rt.environment, '/work': rt.work, '/state': rt.state, '/sensors': rt.sensors, '/geometry': lambda: rt._geometry, '/map': lambda: rt._map, '/capture': rt.capture}
                if self.path not in actions: return self.respond(404, {'error': 'unknown endpoint'})
                self.respond(200, actions[self.path]())
            except Exception as exc: self.respond(503, {'error': str(exc)})
        def do_POST(self):
            try:
                size = int(self.headers.get('Content-Length', '0'))
                if not 0 < size <= 4096: raise ValueError('invalid request size')
                body = json.loads(self.rfile.read(size))
                if self.path == '/command': result = rt.command(body)
                elif self.path == '/capture': result = rt.capture()
                elif self.path == '/fault/obstacle':
                    x, y = body.get('x', 0.), body.get('y', 0.)
                    active = body.get('active', False)
                    if type(active) is not bool or any(isinstance(v, bool) or not isinstance(v, (int, float)) or not math.isfinite(v) for v in (x, y)) or abs(x) > 8 or abs(y) > 6:
                        raise ValueError('obstacle needs finite office coordinates and boolean active')
                    with rt.lock:
                        mocap = rt.model.body_mocapid[rt.model.body('patrol-obstacle').id]
                        rt.data.mocap_pos[mocap] = [x, y, .5 if active else -5.]
                        mj.mj_forward(rt.model, rt.data)
                    result = {'accepted': True, 'active': active}
                elif self.path == '/fault/world-source':
                    with rt.lock:
                        result = rt.world_field.move(body.get('x'),body.get('z'))
                elif self.path == '/fault/environment':
                    duration, mode = body.get('seconds'), body.get('mode')
                    ppm = body.get('added_ppm', 0.)
                    if mode not in ('freeze', 'pulse') or any(isinstance(v, bool) or not isinstance(v, (int, float)) or not math.isfinite(v) for v in (duration, ppm)) or not 0 <= duration <= 60 or not 0 <= ppm <= 1000:
                        raise ValueError('environment fixture needs freeze/pulse, 0..60 seconds and 0..1000 added ppm')
                    with rt.lock:
                        if mode == 'freeze':
                            rt.environment_frozen = rt.environment_meter.snapshot()
                            rt.environment_loss_until = rt.data.time + duration
                        else:
                            rt.environment_pulse_until = rt.data.time + duration
                            rt.environment_pulse_ppm = ppm
                    result = {'accepted': True, 'simulation_fixture': True, 'mode': mode, 'seconds': duration, 'added_ppm': ppm}
                elif self.path == '/fault/work':
                    duration = body.get('seconds')
                    if isinstance(duration, bool) or not isinstance(duration, (int,float)) or not math.isfinite(duration) or not 0 <= duration <= 60:
                        raise ValueError('work sample freeze must be 0..60 simulation seconds')
                    with rt.lock:
                        rt.work_frozen = rt.work_meter.snapshot()
                        rt.work_loss_until = rt.data.time + duration
                    result = {'accepted': True, 'fault': 'export sample frozen; integration continues'}
                elif self.path == '/fault/lidar':
                    duration = float(body.get('seconds', 0))
                    if not 0 <= duration <= 60: raise ValueError('fault duration must be 0..60 seconds')
                    with rt.lock: rt.lidar_loss_until = rt.data.time + duration
                    result = {'accepted': True}
                else: return self.respond(404, {'error': 'unknown endpoint'})
                self.respond(200, result)
            except Exception as exc: self.respond(400, {'error': str(exc)})
    thread = threading.Thread(target=rt.run, daemon=True, name='physics')
    thread.start()
    server = ThreadingHTTPServer((host, port), Handler)
    try: server.serve_forever()
    finally:
        rt.done.set(); thread.join(timeout=2); server.server_close()
        if rt.renderer: rt.capture_worker.submit(rt.renderer.close).result(timeout=5)
        rt.capture_worker.shutdown(wait=False)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--source', required=True, type=Path); p.add_argument('--assets', required=True, type=Path)
    p.add_argument('--office', default='examples/collaborative-office/office.xml', type=Path)
    p.add_argument('--host', default='0.0.0.0'); p.add_argument('--port', default=8080, type=int); p.add_argument('--run-id')
    a = p.parse_args()
    serve(PatrolPhysics(a.source, a.assets, a.office, a.run_id), a.host, a.port)

if __name__ == '__main__': main()
