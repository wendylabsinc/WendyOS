#!/usr/bin/env python3
"""Measure a running patrol without supplying truth to its controller."""
import argparse
import json
import math
import os
from pathlib import Path
import signal
import time
import urllib.request


def read(url):
    with urllib.request.urlopen(url,timeout=3) as r:
        return json.load(r)


def quantile(values,p):
    if not values:return None
    return sorted(values)[min(len(values)-1,math.ceil(len(values)*p)-1)]


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--run',type=Path,required=True)
    p.add_argument('--out',type=Path,required=True)
    p.add_argument('--seconds',type=int,default=1800)
    p.add_argument('--disconnect-viewer-at',type=int,default=60)
    a=p.parse_args();a.out.mkdir(parents=True,exist_ok=False)
    run=json.loads(a.run.read_text());errors=[];moving_errors=[];joint_min=[];joint_max=[];foot_samples=0;samples=0
    disconnected_visits=None
    previous=None;first=None;max_step=0.;last_visits={};disconnected=False;started=time.monotonic();last_report=started
    failures=[]
    def report(final=False):
        elapsed=time.monotonic()-started
        result=dict(schema='wendy.nav.patrol.acceptance.v1',run_id=run['run_id'],wall_seconds=elapsed,
            required_seconds=a.seconds,samples=samples,localization_samples=len(errors),position_error_p95_m=quantile(errors,.95),
            position_error_max_m=max(errors) if errors else None,foot_contact_samples=foot_samples,
            moving_position_error_p95_m=quantile(moving_errors,.95),moving_localization_samples=len(moving_errors),
            joint_ranges_rad=[b-a for a,b in zip(joint_min,joint_max)],
            observations_after_viewer_disconnect=0 if disconnected_visits is None else sum(last_visits.values())-disconnected_visits,
            maximum_adjacent_displacement_m=max_step,visits=last_visits,viewer_disconnected=disconnected,failures=failures,
            final=final,passed=final and elapsed>=a.seconds and len(errors)>100 and quantile(errors,.95)<.30 and
                len(last_visits)==6 and all(v>=2 for v in last_visits.values()) and disconnected_visits is not None and
                sum(last_visits.values())>disconnected_visits and len(joint_min)==12 and
                all(b-a>.1 for a,b in zip(joint_min,joint_max)) and not failures)
        temp=a.out/'.report.next';temp.write_text(json.dumps(result,indent=2)+'\n');os.replace(temp,a.out/'report.json')
        return result
    with (a.out/'samples.jsonl').open('x') as log:
        while time.monotonic()-started<a.seconds:
            iteration=time.monotonic()
            try:
                physical=read(run['physics_url']+'/state');nav=read(run['navigation_url']+'/status');mission=read(run['mission_url']+'/status')
                if physical['run_id']!=run['run_id'] or mission['run_id']!=run['run_id']:raise ValueError('run identity changed')
                if previous and physical['stamp']<=previous['stamp']:raise ValueError('physics time did not advance or reset')
                if physical.get('fault'):raise ValueError('physics fault: '+physical['fault'])
                if first is None:first=physical
                if previous:
                    delta=math.dist(physical['position'],previous['position']);max_step=max(max_step,delta)
                    if delta>2*(physical['stamp']-previous['stamp'])+.05:raise ValueError('body displacement exceeds measured walking envelope')
                samples+=1;foot_samples+=any(physical['foot_contacts']);last_visits={r['id']:r['visits'] for r in mission['patrol']['rooms']}
                q=physical['joints']['q']
                if not joint_min:joint_min=list(q);joint_max=list(q)
                joint_min=[min(a,b) for a,b in zip(joint_min,q)];joint_max=[max(a,b) for a,b in zip(joint_max,q)]
                error=None
                if nav.get('ready') and time.monotonic()-started>=20:
                    error=math.hypot(physical['position'][0]-nav['pose']['x'],physical['position'][1]-nav['pose']['y']);errors.append(error)
                    if abs(nav.get('speed_mps',0))>.1:moving_errors.append(error)
                log.write(json.dumps(dict(elapsed_s=time.monotonic()-started,simulation_s=physical['stamp'],
                    truth=physical['position'],estimate=nav.get('pose'),position_error_m=error,joints=physical['joints']['q'],
                    contacts=physical['foot_contacts'],patrol=mission['patrol'],goal=nav['goal']))+'\n');log.flush()
                previous=physical
                if mission['patrol']['state'] in ('failed','stopped','expired'):raise ValueError('mission ended during acceptance: '+mission['patrol']['state'])
                if not disconnected and time.monotonic()-started>=a.disconnect_viewer_at:
                    pid=run.get('viewer_pid')
                    if pid:
                        try:os.kill(pid,signal.SIGTERM)
                        except ProcessLookupError:pass
                    disconnected=True
                    disconnected_visits=sum(last_visits.values())
            except Exception as exc:
                failures.append(str(exc));report(True);raise
            if time.monotonic()-last_report>=10:
                report();last_report=time.monotonic()
            time.sleep(max(0,.5-(time.monotonic()-iteration)))
    result=report(True);print(json.dumps(result,indent=2));return 0 if result['passed'] else 1

if __name__=='__main__':raise SystemExit(main())
