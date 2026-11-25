#!/usr/bin/env python3
"""Offline D/E motion diagnostics. No network, commands, or controller inputs.

`analyze --evidence DIR --out NEW.json` consumes the archived Go2 mission bundle.
`self-test` exercises bounds, geometry and missing-observation interpretation.
Passing sampled checks is not continuous containment or dynamics calibration.
"""
import argparse
import gzip
import hashlib
import io
import json
import math
import os
from pathlib import Path
import stat
import tempfile
import unittest


MAX_FILE = 16 << 20
MAX_ROWS = 1200
MAX_ROW = 64 << 10
MAX_GAP_MS = 150.0


def finite(value):
    return type(value) in (int, float) and math.isfinite(value)


def require_numbers(values):
    if not all(finite(v) for v in values):
        raise ValueError('nonfinite or nonnumeric value')


def bounded_read(path, limit=MAX_FILE):
    if not stat.S_ISREG(path.lstat().st_mode):
        raise ValueError('regular file required')
    fd = os.open(str(path), os.O_RDONLY | os.O_NONBLOCK | os.O_NOFOLLOW)
    with os.fdopen(fd, 'rb') as stream:
        if not stat.S_ISREG(os.fstat(stream.fileno()).st_mode):
            raise ValueError('regular file required')
        data = stream.read(limit + 1)
    if len(data) > limit:
        raise ValueError('file bound exceeded')
    return data


def decode(data):
    def pairs(items):
        result = {}
        for key, value in items:
            if key in result:
                raise ValueError('duplicate JSON key')
            result[key] = value
        return result
    def invalid_constant(_):
        raise ValueError('nonfinite JSON constant')
    return json.loads(data, object_pairs_hook=pairs, parse_constant=invalid_constant)


def binding(path, raw=None):
    raw = bounded_read(path) if raw is None else raw
    return {'sha256': hashlib.sha256(raw).hexdigest(), 'bytes': len(raw)}


def envelope(speed, yaw_rate, prior_forward, proposed_forward, prior_yaw,
             proposed_yaw, horizon, extra_stop, extra_yaw, yaw_uncertainty):
    values = [speed, yaw_rate, prior_forward, proposed_forward, prior_yaw,
              proposed_yaw, horizon, extra_stop, extra_yaw, yaw_uncertainty]
    require_numbers(values)
    if (not 0 < horizon <= 2 or min(speed, prior_forward, proposed_forward,
                                   extra_stop, extra_yaw, yaw_uncertainty) < 0
            or speed > 1 or max(prior_forward, proposed_forward) > .35
            or max(abs(yaw_rate), abs(prior_yaw), abs(proposed_yaw)) > 2
            or max(extra_stop, extra_yaw) > .5 or yaw_uncertainty > math.pi):
        raise ValueError('invalid envelope bounds')
    residual = speed * horizon + extra_stop
    length = (prior_forward + proposed_forward) * horizon
    theta = (abs(yaw_rate) + abs(prior_yaw) + abs(proposed_yaw)) * horizon + extra_yaw + yaw_uncertainty
    fallback = theta >= math.pi / 2
    return {'residual_radius_m': residual, 'forward_length_m': length,
            'heading_half_range_rad': theta,
            'center_region': 'centered_disk' if fallback else 'forward_capsule',
            'center_radius_m': residual + (length if fallback else length * math.sin(theta))}


def center_distance(x, y, model):
    require_numbers([x, y])
    length = 0 if model['center_region'] == 'centered_disk' else model['forward_length_m']
    return math.hypot(x - min(length, max(0, x)), y)


def yaw(quaternion):
    if len(quaternion) != 4:
        raise ValueError('invalid quaternion')
    require_numbers(quaternion)
    if abs(sum(x*x for x in quaternion) - 1) > .01:
        raise ValueError('nonunit quaternion')
    w, x, y, z = quaternion
    return math.atan2(2*(w*z + x*y), 1 - 2*(y*y + z*z))


def angle_delta(a, b):
    return math.atan2(math.sin(a-b), math.cos(a-b))


def observation_valid(row, epoch):
    value = row.get('observation')
    if not isinstance(value, dict) or value.get('epoch') != epoch:
        return False
    if (value.get('pin_violations') or value.get('runtime_error_present')
            or value.get('healthy') is not True):
        return False
    for name in ['command_target', 'applied_command']:
        if len(value[name]) != 3:
            raise ValueError('invalid controls')
        require_numbers(value[name])
    actual = value['oracle_only']
    for name in ['position_m', 'linear_velocity_world_mps', 'angular_velocity_body_rps']:
        if len(actual[name]) != 3:
            raise ValueError('invalid oracle vector')
        require_numbers(actual[name])
    yaw(actual['quaternion_wxyz'])
    require_numbers([value['simulation_time_s']])
    return True


def load_rows(directory):
    summary_path = directory/'summary.json'
    samples_path = directory/'samples.ndjson.gz'
    summary_raw = bounded_read(summary_path, 1 << 20)
    summary = decode(summary_raw)
    if samples_path.exists() and (directory/'samples.ndjson').exists():
        raise ValueError('ambiguous compressed and plain sample inputs')
    compressed = None
    if samples_path.exists():
        compressed = bounded_read(samples_path)
        with gzip.GzipFile(fileobj=io.BytesIO(compressed)) as stream:
            raw = stream.read(MAX_FILE + 1)
    else:
        samples_path=directory/'samples.ndjson'
        raw=bounded_read(samples_path)
    if len(raw) > MAX_FILE or hashlib.sha256(raw).hexdigest() != summary['sample_file_sha256']:
        raise ValueError('sample size or digest mismatch')
    lines = raw.splitlines()
    if not 0 < len(lines) <= MAX_ROWS or any(len(line) > MAX_ROW for line in lines):
        raise ValueError('sample count or line bound exceeded')
    rows = [decode(line) for line in lines]
    previous_end = -1
    previous_sim = None
    epoch = summary['expected']['epoch']
    for i, row in enumerate(rows):
        start, end = row['request_started_offset_ms'], row['response_completed_offset_ms']
        require_numbers([start, end])
        if (type(row['sequence']) is not int or row['sequence'] != i+1
                or not 0 <= start <= end <= 121000 or start < previous_end):
            raise ValueError('sample sequence or monotonic interval invalid')
        previous_end = end
        if observation_valid(row, epoch):
            current_sim = row['observation']['simulation_time_s']
            if previous_sim is not None and current_sim < previous_sim:
                raise ValueError('same-epoch simulation clock regressed')
            previous_sim = current_sim
    return rows, summary, {'summary': binding(summary_path, summary_raw),
                           'samples_file': dict(binding(samples_path,compressed if compressed is not None else raw),
                                                filename=samples_path.name,compression='gzip' if compressed is not None else 'none'),
                           'uncompressed_samples': {'bytes':len(raw), 'sha256':hashlib.sha256(raw).hexdigest()}}


