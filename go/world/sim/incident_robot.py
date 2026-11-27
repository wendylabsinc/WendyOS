"""Bounded planar MuJoCo inspection adapter, with separate read/control tokens.

The two slide joints and ideal brake are a synthetic robot fixture. Original
robot-local cameras, analytic scalar gas and generated audio are captured after
settling. No hardware endpoint or learned controller is used.
"""
from __future__ import annotations
import base64,copy,hashlib,hmac,http.server,json,math,queue,threading,time,uuid
import xml.etree.ElementTree as ET
from incident_association import scene_xml,camera_png
from sim.sensors import audio_wav

def inspection_scene():
    root=ET.fromstring(scene_xml());root.find('option').set('timestep','0.005')
    world=root.find('worldbody')
    for geom in world.iter('geom'):
        if geom.get('name')=='source-marker' or geom.get('rgba','').endswith(' 0'):geom.set('group','2')
    robot=ET.SubElement(world,'body',name='inspection-robot',pos='2 -1.0 .24')
    for name,axis in [('x','1 0 0'),('y','0 1 0')]:ET.SubElement(robot,'joint',name='inspect-'+name,type='slide',axis=axis,damping='1')
    ET.SubElement(robot,'geom',name='inspection-puck',type='sphere',size='.23',mass='2',rgba='.15 .35 .8 1')
    # Cameras follow the actual moving body. B is .45m above the floor pose.
    for name,z in [('camera-a','.35'),('camera-b','.45')]:ET.SubElement(robot,'camera',name='inspect-'+name,pos='0 0 '+z,xyaxes='1 0 0 0 0 1',fovy='65')
    actuators=ET.SubElement(root,'actuator')
    for name in ('x','y'):ET.SubElement(actuators,'velocity',name='inspect-'+name,joint='inspect-'+name,kv='30',ctrllimited='true',ctrlrange='-.5 .5')
    return ET.tostring(root,encoding='unicode')

