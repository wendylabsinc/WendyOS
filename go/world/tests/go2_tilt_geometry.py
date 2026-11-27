#!/usr/bin/env python3
"""Offline synthetic tilted-ray fixtures; never reads or controls a live robot.

Use the pinned Linux MuJoCo environment and an explicitly supplied, hash-checked
runtime source cache. Compare its actual Lidar implementation against mj_ray,
then retain both observations and geometry. This is ray geometry evidence, not
motion calibration, continuous clearance, surveyed positioning or authority.
"""
import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import sys
import unittest
from types import SimpleNamespace
import xml.etree.ElementTree as ET

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from sim.mujoco_compat import legacy_multiray


SOURCE_DIGEST = 'sha256:2869d018ae55e4c83318329bb20cc670e16f6f62838989bb5192829002af8d2b'
SOURCE_FILES = {
    'go2_sim/__init__.py': 'fcce4b00268817450fca3b816bf78ddf70c86c777dff74f07a3f706c8e3e51c8',
    'go2_sim/lidar.py': 'c98e85e096c9650dfd4db75c60b710ea4354d95239ea47b3838b7be16339cf46',
    'go2_sim/sensors.py': 'c17f67715e3242f6180d7463b8e73be2f9a47dd6519f156686c23ae11b606966',
}
MOUNT = [.28, 0., .08]


def quaternion(roll, pitch, yaw):
    cr, sr = math.cos(roll/2), math.sin(roll/2)
    cp, sp = math.cos(pitch/2), math.sin(pitch/2)
    cy, sy = math.cos(yaw/2), math.sin(yaw/2)
    return [cr*cp*cy+sr*sp*sy, sr*cp*cy-cr*sp*sy,
            cr*sp*cy+sr*cp*sy, cr*cp*sy-sr*sp*cy]


def rotate(q, value):
    w, x, y, z = q
    a, b, c = value
    return [(1-2*(y*y+z*z))*a+2*(x*y-z*w)*b+2*(x*z+y*w)*c,
            2*(x*y+z*w)*a+(1-2*(x*x+z*z))*b+2*(y*z-x*w)*c,
            2*(x*z-y*w)*a+2*(y*z+x*w)*b+(1-2*(x*x+y*y))*c]


def box(name, position, half_size, yaw=0.):
    return {'name': name, 'position_m': position, 'half_size_m': half_size,
            'quaternion_wxyz': quaternion(0., 0., yaw)}


def walls():
    return [box('east', [4., 0., 1.], [.1, 4.1, 1.]),
            box('west', [-4., 0., 1.], [.1, 4.1, 1.]),
            box('north', [0., 4., 1.], [4., .1, 1.]),
            box('south', [0., -4., 1.], [4., .1, 1.])]


def definitions():
    result = []
    rotations = [('level', 0., 0., 0.), ('roll_positive', .12, 0., .37),
                 ('roll_negative', -.12, 0., .37),
                 ('pitch_positive', 0., .12, -.22),
                 ('pitch_negative', 0., -.12, -.22),
                 ('combined', .10, -.12, .7), ('wrapped_yaw', -.10, .12, 3.12),
                 ('downward_ground', 0., .20, 0.)]
    for name, roll, pitch, yaw in rotations:
        result.append({'name': name, 'rpy_rad': [roll, pitch, yaw],
                       'boxes': walls(), 'solid_height_lower_bound_m': 2.,
                       'dropout': 0.})
    for name, distance, height in [('low_obstacle', 1.6, .10),
                                  ('far_medium_obstacle', 3.0, .80),
                                  ('near_medium_obstacle', 1.6, .80)]:
        result.append({'name': name, 'rpy_rad': [0., -.15, 0.],
                       'boxes': walls()+[box(name, [distance, 0., height/2], [.20, .8, height/2])],
                       'solid_height_lower_bound_m': height, 'dropout': 0.})
    result.append({'name': 'rotated_column', 'rpy_rad': [.10, -.08, .2],
                   'boxes': walls()+[box('column', [1.8, .3, .4], [.15, .8, .4], .4)],
                   'solid_height_lower_bound_m': .8, 'dropout': 0.})
    result.append({'name': 'open_ground', 'rpy_rad': [.10, -.12, .7],
                   'boxes': [], 'solid_height_lower_bound_m': None, 'dropout': 0.})
    result.append({'name': 'missing_returns', 'rpy_rad': [.10, -.12, .7],
                   'boxes': walls(), 'solid_height_lower_bound_m': 2., 'dropout': .25})
    return result