def window(rows, index, horizon, epoch, max_gap_ms=MAX_GAP_MS):
    """Include every later status interval that could overlap the H-second horizon.

    A status instant lies somewhere in its request/response interval. The next
    sample starting after anchor.response+H brackets the window end; it is not
    counted as motion within the window. No interpolation fills sample gaps.
    """
    require_numbers([horizon,max_gap_ms])
    if not 0<horizon<=2 or not 0<max_gap_ms<=1000:
        raise ValueError('horizon or gap bound invalid')
    anchor = rows[index]
    deadline = anchor['response_completed_offset_ms'] + horizon*1000
    selected = [anchor]
    boundary = None
    for row in rows[index+1:]:
        if row['request_started_offset_ms'] > deadline:
            boundary = row
            break
        selected.append(row)
    observed = selected + ([boundary] if boundary else [])
    errors = sum(not observation_valid(row, epoch) for row in observed)
    gaps = [b['request_started_offset_ms']-a['request_started_offset_ms']
            for a, b in zip(observed, observed[1:])]
    quality = ('unknown_incomplete_horizon' if boundary is None else
               'unknown_invalid_observation' if errors else
               'unknown_sampling_gap' if max(gaps, default=0) > max_gap_ms else 'sampled_horizon_bracketed')
    simulation_times=[r['observation']['simulation_time_s'] for r in selected if observation_valid(r,epoch)]
    return selected, {'quality':quality, 'future_sample_count':len(selected)-1,
                      'horizon_clock':'observer_host_monotonic_request_offsets',
                      'anchor_request_interval_ms':[anchor['request_started_offset_ms'],anchor['response_completed_offset_ms']],
                      'possible_horizon_end_ms':deadline,
                      'following_bracket_start_ms':None if boundary is None else boundary['request_started_offset_ms'],
                      'sampled_response_time_span_ms':selected[-1]['response_completed_offset_ms']-anchor['response_completed_offset_ms'],
                      'sampled_simulation_time_span_s':None if len(simulation_times)<2 else simulation_times[-1]-simulation_times[0],
                      'maximum_request_start_gap_ms':max(gaps, default=None),
                      'invalid_samples_including_bracket':errors}


def motion_metrics(samples, reference_position, reference_yaw):
    positions = [r['observation']['oracle_only']['position_m'] for r in samples]
    headings = [yaw(r['observation']['oracle_only']['quaternion_wxyz']) for r in samples]
    return {'max_center_displacement_m':max(math.hypot(p[0]-reference_position[0],p[1]-reference_position[1]) for p in positions),
            'sampled_path_length_lower_bound_m':sum(math.hypot(b[0]-a[0],b[1]-a[1]) for a,b in zip(positions,positions[1:])),
            'max_heading_offset_rad':max(abs(angle_delta(h, reference_yaw)) for h in headings),
            'sampled_heading_travel_lower_bound_rad':sum(abs(angle_delta(b,a)) for a,b in zip(headings,headings[1:]))}


def control_exposure(rows, epoch):
    valid=[r for r in rows if observation_valid(r,epoch)]
    result={'valid_samples':len(valid),'target_nonzero_counts':[], 'applied_nonzero_counts':[],
            'target_max_absolute':[], 'applied_max_absolute':[],
            'continuous_exposure_duration_known':False}
    for field,prefix in [('command_target','target'),('applied_command','applied')]:
        result[prefix+'_nonzero_counts']=[sum(abs(r['observation'][field][axis])>1e-9 for r in valid) for axis in range(3)]
        result[prefix+'_max_absolute']=[max((abs(r['observation'][field][axis]) for r in valid),default=None) for axis in range(3)]
    forward=[r for r in valid if r['observation']['command_target'][0]>1e-9]
    result['forward_target_observation_count']=len(forward)
    result['first_forward_target_response_ms']=forward[0]['response_completed_offset_ms'] if forward else None
    result['last_forward_target_response_ms']=forward[-1]['response_completed_offset_ms'] if forward else None
    result['first_to_last_forward_observation_span_ms']=(forward[-1]['response_completed_offset_ms']-forward[0]['response_completed_offset_ms']) if forward else None
    result['after_last_forward_sampled_targets_zero']=None
    if forward:
        later=[r for r in valid if r['sequence']>forward[-1]['sequence']]
        result['after_last_forward_sampled_targets_zero']=bool(later) and all(all(abs(x)<=1e-9 for x in r['observation']['command_target']) for r in later)
        result['first_later_zero_response_ms']=next((r['response_completed_offset_ms'] for r in later if all(abs(x)<=1e-9 for x in r['observation']['command_target'])),None)
    result['max_sampled_planar_speed_mps']=max((math.hypot(*r['observation']['oracle_only']['linear_velocity_world_mps'][:2]) for r in valid),default=None)
    result['interpretation']='Counts and first/last sampled spans are not continuous command durations. Applied target is not actual body velocity.'
    return result


