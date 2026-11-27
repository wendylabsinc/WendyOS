"""Offline cloud-grid comparison using retained ROS data and declared bounds.

No runtime calls or task dispatch. Status supplies only an observed epoch,
project identity and a conservative post-capture epoch-age bound. Pose and
velocity come exclusively from original ROS odometry. This is not a proof of
runtime continuity, calibrated time, physical clearance or motion authority.
"""
import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import stat
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from sim.go2_cloud import SOURCE_DIGEST
from sim.go2_cloud_recording import load_recording


def require(value, code):
    if not value:
        raise ValueError(code)


def encoded(value):
    return (json.dumps(value, sort_keys=True, separators=(',', ':'), allow_nan=False)+'\n').encode()


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def read(path, limit):
    fd = os.open(path, os.O_RDONLY | os.O_NONBLOCK | os.O_NOFOLLOW)
    with os.fdopen(fd, 'rb') as source:
        s = os.fstat(source.fileno())
        require(stat.S_ISREG(s.st_mode) and s.st_size <= limit, 'bounded regular input required')
        raw = source.read(limit+1)
    require(len(raw) <= limit, 'input grew beyond bound')
    return raw


def strict(raw):
    def pairs(items):
        value = {}
        for key, item in items:
            require(key not in value, 'duplicate JSON field')
            value[key] = item
        return value
    def bad(_):
        raise ValueError('nonfinite JSON')
    return json.loads(raw, object_pairs_hook=pairs, parse_constant=bad)


def context(report, before, after, profile, alignment):
    for s in (before, after):
        require(s['source_digest'] == SOURCE_DIGEST and s['simulation'] is True and s['epoch'] == report['expected_epoch'], 'runtime identity differs')
        require(s['clock_mode'] == 'device' and s['robot_kind'] == 'go2' and s['project']['error'] is None, 'runtime context unsupported')
    require(before['project']['project_id'] == after['project']['project_id'] and before['project']['revision'] == after['project']['revision'] and before['project']['epoch'] == after['project']['epoch'], 'project changed')
    age = after['time']
    require(type(age) in (int, float) and math.isfinite(age) and 0 <= before['time'] <= age <= profile['odometry_max_epoch_age_s'] <= 120, 'epoch age invalid')
    require(profile['schema'] == 'wendy.world.go2-profile.v2' and profile['odometry_integration_allowance_m'] == .05 and profile['motion_envelope'] == 'isotropic-gait-v1', 'unsupported analysis profile')
    require(alignment['synthetic'] is True and alignment['source_frame'] == 'odom' and alignment['translation_xy_m'] == [0,0] and alignment['yaw_rad'] == 0, 'only declared synthetic identity alignment')
    return age


def nav_profile(p):
    fields = {'TiltedScan':'tilted_scan','MaxAbsRollRad':'max_abs_roll_rad','MaxAbsPitchRad':'max_abs_pitch_rad',
              'MotionEnvelope':'motion_envelope','FlatFloor':'flat_floor','HalfLengthM':'half_length_m',
              'HalfWidthM':'half_width_m','ClearanceM':'clearance_m','MaxPositionUncertaintyM':'max_position_uncertainty_m',
              'MaxYawUncertaintyRad':'max_yaw_uncertainty_rad'}
    return {key:p[source] for key,source in fields.items()}