def scene(case):
    root = ET.Element('mujoco', model='world_tilt_fixture')
    ET.SubElement(root, 'compiler', angle='radian')
    world = ET.SubElement(root, 'worldbody')
    ET.SubElement(world, 'geom', name='floor', type='plane', size='10 10 .1', group='0')
    for geom in case['boxes']:
        ET.SubElement(world, 'geom', name=geom['name'], type='box', group='0',
                      pos=' '.join(map(str, geom['position_m'])),
                      size=' '.join(map(str, geom['half_size_m'])),
                      quat=' '.join(map(str, geom['quaternion_wxyz'])))
    body = ET.SubElement(world, 'body', name='sensor_body', pos='0 0 .33',
                         quat=' '.join(map(str, quaternion(*case['rpy_rad']))))
    ET.SubElement(body, 'site', name='lidar', pos=' '.join(map(str, MOUNT)), size='.003')
    return ET.tostring(root, encoding='unicode')


def generate(runtime_source):
    for relative, expected in SOURCE_FILES.items():
        path = runtime_source/relative
        if not path.is_file() or path.is_symlink() or path.stat().st_size > 128 << 10:
            raise ValueError('invalid runtime source file')
        if hashlib.sha256(path.read_bytes()).hexdigest() != expected:
            raise ValueError('runtime source hash mismatch')
    import mujoco
    import numpy as np
    if mujoco.__version__ != '4.0.0':
        raise ValueError('MuJoCo 4.0.0 required')
    sys.path.insert(0, str(runtime_source))
    from go2_sim import lidar as lidar_module
    from go2_sim.lidar import Lidar
    from go2_sim.sensors import SCAN_ANGLES, SCAN_MIN, SCAN_MAX
    cases = []
    for definition in definitions():
        xml = scene(definition)
        model = mujoco.MjModel.from_xml_string(xml)
        data = mujoco.MjData(model)
        mujoco.mj_forward(model, data)
        group = np.array([1, 0, 0, 0, 0, 0], dtype=np.uint8)
        sampler = SimpleNamespace(model=model, data=data, epoch=1,
                                  wall_timestamp_ns=1800000000000000000,
                                  lidar_id=model.site('lidar').id, geom_group=group)
        with legacy_multiray(lidar_module, mujoco) as compatibility:
            observed = Lidar(dropout=definition['dropout'], seed=17).sample(sampler)
        position, q = [0., 0., .33], quaternion(*definition['rpy_rad'])
        mount = rotate(q, MOUNT)
        origin = [a+b for a, b in zip(position, mount)]
        site_origin = data.site_xpos[sampler.lidar_id]
        if max(abs(a-b) for a, b in zip(origin, site_origin)) > 1e-12:
            raise ValueError('full mount transformation mismatch')
        rays = []
        maximum_error = 0.
        for i, angle in enumerate(SCAN_ANGLES):
            direction = rotate(q, [math.cos(angle), math.sin(angle), 0.])
            gid = np.array([-1], dtype=np.int32)
            reference = float(mujoco.mj_ray(model, data, np.asarray(origin), np.asarray(direction), group, True, -1, gid))
            valid = math.isfinite(reference) and SCAN_MIN <= reference <= SCAN_MAX
            received = float(observed['ranges'][i])
            if math.isfinite(received):
                if not valid or abs(received-reference) > 2e-6:
                    raise ValueError('batch lidar and independent mj_ray disagree')
                maximum_error = max(maximum_error, abs(received-reference))
            elif valid and definition['dropout'] == 0:
                raise ValueError('unexpected missing finite ray')
            rays.append({'angle_body_rad': float(angle), 'direction_world': direction,
                         'range_m': received if math.isfinite(received) else None,
                         'reference_range_m': reference if valid else None,
                         'reference_geom': model.geom(int(gid[0])).name if valid else None,
                         'reference_endpoint_world_m': [o+reference*d for o,d in zip(origin,direction)] if valid else None})
        cases.append({**definition, 'body_position_world_m': position,
                      'body_quaternion_wxyz': q, 'mount_translation_body_m': MOUNT,
                      'mount_quaternion_wxyz': [1.,0.,0.,0.], 'origin_world_m': origin,
                      'range_min_m': SCAN_MIN, 'range_max_m': SCAN_MAX,
                      'angle_min_rad': float(SCAN_ANGLES[0]),
                      'angle_increment_rad': float(SCAN_ANGLES[1]-SCAN_ANGLES[0]),
                      'scene_xml_sha256': hashlib.sha256(xml.encode()).hexdigest(),
                      'maximum_lidar_reference_error_m': maximum_error, 'rays': rays})
    forward = len(cases[0]['rays'])//2
    lookup = {c['name']: c for c in cases}
    if lookup['low_obstacle']['rays'][forward]['reference_geom'] != 'east':
        raise ValueError('low obstacle scenario did not demonstrate overflight')
    if lookup['far_medium_obstacle']['rays'][forward]['reference_geom'] != 'east':
        raise ValueError('far obstacle scenario did not demonstrate overflight')
    if lookup['near_medium_obstacle']['rays'][forward]['reference_geom'] != 'near_medium_obstacle':
        raise ValueError('near obstacle scenario did not produce an intercept')
    if not any(r['reference_geom']=='floor' for r in lookup['downward_ground']['rays']):
        raise ValueError('downward case did not hit ground')
    return {'schema':'wendy.world.go2-tilt-ray-fixtures.v1', 'simulation':True,
            'operational_authority':False, 'mujoco_version':mujoco.__version__,
            'numpy_version':np.__version__, 'runtime_source_digest':SOURCE_DIGEST,
            'runtime_source_files':SOURCE_FILES,
            'offline_api_adapter':compatibility,
            'generator_sha256':hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
            'scope':'Static synthetic ray geometry only; no robot dynamics, capture-time bound or continuous clearance validation',
            'cases':cases}