def configured_directional_windows(rows, summary, profile, forward_cap):
    """A deliberately loose configuration upper bound, not a reconstructed decision."""
    require_numbers([forward_cap])
    if not 0<forward_cap<=.35:raise ValueError('invalid forward cap')
    results=[]
    for index,row in enumerate(rows):
        samples,quality=window(rows,index,profile['horizon_s'],summary['expected']['epoch'])
        result={'sequence':row['sequence'],**quality}
        if quality['quality']!='sampled_horizon_bracketed':results.append(result);continue
        controls=[r['observation'][key] for r in samples for key in ['command_target','applied_command']]
        if any(c[0]<-1e-9 or c[0]>forward_cap+1e-9 or abs(c[1])>1e-9 or abs(c[2])>profile['max_yaw_rate_rps']+1e-9 for c in controls):
            result['quality']='unknown_control_outside_model';results.append(result);continue
        actual=row['observation']['oracle_only'];heading=yaw(actual['quaternion_wxyz']);position=actual['position_m']
        model=envelope(math.hypot(*actual['linear_velocity_world_mps'][:2]),actual['angular_velocity_body_rps'][2],
                       forward_cap,forward_cap,profile['max_yaw_rate_rps'],profile['max_yaw_rate_rps'],
                       profile['horizon_s'],profile['extra_stop_m'],profile['extra_stop_rad'],0)
        distances=[]
        for candidate in samples:
            point=candidate['observation']['oracle_only']['position_m'];dx=point[0]-position[0];dy=point[1]-position[1]
            distances.append(center_distance(math.cos(heading)*dx+math.sin(heading)*dy,-math.sin(heading)*dx+math.cos(heading)*dy,model))
        movement=motion_metrics(samples,position,heading)
        result.update(model=model,sampled_forward_target_or_applied_present=any(c[0]>1e-9 for c in controls),
                      max_sampled_distance_to_segment_m=max(distances),
                      minimum_sampled_center_margin_m=model['center_radius_m']-max(distances),
                      sampled_center_within_configured_envelope=max(distances)<=model['center_radius_m'],
                      sampled_heading_within_configured_envelope=movement['max_heading_offset_rad']<=model['heading_half_range_rad'],**movement)
        results.append(result)
    known=[r for r in results if 'model' in r];exposed=[r for r in known if r['sampled_forward_target_or_applied_present']]
    return {'basis':'Oracle initial pose/motion; prior and proposed targets each set to the commissioned maxima. This upper bound is wider than a particular decision envelope and does not reconstruct dispatch history.',
            'horizon_clock':'observer_host_monotonic_request_offsets',
            'simulation_velocity_time_normalization_performed':False,
            'forward_cap_mps':forward_cap,'evaluated_windows':len(known),'windows_with_sampled_forward_exposure':len(exposed),
            'sampled_center_exceedances':sum(not r['sampled_center_within_configured_envelope'] for r in known),
            'sampled_heading_exceedances':sum(not r['sampled_heading_within_configured_envelope'] for r in known),
            'minimum_sampled_center_margin_m':min((r['minimum_sampled_center_margin_m'] for r in known),default=None),
            'exposure_max_sampled_center_displacement_m':max((r['max_center_displacement_m'] for r in exposed),default=None),
            'quality_counts':{q:sum(r['quality']==q for r in results) for q in sorted({r['quality'] for r in results})},
            'windows':results,'calibration_passed':False,'continuous_containment_proven':False}


def evaluate_windows(rows, summary, profile):
    results = []
    for index, row in enumerate(rows):
        samples, quality = window(rows,index,profile['horizon_s'],summary['expected']['epoch'])
        result = {'sequence':row['sequence'], **quality}
        if quality['quality'] != 'sampled_horizon_bracketed':
            results.append(result)
            continue
        if any(abs(r['observation'][key][2])>profile['max_yaw_rate_rps']+1e-9
               for r in samples for key in ['command_target','applied_command']):
            result['quality']='unknown_control_outside_model'
            results.append(result)
            continue
        value = row['observation'];actual = value['oracle_only']
        forward = any(abs(r['observation'][key][axis]) > 1e-9 for r in samples
                      for key in ['command_target','applied_command'] for axis in [0,1])
        result['sampled_forward_or_lateral_target_present'] = forward
        result['initial_target'] = value['command_target']
        result['initial_applied_target'] = value['applied_command']
        velocity = actual['linear_velocity_world_mps']
        result['initial_actual_world_velocity_mps'] = velocity
        result['initial_actual_body_yaw_rate_rps'] = actual['angular_velocity_body_rps'][2]
        result['sampled_yaw_target_or_applied_present']=any(abs(r['observation'][key][2])>1e-9 for r in samples for key in ['command_target','applied_command'])
        if forward:
            result['quality'] = 'not_zero_forward_diagnostic'
            results.append(result)
            continue
        measured = math.hypot(*velocity[:2])
        model = envelope(measured,actual['angular_velocity_body_rps'][2],0,0,
                         profile['max_yaw_rate_rps'],profile['max_yaw_rate_rps'],
                         profile['horizon_s'],profile['extra_stop_m'],profile['extra_stop_rad'],0)
        movement = motion_metrics(samples,actual['position_m'],yaw(actual['quaternion_wxyz']))
        result.update(model=model,**movement)
        result['sampled_center_within_residual_disk'] = movement['max_center_displacement_m'] <= model['residual_radius_m']
        result['sampled_path_lower_bound_within_residual_budget'] = movement['sampled_path_length_lower_bound_m'] <= model['residual_radius_m']
        result['sampled_heading_within_configured_command_upper_bound'] = movement['max_heading_offset_rad'] <= model['heading_half_range_rad']
        results.append(result)
    counts = {}
    for row in results:counts[row['quality']] = counts.get(row['quality'],0)+1
    known = [r for r in results if 'model' in r]
    return {'quality_counts':counts,'evaluated_windows':len(known),
            'horizon_clock':'observer_host_monotonic_request_offsets',
            'simulation_velocity_time_normalization_performed':False,
            'evaluated_windows_with_sampled_yaw_control':sum(r['sampled_yaw_target_or_applied_present'] for r in known),
            'sampled_center_violations':sum(not r['sampled_center_within_residual_disk'] for r in known),
            'sampled_path_lower_bound_violations':sum(not r['sampled_path_lower_bound_within_residual_budget'] for r in known),
            'sampled_heading_upper_bound_violations':sum(not r['sampled_heading_within_configured_command_upper_bound'] for r in known),
            'max_sampled_center_displacement_m':max((r['max_center_displacement_m'] for r in known),default=None),
            'max_sampled_path_length_lower_bound_m':max((r['sampled_path_length_lower_bound_m'] for r in known),default=None),
            'max_sampled_heading_offset_rad':max((r['max_heading_offset_rad'] for r in known),default=None),
            'windows':results,
            'basis':'Initial oracle velocity/pose, L=0 conditional on sampled zero forward/lateral targets. Angular budget uses two configured yaw-command maxima, not recovered prior/proposed commands.',
            'continuous_containment_proven':False,'forward_dynamics_validated':False}


