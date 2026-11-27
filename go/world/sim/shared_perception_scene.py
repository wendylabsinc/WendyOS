"""One furnished MuJoCo office for hazard and image-based people perception.

The office geometry and hazard camera bindings are retained. Two articulated,
noncolliding mannequins reuse the existing pose controller in the clear central
corridor. They are a kinematic image fixture, not simulated human dynamics.
Render/calibration methods expose pixels and camera geometry only; actor joint
positions belong solely to a caller's separate evaluation oracle.
"""
from __future__ import annotations

import copy
import hashlib
import json
import math
from pathlib import Path
import re
import xml.etree.ElementTree as ET

from sim import people_scene
from sim.sensors import calibration as hazard_calibration, png_rgb

SCENE = people_scene.OFFICE.with_name('scene.json')
PEOPLE_CAMERAS = people_scene.CAMERAS
CAMERA_DIMENSIONS = {'camera-a': (320, 240), 'corridor-view': (320, 240),
                     'overview': (640, 480), **{name: (640, 480) for name in PEOPLE_CAMERAS}}
SCENARIO_VERSION = 'shared-office-perception-v1'
INSPECTION_CAMERAS = {'inspection-camera-a': (320, 240), 'inspection-camera-b': (320, 240)}


def assemble_office_xml(office=people_scene.OFFICE, *, inspection=False):
    """Return combined XML without writing or changing the original office."""
    root = ET.fromstring(Path(office).read_bytes())
    root.set('model', SCENARIO_VERSION)
    root.find('./visual/global').set('offwidth', '640')
    root.find('./visual/global').set('offheight', '480')
    assets, world = root.find('asset'), root.find('worldbody')
    # Reuse the original mannequin builder and its exact palette.
    annex = ET.fromstring(people_scene.scene_xml(office))
    for material in annex.findall('./asset/material'):
        if material.get('name', '').startswith('people-'):
            assets.append(copy.deepcopy(material))
    ET.SubElement(world, 'body', name='shared-people-focus', pos='-.3 0 .95')
    for name, position, fovy in [('people-front', '5.3 -.4 2.1', '40'),
                                 ('people-oblique', '4.1 1.15 2.4', '43')]:
        ET.SubElement(world, 'camera', name=name, pos=position, mode='targetbody',
                      target='shared-people-focus', fovy=fovy)
    people_scene._person(world, 1, 'people-teal')
    people_scene._person(world, 2, 'people-ochre')
    if inspection:
        atlas = world.find("body[@name='robot-atlas']")
        for name, position in [('inspection-camera-a', '0 .12 .45'),
                               ('inspection-camera-b', '0 0 .45')]:
            # Fixed robot-local optical axes. The planar chassis has no yaw
            # joint; these are cameras on that chassis, not target-following views.
            ET.SubElement(atlas, 'camera', name=name, pos=position,
                          xyaxes='1 0 0 0 .25 1', fovy='60')
    return ET.tostring(root, encoding='unicode')


