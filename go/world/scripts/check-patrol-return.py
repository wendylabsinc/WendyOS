#!/usr/bin/env python3
"""Read-only acceptance of an owned articulated simulation's signed patrol return.

Use an existing run receipt after native recall, or wait for its signed return
window. This tool never sends a control, changes a pose or adjusts policy.
"""
import argparse
import base64
import json
import math
import os
from pathlib import Path
import time
import urllib.request


def get(base, path):
    with urllib.request.urlopen(base + path, timeout=4) as response:
        return json.load(response)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--run', type=Path, required=True)
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--wait-seconds', type=int, default=180)
    args = parser.parse_args()
    if not 1 <= args.wait_seconds <= 600:
        parser.error('wait must be 1..600 seconds')
    root = Path(__file__).resolve().parents[1]
    out = args.out.resolve()
    if out.is_relative_to(root):
        parser.error('runtime evidence must remain outside Git')
    os.umask(0o077)
    out.mkdir(parents=True, exist_ok=False)
    receipt = json.loads((args.run/'run.json').read_text())
    if receipt.get('hardware') is not False or receipt.get('simulation') is not True:
        raise ValueError('expected an isolated simulation receipt')
    deadline = time.monotonic() + args.wait_seconds
    with (out/'observations.jsonl').open('x') as log:
        while True:
            state = get(receipt['mission_url'], '/status')
            physical = get(receipt['physics_url'], '/state')
            if state['run_id'] != receipt['run_id'] or physical['run_id'] != receipt['run_id'] or not physical.get('simulation'):
                raise ValueError('run or simulation identity changed')
            log.write(json.dumps({'wall_unix_ms':int(time.time()*1000),'mission':state,'physics':{
                k:physical[k] for k in ('stamp','position','linear_velocity_world','foot_contacts','model_sha256')}})+'\n')
            log.flush()
            if state['patrol']['state'] in ('completed','stopped','failed','expired') or time.monotonic() >= deadline:
                break
            time.sleep(.5)
    instruction = json.loads((args.run/'executor/mission.json').read_text())['instruction']
    payload = instruction['payload']
    if isinstance(payload,str):
        payload = json.loads(base64.b64decode(payload))
    policy = payload['patrol']['return']
    destination = state['patrol']['return']['destination']
    policy_matches = (destination['x']==policy['position'][0] and destination['y']==-policy['position'][2]
                      and destination['yaw']==policy['yaw'] and state['patrol']['return']['before_expiry_seconds']==policy['before_expiry_seconds'])
    goal = [destination['x'],destination['y']]
    nav = state['navigation']
    truth_error = math.dist(physical['position'][:2],goal)
    estimate_error = math.dist([nav['pose']['x'],nav['pose']['y']],goal)
    result = state['task'].get('patrol_return_result')
    accepted = bool(state['patrol']['state']=='completed' and state['task']['status']['Phase']=='succeeded'
                    and result and policy_matches and truth_error <= .6 and estimate_error <= .4)
    summary = {'passed':accepted,'scope':'Actual articulated MuJoCo/ONNX gait and Nav2; no hardware or battery model',
               'run_id':receipt['run_id'],'state':state['patrol']['state'],'reason':state['patrol'].get('reason'),
               'policy_matches_signed_payload':policy_matches,'rendezvous':policy,
               'estimated_arrival_error_m':estimate_error,'physical_arrival_error_m':truth_error,
               'physical_speed_mps':math.hypot(*physical['linear_velocity_world'][:2]),'arrival_result':result,
               'original_deadline_unix_ms':payload['not_after_unix_ms'],'trigger':state['patrol']['return'].get('trigger')}
    (out/'result.json').write_text(json.dumps(summary,indent=2)+'\n')
    print(json.dumps(summary,indent=2))
    if not accepted:
        raise SystemExit(1)

if __name__ == '__main__':
    main()