def consumed_e(rows, summary, profile, observations):
    results=[]
    valid_indices=[i for i,r in enumerate(rows) if observation_valid(r,summary['expected']['epoch'])]
    for observation in observations:
        age=observation['odometry_epoch_age_s']
        if not valid_indices:
            results.append({'capture_sequence':observation['capture_sequence'],'quality':'unknown_no_valid_oracle'})
            continue
        index=min(valid_indices,key=lambda i:abs(rows[i]['observation']['simulation_time_s']-age))
        offset=(rows[index]['observation']['simulation_time_s']-age)*1000
        result={'capture_sequence':observation['capture_sequence'],'logged_postcapture_epoch_age_s':age,
                'nearest_oracle_sequence':rows[index]['sequence'],'nearest_oracle_minus_epoch_age_ms':offset,
                'odom_captured_unix_ns':observation['odom_captured_unix_ns'],
                'scan_captured_unix_ns':observation['scan_captured_unix_ns']}
        if abs(offset)>150:
            result['quality']='unknown_pair_outside_150ms';results.append(result);continue
        samples,quality=window(rows,index,profile['horizon_s'],summary['expected']['epoch'])
        result.update(quality)
        if quality['quality']!='sampled_horizon_bracketed':results.append(result);continue
        if any(abs(r['observation'][key][2])>profile['max_yaw_rate_rps']+1e-9
               for r in samples for key in ['command_target','applied_command']):
            result['quality']='unknown_control_outside_model';results.append(result);continue
        if any(abs(r['observation'][key][axis])>1e-9 for r in samples
               for key in ['command_target','applied_command'] for axis in [0,1]):
            result['quality']='not_zero_forward_diagnostic';results.append(result);continue
        v=observation['body_velocity'];pose=observation['pose']
        model=envelope(math.hypot(v[0],v[1]),v[2],0,0,profile['max_yaw_rate_rps'],profile['max_yaw_rate_rps'],
                       profile['horizon_s'],profile['extra_stop_m'],profile['extra_stop_rad'],observation['yaw_uncertainty_rad'])
        movement=motion_metrics(samples,[pose['X'],pose['Y']],pose['Yaw'])
        limit=model['residual_radius_m']+observation['position_uncertainty_m']
        result.update(model=model,reported_position_uncertainty_m=observation['position_uncertainty_m'],
                      sampled_center_limit_m=limit,**movement,
                      sampled_center_within_limit=movement['max_center_displacement_m']<=limit,
                      sampled_heading_within_limit=movement['max_heading_offset_rad']<=model['heading_half_range_rad'])
        results.append(result)
    known=[r for r in results if 'model' in r]
    return {'rows':results,'evaluated':len(known),'unknown':len(results)-len(known),
            'quality_counts':{q:sum(r['quality']==q for r in results) for q in sorted({r['quality'] for r in results})},
            'unevaluated_count_includes_forward_exclusion':True,
            'sampled_center_exceedances':sum(not r['sampled_center_within_limit'] for r in known),
            'sampled_heading_exceedances':sum(not r['sampled_heading_within_limit'] for r in known),
            'interpretation':'Asynchronous centre/heading diagnostic with reported uncertainty; no full-footprint, calibrated accuracy, exact command-history or forward-dynamics proof.'}


def validate_consumed(observations, fault):
    if not isinstance(observations,list) or not 0<len(observations)<=MAX_ROWS:
        raise ValueError('consumed count bound')
    previous=0
    for value in observations:
        if (value['event']!='executor_observation' or value['simulation'] is not True
                or value['operational_authority'] is not False
                or value['run_id']!=fault['spec']['go2']['run_id']
                or value['task_id']!=fault['task_id'] or value['executor_epoch']!=fault['executor_epoch']
                or type(value['capture_sequence']) is not int or value['capture_sequence']<=previous):
            raise ValueError('consumed identity or sequence mismatch')
        previous=value['capture_sequence']
        pose=value['pose'];velocity=value['body_velocity']
        if len(velocity)!=3:raise ValueError('consumed velocity shape')
        require_numbers([pose['X'],pose['Y'],pose['Yaw'],*velocity,value['odometry_epoch_age_s'],
                         value['position_uncertainty_m'],value['yaw_uncertainty_rad']])
        if min(value['odometry_epoch_age_s'],value['position_uncertainty_m'],value['yaw_uncertainty_rad'])<0:
            raise ValueError('negative consumed age or uncertainty')
        for field in ['odom_captured_unix_ns','scan_captured_unix_ns']:
            if type(value[field]) is not int or value[field]<=0:raise ValueError('invalid capture timestamp')


def require_identity_alignment(alignment, frame):
    if (alignment.get('schema')!='wendy.world.go2-alignment.v1'
            or alignment.get('source_frame')!='odom' or alignment.get('target_frame')!=frame
            or alignment.get('reference_point')!='base_footprint'
            or alignment.get('synthetic') is not True
            or alignment.get('translation_xy_m')!=[0,0] or alignment.get('yaw_rad')!=0):
        raise ValueError('only explicit historical identity alignment is evaluated')


def comparable_spec(spec):
    result=dict(spec)
    result.setdefault('trigger',None)
    return result


