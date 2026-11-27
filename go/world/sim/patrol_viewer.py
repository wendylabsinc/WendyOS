"""Display-only native twin bridge for the detached Go2 patrol services.

Physics truth goes only to rendering and evaluation.jsonl. Mission control uses
Nav2's AMCL estimate. Closing the app never terminates the mission or this bridge.
"""
import argparse
import concurrent.futures
import copy
import hashlib
import json
import math
import os
from pathlib import Path
import time
import urllib.request


def request(base, path, body=None, token=None, timeout=3):
    data = None if body is None else json.dumps(body).encode()
    headers = {'Content-Type': 'application/json'}
    if token:
        headers['Authorization'] = 'Bearer ' + token
    with urllib.request.urlopen(urllib.request.Request(base + path, data=data, headers=headers), timeout=timeout) as response:
        return json.load(response)


def atomic(path, value):
    temp = path.with_name('.' + path.name + '.next')
    with temp.open('w') as stream:
        json.dump(value, stream, allow_nan=False, separators=(',', ':'))
    temp.chmod(0o600)
    os.replace(temp, path)


def fusion_view(output, session, evidence, now=None):
    if not output:
        return None
    result = output['result']
    hazard = result.get('hazard')
    observations = []
    fresh = now is None or 0 <= now - output['now_ns']/1e9 <= 2
    for item in output.get('observations', []):
        x, y, z = item['position_m']
        obs = dict(id=item['sensor_id'], kind=item['kind'], label=item['sensor_id'],
                   position=[x, -z, y], active=fresh)
        if item['kind'] == 'gas':
            obs.update(value=item['gas']['value_ppm'], unit='ppm', label='Gas · simulated')
        if item.get('evidence'):
            ref = item['evidence']; suffix = {'image/png': '.png', 'audio/wav': '.wav'}.get(ref['content_type'])
            if suffix:
                name = ref['sha256'] + suffix
                target = session / 'evidence' / name
                source = evidence / name
                if not target.exists() and source.exists():
                    raw = source.read_bytes()
                    if len(raw) == ref['size'] and hashlib.sha256(raw).hexdigest() == ref['sha256']:
                        target.write_bytes(raw); target.chmod(0o600)
                if item['kind'] == 'camera' and target.exists():
                    obs['evidence_path'] = 'evidence/' + name
        observations.append(obs)
    value = dict(state=result['state'], label='Localized warning' if hazard else 'Passive observation',
                 reason=result.get('reason', ''), sample_time_s=output['now_ns']/1e9, observations=observations)
    if not fresh:
        value['label'] = 'Recorded observation'
    if hazard:
        x, y, z = hazard['position_m']
        value.update(position=[x, -z, y], uncertainty_m=hazard['uncertainty_m'], confidence=hazard['confidence'],
                     basis='Existing simulated multimodal fusion')
    return value


