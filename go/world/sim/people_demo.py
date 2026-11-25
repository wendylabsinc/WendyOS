"""Live MuJoCo camera -> learned pose -> calibrated anonymous tracking demo."""
from __future__ import annotations

import hashlib
import copy
import stat
import json
import os
from pathlib import Path
import select
import subprocess
import time
import uuid
from types import SimpleNamespace

from sim.people_scene import PeopleScene, CAMERAS
from sim.macos_demo_bridge import geometry, _transform, SessionFiles, COMMAND_SCHEMA, _unique_object


class JSONProcess:
    def __init__(self, command, log):
        self.log = Path(log).open('w')
        self.process = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                        stderr=self.log, text=True, bufsize=1)

    def call(self, value):
        self.process.stdin.write(json.dumps(value, allow_nan=False, separators=(',', ':')) + '\n')
        self.process.stdin.flush()
        if not select.select([self.process.stdout], [], [], 30)[0]:
            raise RuntimeError('perception worker did not respond')
        line = self.process.stdout.readline()
        if not line:
            raise RuntimeError('perception worker exited')
        result = json.loads(line)
        if 'error' in result:
            raise RuntimeError(result['error'])
        return result

    def close(self):
        if self.process.poll() is None:
            self.process.stdin.close()
            try:
                self.process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.process.terminate()
                self.process.wait(timeout=5)
        self.log.close()