def analyze(evidence):
    result={'schema':'wendy.world.go2-offline-motion-verification.v1','simulation':True,
            'operational_authority':False,'analysis_source':binding(Path(__file__)),
            'cases':{},'continuous_containment_proven':False,'forward_dynamics_validated':False,
            'required_next_evidence':[
                'Explicitly authorized, bounded controlled forward-and-zero trials on the existing simulator, with a reviewed flat-floor operating area and independent stop path.',
                'Record exact dispatch, retained prior targets, applied targets, capture times and epoch/generation along with independent high-rate pose/velocity observations.',
                'Exercise forward ramps, zero/stop tails, turning-to-forward transitions, mixed residual motion and repeated headings at the commissioned limits.',
                'Predeclare horizon/decomposition and acceptance limits; retain failures and missing intervals without changing assumptions to fit observations.',
                'Keep oracle data out of the planner. Geometry unit tests establish set inclusion under assumptions, not the robot dynamics that must satisfy them.'
            ],'limitations':[
                'Historical D/E targets are yaw/zero only. Sampled zero targets cannot exclude a command between samples.',
                'Status capture instants lie inside request/response intervals. Horizons include every later sample interval that could overlap, with a following sample bracketing the end.',
                'Horizon windows use observer host monotonic time; actual velocities are simulator derivatives. Simulation-time spans are retained separately. Equal host/guest/simulation rates are not assumed or calibrated, and no velocity time normalization is performed.',
                'Nominal 100 ms observations leave unsampled motion. Path/changing-heading sums are lower bounds, not continuous travel bounds.',
                'Initial oracle motion and configured command maxima are diagnostic inputs only; they do not reconstruct the executor’s exact prior/proposed command state.',
                'D’s historical constant odometry uncertainty was invalid at its epoch age. Its oracle-only motion remains a simulation observation, not alignment evidence.',
                'Consumed E time pairing uses a postcapture status age, not the actual capture simulation time. No calibrated temporal or pose bound follows.',
                'Only centre/heading containment is evaluated. No independent full gait-footprint, braking, collision clearance or physical transfer claim is made.'
            ]}
    for name in ['d','e']:
        directory=evidence/'missions'/name
        rows,summary,sources=load_rows(directory/'observer')
        profile_path=directory/'commission/profile.json';profile_raw=bounded_read(profile_path,65536);profile=decode(profile_raw)
        for k in ['horizon_s','extra_stop_m','extra_stop_rad','max_yaw_rate_rps']:require_numbers([profile[k]])
        valid=[r for r in rows if observation_valid(r,summary['expected']['epoch'])]
        case={'sources':dict(sources,profile=binding(profile_path,profile_raw)),'runtime_epoch':summary['expected']['epoch'],
              'profile':{k:profile[k] for k in ['horizon_s','extra_stop_m','extra_stop_rad','max_forward_mps','max_yaw_rate_rps']},
              'retained_samples':len(rows),'valid_observations':len(valid),
              'sampled_nonzero_target_counts':[sum(abs(r['observation']['command_target'][axis])>1e-9 for r in valid) for axis in range(3)],
              'sampled_nonzero_applied_counts':[sum(abs(r['observation']['applied_command'][axis])>1e-9 for r in valid) for axis in range(3)],
              'summary_errors':{k:summary[k] for k in ['request_errors','report_errors','pin_violations','file_cap_reached','interrupted']},
              'control_exposure':control_exposure(rows,summary['expected']['epoch']),
              'oracle_windows':evaluate_windows(rows,summary,profile)}
        if name=='e':
            op=directory/'corridor-e-observations.json';observation_raw=bounded_read(op,1<<20);observations=decode(observation_raw)
            fp=directory/'corridor-e-fault-event.json';fault_raw=bounded_read(fp,1<<20);fault=decode(fault_raw)['snapshot']
            validate_consumed(observations,fault)
            alignment_path=directory/'commission/alignment.json';alignment_raw=bounded_read(alignment_path,65536)
            require_identity_alignment(decode(alignment_raw),fault['spec']['frame'])
            case['sources']['alignment']=binding(alignment_path,alignment_raw)
            case['sources']['consumed_observations']=binding(op,observation_raw)
            case['sources']['fault']=binding(fp,fault_raw)
            case['consumed_pose_windows']=consumed_e(rows,summary,profile,observations)
            state=fault['state'];v=state['body_velocity']
            prior=[fault['controller_previous_command'],state['previous_command']]
            for key in ['last_attempt','last_acknowledged']:
                value=fault[key];prior.append([value['body_vx'],value['body_vy'],value['yaw_rate']])
            model=envelope(math.hypot(v[0],v[1]),v[2],max(0,*(p[0] for p in prior)),.15,max(abs(p[2]) for p in prior),0,
                           profile['horizon_s'],profile['extra_stop_m'],profile['extra_stop_rad'],state['scan']['yaw_uncertainty_rad'])
            case['refused_forward_proposal_math_only']={'proposed_forward_mps':.15,'model':model,
                'executed':False,'empirical_forward_validation':False,
                'retained_prior_commands':prior,
                'basis':'Historical E replay proposed +0.15 m/s but refused it. This numeric directional envelope cannot be tested against a forward trajectory that did not occur.'}
        else:
            case['consumed_pose_windows']={'quality':'unavailable','reason':'D lacks logged postcapture epoch age and a consumed-pose series; no host/guest wall-time equivalence is invented.'}
        result['cases'][name]=case
    return result


def require_calibration_name(name):
    if name not in ('calibration-f','calibration-g','calibration-h'):
        raise ValueError('only explicitly selected F/G/H follow-ups are supported')


def status_qualification(fault, status):
    if fault is None:
        return 'No fault snapshot is present in the supplied artifacts. The durable mission outcome is retained separately and is not a dynamics calibration result or proof that no fault occurred.'
    if fault['stage']=='mission-authority' and status.get('Reason')=='outside authorized area':
        return 'Retained status reason is not substituted for the exact consumed failure. The historical authority-timeout path could persist a misleading outside-area reason.'
    return 'Retain the exact fault stage/failure separately from the durable terminal reason; a local-fault status does not by itself identify the underlying failure.'


def calibration_context(spec, tasks, observations, fault):
    matches=[t for t in tasks if comparable_spec(t['spec'])==comparable_spec(spec)]
    if len(matches)!=1:raise ValueError('exact task status unavailable')
    if fault is not None and (fault['task_id']!=spec['id']
                             or comparable_spec(fault['spec'])!=comparable_spec(spec)):
        raise ValueError('fault and explicitly supplied task differ')
    if not isinstance(observations,list) or len(observations)>MAX_ROWS:
        raise ValueError('consumed count bound')
    epoch=fault['executor_epoch'] if fault is not None else (
        observations[0]['executor_epoch'] if observations else None)
    if epoch is not None and (type(epoch) is not int or epoch<1):
        raise ValueError('invalid consumed executor epoch')
    if observations:
        validate_consumed(observations,{'spec':spec,'task_id':spec['id'],'executor_epoch':epoch})
    return matches[0]['status'],epoch


