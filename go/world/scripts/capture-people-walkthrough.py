#!/usr/bin/env python3
"""Record image-derived people tracking and implemented controls for native playback."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import sys
import time
import uuid

ROOT=Path(__file__).resolve().parents[1]
sys.path.insert(0,str(ROOT))
from sim.people_demo import PeopleDemo


def write(path,value):
    path.write_text(json.dumps(value,allow_nan=False,indent=2)+'\n')


def capture(args):
    os.umask(0o077)
    args.out.mkdir(parents=True,exist_ok=False,mode=0o700)
    timeline=args.out/'timeline'; timeline.mkdir()
    assets=timeline/'assets'; assets.mkdir()
    truth=args.out/'ground-truth'; truth.mkdir()
    run='people-'+uuid.uuid4().hex[:12]
    demo=PeopleDemo(args.out,args.pose_python,ROOT/'perception/pose.py',args.model,args.tracker,run)
    report={'run_id':run,'status':'running','rendered_camera_inference':True,'hardware':False,
            'fps':args.fps,'duration_s':args.duration,'frames':0,'controls':[],
            'worker_pids':[demo.pose.process.pid,demo.tracker.process.pid]}
    write(timeline/'geometry.json',demo.geometry)
    (args.out/'scene.xml').write_text(demo.scene.xml)
    events=set(); start=time.monotonic()
    chapter={'series':'ANONYMOUS CAMERA TRACKING','title':'Camera pixels become shared understanding.',
             'detail':'A learned pose model reads two rendered camera views. No scene labels enter inference.'}
    try:
        with (timeline/'frames.jsonl').open('w') as output,(args.out/'perception-inputs.jsonl').open('w') as inputs:
            for frame in range(round(args.duration*args.fps)):
                seconds=frame/args.fps
                actions=[];pointer=None
                schedule=[(0,'intro'),(3,'hide'),(4.5,'show'),(7,'one'),(10,'none'),(12,'expired'),(15,'restore'),(18,'reference'),(20,'return')]
                for at,event in schedule:
                    if seconds<at or event in events:continue
                    events.add(event)
                    if event=='intro':
                        actions=[{'kind':'camera','target':'Overview'},{'kind':'layer','target':'Tracks','enabled':True}]
                    elif event=='hide':
                        actions=[{'kind':'layer','target':'Tracks','enabled':False}]
                        chapter={'series':'ANONYMOUS CAMERA TRACKING','title':'Separate the scene from its observations.',
                                 'detail':'Toggle inferred tracks independently of the reference geometry.'}
                    elif event=='show':
                        actions=[{'kind':'layer','target':'Tracks','enabled':True},{'kind':'camera','target':'Floor plan'}]
                    elif event in ('one','none','restore'):
                        names=['people-front'] if event=='one' else ['people-oblique'] if event=='none' else ['people-front','people-oblique']
                        for name in names:report['controls'].append(demo.command('toggle_camera',name))
                        actions=[{'kind':'camera','target':'Overview'}]
                        pointer={'x':1795 if event=='none' else 1782,'y':300 if event=='none' else 269,'label':'Toggle camera source'}
                        chapter={'series':'ANONYMOUS CAMERA TRACKING','title':{'one':'Keep the view that is still available.','none':'Lose both cameras. Knowledge expires.','restore':'Restore cameras. Observe again.'}[event],
                                 'detail':{'one':'Only received camera observations can update a track.','none':'Short-lived estimates coast, then disappear without fresh evidence.','restore':'New images create fresh anonymous tracks with source and model lineage.'}[event]}
                    elif event=='expired':
                        chapter={'series':'ANONYMOUS CAMERA TRACKING','title':'Missing observations remain unknown.',
                                 'detail':'No detections does not establish that the room is empty.'}
                    elif event=='reference':
                        actions=[{'kind':'layer','target':'Building','enabled':False}]
                        chapter={'series':'ANONYMOUS CAMERA TRACKING','title':'Inspect the inferred world.',
                                 'detail':'Position, motion, uncertainty and original camera evidence. No appearance identity.'}
                    elif event=='return':
                        actions=[{'kind':'layer','target':'Building','enabled':True}]
                        chapter={'series':'ANONYMOUS CAMERA TRACKING','title':'One perception pipeline. Simulated cameras today.',
                                 'detail':'MuJoCo 4 · MoveNet image inference · Calibrated multi-camera tracking'}
                state,request=demo.tick(seconds)
                mapping={}
                for camera in state['snapshot']['tracking']['cameras']:
                    path=camera['evidence_path']; raw=(args.out/path).read_bytes()
                    name=hashlib.sha256(raw).hexdigest()+'.png'
                    if not (assets/name).exists():(assets/name).write_bytes(raw)
                    mapping[path]='assets/'+name
                item={'at_s':seconds,'state':state,'assets':mapping,'actions':actions,'chapter':chapter}
                if pointer:item['pointer']=pointer
                output.write(json.dumps(item,allow_nan=False,separators=(',',':'))+'\n')
                inputs.write(json.dumps(request,allow_nan=False,separators=(',',':'))+'\n')
                write(truth/f'{frame:04d}.json',demo.scene.truth())
                report['frames']+=1
                if frame%args.fps==0:print(f'Captured {frame}/{round(args.duration*args.fps)}',flush=True)
        report.update(status='passed',wall_seconds=time.monotonic()-start)
    finally:
        demo.close()
        report['workers_closed']=demo.pose.process.poll() is not None and demo.tracker.process.poll() is not None
        report['timeline_sha256']=hashlib.sha256((timeline/'frames.jsonl').read_bytes()).hexdigest()
        write(args.out/'capture.json',report)
    print(json.dumps(report))


if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--out',type=Path,required=True)
    p.add_argument('--pose-python',type=Path,required=True)
    p.add_argument('--model',type=Path,required=True)
    p.add_argument('--tracker',type=Path,required=True)
    p.add_argument('--duration',type=float,default=24)
    p.add_argument('--fps',type=int,default=12)
    a=p.parse_args()
    if not 1<=a.duration<=60 or not 1<=a.fps<=30:p.error('invalid bounded recording settings')
    capture(a)