class PeopleDemo:
    """Owns a separate simulator and workers. It cannot command physical devices."""
    def __init__(self, out, pose_python, pose_script, model, tracker, run_id, zones=None, zone_reader=None, zone_trust=None):
        self.out, self.run_id = Path(out), run_id
        self.zones = zones
        self.tracker_binary = Path(tracker)
        self.zone_reader, self.zone_trust = zone_reader, zone_trust
        self.zone_selection_sequence = None
        self.last_request = self.last_tracking = None
        self.site_id = 'studio-01'
        if bool(zone_reader) != bool(zone_trust): raise ValueError('both zone reader and trust are required')
        if zone_reader:
            def private_json(path):
                fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW)
                try:
                    info=os.fstat(fd)
                    if not stat.S_ISREG(info.st_mode) or info.st_uid!=os.geteuid() or info.st_mode&0o077 or not 0<info.st_size<=131072:raise ValueError('zone configuration must be private and bounded')
                    with os.fdopen(fd,'rb',closefd=False) as f: raw=f.read(131073)
                    return json.loads(raw,object_pairs_hook=_unique_object)
                finally:os.close(fd)
            credentials=private_json(zone_reader);private_json(zone_trust)
            if credentials['grant']['grant']['domain']!='simulation':raise ValueError('zone reader must be simulation scoped')
            self.site_id=credentials['grant']['grant']['site']
            self.tenant=credentials['grant']['grant']['tenant']
        self.scene = PeopleScene()
        self.digest = hashlib.sha256(self.scene.xml.encode()).hexdigest()
        self.frame = {'id': 'people-annex-floor', 'revision': self.digest[:16]}
        self.enabled = {camera: True for camera in CAMERAS}
        self.sequence = self.capture_sequence = self.command_sequence = 0
        self.last_capture = -1.
        self.cameras = {}
        self.commands = []
        self.images = self.out / 'tracking' / 'hub'
        self.images.mkdir(parents=True, mode=0o700)
        self.pose = JSONProcess([str(pose_python), str(pose_script), '--model', str(model), '--serve'], self.out/'pose.log')
        self.tracker = JSONProcess([str(tracker)], self.out/'tracker.log')
        runtime = SimpleNamespace(model=self.scene.model, data=self.scene.data, mj=self.scene.mj,
            bindings={}, run_id=run_id, config={'model': {'sha256': self.digest}, 'presentation': {
                'name': 'Studio 01 / Perception', 'subtitle': 'Anonymous camera tracking', 'labels': []}})
        self.geometry, _ = geometry(runtime)
        self.runtime = runtime
        # Mocap bodies move even without a DOF. These transforms are visualization
        # only; the pose worker receives solely camera image bytes.
        self.dynamic_ids = []
        for geom in self.geometry['geoms']:
            if geom['name'].startswith('person-'):
                geom['dynamic'] = True
                self.dynamic_ids.append(geom['id'])
        self.geometry['semantics'] = 'Reference simulation geometry, separate from image-inferred anonymous tracks'

    def command(self, action, target):
        if action=='zone_select':
            self.command_sequence+=1
            accepted=False
            try:
                self.select_zones(target,self.command_sequence)
                accepted=True
            except (ValueError,KeyError,TypeError,RuntimeError,OSError):pass
            receipt={'schema':'wendy.macos-demo.command-result.v1','run_id':self.run_id,'sequence':self.command_sequence,'action':action,'target':target,'accepted':accepted,'time_s':float(self.scene.data.time)}
            if not accepted:receipt['error']='Catalog selection unavailable or invalid; prior session retained'
            self.commands.append(receipt);return receipt
        if action != 'toggle_camera' or target not in self.enabled:
            raise ValueError('unknown perception control')
        self.enabled[target] = not self.enabled[target]
        self.command_sequence += 1
        receipt = {'schema':'wendy.macos-demo.command-result.v1','run_id':self.run_id,
                   'sequence':self.command_sequence,'action':action,'target':target,
                   'accepted':True,'time_s':float(self.scene.data.time)}
        self.commands.append(receipt)
        return receipt

    def select_zones(self,target,sequence):
        if not self.zone_reader or not isinstance(target,str) or len(target.encode())>2048 or self.last_request is None:raise ValueError('zone selection unavailable')
        selected=json.loads(target,object_pairs_hook=_unique_object)
        if set(selected)!={'sha256','source','run_id','frame'} or selected['run_id']!=self.run_id or selected['frame']!=self.frame:raise ValueError('zone selection context differs')
        digest=selected['sha256'];source=selected['source']
        if not isinstance(digest,str) or len(digest)!=64 or any(c not in '0123456789abcdef' for c in digest) or not isinstance(source,dict) or set(source)!={'device','app','service','stream'} or any(not isinstance(v,str) or not 0<len(v.encode())<=256 for v in source.values()):raise ValueError('invalid zone selection')
        source_path=self.out/f'zone-source-{sequence}.json'
        with source_path.open('x') as stream:json.dump(source,stream)
        source_path.chmod(0o600)
        replacement=JSONProcess([str(self.tracker_binary),'--zone-credentials',str(self.zone_reader),'--zone-trust',str(self.zone_trust),'--zone-sha256',digest,'--zone-source',str(source_path)],self.out/f'zone-tracker-{sequence}.log')
        try:
            request=copy.deepcopy(self.last_request);request.pop('zones',None)
            result=replacement.call(request)
            status=result.get('zone_catalog',{})
            if status.get('sha256')!=digest or status.get('source')!=source or status.get('state')!='available_at_replica':raise ValueError('catalog was not verified')
            if self.last_tracking is not None:
                with (self.out/f'zone-history-before-{sequence}.json').open('x') as stream:json.dump(self.last_tracking,stream)
        except BaseException:
            replacement.close()
            raise
        # The new session is committed only after verification and history export.
        # A failure while reaping the old process cannot undo that handoff.
        prior = self.tracker
        self.tracker = replacement
        self.zones = None
        self.zone_selection_sequence = sequence
        self.cameras = {}
        try:
            prior.close()
        except (OSError, subprocess.SubprocessError):
            try:
                if prior.process.poll() is None:
                    prior.process.kill()
                    prior.process.wait(timeout=5)
            except (OSError, subprocess.SubprocessError):
                pass

    def sample(self, camera, raw, seconds):
        cal = self.scene.calibration(camera)
        m = cal['camera_to_scene']
        world = m[0:4] + m[8:12] + [-x for x in m[4:8]] + m[12:16]
        camera_data = {k:cal[k] for k in ('width','height','fx','fy','cx','cy')}
        camera_data.update(camera_to_site=world,calibration_valid_for_ns=60_000_000_000)
        return {'schema_version':'wendy.sensor.sample.v1','id':f'{self.run_id}/{camera}/{self.capture_sequence}',
                'site_id':self.site_id,'frame':self.frame,'sensor_id':camera,'kind':'camera',
                'sequence':self.capture_sequence,'sample_ns':round(seconds*1e9),'run_id':self.run_id,
                'scenario_version':'people-annex-v1','seed':7,'package_sha256':self.digest,
                'sensor_model':'mujoco-4.0.0-rgb-pinhole','position_m':[world[3],world[7],world[11]],
                'uncertainty_m':.02,'calibration_revision':'annex-camera-v1','camera':camera_data,
                'evidence':{'sha256':hashlib.sha256(raw).hexdigest(),'content_type':'image/png','size':len(raw)}}

    def tick(self, seconds):
        self.scene.pose(seconds)
        images = []
        if seconds - self.last_capture >= .249999:
            self.last_capture = seconds
            self.capture_sequence += 1
            for camera in CAMERAS:
                if not self.enabled[camera]:
                    continue
                raw = self.scene.render(camera)
                path = self.images / (camera+'.png')
                path.write_bytes(raw)
                poses = self.pose.call({'image_path':str(path.resolve())})
                images.append({'sample':self.sample(camera,raw,seconds),'poses':poses})
        request = {'participant_id':'hub','run_id':self.run_id,'frame':self.frame,
                   'now_ns':round(seconds*1e9),'floor_y':0,'images':images}
        if self.zones is not None:
            request['zones'] = self.zones
        result = self.tracker.call(request)
        self.last_request=copy.deepcopy(request)
        if self.zone_selection_sequence is not None and 'zone_catalog' in result:result['zone_catalog']['selection_sequence']=self.zone_selection_sequence
        self.last_tracking=copy.deepcopy(result)
        for camera in result['cameras']:
            camera.update(evidence_path=f'tracking/hub/{camera["sensor_id"]}.png',captured_at_s=seconds)
            self.cameras[camera['sensor_id']] = camera
        self.cameras = {name:value for name,value in self.cameras.items() if seconds-value['captured_at_s'] < 1.25}
        result['cameras'] = list(self.cameras.values())
        result['camera_sources'] = [{'id':name,'label':label,'enabled':self.enabled[name]}
            for name,label in zip(CAMERAS,('Front camera','Oblique camera'))]
        self.sequence += 1
        empty_edge = {'id':'internet','a':'hub','b':'internet','enabled':False,'up':False,'packets':0}
        snapshot = {'run_id':self.run_id,'time_s':seconds,'phase':'tracking','authorized':True,'stopped':False,
                    'robots':[],'jobs':[],'completed':0,'total':0,'events':[],'voxels':[],'voxel_size':.2,
                    'map_node':'hub','network':{'edges':[],'internet':empty_edge,'gossip_rounds':0,'peer_voxels':{}},
                    'tracking':result}
        state = {'schema':'wendy.macos-demo.state.v1','run_id':self.run_id,'sequence':self.sequence,
                 'worker_status':'running','last_command_sequence':self.command_sequence,'snapshot':snapshot,
                 'dynamic_geoms':[_transform(self.runtime,i) for i in self.dynamic_ids]}
        return state,request

    def close(self):
        self.pose.close()
        self.tracker.close()
        self.scene.close()