def analyze_calibration(directory, name, profile_path, spec_path):
    require_calibration_name(name)
    rows,summary,sources=load_rows(directory/(name+'-observer'))
    if summary.get('completed_requested_window') is not True:
        raise ValueError('observer is not terminal with a complete declared window')
    profile_raw=bounded_read(profile_path,65536);profile=decode(profile_raw)
    spec_raw=bounded_read(spec_path,65536);spec=decode(spec_raw)
    if (spec['type']!='wendy.go2.nav.go_to.v1' or spec['domain']!='simulation'
            or spec['mode']!='autonomous' or spec['class']!='MOVE' or spec['fallback']!='stop'
            or spec['go2']['world_epoch']!=summary['expected']['epoch']
            or profile.get('motion_envelope')!='isotropic-v1'):
        raise ValueError('unexpected calibration identity or historical model')
    fault_path=directory/(name+'-fault-event.json');fault_raw=None;fault=None
    if name!='calibration-h' or os.path.lexists(fault_path):
        fault_raw=bounded_read(fault_path,1<<20);fault=decode(fault_raw)['snapshot']
    observations_path=directory/(name+'-observations.json');observations_raw=bounded_read(observations_path,1<<20);observations=decode(observations_raw)
    alignment_path=directory/(name+'-commission')/'alignment.json';alignment_raw=bounded_read(alignment_path,65536)
    require_identity_alignment(decode(alignment_raw),spec['frame'])
    task_path=directory/(name+'-tasks-latest.json');task_raw=bounded_read(task_path,1<<20);tasks=decode(task_raw)['tasks']
    status,executor_epoch=calibration_context(spec,tasks,observations,fault)
    cap=min(profile['max_forward_mps'],spec['speed_mps'])
    sources.update(profile=binding(profile_path,profile_raw),spec=binding(spec_path,spec_raw),
                   consumed_observations=binding(observations_path,observations_raw),
                   alignment=binding(alignment_path,alignment_raw),durable_task_status=binding(task_path,task_raw))
    if fault_raw is not None:sources['fault']=binding(fault_path,fault_raw)
    return {'schema':'wendy.world.go2-offline-calibration-observation.v1','simulation':True,'operational_authority':False,
            'analysis_source':binding(Path(__file__)),'sources':sources,'name':name,'task_id':spec['id'],
            'run_id':spec['go2']['run_id'],'runtime_epoch':summary['expected']['epoch'],
            'executed_model':'isotropic-v1','evaluated_hypothesis':'directional-v1 configured upper envelope',
            'calibration_completed':False,'calibration_passed':False,'forward_dynamics_validated':False,
            'fault':None if fault is None else {'stage':fault['stage'],'failure':fault['failure'],
                     'decision_started_unix_ms':fault['decision_started_unix_ms'],'timing':fault.get('timing')},
            'fault_evidence':'absent_from_supplied_artifacts' if fault is None else 'snapshot_present',
            'consumed_executor_epoch':executor_epoch,
            'recorded_durable_status':status,
            'status_qualification':status_qualification(fault,status),
            'spec_comparison':'Exact decoded fields; absent trigger and explicit null trigger are equivalent Go Spec encodings.',
            'observer_completed':summary['completed_requested_window'],
            'observer_quality':{k:summary[k] for k in ['sample_attempts','retained_samples','retained_status_samples',
                'missed_schedule_slots','request_start_gaps','successful_completion_gaps','elapsed_monotonic_s',
                'requested_duration_s','result','sample_errors_present'] if k in summary},
            'summary_errors':{k:summary[k] for k in ['request_errors','report_errors','pin_violations','file_cap_reached','interrupted']},
            'control_exposure':control_exposure(rows,summary['expected']['epoch']),
            'zero_forward_residual_windows':evaluate_windows(rows,summary,profile),
            'configured_directional_windows':configured_directional_windows(rows,summary,profile,cap),
            'consumed_zero_forward_windows':consumed_e(rows,summary,profile,observations),
            'limitations':[
                'The actual trial used isotropic-v1. Its durable mission outcome and any captured fault are reported separately; neither establishes completion of a dynamics calibration.',
                'The proposed directional model is evaluated only as a configured upper bound on sampled motion. Actual per-round dispatch/prior targets are not reconstructed.',
                'Initial oracle state is used only for offline evaluation, never planner input. A centre/heading comparison does not verify full gait footprint or scan clearance.',
                'Intervals between nominal 100 ms samples and asynchronous consumed poses remain unobserved. Zero sampled exceedances do not establish continuous dynamics containment.',
                'Horizon windows use observer host monotonic time, while oracle velocities are simulator derivatives. Per-window simulation spans remain explicit; no equal-rate assumption or calibrated time normalization is established.',
                'Sampled forward exposure, including a successful mission if observed, does not establish the general forward, braking, transition or repeated-trial assumptions.'
            ]}


def fake_row(n,time_s,x=0,forward=0,error=False):
    row={'sequence':n,'request_started_offset_ms':time_s*1000,'response_completed_offset_ms':time_s*1000+1}
    if not error:row['observation']={'epoch':1,'pin_violations':[],'runtime_error_present':False,'healthy':True,
        'simulation_time_s':time_s,'command_target':[forward,0,0],'applied_command':[forward,0,0],
        'oracle_only':{'position_m':[x,0,0],'quaternion_wxyz':[1,0,0,0],
                       'linear_velocity_world_mps':[0,0,0],'angular_velocity_body_rps':[0,0,0]}}
    return row