class Robot:
    def __init__(self,demo,port,read_token,control_token):
        self.d=demo;self.mj=demo.mj;self.np=demo.np;self.model=demo.model;self.data=self.mj.MjData(self.model)
        self.body=self.mj.mj_name2id(self.model,self.mj.mjtObj.mjOBJ_BODY,'inspection-robot')
        self.joints=[self.mj.mj_name2id(self.model,self.mj.mjtObj.mjOBJ_JOINT,'inspect-'+s)for s in ('x','y')]
        self.dofs=[int(self.model.jnt_dofadr[j])for j in self.joints]
        self.act=[self.mj.mj_name2id(self.model,self.mj.mjtObj.mjOBJ_ACTUATOR,'inspect-'+s)for s in ('x','y')]
        self.lock=threading.Lock();self.done=threading.Event();self.boot=uuid.uuid4().hex;self.session=uuid.uuid4().hex;self.sequence=0;self.capture_sequence=1<<32
        self.deadline=0;self.velocity=[0.,0.,0.];self.read_token=read_token;self.control_token=control_token;self.requests=queue.Queue(maxsize=1);self.commands=[];self.poses=[];self.gas=True;self.error=None
        self.mj.mj_forward(self.model,self.data);self.server=http.server.ThreadingHTTPServer(('127.0.0.1',port),Handler);self.server.robot=self;self.server.daemon_threads=True
        self.http=threading.Thread(target=self.server.serve_forever,daemon=True);self.physics=threading.Thread(target=self.loop,daemon=True);self.http.start();self.physics.start()
        self.capture_ready=threading.Event();self.capture_error=None;self.capture_thread=threading.Thread(target=self.capture_loop,daemon=True);self.capture_thread.start()
        if not self.capture_ready.wait(5)or self.capture_error: self.close();raise ValueError('robot renderer failed to initialize')
    def brake(self):self.velocity=[0.,0.,0.];self.deadline=0;self.data.ctrl[self.act]=0;self.data.qvel[self.dofs]=0
    def clear(self):
        origin=self.data.xpos[self.body].copy();mask=self.np.array([1,1,0,0,0,0],dtype=self.np.uint8)
        for i in range(16):
            angle=i*math.pi/8;direction=self.np.array([math.cos(angle),math.sin(angle),0.]);gid=self.np.array([-1],dtype=self.np.int32)
            distance=self.mj.mj_ray(self.model,self.data,origin,direction,mask,True,self.body,gid)
            if 0<=distance<.29:return False
        return True
    def loop(self):
        try:
            while not self.done.is_set():
                start=time.monotonic()
                with self.lock:
                    if start>=self.deadline or not self.clear():self.brake()
                    self.data.mocap_pos[0]=[self.d.source[0],-self.d.source[1],.16];self.data.mocap_pos[1]=[self.d.visual_position[0],-self.d.visual_position[1],.6]
                    self.data.ctrl[self.act]=[self.velocity[0],-self.velocity[2]];self.mj.mj_step(self.model,self.data);self.mj.mj_forward(self.model,self.data)
                    if not all(math.isfinite(float(v))for v in (*self.data.qpos,*self.data.qvel)):raise ValueError('nonfinite physics')
                    if len(self.poses)<36000 and (not self.poses or time.time()-self.poses[-1]['wall']>.1):self.poses.append(dict(wall=time.time(),position=self.state()['position_m']))
                self.done.wait(max(0,.005-(time.monotonic()-start)))
        except Exception as error:
            self.error=str(error);self.done.set()
            with self.lock:self.brake()
    def state(self):
        if self.error:raise ValueError('physics unavailable')
        p=self.data.xpos[self.body];v=self.data.qvel[self.dofs];clear=self.clear()
        dx=float(p[0])-self.d.source[0];dz=-float(p[1])-self.d.source[1];wind=self.d.true_wind;along=dx*math.cos(wind)+dz*math.sin(wind);cross=-dx*math.sin(wind)+dz*math.cos(wind);gas=2+self.d.strength*math.exp(-.5*((along/(3 if along>=0 else 1))**2+(cross/1.3)**2))
        return dict(schema_version='wendy.sim.actuator.planar.v1',synthetic=True,robot_id='atlas',run_id=self.d.run,boot_id=self.boot,session=self.session,sequence=self.sequence,site_id=self.d.config['site'],frame=self.d.config['frame'],position_m=[float(p[0]),float(p[2]),-float(p[1])],velocity_mps=[float(v[0]),0.,-float(v[1])],sample_unix_ms=time.time_ns()//1_000_000,simulation_time_s=float(self.data.time),sensors={'gas':self.gas,'local-obstacles':True},obstacle_clear=clear,gas_ppm=gas,energy=1.,commanded_velocity_mps=self.velocity[:],lease_remaining_ms=max(0,int((self.deadline-time.monotonic())*1000)),watchdog_stopped=self.deadline==0,provenance=dict(mode='simulated',package_sha256=self.d.contract['visual']['package_sha256'],robot_model='mujoco-two-slide-joint-ideal-brake-v1',gas_model='analytic-fixture-v1'))
    def command(self,path,q):
        with self.lock:
            if path=='session':
                if q!={}:raise ValueError('empty session request required')
                self.brake();self.session=uuid.uuid4().hex;self.sequence=0
            else:
                keys={'session','sequence'}|({'velocity_mps','lease_ms'}if path=='command'else set())
                if set(q)!=keys or q['session']!=self.session or type(q['sequence'])is not int or not self.sequence<q['sequence']<2**53:raise ValueError('session/sequence differs')
                if path=='stop':self.brake()
                elif path=='command':
                    v=q['velocity_mps'];lease=q['lease_ms']
                    if not isinstance(v,list)or len(v)!=3 or any(type(x)not in (int,float)or not math.isfinite(x)for x in v)or v[1]!=0 or math.hypot(v[0],v[2])>.5 or type(lease)is not int or not 1<=lease<=500 or not self.clear():raise ValueError('invalid motion request')
                    self.velocity=list(v);self.deadline=time.monotonic()+lease/1000
                else:raise ValueError('unknown command')
                self.sequence=q['sequence']
            if len(self.commands)<10000:self.commands.append(dict(wall=time.time(),path=path,sequence=self.sequence,velocity=self.velocity[:]))
            return self.state()
    def copy_for_render(self):
        with self.lock:self.d.data.qpos[:]=self.data.qpos;self.d.data.qvel[:]=self.data.qvel
    def poll_capture(self):
        # Backward-compatible runner hook. The independent renderer services
        # bounded capture requests even while replica polling is waiting.
        pass
    def capture_loop(self):
        renderer=None
        try:
            self.capture_data=self.mj.MjData(self.model);renderer=self.mj.Renderer(self.model,height=120,width=160);self.capture_renderer=renderer;self.capture_ready.set()
            while not self.done.is_set():
                try:request=self.requests.get(timeout=.1)
                except queue.Empty:continue
                try:
                    if time.monotonic()>=request['deadline']:raise ValueError('capture request expired')
                    request['value']=self.capture()
                except Exception as error:request['error']=str(error)
                finally:request['done'].set()
        except Exception as error:self.capture_error=str(error);self.capture_ready.set()
        finally:
            if renderer:renderer.close()
    def capture(self):
        # Executed on the GL owner thread, using one frozen physical state.
        with self.lock:
            state=self.state();self.capture_data.qpos[:]=self.data.qpos;self.capture_data.qvel[:]=self.data.qvel;self.capture_data.mocap_pos[:]=self.data.mocap_pos;self.capture_data.time=self.data.time
        self.mj.mj_forward(self.model,self.capture_data);self.capture_sequence+=1;seq=self.capture_sequence;ns=round(state['simulation_time_s']*1e9);observations=[];media=[];axes=self.np.array([[1,0,0],[0,0,1],[0,-1,0.]])
        for name in ('camera-a','camera-b','gas-1','audio-1'):
            kind='camera'if name.startswith('camera')else'gas'if name=='gas-1'else'audio'
            sample=dict(schema_version='wendy.sensor.sample.v1',id=f'{self.d.run}/{name}/{seq}',site_id=self.d.config['site'],frame=self.d.config['frame'],sensor_id=name,kind=kind,sequence=seq,sample_ns=ns,run_id=self.d.run,scenario_version='incident-inspection-v1',seed=self.d.config['simulation']['seed'],package_sha256=self.d.contract['visual']['package_sha256'],uncertainty_m=.02,calibration_revision='incident-robot-cameras-v1',position_m=state['position_m'][:])
            blob=None
            if kind=='camera':
                cid=self.mj.mj_name2id(self.model,self.mj.mjtObj.mjOBJ_CAMERA,'inspect-'+name);rotation=self.capture_data.cam_xmat[cid].reshape(3,3);T=self.np.eye(4);T[:3,:3]=axes@rotation@self.np.diag([1,-1,-1]);T[:3,3]=axes@self.capture_data.cam_xpos[cid];sample['position_m']=T[:3,3].tolist();f=60/math.tan(math.radians(float(self.model.cam_fovy[cid]))/2);sample.update(sensor_model='mujoco-rgb-pinhole-v1',camera=dict(width=160,height=120,fx=f,fy=f,cx=80,cy=60,camera_to_site=T.reshape(-1).tolist(),calibration_valid_for_ns=10_000_000_000));self.capture_renderer.update_scene(self.capture_data,camera='inspect-'+name);blob=camera_png(self.capture_renderer.render());mime='image/png'
            elif kind=='gas':sample.update(sensor_model='analytic-fixture-v1',gas=dict(value_ppm=state['gas_ppm'],uncertainty_ppm=1.,species='synthetic-test-gas'))
            else:sample.update(sensor_model='generated-tone-v1',audio=dict(sample_rate_hz=8000,channels=1,duration_ns=250_000_000));blob=audio_wav(7,seq,True);mime='audio/wav'
            if blob is not None:
                sha=hashlib.sha256(blob).hexdigest();sample['evidence']=dict(sha256=sha,size=len(blob),content_type=mime);media.append(dict(sha256=sha,data=base64.b64encode(blob).decode()))
            observations.append(sample)
        return dict(schema_version='wendy.sim.live.v1',run_id=self.d.run,boot_id=self.boot,sequence=seq,sample_unix_ms=state['sample_unix_ms'],simulation_time_ns=ns,state=state,observations=observations,media=media)
    def close(self):
        self.done.set();self.physics.join(timeout=2);self.server.shutdown();self.server.server_close();self.http.join(timeout=2);self.capture_thread.join(timeout=2)
        with self.lock:self.brake()

class Handler(http.server.BaseHTTPRequestHandler):
    def setup(self):
        super().setup();self.connection.settimeout(3)
    def log_message(self,*a):pass
    def do_GET(self):self.respond(False)
    def do_POST(self):self.respond(True)
    def respond(self,post):
        self.connection.settimeout(3);r=self.server.robot;token=r.control_token if post else r.read_token
        if not hmac.compare_digest(self.headers.get('Authorization',''),'Bearer '+token):self.send_error(403);return
        try:
            name=self.path.removeprefix('/v1/inspection/')
            if post:
                size=int(self.headers.get('Content-Length','0'))
                if not 0<size<=4096:raise ValueError('bounded request required')
                value=r.command(name,json.loads(self.rfile.read(size)))
            elif name=='state':
                with r.lock:value=r.state()
            elif name=='samples':
                q={'done':threading.Event(),'deadline':time.monotonic()+.8};r.requests.put_nowait(q)
                if not q['done'].wait(3):raise ValueError('capture timeout')
                if 'error'in q:raise ValueError(q['error'])
                value=q['value']
            else:raise ValueError('unknown endpoint')
            raw=json.dumps(value,allow_nan=False,separators=(',',':')).encode();self.send_response(200);self.send_header('Content-Type','application/json');self.send_header('Content-Length',str(len(raw)));self.end_headers();self.wfile.write(raw)
        except Exception as e:self.send_error(400,str(e))