def serve(args):
    """Run the same controller behind the native app's existing private file API."""
    os.umask(0o077)
    args.out.mkdir(parents=True,exist_ok=False,mode=0o700)
    run=args.run_id or 'people-'+uuid.uuid4().hex[:12]
    files=SessionFiles(args.out,run)
    demo=None
    try:
        demo=PeopleDemo(args.out,args.pose_python,Path(__file__).resolve().parents[1]/'perception/pose.py',
                        args.model,args.tracker,run,zone_reader=args.zone_reader,zone_trust=args.zone_trust)
        files.write('geometry.json',demo.geometry)
        if args.zone_reader:files.write('zone-context.json',{'schema':'wendy.native-zone.context.v1','run_id':run,'model_sha256':demo.digest,'context':{'tenant':demo.tenant,'site':demo.site_id,'runID':run,'frameID':demo.frame['id'],'frameRevision':demo.frame['revision']}})
        files.write('bridge-ready.json',{'schema':'wendy.macos-demo.ready.v1','run_id':run,'scope':'simulated-image-perception'})
        start=time.monotonic(); state=None
        while time.monotonic()-start<args.duration and not files.stop_requested():
            sequence=demo.command_sequence+1
            command=files.command(sequence)
            if command is not None:
                if command.get('schema')!=COMMAND_SCHEMA or command.get('run_id')!=run or command.get('sequence')!=sequence:
                    raise ValueError('command belongs to another simulator session')
                receipt=demo.command(command['action'],command['target'])
                files.write(f'{sequence:06d}.json',receipt,receipt=True)
            seconds=min(119.9,time.monotonic()-start)
            state,_=demo.tick(seconds)
            files.write('state.json',state,replace=demo.sequence>1)
            time.sleep(.025)
        if state is not None:
            state['sequence']+=1;state['worker_status']='closed'
            files.write('state.json',state,replace=True)
    finally:
        if demo:demo.close()
        files.close()


if __name__=='__main__':
    import argparse
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--out',required=True,type=Path)
    parser.add_argument('--pose-python',required=True,type=Path)
    parser.add_argument('--model',required=True,type=Path)
    parser.add_argument('--tracker',required=True,type=Path)
    parser.add_argument('--duration',type=float,default=120)
    parser.add_argument('--run-id')
    parser.add_argument('--zone-reader',type=Path)
    parser.add_argument('--zone-trust',type=Path)
    options=parser.parse_args()
    if not 1<=options.duration<=120:parser.error('duration must be 1..120 seconds')
    if options.run_id and (not options.run_id.isascii() or not 1<=len(options.run_id)<=100 or not all(c.isalnum() or c=='-' for c in options.run_id)):parser.error('invalid run ID')
    serve(options)