class Tests(unittest.TestCase):
    profile={'horizon_s':1,'extra_stop_m':.15,'extra_stop_rad':.15,'max_yaw_rate_rps':.1}
    @staticmethod
    def archive(directory, rows):
        raw=b'\n'.join(json.dumps(r).encode() for r in rows)+b'\n'
        (directory/'samples.ndjson.gz').write_bytes(gzip.compress(raw))
        (directory/'summary.json').write_text(json.dumps({'expected':{'epoch':1},'sample_file_sha256':hashlib.sha256(raw).hexdigest()}))
    def test_directional_segment_and_backward_residual(self):
        m=envelope(0,0,0,.15,0,0,1,.05,0,0)
        self.assertLessEqual(center_distance(.19,0,m),m['center_radius_m'])
        self.assertGreater(center_distance(-.06,0,m),m['center_radius_m'])
    def test_heading_cone_support(self):
        m=envelope(.02,.2,.1,.15,.1,0,1.2,.05,.05,.02)
        for k in range(101):
            a=m['heading_half_range_rad']*(2*k/100-1)
            self.assertLessEqual(center_distance(m['forward_length_m']*math.cos(a),m['forward_length_m']*math.sin(a),m),m['center_radius_m']+1e-12)
    def test_half_pi_fallback(self):
        m=envelope(0,math.pi/2,0,.15,0,0,1,.05,0,0)
        self.assertEqual(m['center_region'],'centered_disk')
        self.assertAlmostEqual(m['center_radius_m'],.2)
    def test_bad_inputs(self):
        for value in [-1,float('nan'),True]:
            with self.assertRaises(ValueError):envelope(value,0,0,0,0,0,1,.05,.05,0)
        with self.assertRaises(ValueError):decode('{"x":1,"x":2}')
        with self.assertRaises(ValueError):decode('{"x":NaN}')
    def test_horizon_has_endpoint_bracket_and_tail_unknown(self):
        rows=[fake_row(i+1,i/10) for i in range(15)]
        selected,q=window(rows,0,1,1)
        self.assertEqual(len(selected),11);self.assertEqual(q['quality'],'sampled_horizon_bracketed')
        self.assertEqual(window(rows,10,1,1)[1]['quality'],'unknown_incomplete_horizon')
    def test_error_and_gap_are_not_success(self):
        rows=[fake_row(i+1,i/10,error=i==4) for i in range(15)]
        self.assertEqual(window(rows,0,1,1)[1]['quality'],'unknown_invalid_observation')
        rows=[fake_row(i+1,t) for i,t in enumerate([0,.1,.4,.5,.6,.7,.8,.9,1,1.1])]
        self.assertEqual(window(rows,0,1,1)[1]['quality'],'unknown_sampling_gap')
    def test_center_violation_and_forward_exclusion(self):
        rows=[fake_row(i+1,i/10,x=.2 if i==5 else 0) for i in range(15)]
        result=evaluate_windows(rows,{'expected':{'epoch':1}},self.profile)
        self.assertGreater(result['sampled_center_violations'],0)
        rows[5]['observation']['command_target'][0]=.1
        self.assertEqual(evaluate_windows(rows,{'expected':{'epoch':1}},self.profile)['windows'][0]['quality'],'not_zero_forward_diagnostic')
    def test_returning_path_can_exceed_budget_inside_disk(self):
        rows=[fake_row(i+1,i/10,x=.1 if i%2 else 0) for i in range(15)]
        result=evaluate_windows(rows,{'expected':{'epoch':1}},self.profile)['windows'][0]
        self.assertTrue(result['sampled_center_within_residual_disk'])
        self.assertFalse(result['sampled_path_lower_bound_within_residual_budget'])
    def test_read_refuses_symlink_fifo_and_size(self):
        with tempfile.TemporaryDirectory() as td:
            p=Path(td)/'input';p.write_bytes(b'123')
            with self.assertRaises(ValueError):bounded_read(p,2)
            link=Path(td)/'link';link.symlink_to(p)
            with self.assertRaises(ValueError):bounded_read(link)
            fifo=Path(td)/'fifo';os.mkfifo(str(fifo))
            with self.assertRaises(ValueError):bounded_read(fifo)
    def test_quaternion_and_wrap(self):
        self.assertAlmostEqual(abs(angle_delta(math.pi-.01,-math.pi+.01)),.02)
        with self.assertRaises(ValueError):yaw([0,0,0,0])
    def test_archive_digest_and_clock_regression(self):
        with tempfile.TemporaryDirectory() as td:
            directory=Path(td);rows=[fake_row(1,0),fake_row(2,.1)]
            self.archive(directory,rows);self.assertEqual(len(load_rows(directory)[0]),2)
            summary=decode((directory/'summary.json').read_bytes());summary['sample_file_sha256']='0'*64
            (directory/'summary.json').write_text(json.dumps(summary))
            with self.assertRaises(ValueError):load_rows(directory)
            rows[1]['observation']['simulation_time_s']=-1;self.archive(directory,rows)
            with self.assertRaises(ValueError):load_rows(directory)
    def test_plain_collector_input_preserves_same_digest(self):
        with tempfile.TemporaryDirectory() as td:
            directory=Path(td);rows=[fake_row(1,0),fake_row(2,.1)];self.archive(directory,rows)
            compressed=directory/'samples.ndjson.gz'
            (directory/'samples.ndjson').write_bytes(gzip.decompress(compressed.read_bytes()))
            with self.assertRaises(ValueError):load_rows(directory)
            compressed.unlink()
            loaded,_,sources=load_rows(directory)
            self.assertEqual(len(loaded),2)
            self.assertEqual(sources['samples_file']['compression'],'none')
    def test_first_sequence_cannot_bypass_interval_validation(self):
        with tempfile.TemporaryDirectory() as td:
            directory=Path(td);rows=[fake_row(2,0)];self.archive(directory,rows)
            with self.assertRaises(ValueError):load_rows(directory)
    def test_changed_epoch_is_unknown_not_motion_success(self):
        rows=[fake_row(i+1,i/10) for i in range(15)]
        rows[4]['observation']['epoch']=2
        result=evaluate_windows(rows,{'expected':{'epoch':1}},self.profile)
        self.assertEqual(result['windows'][0]['quality'],'unknown_invalid_observation')
        self.assertNotIn('sampled_center_within_residual_disk',result['windows'][0])
    def test_consumed_unknown_pair_and_forward_are_retained(self):
        rows=[fake_row(i+1,i/10) for i in range(15)]
        base={'capture_sequence':1,'odometry_epoch_age_s':0,'odom_captured_unix_ns':1,'scan_captured_unix_ns':1,
              'body_velocity':[0,0,0],'pose':{'X':0,'Y':0,'Yaw':0},'yaw_uncertainty_rad':.02,'position_uncertainty_m':.02}
        distant=dict(base,capture_sequence=2,odometry_epoch_age_s=5)
        result=consumed_e(rows,{'expected':{'epoch':1}},self.profile,[base,distant])
        self.assertEqual((result['evaluated'],result['unknown']),(1,1))
        self.assertEqual(result['rows'][1]['quality'],'unknown_pair_outside_150ms')
        rows[5]['observation']['applied_command'][0]=.1
        result=consumed_e(rows,{'expected':{'epoch':1}},self.profile,[base])
        self.assertEqual(result['rows'][0]['quality'],'not_zero_forward_diagnostic')
    def test_no_valid_oracle_does_not_disappear(self):
        rows=[fake_row(i+1,i/10,error=True) for i in range(15)]
        result=consumed_e(rows,{'expected':{'epoch':1}},self.profile,[{'capture_sequence':1,'odometry_epoch_age_s':0}])
        self.assertEqual(result['unknown'],1)
        self.assertEqual(result['rows'][0]['quality'],'unknown_no_valid_oracle')
    def test_forward_sample_evidence_is_not_calibration_success(self):
        rows=[fake_row(i+1,i/10,x=.01*i,forward=.05 if i<5 else 0) for i in range(15)]
        exposure=control_exposure(rows,1)
        self.assertEqual(exposure['forward_target_observation_count'],5)
        self.assertFalse(exposure['continuous_exposure_duration_known'])
        result=configured_directional_windows(rows,{'expected':{'epoch':1}},self.profile,.05)
        self.assertGreater(result['windows_with_sampled_forward_exposure'],0)
        self.assertFalse(result['calibration_passed'])
    def test_negative_command_and_lateral_target_are_outside_model(self):
        for command in [[-.01,0,0],[0,.01,0]]:
            rows=[fake_row(i+1,i/10) for i in range(15)]
            rows[1]['observation']['command_target']=command
            result=configured_directional_windows(rows,{'expected':{'epoch':1}},self.profile,.05)
            self.assertEqual(result['windows'][0]['quality'],'unknown_control_outside_model')
    def test_nonidentity_alignment_is_not_implicitly_applied(self):
        frame={'id':'floor','revision':'one'}
        value={'schema':'wendy.world.go2-alignment.v1','source_frame':'odom','target_frame':frame,
               'reference_point':'base_footprint','synthetic':True,'translation_xy_m':[0,0],'yaw_rad':0}
        require_identity_alignment(value,frame)
        value['translation_xy_m']=[.1,0]
        with self.assertRaises(ValueError):require_identity_alignment(value,frame)
    def test_only_missing_null_trigger_is_equivalent(self):
        value={'id':'one','type':'go2'}
        self.assertEqual(comparable_spec(value),comparable_spec(dict(value,trigger=None)))
        self.assertNotEqual(comparable_spec(value),comparable_spec(dict(value,extra=None)))
    def test_yaw_cap_exceedance_is_unknown_in_zero_forward_windows(self):
        rows=[fake_row(i+1,i/10) for i in range(15)]
        rows[3]['observation']['command_target'][2]=.2
        result=evaluate_windows(rows,{'expected':{'epoch':1}},self.profile)
        self.assertEqual(result['windows'][0]['quality'],'unknown_control_outside_model')
        self.assertNotIn('model',result['windows'][0])
    def test_simulation_rate_is_recorded_not_assumed(self):
        rows=[fake_row(i+1,i/10) for i in range(15)]
        for row in rows:row['observation']['simulation_time_s']*=2
        _,quality=window(rows,0,1,1)
        self.assertEqual(quality['horizon_clock'],'observer_host_monotonic_request_offsets')
        self.assertAlmostEqual(quality['sampled_simulation_time_span_s'],2)
        self.assertAlmostEqual(quality['sampled_response_time_span_ms'],1000)
    def test_calibration_scope_and_fault_qualification(self):
        require_calibration_name('calibration-f');require_calibration_name('calibration-g');require_calibration_name('calibration-h')
        with self.assertRaises(ValueError):require_calibration_name('../calibration-g')
        with self.assertRaises(ValueError):require_calibration_name('calibration-i')
        self.assertIn('historical authority-timeout',status_qualification({'stage':'mission-authority'},{'Reason':'outside authorized area'}))
        self.assertNotIn('authority-timeout',status_qualification({'stage':'dispatch'},{'Reason':'executor reported local fault'}))
    def test_calibration_without_fault_retains_status_without_inventing_epoch(self):
        spec={'id':'task','go2':{'run_id':'run'}}
        for phase in ['succeeded','failed','running']:
            status={'Phase':phase,'Version':3}
            observed,epoch=calibration_context(spec,[{'spec':spec,'status':status}],[],None)
            self.assertEqual(observed,status);self.assertIsNone(epoch)
            self.assertIn('not a dynamics calibration',status_qualification(None,status))
        with self.assertRaises(ValueError):calibration_context(spec,[],[],None)
        with self.assertRaises(ValueError):calibration_context(spec,[{'spec':dict(spec,id='other'),'status':status}],[],None)
    def test_calibration_without_fault_still_binds_each_consumed_observation(self):
        import copy
        spec={'id':'task','go2':{'run_id':'run'}}
        tasks=[{'spec':spec,'status':{'Phase':'succeeded'}}]
        observation={'event':'executor_observation','simulation':True,'operational_authority':False,
            'run_id':'run','task_id':'task','executor_epoch':1,'capture_sequence':1,
            'pose':{'X':0,'Y':0,'Yaw':0},'body_velocity':[0,0,0],'odometry_epoch_age_s':1,
            'position_uncertainty_m':.1,'yaw_uncertainty_rad':.1,'odom_captured_unix_ns':1,'scan_captured_unix_ns':1}
        self.assertEqual(calibration_context(spec,tasks,[observation],None)[1],1)
        for field,value in [('run_id','other'),('task_id','other'),('executor_epoch',2)]:
            second=copy.deepcopy(observation);second.update(capture_sequence=2);second[field]=value
            with self.assertRaises(ValueError):calibration_context(spec,tasks,[observation,second],None)
        with self.assertRaises(ValueError):calibration_context(spec,tasks,[dict(observation,executor_epoch=0)],None)
        with self.assertRaises(ValueError):calibration_context(spec,tasks,[observation],{'task_id':'other','spec':spec,'executor_epoch':1})


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    sub=parser.add_subparsers(dest='mode',required=True)
    sub.add_parser('self-test')
    command=sub.add_parser('analyze')
    command.add_argument('--evidence',type=Path,required=True)
    command.add_argument('--out',type=Path,required=True)
    command=sub.add_parser('calibration')
    command.add_argument('--directory',type=Path,required=True)
    command.add_argument('--name',choices=['calibration-f','calibration-g','calibration-h'],required=True)
    command.add_argument('--profile',type=Path,required=True)
    command.add_argument('--spec',type=Path,required=True)
    command.add_argument('--out',type=Path,required=True)
    args=parser.parse_args()
    if args.mode=='self-test':
        outcome=unittest.TextTestRunner(verbosity=2).run(unittest.defaultTestLoader.loadTestsFromTestCase(Tests))
        raise SystemExit(0 if outcome.wasSuccessful() else 1)
    result=(analyze(args.evidence) if args.mode=='analyze' else
            analyze_calibration(args.directory,args.name,args.profile,args.spec))
    encoded=(json.dumps(result,indent=2,allow_nan=False)+'\n').encode()
    if len(encoded)>8<<20:raise ValueError('output bound exceeded')
    fd=os.open(str(args.out),os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
    with os.fdopen(fd,'wb') as stream:stream.write(encoded)
    print(json.dumps({'schema':result['schema'],'cases':len(result.get('cases',{})),
                      'continuous_containment_proven':False,'forward_dynamics_validated':False,
                      'output':binding(args.out)}))


if __name__=='__main__':main()