class GeometryMathTests(unittest.TestCase):
    def test_full_mount_rotation(self):
        for r,p,y in [(0.,0.,0.),(.12,-.08,.7),(-.10,.20,3.12)]:
            q=quaternion(r,p,y)
            self.assertAlmostEqual(sum(v*v for v in q),1.)
            v=rotate(q,MOUNT)
            self.assertAlmostEqual(sum(x*x for x in v),sum(x*x for x in MOUNT))
        self.assertNotAlmostEqual(rotate(quaternion(.1,.1,0.),MOUNT)[2],MOUNT[2])
    def test_cases_are_distinct_and_bounded(self):
        items=definitions()
        self.assertEqual(len(items),14)
        self.assertEqual(len({c['name'] for c in items}),14)
        for c in items:
            self.assertLess(max(abs(v) for v in c['rpy_rad'][:2]),.25)
            self.assertEqual(ET.fromstring(scene(c)).tag,'mujoco')


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    sub=parser.add_subparsers(dest='command',required=True)
    sub.add_parser('self-test')
    gen=sub.add_parser('generate')
    gen.add_argument('--runtime-source',type=Path,required=True)
    gen.add_argument('--out',type=Path,required=True)
    args=parser.parse_args()
    if args.command=='self-test':
        result=unittest.TextTestRunner(verbosity=2).run(unittest.defaultTestLoader.loadTestsFromTestCase(GeometryMathTests))
        return 0 if result.wasSuccessful() else 1
    result=generate(args.runtime_source.resolve(strict=True))
    raw=(json.dumps(result,sort_keys=True,separators=(',',':'),allow_nan=False)+'\n').encode()
    if len(raw)>4<<20:
        raise ValueError('fixture size exceeds bound')
    fd=os.open(args.out,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
    with os.fdopen(fd,'wb') as f:
        f.write(raw);f.flush();os.fsync(f.fileno())
    print(json.dumps({'result':'generated','cases':len(result['cases']),
                      'rays':sum(len(c['rays']) for c in result['cases']),
                      'bytes':len(raw),'sha256':hashlib.sha256(raw).hexdigest()}))
    return 0


if __name__=='__main__':
    raise SystemExit(main())