class SharedPerceptionScene:
    """Single-threaded scene owned by the source's existing render worker.

    All cameras share model/data, simulation time, package digest and World
    Y-up frame. Presentation geometry remains MuJoCo Z-up, like the office UI.
    Source adapters may reuse the office gas/audio/ray helpers on model/data.
    """
    def __init__(self, run_id, *, inspection=False):
        import mujoco
        import numpy as np
        if mujoco.__version__ != '4.0.0':
            raise ValueError('MuJoCo 4.0.0 required')
        if not isinstance(run_id, str) or re.fullmatch(r'[A-Za-z0-9_-]{1,80}', run_id) is None:
            raise ValueError('bounded run identity required')
        self.mj, self.np, self.run_id = mujoco, np, run_id
        source = SCENE.read_bytes()
        self.config = json.loads(source)
        office = SCENE.parent / self.config['model']['path']
        self.office_sha256 = hashlib.sha256(office.read_bytes()).hexdigest()
        if self.office_sha256 != self.config['model']['sha256']:
            raise ValueError('office model digest differs')
        self.camera_dimensions = dict(CAMERA_DIMENSIONS)
        if inspection:
            self.camera_dimensions.update(INSPECTION_CAMERAS)
        self.xml = assemble_office_xml(office, inspection=inspection)
        self.model_sha256 = hashlib.sha256(self.xml.encode()).hexdigest()
        self.config['model'] = {'path': 'shared-perception.xml', 'sha256': self.model_sha256}
        self.config['shared_perception'] = {
            'scenario_version': SCENARIO_VERSION,
            'base_scene_sha256': hashlib.sha256(source).hexdigest(),
            'base_model_sha256': self.office_sha256,
            'camera_dimensions': {name: list(size) for name, size in self.camera_dimensions.items()},
            'kinematic_people': True}
        if inspection:
            self.config['shared_perception']['inspection'] = {
                'robot_id': 'atlas', 'state_schema': 'wendy.sim.actuator.planar.v1',
                'camera_names': list(INSPECTION_CAMERAS), 'floor_y': .24}
        self.package_json = json.dumps(self.config, sort_keys=True, separators=(',', ':'), allow_nan=False).encode()
        self.package_sha256 = hashlib.sha256(self.package_json).hexdigest()
        self.frame = {'id': 'office-floor', 'revision': 'shared-office-' + self.model_sha256[:16]}
        self.model = mujoco.MjModel.from_xml_string(self.xml)
        self.data = mujoco.MjData(self.model)
        self.bindings = {r['id']: {'body': mujoco.mj_name2id(self.model, mujoco.mjtObj.mjOBJ_BODY, r['body'])}
                         for r in self.config['robots']}
        if any(binding['body'] <= 0 for binding in self.bindings.values()):
            raise ValueError('office robot binding missing')
        self.camera_ids = {name: mujoco.mj_name2id(self.model, mujoco.mjtObj.mjOBJ_CAMERA, name)
                           for name in self.camera_dimensions}
        if min(self.camera_ids.values()) < 0:
            raise ValueError('shared camera missing')
        self.renderers = {}
        self.geometry = None
        self._pose_people(0.)

    def _pose_people(self, seconds):
        # Reuse articulation/crossing; rotate the path into the office corridor
        # and shorten its lateral extent to stay inside the existing walls.
        people_scene.PeopleScene.pose(self, seconds)
        for number in (1, 2):
            body = self.mj.mj_name2id(self.model, self.mj.mjtObj.mjOBJ_BODY, f'person-{number}')
            mocap = int(self.model.body_mocapid[body])
            x, y, z = self.data.mocap_pos[mocap]
            self.data.mocap_pos[mocap] = [-y, .7 * x, z]
            w, _, _, qz = self.data.mocap_quat[mocap]
            half = math.sqrt(.5)
            self.data.mocap_quat[mocap] = [half * (w-qz), 0., 0., half * (w+qz)]
        self.mj.mj_forward(self.model, self.data)

    def advance(self, ns):
        """Advance the shared physics clock and apply the current actor pose."""
        if type(ns) is not int or not 0 <= ns <= 120_000_000_000 or ns < round(self.data.time * 1e9):
            raise ValueError('increasing simulation nanoseconds within 120 seconds required')
        target, original = ns / 1e9, self.model.opt.timestep
        try:
            while self.data.time < target - 1e-10:
                self.model.opt.timestep = min(original, target - self.data.time)
                self.mj.mj_step(self.model, self.data)
        finally:
            self.model.opt.timestep = original
        self._pose_people(target)

    def calibration(self, camera):
        if camera not in self.camera_ids:
            raise ValueError('unknown camera')
        cid = self.camera_ids[camera]
        position, result = hazard_calibration(self.model, self.data, cid)
        width, height = self.camera_dimensions[camera]
        # The inherited extrinsics use World Y-up for both camera families.
        focal = height / (2 * math.tan(math.radians(float(self.model.cam_fovy[cid])) / 2))
        result.update(width=width, height=height, fx=focal, fy=focal, cx=width/2, cy=height/2)
        return position, result

    def render(self, camera):
        if camera not in self.camera_ids:
            raise ValueError('unknown camera')
        width, height = self.camera_dimensions[camera]
        size = width, height
        if size not in self.renderers:
            self.renderers[size] = self.mj.Renderer(self.model, height=height, width=width)
        renderer = self.renderers[size]
        renderer.update_scene(self.data, camera=camera)
        rgb = renderer.render()
        return png_rgb(rgb) if size == (320, 240) else people_scene.png(rgb)

    def presentation(self):
        from sim.macos_demo_bridge import geometry, _transform
        if self.geometry is None:
            self.geometry, dynamic = geometry(self)
            # Mocap roots have no DOFs; the existing generic exporter otherwise
            # marks only their jointed limbs dynamic, leaving torsos behind.
            for geom in self.geometry['geoms']:
                if geom['name'].startswith('person-'):
                    geom['dynamic'] = True
                    dynamic.append(geom['id'])
            self.dynamic_ids = sorted(set(dynamic))
            self.geometry['semantics'] = ('Reference simulation geometry with kinematic mannequins; '
                                          'perception results come from camera pixels and sensor observations')
        return {'geometry': self.geometry,
                'dynamic_geoms': [_transform(self, i) for i in self.dynamic_ids]}

    def close(self):
        for renderer in self.renderers.values():
            renderer.close()
        self.renderers.clear()
