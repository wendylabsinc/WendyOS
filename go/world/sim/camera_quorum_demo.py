"""Interactive two-of-three camera fusion using real MuJoCo RGB and ray returns.

Only synthetic camera/scalar/audio/LiDAR observations enter the ordinary Go
participant. Geometry is separate display/oracle data, never an inference input.
"""
import argparse
import base64
import hashlib
import json
import math
from pathlib import Path
import select
import subprocess
import time
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import urlsplit

from sim.sensors import calibration, png_rgb, audio_wav

CAMERAS = ['camera-a', 'camera-b', 'camera-c']
XML = '''<mujoco model="camera-quorum-lab"><option timestep=".01"/><visual><global offwidth="320" offheight="240"/><headlight ambient=".5 .5 .5" diffuse=".6 .6 .6"/></visual><worldbody>
<light pos="0 -2 5"/><geom name="floor" type="plane" size="5 5 .1" rgba=".22 .27 .30 1"/>
<body name="fixture" mocap="true" pos="0 0 1"><geom name="red-fixture" type="box" size=".22 .15 .24" rgba="1 .02 .02 1"/></body>
<body name="focus" pos="0 0 1"/>
<camera name="camera-a" pos="-1.7 -3 1.7" mode="targetbody" target="focus" fovy="42"/>
<camera name="camera-b" pos="1.7 -3 1.7" mode="targetbody" target="focus" fovy="42"/>
<camera name="camera-c" pos=".1 -4 2.4" mode="targetbody" target="focus" fovy="42"/>
</worldbody></mujoco>'''


def atomic(path, value):
    temporary=path.with_suffix('.tmp')
    temporary.write_text(json.dumps(value,allow_nan=False))
    temporary.replace(path)