def make_request(report, decoded, cloud, item, profile, alignment, age, bounds, prior):
    require(not item['missing_joins'], 'incomplete companion set')
    scan = decoded[item['scan_message_index']]
    odom = decoded[item['odom_message_index']]
    require(scan['capture_unix_ns'] == cloud.capture_unix_ns and scan['frame_id'] == 'lidar_link', 'scan mismatch')
    require(odom['frame_id'] == 'odom' and odom['child_frame_id'] == 'base_link', 'odometry frame mismatch')
    mounts = [t for value, row in zip(decoded, report['messages']) if row['topic'] == '/tf_static'
              for t in value['transforms'] if t['frame_id'] == 'base_link' and t['child_frame_id'] in {'utlidar_lidar','lidar_link'}]
    require({t['child_frame_id'] for t in mounts} == {'utlidar_lidar','lidar_link'}
            and all(t['translation_xyz'] == [.28,0,.08] and t['quaternion_wxyz'] == [1,0,0,0] for t in mounts), 'mount differs')
    q = odom['quaternion_wxyz']; w,x,y,z = q
    yaw = math.atan2(2*(w*z+x*y),1-2*(y*y+z*z))
    robot = {'X':odom['position_xyz'][0],'Y':odom['position_xyz'][1],'Yaw':yaw}
    p = profile['tilted_scan']
    skew = abs(cloud.capture_unix_ns-odom['capture_unix_ns'])/1e9
    require(skew <= .05, 'odometry skew')
    translation = p['max_translation_m_per_wall_s']*skew
    attitude = p['max_rotation_rad_per_wall_s']*skew
    drift = math.hypot(.001,-.0005)*age
    he = p['height_calibration_error_m']+.05+drift+translation
    height = odom['position_xyz'][2]-p['odom_floor_z_m']
    pose = {'quaternion_wxyz':q,'odom_position_xyz':odom['position_xyz'],
            'mount_translation_xyz':[.28,0,.08],'mount_quaternion_wxyz':[1,0,0,0],
            'odom_captured_unix_ns':odom['capture_unix_ns'],'pair_translation_bound_m':translation,
            'pair_attitude_bound_rad':attitude,'odometry_epoch_age_s':age,
            'base_height_min_m':height-he,'base_height_max_m':height+he}
    wire_scan = {'TiltedPose':pose,'Sequence':item['message_index']+1,'CapturedUnixNS':cloud.capture_unix_ns,
                 'WorldEpoch':report['expected_epoch'],'Frame':'odom','Ranges':[v if math.isfinite(v) else None for v in scan['ranges']],
                 'AngleMin':scan['angle_min'],'AngleIncrement':scan['angle_increment'],'RangeMin':scan['range_min'],'RangeMax':scan['range_max'],
                 'Robot':robot,'SensorMount':{'X':.28,'Y':0,'Yaw':0},'MountKnown':True,
                 'PositionUncertaintyM':alignment['position_uncertainty_m']+.05+drift,
                 'YawUncertaintyRad':alignment['yaw_uncertainty_rad']+.0002*age,'UncertaintyKnown':True}
    limits = {'MaxMeasuredLateralMPS':profile['max_measured_lateral_mps'],'MaxForwardMPS':profile['max_forward_mps'],
              'MaxYawRateRPS':profile['max_yaw_rate_rps'],'HorizonS':profile['horizon_s'],'ExtraStopM':profile['extra_stop_m'],
              'ExtraStopRad':profile['extra_stop_rad'],'WaypointToleranceM':.18}
    return {'schema':'wendy.world.go2-cloud-check.v1','simulation':True,'observation':{
        'model':'coherent-multiring-slab-v1','source_digest':SOURCE_DIGEST,'frame':cloud.frame_id,
        'publisher_gid':report['messages'][item['message_index']]['publisher_gid'],'serialized_sha256':cloud.serialized_sha256,
        'captured_unix_ns':cloud.capture_unix_ns,'scan':wire_scan,'ranges':list(cloud.ranges_m)},
        'bounds':bounds,'profile':nav_profile(profile),'continuation':{'pose':robot,
        'measured':{'ForwardMPS':odom['linear_velocity_xyz'][0],'LateralMPS':odom['linear_velocity_xyz'][1],
                    'YawRateRPS':odom['angular_velocity_xyz'][2]},'prior':prior,
        'proposed':{'ForwardMPS':0,'YawRateRPS':0},'limits':limits}}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ['capture','runtime-before','runtime-after','profile','alignment','checker','out']:
        parser.add_argument('--'+name, required=True, type=Path)
    parser.add_argument('--area-min', nargs=2, required=True, type=float)
    parser.add_argument('--area-max', nargs=2, required=True, type=float)
    args = parser.parse_args()
    require(not args.out.exists(), 'new output directory required')
    sources = {name:read(getattr(args,name),4<<20 if name=='capture' else 64<<10)
               for name in ['capture','runtime_before','runtime_after','profile','alignment']}
    report, decoded, clouds = load_recording(sources['capture'])
    before,after,profile,alignment = [strict(sources[n]) for n in ['runtime_before','runtime_after','profile','alignment']]
    age = context(report,before,after,profile,alignment)
    bounds = {'Min':dict(zip(('X','Y'),args.area_min)),'Max':dict(zip(('X','Y'),args.area_max))}
    require(all(math.isfinite(x) for x in args.area_min+args.area_max), 'finite bounds required')
    binary = read(args.checker,32<<20)
    args.out.mkdir(mode=0o700)
    summary = {'schema':'wendy.world.go2-cloud-recorded-check.v1','simulation':True,'operational_authority':False,
               'sources':{k:{'path':str(getattr(args,k)),'sha256':digest(v),'bytes':len(v)} for k,v in sources.items()},
               'checker_sha256':digest(binary),'epoch':report['expected_epoch'],'epoch_age_upper_bound_s':age,
               'context_role':'unsigned before/after observations; no runtime continuity or epoch-in-cloud proof',
               'sequence_role':'local diagnostic ordinal derived from message index, not source sequence',
               'bounds':bounds,'cases':[],'skipped':[]}
    for item,cloud in zip(report['clouds'],clouds):
        if item['missing_joins']:
            summary['skipped'].append({'message_index':item['message_index'],'reasons':item['missing_joins']}); continue
        for name,prior in [('measured-zero',[]),('hypothetical-forward-prior-zero',[{'ForwardMPS':profile['max_forward_mps'],'YawRateRPS':0}])]:
            case = f"cloud-{item['message_index']}-{name}"
            request = make_request(report,decoded,cloud,item,profile,alignment,age,bounds,prior)
            raw = encoded(request); (args.out/(case+'.input.json')).write_bytes(raw)
            with (args.out/(case+'.output.json')).open('xb') as out, (args.out/(case+'.stderr')).open('xb') as error:
                result = subprocess.run([str(args.checker.resolve())], input=raw, stdout=out, stderr=error, timeout=15, check=False)
            row={'case':case,'message_index':item['message_index'],'input_sha256':digest(raw),'exit_code':result.returncode,
                 'prior_role':'none recorded during passive capture' if not prior else 'counterfactual prior, never executed'}
            output=read(args.out/(case+'.output.json'),8<<20); row['output_sha256']=digest(output)
            if result.returncode==0:
                value=strict(output); require(value['input_sha256']==digest(raw) and value['operational_authority'] is False,'checker binding')
                row.update(cloud=value['cloud'],horizontal=value['horizontal'],cell_counts={s:sum(c['state']==s for c in value['evidence']['cells']) for s in ['free','unknown','occupied']},
                           resolved_ambiguous_cells=sum(bool(c['resolved_ambiguous_rings']) for c in value['evidence']['cells']))
            summary['cases'].append(row)
    (args.out/'summary.json').write_bytes(encoded(summary))
    assessed = bool(summary['cases']) and all(c['exit_code']==0 for c in summary['cases'])
    print(json.dumps({'cases':len(summary['cases']),'skipped':summary['skipped'],'all_checks_ran':assessed}))
    return 0 if assessed else 1


if __name__ == '__main__':
    raise SystemExit(main())
