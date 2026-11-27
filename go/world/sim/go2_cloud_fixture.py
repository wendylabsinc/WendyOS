"""Independent static MuJoCo cloud fixtures and planar geometry oracle.

This module never connects to ROS, HTTP, a robot, or a controller. Known fixture
poses are synthetic inputs, not corrections to a live odometry observation.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import struct
import sys
from types import SimpleNamespace
import xml.etree.ElementTree as ET

from .go2_cloud import decode_cloud, SOURCE_DIGEST
from .mujoco_compat import legacy_multiray

SOURCE_FILES = {
    'go2_sim/__init__.py': 'fcce4b00268817450fca3b816bf78ddf70c86c777dff74f07a3f706c8e3e51c8',
    'go2_sim/lidar.py': 'c98e85e096c9650dfd4db75c60b710ea4354d95239ea47b3838b7be16339cf46',
    'go2_sim/sensors.py': 'c17f67715e3242f6180d7463b8e73be2f9a47dd6519f156686c23ae11b606966',
}
MOUNT = [.28, 0., .08]
CAPTURE_NS = 1_800_000_000_000_000_000
MAX_BYTES = 24 << 20
CELL = .1


def encoded(value):
    return (json.dumps(value, sort_keys=True, separators=(',', ':'), allow_nan=False) + '\n').encode()


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def column(name, x, y, hx, hy, height=1.5, yaw=0.):
    return {'name': name, 'position_m': [x, y, height/2],
            'half_size_m': [hx, hy, height/2], 'yaw_rad': yaw}


def walls():
    return [column('east', 3.8, 0, .1, 3.9, 2.),
            column('west', -3.8, 0, .1, 3.9, 2.),
            column('north', 0, 3.8, 3.8, .1, 2.),
            column('south', 0, -3.8, 3.8, .1, 2.)]


def definitions():
    base = {'supported_model': True, 'unsupported_reasons': [], 'dropout': 0.,
            'body_xyz_m': [0., 0., .33], 'body_euler_xyz_rad': [0., 0., 0.],
            'floor_hole': None, 'columns': walls()}
    import copy
    def case(name, **changes):
        value = copy.deepcopy(base); value.update(name=name, **changes); return value
    result = [case('level_room'),
              case('combined_tilt_columns', body_euler_xyz_rad=[.10, -.12, .7],
                   columns=walls()+[column('column', 1.7, .45, .28, .40, 1.5, .43)]),
              case('wrapped_yaw_columns', body_euler_xyz_rad=[-.08, .14, 3.12],
                   columns=walls()+[column('left', -1.8, -.4, .3, .22, 1.2, -.37), column('right', .8, 1.9, .25, .35, 2.1, .61)]),
              case('floor_grazing', body_xyz_m=[0., 0., .24], body_euler_xyz_rad=[.16, -.13, -.2]),
              case('top_grazing', body_xyz_m=[0., 0., .50], body_euler_xyz_rad=[-.12, .11, .4],
                   columns=walls()+[column('minimum_height', 1.7, -.35, .45, .3, 1.)]),
              case('seeded_dropout', body_euler_xyz_rad=[.10, -.12, .7], dropout=.20,
                   columns=walls()+[column('column', 1.7, .45, .28, .40, 1.5, .43)]),
              case('low_obstacle_unsupported', supported_model=False,
                   unsupported_reasons=['obstacle_below_commissioned_minimum_height'],
                   columns=walls()+[column('low', 1.25, .2, .25, .25, .06)]),
              case('floor_hole_unsupported', supported_model=False,
                   unsupported_reasons=['hole_violates_flat_supporting_floor'],
                   floor_hole=[.9, 1.3, -.2, .2]),
              case('subangular_column_unsupported', supported_model=False,
                   unsupported_reasons=['arbitrarily_thin_column_between_discrete_rays'],
                   columns=walls()+[column('thin', 1.8, .0133, .001, .001, 1.5)])]
    return result


def scene_xml(case):
    root = ET.Element('mujoco', model='world_cloud_grid_fixture')
    ET.SubElement(root, 'compiler', angle='radian', eulerseq='xyz')
    world = ET.SubElement(root, 'worldbody')
    hole = case['floor_hole']
    floors = [[-10., 10., -10., 10.]]
    if hole is None:
        ET.SubElement(world, 'geom', name='floor', type='plane', size='10 10 .1', group='0')
    else:
        x0, x1, y0, y1 = hole
        floors = [[-10., x0, -10., 10.], [x1, 10., -10., 10.], [x0, x1, -10., y0], [x0, x1, y1, 10.]]
        for i, (a, b, c, d) in enumerate(floors):
            ET.SubElement(world, 'geom', name='floor_'+str(i), type='box', group='0',
                          pos=f'{(a+b)/2} {(c+d)/2} -.1', size=f'{(b-a)/2} {(d-c)/2} .1')
        ET.SubElement(world, 'geom', name='pit_bottom', type='plane', pos='0 0 -1', size='10 10 .1', group='0')
    for geom in case['columns']:
        ET.SubElement(world, 'geom', name=geom['name'], type='box', group='0',
                      pos=' '.join(map(str, geom['position_m'])), size=' '.join(map(str, geom['half_size_m'])),
                      euler='0 0 '+str(geom['yaw_rad']))
    body = ET.SubElement(world, 'body', name='sensor_body', pos=' '.join(map(str, case['body_xyz_m'])),
                         euler=' '.join(map(str, case['body_euler_xyz_rad'])))
    ET.SubElement(body, 'site', name='lidar', pos=' '.join(map(str, MOUNT)), size='.003')
    return ET.tostring(root, encoding='unicode'), floors


def cloud_wire(xyz, captured):
    """Independent CDR1 LE PointCloud2 writer for original float32 XYZ values."""
    buf = bytearray(b'\x00\x01\x00\x00')
    def put(fmt, *values):
        buf.extend(b'\0' * ((-(len(buf)-4)) % (1 if fmt == 'B' else 4)))
        buf.extend(struct.pack('<'+fmt, *values))
    def string(value):
        raw = value.encode('ascii') + b'\0'; put('I', len(raw)); buf.extend(raw)
    sec, nano = divmod(captured, 10**9)
    put('i', sec); put('I', nano); string('utlidar_lidar')
    put('I', 1); put('I', len(xyz)); put('I', 3)
    for i, name in enumerate(('x', 'y', 'z')):
        string(name); put('I', i*4); put('B', 7); put('I', 1)
    put('B', 0); put('I', 12); put('I', len(xyz)*12); put('I', len(xyz)*12)
    for point in xyz:
        buf.extend(struct.pack('<fff', *point))
    put('B', 1)
    return bytes(buf)


def cross(a, b, c):
    return (b[0]-a[0])*(c[1]-a[1]) - (b[1]-a[1])*(c[0]-a[0])


def hull(points):
    points = sorted(set(tuple(p) for p in points))
    if len(points) <= 1: return points
    def half(values):
        out = []
        for p in values:
            while len(out) >= 2 and cross(out[-2], out[-1], p) <= 0: out.pop()
            out.append(p)
        return out
    return half(points)[:-1] + half(reversed(points))[:-1]


def polygon_area(poly):
    return abs(sum(a[0]*b[1]-a[1]*b[0] for a, b in zip(poly, poly[1:]+poly[:1])))/2 if poly else 0.


def clip_polygon(subject, clip):
    """Convex polygon clipping independent of the Go cell/ray/SAT algorithms."""
    result = [list(p) for p in subject]
    for a, b in zip(clip, clip[1:]+clip[:1]):
        if not result: break
        old, result = result, []
        previous = old[-1]; prior = cross(a, b, previous)
        for current in old:
            side = cross(a, b, current)
            if (side >= -1e-12) != (prior >= -1e-12):
                denominator = prior-side
                if abs(denominator) > 1e-15:
                    t = prior/denominator
                    result.append([previous[i]+t*(current[i]-previous[i]) for i in range(2)])
            if side >= -1e-12: result.append(list(current))
            previous, prior = current, side
    return result


def rectangle(x0, x1, y0, y1):
    return [[x0,y0],[x1,y0],[x1,y1],[x0,y1]]


def ground_truth(poly, oracle):
    obstacles = [g['name'] for g in oracle['columns'] if clip_polygon(poly, g['polygon_xy_m'])]
    covered = sum(polygon_area(clip_polygon(poly, rectangle(*r))) for r in oracle['floor_rectangles_xy_m'])
    return {'obstacles': obstacles, 'supporting_floor': covered >= polygon_area(poly)-1e-10}


def request(cloud, scan_ranges, position, q, rotation, sequence):
    # Actual MuJoCo body transform supplies the known static fixture pose.
    # No source runtime odometry or dynamics accuracy is claimed by this fixture.
    yaw = math.atan2(float(rotation[1,0]), float(rotation[0,0]))
    h = float(position[2]); he = .02+.05
    pose = {'quaternion_wxyz': list(map(float,q)), 'odom_position_xyz': list(map(float,position)),
            'mount_translation_xyz': MOUNT, 'mount_quaternion_wxyz': [1.,0.,0.,0.],
            'odom_captured_unix_ns': cloud.capture_unix_ns, 'pair_translation_bound_m': 0.,
            'pair_attitude_bound_rad': 0., 'odometry_epoch_age_s': 0.,
            'base_height_min_m': h-he, 'base_height_max_m': h+he}
    scan = {'TiltedPose': pose, 'Sequence': sequence, 'CapturedUnixNS': cloud.capture_unix_ns,
            'WorldEpoch': 1, 'Frame': 'odom', 'Ranges': scan_ranges,
            'AngleMin': -math.pi, 'AngleIncrement': math.tau/360, 'RangeMin': .1, 'RangeMax': 12.,
            'Robot': {'X': float(position[0]), 'Y': float(position[1]), 'Yaw': yaw},
            'SensorMount': {'X': .28, 'Y': 0., 'Yaw': 0.}, 'MountKnown': True,
            'PositionUncertaintyM': .02, 'YawUncertaintyRad': .005, 'UncertaintyKnown': True}
    profile = {'TiltedScan': {'model': 'solid-vertical-slab-v1', 'base_height_min_m': .15,
        'base_height_max_m': .6, 'obstacle_min_height_m': 1., 'slab_margin_m': .03,
        'body_half_height_m': .35, 'max_support_range_m': 4., 'range_error_m': .02,
        'attitude_error_rad': .01, 'max_translation_m_per_wall_s': .5,
        'max_rotation_rad_per_wall_s': 1., 'odom_floor_z_m': 0., 'height_calibration_error_m': .02},
        'MaxAbsRollRad': .25, 'MaxAbsPitchRad': .25, 'MotionEnvelope': 'isotropic-gait-v1',
        'FlatFloor': True, 'HalfLengthM': .425, 'HalfWidthM': .30, 'ClearanceM': .02,
        'MaxPositionUncertaintyM': .2, 'MaxYawUncertaintyRad': .2}
    observation = {'model': 'coherent-multiring-slab-v1', 'source_digest': SOURCE_DIGEST,
        'frame': cloud.frame_id, 'publisher_gid': '12'*24, 'serialized_sha256': cloud.serialized_sha256,
        'captured_unix_ns': cloud.capture_unix_ns, 'scan': scan, 'ranges': list(cloud.ranges_m)}
    return {'schema': 'wendy.world.go2-cloud-check.v1', 'simulation': True,
            'observation': observation, 'bounds': {'Min': {'X': -3., 'Y': -3.}, 'Max': {'X': 3., 'Y': 3.}},
            'profile': profile, 'continuation': None}


def generate(runtime_source):
    for relative, digest in SOURCE_FILES.items():
        path = runtime_source/relative
        if path.is_symlink() or not path.is_file() or path.stat().st_size > 128 << 10 or sha(path.read_bytes()) != digest:
            raise ValueError('runtime source identity mismatch')
    import mujoco
    import numpy as np
    if mujoco.__version__ != '4.0.0': raise ValueError('MuJoCo 4.0.0 required')
    sys.path.insert(0, str(runtime_source))
    from go2_sim import lidar as lidar_module
    from go2_sim.lidar import Lidar
    cases = []
    for index, definition in enumerate(definitions()):
        xml, floors = scene_xml(definition)
        model = mujoco.MjModel.from_xml_string(xml); data = mujoco.MjData(model)
        mujoco.mj_forward(model, data)
        group = np.array([1,0,0,0,0,0], dtype=np.uint8)
        stamp = CAPTURE_NS+index*100_000_000
        sampler = SimpleNamespace(model=model, data=data, epoch=1, wall_timestamp_ns=stamp,
                                  lidar_id=model.site('lidar').id, geom_group=group)
        with legacy_multiray(lidar_module, mujoco) as compatibility:
            observed = Lidar(dropout=definition['dropout'], seed=19).sample(sampler)
        raw = cloud_wire(observed['xyz'], stamp)
        decoded = decode_cloud(raw, source_digest=SOURCE_DIGEST)
        if [p.ray_index for p in decoded.points] != list(map(int, observed['ray_indices'])):
            raise ValueError('float32 decoder indices differ from source sampler')
        if any(p.xyz_m != tuple(map(float, xyz)) for p, xyz in zip(decoded.points, observed['xyz'])):
            raise ValueError('original point values changed')
        origin = data.site_xpos[sampler.lidar_id].copy()
        rotation = data.site_xmat[sampler.lidar_id].reshape(3,3).copy()
        reference, geom_ids = [], []
        maximum = 0.; valid_references = 0
        present = {p.ray_index: p for p in decoded.points}
        for ray_index in range(9000):
            ring, azimuth = divmod(ray_index,360)
            elevation = math.radians(-30+ring*2.5)
            angle = -math.pi+azimuth*(math.tau/360)
            # Independent single-ray call, not the sampler's batch distances,
            # Go projected bearings, clipping, support or endpoint formulas.
            local = np.array([math.cos(elevation)*math.cos(angle), math.cos(elevation)*math.sin(angle), math.sin(elevation)])
            direction = rotation @ local
            gid = np.array([-1], dtype=np.int32)
            distance = float(mujoco.mj_ray(model,data,origin,direction,group,True,-1,gid))
            valid = math.isfinite(distance) and .1 <= distance <= 12.
            reference.append(distance if valid else None);geom_ids.append(int(gid[0]) if valid else None)
            valid_references += valid
            if ray_index in present:
                point = present[ray_index]
                if not valid or abs(point.range_m-distance)>decoded.range_quantization_m+1e-9:
                    raise ValueError('actual batch point and independent mj_ray disagree')
                maximum=max(maximum,abs(point.range_m-distance))
            elif valid and definition['dropout']==0:
                raise ValueError('source sampler unexpectedly omitted finite ray')
        body_id=model.body('sensor_body').id
        position=data.xpos[body_id].copy();q=data.xquat[body_id].copy();body_rotation=data.xmat[body_id].reshape(3,3).copy()
        oracle_columns=[]
        for g in definition['columns']:
            gid=model.geom(g['name']).id;center=data.geom_xpos[gid];r=data.geom_xmat[gid].reshape(3,3);half=model.geom_size[gid]
            points=[(center+r@np.array([sx*half[0],sy*half[1],sz*half[2]])) for sx in (-1,1) for sy in (-1,1) for sz in (-1,1)]
            oracle_columns.append({'name':g['name'],'polygon_xy_m':hull([[float(v[0]),float(v[1])] for v in points]),
                                   'z_min_m':min(float(v[2]) for v in points),'z_max_m':max(float(v[2]) for v in points)})
        body_points=[position+body_rotation@np.array([sx*.425,sy*.30,sz*.35]) for sx in (-1,1) for sy in (-1,1) for sz in (-1,1)]
        scan=[float(v) if math.isfinite(v) else None for v in observed['ranges']]
        req=request(decoded,scan,position,q,body_rotation,index+1)
        if len(encoded(req)) > 1 << 20: raise ValueError('Go request size bound')
        cases.append({'name':definition['name'],'supported_model':definition['supported_model'],
            'unsupported_reasons':definition['unsupported_reasons'],'input':req,
            'observed':{'cloud_cdr_hex':raw.hex(),'serialized_sha256':sha(raw),
                'finite_points':len(decoded.points),'missing_rays':9000-len(decoded.points),
                'source_sampler_dropout':definition['dropout'],'source_sampler_seed':19,
                'pose_role':'known_static_fixture_transform_not_runtime_odometry',
                'identity_role':'synthetic_metadata_not_authenticated_or_received_DDS',
                'cloud_epoch_role':'fixture_context_not_embedded_in_PointCloud2'},
            'oracle':{'scene_xml':xml,'scene_xml_sha256':sha(xml.encode()),'floor_rectangles_xy_m':floors,
                'columns':oracle_columns,'nominal_body_projected_polygon_xy_m':hull([[float(v[0]),float(v[1])] for v in body_points]),
                'reference_ranges_m':reference,'reference_geom_indices':geom_ids,
                'geom_names':[model.geom(i).name for i in range(model.ngeom)],
                'finite_reference_rays':valid_references,'max_source_vs_single_ray_range_error_m':maximum,
                'role':'independent_static_geometry_oracle_never_passed_to_grid_builder'}})
    return {'schema':'wendy.world.go2-cloud-grid-fixtures.v1','simulation':True,'operational_authority':False,
        'mujoco_version':mujoco.__version__,'numpy_version':np.__version__,'source_digest':SOURCE_DIGEST,
        'source_files':SOURCE_FILES,'generator_sha256':sha(Path(__file__).read_bytes()),'cases':cases,
        'offline_api_adapter':compatibility,
        'limits':['Static synthetic geometry, not robot dynamics or a controller trial.',
                  'Finite angular support does not exclude arbitrary sub-ray obstacles.',
                  'Low obstacles and holes intentionally violate the commissioned model.',
                  'Known fixture pose is not a calibration of runtime odometry, mounting, or clock bounds.']}


def compare(case, result, raw_request):
    if result.get('schema') != 'wendy.world.go2-cloud-check-result.v1' or result.get('operational_authority') is not False or result.get('input_sha256') != sha(raw_request):
        raise ValueError('Go result/request binding mismatch')
    cells=result['evidence']['cells'];seen=set();counts={'free':0,'occupied':0,'unknown':0};conflicts=[]
    if len(cells)>6561:raise ValueError('grid cardinality bound')
    for cell in cells:
        x,y,state=cell['x'],cell['y'],cell['state']
        if type(x) is not int or type(y) is not int or (x,y) in seen or not(-31<=x<=30 and -31<=y<=30) or state not in counts:raise ValueError('grid cell shape')
        seen.add((x,y));counts[state]+=1
        if state=='free':
            truth=ground_truth(rectangle(x*CELL,(x+1)*CELL,y*CELL,(y+1)*CELL),case['oracle'])
            if truth['obstacles'] or not truth['supporting_floor']:conflicts.append({'x':x,'y':y,**truth})
    body_truth=ground_truth(case['oracle']['nominal_body_projected_polygon_xy_m'],case['oracle'])
    body_conflict=result['cloud']['current_footprint_clear'] and (body_truth['obstacles'] or not body_truth['supporting_floor'])
    supported=case['supported_model']
    return {'name':case['name'],'supported_model':supported,'unsupported_reasons':case['unsupported_reasons'],
            'cell_counts':counts,'free_cell_oracle_conflicts':conflicts,'current_body_truth':body_truth,
            'reported_current_footprint_clear':result['cloud']['current_footprint_clear'],
            'horizontal_current_footprint_clear':result['horizontal']['current_footprint_clear'],
            'current_body_conflict':bool(body_conflict),'supported_scene_pass':supported and not conflicts and not body_conflict and counts['free']>0,
            'unsupported_counterexample_observed':not supported and bool(conflicts),
            'qualification':False,'oracle_used_as_algorithm_input':False}


def main(argv=None):
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--runtime-source',type=Path,required=True)
    parser.add_argument('--out',type=Path,required=True)
    args=parser.parse_args(argv)
    result=generate(args.runtime_source.resolve(strict=True));raw=encoded(result)
    if len(raw)>MAX_BYTES:raise ValueError('fixture byte bound')
    fd=os.open(args.out,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
    with os.fdopen(fd,'wb') as stream:stream.write(raw);stream.flush();os.fsync(stream.fileno())
    print(json.dumps({'schema':result['schema'],'cases':len(result['cases']),'bytes':len(raw),'sha256':sha(raw)}))
    return 0


if __name__=='__main__':raise SystemExit(main())