class Demo:
    def __init__(self, binary, output):
        import mujoco
        import numpy as np
        if mujoco.__version__!='4.0.0':raise ValueError('MuJoCo 4.0.0 required')
        self.mj,self.np=mujoco,np
        self.out=Path(output);self.out.mkdir(parents=True,exist_ok=False)
        self.model=mujoco.MjModel.from_xml_string(XML);self.data=mujoco.MjData(self.model)
        self.renderer=mujoco.Renderer(self.model,height=240,width=320)
        self.run='camera-quorum-demo';self.digest=hashlib.sha256(XML.encode()).hexdigest()
        self.frame={'id':'quorum-lab','revision':self.digest[:16]}
        self.enabled={name:True for name in CAMERAS+['lidar-hub','gas-1','audio-1']}
        self.expired={name:False for name in CAMERAS};self.target_x=0.
        self.sequence=0;self.seconds=.25;self.last=time.monotonic();self.latest={}
        config={'version':1,'participant_id':'hub','run_id':self.run,'frame':self.frame,'camera_quorum':CAMERAS,
                'sensors':[{'sensor_id':name,'kind':'camera' if name in CAMERAS else 'lidar' if name=='lidar-hub' else 'gas' if name=='gas-1' else 'audio','owner':'hub'} for name in self.enabled]}
        atomic(self.out/'config.json',config)
        self.log=(self.out/'worker.log').open('w')
        self.process=subprocess.Popen([str(Path(binary).resolve()),'--config',str(self.out/'config.json'),'--state-dir',str(self.out/'participant')],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=self.log,text=True)
        self.timeline=(self.out/'timeline.jsonl').open('w')
        self.capture(.25)

    def capture(self, dt=.25):
        self.sequence+=1;self.seconds+=dt;self.data.time=self.seconds
        self.data.mocap_pos[0][0]=self.target_x
        self.mj.mj_forward(self.model,self.data)
        samples=[];media={};images={}
        for name,on in self.enabled.items():
            if not on:continue
            kind='camera' if name in CAMERAS else 'lidar' if name=='lidar-hub' else 'gas' if name=='gas-1' else 'audio'
            sample={'schema_version':'wendy.sensor.sample.v1','id':f'{self.run}/{name}/{self.sequence}', 'site_id':'studio-01','frame':self.frame,'sensor_id':name,'kind':kind,'sequence':self.sequence,'sample_ns':round(self.seconds*1e9),'run_id':self.run,'scenario_version':'camera-quorum-v1','seed':7,'package_sha256':self.digest,'sensor_model':'synthetic-quorum-fixture','position_m':[0,1,3],'uncertainty_m':.02,'calibration_revision':'rendered-camera-v1'}
            blob=None
            if kind=='camera':
                cid=self.mj.mj_name2id(self.model,self.mj.mjtObj.mjOBJ_CAMERA,name)
                sample['position_m'],sample['camera']=calibration(self.model,self.data,cid)
                if self.expired[name]:
                    # The simulated capture is 1 ms old with a 1 ns calibration
                    # lifetime, so expiry is real at evaluation time.
                    sample['camera']['calibration_valid_for_ns']=1
                    sample['sample_ns']-=1_000_000
                self.renderer.update_scene(self.data,camera=name)
                blob=png_rgb(self.renderer.render());mime='image/png'
                images[name]=base64.b64encode(blob).decode()
            elif kind=='gas':sample['gas']={'value_ppm':90.,'uncertainty_ppm':1.,'species':'synthetic-test-gas'}
            elif kind=='audio':
                sample['audio']={'sample_rate_hz':8000,'channels':1,'duration_ns':250000000}
                blob=audio_wav(7,self.sequence,True);mime='audio/wav'
            else:
                origin=self.np.array([0.,-3.,1.1]);points=[]
                # A fixed angular scan observes surfaces. No target position is
                # supplied to rays or to the fusion process.
                for elevation in range(-8,9,2):
                    for azimuth in range(-30,31,2):
                        a,e=math.radians(azimuth),math.radians(elevation)
                        direction=self.np.array([math.sin(a)*math.cos(e),math.cos(a)*math.cos(e),math.sin(e)])
                        geom=self.np.array([-1],dtype=self.np.int32)
                        distance=self.mj.mj_ray(self.model,self.data,origin,direction,None,1,-1,geom)
                        if 0<distance<=6:
                            p=origin+direction*distance;points.append([float(p[0]),float(p[2]),float(-p[1])])
                sample['position_m']=[0,1.1,3]
                sample['lidar']={'points_m':points,'max_range_m':6.}
            if blob is not None:
                digest=hashlib.sha256(blob).hexdigest()
                sample['evidence']={'sha256':digest,'content_type':mime,'size':len(blob)}
                media[digest]=base64.b64encode(blob).decode()
            samples.append(sample)
        tick={'now_ns':round(self.seconds*1e9),'local_samples':samples,'received':[],'media':media}
        self.process.stdin.write(json.dumps(tick)+'\n');self.process.stdin.flush()
        if not select.select([self.process.stdout],[],[],10)[0]:raise TimeoutError('fusion participant timed out')
        line=self.process.stdout.readline(16<<20)
        if not line:raise RuntimeError('fusion participant stopped')
        output=json.loads(line)
        self.latest={'seconds':self.seconds,'sequence':self.sequence,'enabled':self.enabled.copy(),'expired':self.expired.copy(),'images':images,'result':output['result'],'observations':len(output['observations']),'missing':output['missing_observations'],'truth_display_only':[self.target_x,1.,0.],'simulation':True}
        self.timeline.write(json.dumps(self.latest)+'\n');self.timeline.flush()
        atomic(self.out/'latest.json',self.latest)
        self.last=time.monotonic()
        return self.latest

    def control(self, action, source):
        if action=='toggle' and source in self.enabled:self.enabled[source]=not self.enabled[source]
        elif action=='calibration' and source in self.expired:self.expired[source]=not self.expired[source]
        elif action=='move' and source=='fixture':self.target_x=.6 if self.target_x==0 else 0.
        elif action=='advance' and source=='clock':return self.capture(2.25)
        else:raise ValueError('unknown simulation control')
        return self.capture()

    def close(self):
        self.process.terminate()
        try:self.process.wait(timeout=3)
        except subprocess.TimeoutExpired:self.process.kill();self.process.wait()
        self.renderer.close();self.timeline.close();self.log.close()


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary',required=True);parser.add_argument('--out',type=Path,required=True)
    parser.add_argument('--port',type=int,default=8765);parser.add_argument('--seconds',type=int,default=900)
    parser.add_argument('--check',action='store_true',help='run deterministic controls, save actual results and exit')
    args=parser.parse_args();demo=Demo(args.binary,args.out)
    try:
        if args.check:
            records=[demo.latest]
            for action,source in [('toggle','camera-a'),('advance','clock'),('toggle','camera-b'),('advance','clock'),('toggle','camera-a'),('advance','clock'),('calibration','camera-a'),('advance','clock'),('calibration','camera-a'),('toggle','camera-b'),('advance','clock'),('move','fixture')]:
                records.append(demo.control(action,source))
            atomic(args.out/'checks.json',[{'seconds':r['seconds'],'state':r['result']['state'],'selected':r['result'].get('quorum',{}).get('selected',[]),'reason':r['result']['reason']} for r in records])
            assert records[0]['result']['state']=='hazard',records[0]['result']
            assert records[2]['result']['quorum']['selected']==['camera-b','camera-c']
            assert records[4]['result']['state']=='insufficient_evidence'
            assert records[6]['result']['quorum']['selected']==['camera-a','camera-c']
            assert records[8]['result']['state']=='insufficient_evidence'
            assert records[-1]['result']['state']=='hazard'
            print(json.dumps({'passed':True,'states':len(records),'artifact':str(args.out/'checks.json')}));return
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                path=urlsplit(self.path).path
                if path=='/':data=Path(__file__).with_suffix('.html').read_bytes();content='text/html'
                elif path=='/state':
                    if time.monotonic()-demo.last>.25:demo.capture(.25)
                    data=json.dumps(demo.latest).encode();content='application/json'
                else:self.send_error(404);return
                self.send_response(200);self.send_header('Content-Type',content);self.send_header('Cache-Control','no-store');self.end_headers();self.wfile.write(data)
            def do_POST(self):
                if self.path!='/control' or not 0<int(self.headers.get('Content-Length','0'))<=1024:self.send_error(400);return
                try:
                    body=json.loads(self.rfile.read(int(self.headers['Content-Length'])));data=demo.control(body['action'],body['source'])
                except (ValueError,KeyError):self.send_error(400);return
                self.send_response(200);self.send_header('Content-Type','application/json');self.end_headers();self.wfile.write(json.dumps(data).encode())
            def log_message(self,*args):pass
        server=HTTPServer(('127.0.0.1',args.port),Handler);server.timeout=.5
        until=time.monotonic()+args.seconds
        print(f'http://127.0.0.1:{server.server_port}',flush=True)
        while time.monotonic()<until:server.handle_request()
        server.server_close()
    finally:demo.close()


if __name__=='__main__':main()