def display(physical, mission, sequence, command_sequence, session, evidence):
    now = physical['stamp']; nav = mission.get('navigation') or {}
    patrol = copy.deepcopy(mission['patrol'])
    patrol['expires_at_s'] = now + max(0, patrol['expires_at_unix_ms']/1000-time.time())
    if patrol.get('return') and patrol['return'].get('begin_at_unix_ms'):
        patrol['return']['begin_at_s'] = now + patrol['return']['begin_at_unix_ms']/1000-time.time()
    for room in patrol['rooms']:
        until = room.pop('blocked_until_unix_ms', 0)
        if until:
            room['blocked_until_s'] = now + max(0, until/1000-time.time())
        if 'next_due_unix_ms' in room:
            room['next_due_s'] = now + room.pop('next_due_unix_ms')/1000-time.time()
        room['evidence'] = room.get('evidence') or []
        room.pop('candidates', None)
    references = {ref for room in patrol['rooms'] for ref in room['evidence']}
    references.update(ref for finding in patrol.get('recent_findings', []) for ref in finding.get('evidence', []))
    for ref in references:
        if len(ref)!=64 or any(c not in '0123456789abcdef' for c in ref):
            continue
        for suffix in ('.png','.wav','.json'):
            target = session/'evidence'/(ref+suffix)
            source = evidence/(ref+suffix)
            if not target.exists() and source.is_file():
                raw = source.read_bytes()
                if len(raw)<=8<<20 and hashlib.sha256(raw).hexdigest()==ref:
                    target.write_bytes(raw);target.chmod(0o600)
    patrol['exclusions']=mission.get('restricted_areas')
    patrol['offline']=mission.get('offline_kit')
    # Preserve adapter diagnostics and their separate clock domains without inference.
    if nav.get('localization_health') is not None:
        patrol['localization_health'] = copy.deepcopy(nav['localization_health'])
    pose = nav.get('pose') or {}; covariance = nav.get('covariance') or []
    if len(covariance) == 36:
        patrol['localization'] = dict(position=[pose.get('x', 0), pose.get('y', 0), .2], yaw=pose.get('yaw', 0),
            covariance=[covariance[i] for i in (0,1,6,7)], valid=bool(nav.get('ready')))
    route = [[p[0], p[1], .06] for p in nav.get('route') or []] if patrol['state'] in ('navigating','returning') else []
    robot = dict(id='go2', name='Go2', color=[.12,.5,.7], position=physical['position'],
        velocity=physical['linear_velocity_world'], online=True, available=True, state=patrol['state'],
        job_id=patrol.get('current_room',''), path=route, lidar_points=physical.get('lidar_points',[]),
        local_voxels=0, peer_voxels=0, route=[])
    terminal = patrol['state'] in ('failed','stopped','expired','completed')
    snapshot = dict(run_id=physical['run_id'], time_s=now, phase=patrol['state'], authorized=not terminal,
        stopped=terminal or patrol['state'] in ('paused','work_held','return_held','environment_held','world_held','tour_held'), fault=physical.get('fault'), robots=[robot], jobs=[],
        completed=sum(r['visits'] for r in patrol['rooms']), total=len(patrol['rooms']), events=[], voxels=[],
        voxel_size=.1, map_node='go2', network=dict(edges=[], internet=dict(id='internet', a='go2', b='cloud',
        enabled=False, up=False, packets=0), gossip_rounds=0,peer_voxels={}), controls_available=True, patrol=patrol)
    fusion = fusion_view(mission.get('fusion'), session, evidence, now)
    if fusion:
        snapshot['fusion'] = fusion
    return dict(schema='wendy.macos-demo.state.v1', mode='go2-patrol', run_id=physical['run_id'], sequence=sequence,
        worker_status='running', last_command_sequence=command_sequence, snapshot=snapshot,
        dynamic_geoms=physical['dynamic_geoms'])


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--physics', required=True); p.add_argument('--mission', required=True)
    p.add_argument('--executor-dir', required=True, type=Path); p.add_argument('--session', required=True, type=Path)
    args = p.parse_args(); os.umask(0o077)
    args.session.mkdir(mode=0o700, parents=True, exist_ok=False)
    for name in ('commands','receipts','evidence'):
        (args.session/name).mkdir(mode=0o700)
    geometry = request(args.physics, '/geometry', timeout=20)
    run_id = geometry['run_id']; atomic(args.session/'geometry.json', geometry)
    atomic(args.session/'bridge-session.json',dict(schema='wendy.macos-demo.session.v1',run_id=run_id,pid=os.getpid(),restart='refused'))
    token = (args.executor_dir/'control-token').read_text().strip()
    sequence = command_sequence = 0
    with concurrent.futures.ThreadPoolExecutor(2) as pool, (args.session/'evaluation.jsonl').open('x') as trace:
        while not (args.session/'bridge-stop').exists():
            started = time.monotonic()
            path = args.session/'commands'/f'{command_sequence+1:06d}.json'
            if path.exists():
                action = ''; error = None
                try:
                    if path.is_symlink() or path.stat().st_size>4096: raise ValueError('invalid command file')
                    command = json.loads(path.read_text())
                    action = command.get('action', '')
                    if (set(command) != {'schema','run_id','sequence','action','target'} or
                        command['schema']!='wendy.macos-demo.command.v1' or command['run_id']!=run_id or
                        command['sequence']!=command_sequence+1 or command['target']!='' or
                        action not in ('patrol_pause','patrol_resume','patrol_stop','patrol_recall')): raise ValueError('invalid patrol command')
                    request(args.mission, '/control', {'action':action.removeprefix('patrol_')}, token, timeout=15)
                except Exception as exc: error = str(exc)
                command_sequence += 1
                atomic(args.session/'receipts'/f'{command_sequence:06d}.json',dict(schema='wendy.macos-demo.command-result.v1',
                    run_id=run_id, sequence=command_sequence,action=action,accepted=error is None,error=error))
            try:
                physical_future = pool.submit(request,args.physics,'/state')
                mission_future = pool.submit(request,args.mission,'/status')
                physical, mission = physical_future.result(),mission_future.result()
                if physical['run_id']!=run_id or mission['run_id']!=run_id: raise ValueError('runtime restarted or scope changed')
                sequence += 1
                atomic(args.session/'state.json',display(physical,mission,sequence,command_sequence,args.session,args.executor_dir/'evidence'))
                if sequence==1:
                    atomic(args.session/'bridge-ready.json',dict(schema='wendy.macos-demo.bridge-ready.v1',run_id=run_id,synthetic=True))
                if sequence%5==0:
                    nav = mission.get('navigation') or {}
                    trace.write(json.dumps(dict(wall_unix_ms=int(time.time()*1000),time_s=physical['stamp'],
                        truth=physical['position'],quaternion=physical['quaternion_wxyz'],joints=physical['joints']['q'],
                        foot_contacts=physical['foot_contacts'],estimate=nav.get('pose'),ready=nav.get('ready'),
                        patrol_state=mission['patrol']['state'],work=mission['patrol'].get('work'),environment=mission['patrol'].get('environment'),world_watch=mission['patrol'].get('world_watch'),visits={r['id']:r['visits'] for r in mission['patrol']['rooms']}))+'\n'); trace.flush()
            except Exception as exc:
                atomic(args.session/'bridge-error.json',dict(error=str(exc),unix_ms=int(time.time()*1000)))
            time.sleep(max(0,.1-(time.monotonic()-started)))

if __name__=='__main__': main()
