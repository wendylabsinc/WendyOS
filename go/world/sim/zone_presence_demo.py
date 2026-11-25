"""Private interactive zone presence from rendered pixels and image-only MoveNet.

Uses the ordinary world-people stream; actor positions never enter its input.
"""
import argparse
import base64
import json
import os
from pathlib import Path
import time
from http.server import BaseHTTPRequestHandler, HTTPServer
from sim.people_demo import PeopleDemo
from sim.people_scene import CAMERAS

ZONES=[{'id':'west','label':'West work area','min_xz':[-2.5,-1.7],'max_xz':[-.25,.7],'dwell_seconds':1.},
       {'id':'east','label':'East work area','min_xz':[.25,-1.7],'max_xz':[2.5,.7],'dwell_seconds':1.}]


def atomic(path,value):
    temporary=path.with_suffix('.tmp');temporary.write_text(json.dumps(value,allow_nan=False));temporary.replace(path)


class Demo:
    def __init__(self,args):
        self.out=args.out;self.out.mkdir(parents=True,exist_ok=False,mode=0o700)
        self.people=PeopleDemo(self.out,args.pose_python,Path(__file__).resolve().parents[1]/'perception/pose.py',args.model,args.tracker,'zone-presence-demo',zones=ZONES)
        self.seconds=0.;self.motion=0.;self.moving=False;self.last=0.;self.latest={}
        original_pose=self.people.scene.pose
        def controlled_pose(seconds):
            original_pose(self.motion)
            self.people.scene.data.time=seconds
        self.people.scene.pose=controlled_pose
        self.timeline=(self.out/'timeline.jsonl').open('w')
        self.capture()

    def capture(self,dt=.25):
        self.seconds+=dt
        if self.moving:self.motion=min(15.,self.motion+dt)
        state,request=self.people.tick(self.seconds)
        result=state['snapshot']['tracking'];images={}
        for camera in CAMERAS:
            if self.people.enabled[camera]:images[camera]=base64.b64encode((self.people.images/(camera+'.png')).read_bytes()).decode()
        self.latest={'seconds':self.seconds,'motion_seconds':self.motion,'moving':self.moving,'enabled':self.people.enabled.copy(),'images':images,'tracking':result,'simulation':True}
        self.timeline.write(json.dumps({'state':self.latest,'request':request})+'\n');self.timeline.flush()
        atomic(self.out/'latest.json',self.latest);self.last=time.monotonic();return self.latest

    def control(self,action,source):
        if action=='camera':self.people.command('toggle_camera',source)
        elif action=='motion' and source=='actors':self.moving=not self.moving
        elif action=='advance' and source=='clock':return self.capture(1.5)
        else:raise ValueError('unknown simulated control')
        return self.capture()

    def close(self):self.people.close();self.timeline.close()


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--out',type=Path,required=True);parser.add_argument('--pose-python',type=Path,required=True)
    parser.add_argument('--model',type=Path,required=True);parser.add_argument('--tracker',type=Path,required=True)
    parser.add_argument('--port',type=int,default=8766);parser.add_argument('--seconds',type=int,default=900)
    parser.add_argument('--check',action='store_true');args=parser.parse_args();os.umask(0o077)
    demo=Demo(args)
    try:
        if args.check:
            states=[demo.latest]
            for _ in range(6):states.append(demo.capture())
            for camera in CAMERAS:states.append(demo.control('camera',camera))
            states.append(demo.control('advance','clock'))
            for camera in CAMERAS:states.append(demo.control('camera',camera))
            for _ in range(5):states.append(demo.capture())
            demo.moving=True
            for _ in range(36):states.append(demo.capture())
            summary=[{'seconds':s['seconds'],'tracks':len(s['tracking']['tracks']),'zones':[{k:z[k] for k in ('state','observed','possible')} for z in s['tracking']['presence']['zones']],'events':[e['kind'] for e in s['tracking']['presence']['events']]} for s in states]
            atomic(args.out/'checks.json',summary)
            assert any('dwell' in s['events'] for s in summary[:7]),'no image-derived dwell'
            assert all(z['state']=='unknown' for z in summary[9]['zones']),summary[9]
            assert 'lost' in summary[9]['events'] and 'exited' not in summary[9]['events'],summary[9]
            assert any(z['observed']>0 for s in summary[10:17] for z in s['zones']),'no recovery'
            assert all(m['evidence'] and m['model_sha256'] for s in states for t in s['tracking']['tracks'] for m in t['members'])
            print(json.dumps({'passed':True,'captures':len(states),'artifact':str(args.out/'checks.json')}));return
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                if self.path=='/':data=Path(__file__).with_suffix('.html').read_bytes();content='text/html'
                elif self.path=='/state':
                    if time.monotonic()-demo.last>.3:demo.capture()
                    data=json.dumps(demo.latest).encode();content='application/json'
                else:self.send_error(404);return
                self.send_response(200);self.send_header('Content-Type',content);self.send_header('Cache-Control','no-store');self.end_headers();self.wfile.write(data)
            def do_POST(self):
                if self.path!='/control' or not 0<int(self.headers.get('Content-Length','0'))<=1024:self.send_error(400);return
                try:body=json.loads(self.rfile.read(int(self.headers['Content-Length'])));state=demo.control(body['action'],body['source'])
                except (ValueError,KeyError):self.send_error(400);return
                self.send_response(200);self.send_header('Content-Type','application/json');self.end_headers();self.wfile.write(json.dumps(state).encode())
            def log_message(self,*args):pass
        server=HTTPServer(('127.0.0.1',args.port),Handler);server.timeout=.5
        until=time.monotonic()+args.seconds;print(f'http://127.0.0.1:{args.port}',flush=True)
        while time.monotonic()<until:server.handle_request()
        server.server_close()
    finally:demo.close()


if __name__=='__main__':main()
