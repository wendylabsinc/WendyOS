"""Three-node exact-rise inspection with existing explicit operator authority.

Actual MuJoCo slide-joint simulation, rendered cameras and analytic gas. The
product runner never invokes Review or Sign. The separate check driver does so
only as an explicit simulated operator.
"""
from __future__ import annotations
import argparse,json,secrets,signal,subprocess,time,threading
from pathlib import Path
from incident_change import ChangeIncidentDemo
from incident_inspection import InspectionDemo
from incident_robot import Robot,inspection_scene
from gas_source import atomic
from gas_replicas import RPC

class SerialRPC(RPC):
    """One request/reply owner per process, including asynchronous publication."""
    def __init__(self,*args):
        self.call_lock=threading.Lock();super().__init__(*args)
    def call(self,req,allow_error=False):
        with self.call_lock:return super().call(req,allow_error)

class RisingInspectionDemo(ChangeIncidentDemo):
    build_scene=staticmethod(inspection_scene)
    def rpc(self,exe,role,name):
        return SerialRPC(exe,role,self.runtime/(name+'.json'),self.out/'logs'/(name+'.log'))
    def poll_selection(self):
        for future in self.publication_futures.values():future.result(timeout=7)
        InspectionDemo.poll_selection(self)
    poll_status=InspectionDemo.poll_status
    write_status=InspectionDemo.write_status
    def __init__(self,args):
        self.args=args;self.robot=None;self.executor=None;self.executor_log=None;self.request_ids=set();self.prepared={};self.task=None;self.statuses={};self.last_status=0
        super().__init__(args.out,args.executable,args.worldd,args.base_port,args.incident_executable,args.trend_executable,args.seed,args.linux_worldd,args.linux_image,inspection=True)
        try:
            self.inspection=self.runtime/'inspection';atomic(self.out/'public-trust.json',json.loads((self.runtime/'member-trust.json').read_text()));read=secrets.token_hex(32);control=secrets.token_hex(32)
            for name,value in [('scene-read-token',read),('scene-control-token',control)]:(self.inspection/name).write_text(value)
            self.robot=Robot(self,args.base_port+6,read,control)
            self.executor_log=open(self.out/'logs/executor.log','w')
            self.executor=subprocess.Popen([str(args.executor),'--credentials',str(self.inspection/'executor.json'),'--adapter',f'http://127.0.0.1:{args.base_port+6}','--adapter-token-file',str(self.inspection/'scene-control-token'),'--adapter-read-token-file',str(self.inspection/'scene-read-token'),'--run-id',self.run,'--frame',self.config['frame']['id'],'--revision',self.config['frame']['revision'],'--planar-config',str(self.inspection/'planar.json')],stdout=self.executor_log,stderr=subprocess.STDOUT)
            ui=json.loads((self.inspection/'ui.json').read_text());ui['handoff_file']=str(self.out/'viewer-east/mission-handoff.json');atomic(self.out/'viewer-east/mission-ui.json',ui)
            for name in ('east','west'):self.write_status(name)
        except Exception:self.close();raise
    def step(self,archive=True,capture=True):
        # A request binds the exact snapshot already shown. Drain that snapshot's
        # publication before handling selection, without synthesizing a capture.
        for future in self.publication_futures.values():future.result(timeout=7)
        self.poll_selection()
        if self.robot:self.robot.copy_for_render()
        result=super().step(archive,capture)
        if self.robot:self.robot.poll_capture()
        self.poll_status();return result
    def command(self,action,target=''):
        if action=='mission-partition':self.network['mission_partition']=not self.network.get('mission_partition',False);return
        super().command(action,target)
    def close(self):
        processes=[]
        if self.executor:
            if self.executor.poll()is None:
                self.executor.terminate()
                try:self.executor.wait(timeout=3)
                except subprocess.TimeoutExpired:self.executor.kill();self.executor.wait(timeout=2)
            processes.append(dict(role='planar-executor',pid=self.executor.pid,exit_code=self.executor.returncode))
        if self.executor_log:self.executor_log.close();self.executor_log=None
        if self.robot:
            self.robot.close();atomic(self.out/'robot-receipt.json',dict(commands=self.robot.commands,poses=self.robot.poses));self.robot=None
        super().close()
        if hasattr(self,'runtime'):
            removed=[]
            for path in self.runtime.rglob('*'):
                if path.is_file() and ((path.parent==self.runtime and path.suffix=='.json' and path.name!='public.json') or (path.parent==self.runtime/'inspection' and path.name in ('operator.json','observer.json','proposer.json','executor.json','evidence-reader.json','ui.json','scene-read-token','scene-control-token'))):
                    removed.append(str(path.relative_to(self.runtime)));path.unlink()
            for path in (self.out/'viewer-east/mission-ui.json',):
                if path.exists():path.unlink()
            atomic(self.out/'cleanup.json',dict(owned_processes_stopped=True,executor=processes,private_configs_removed=removed))

def arguments():
    p=argparse.ArgumentParser();p.add_argument('--out',type=Path,required=True)
    for name in ('executable','incident-executable','trend-executable','worldd','executor','shared','world'):p.add_argument('--'+name,type=Path,required=True)
    p.add_argument('--linux-worldd',type=Path);p.add_argument('--linux-image');p.add_argument('--base-port',type=int,default=29900);p.add_argument('--seed',type=int,default=73);p.add_argument('--seconds',type=int,default=300);p.add_argument('--app',type=Path);p.add_argument('--check',action='store_true')
    a=p.parse_args()
    if bool(a.linux_worldd)!=bool(a.linux_image)or not 1<=a.seconds<=900:raise ValueError('bounded venue/duration required')
    return a

def main():
    args=arguments();d=None;started=time.time()
    def stop(*_):raise KeyboardInterrupt
    signal.signal(signal.SIGTERM,stop);signal.signal(signal.SIGINT,stop)
    try:
        d=RisingInspectionDemo(args);d.deadline=time.monotonic()+args.seconds
        if args.check:
            from check_rising_inspection import check
            result=check(d,args);result.update(started_unix_s=started,finished_unix_s=time.time(),mujoco_version=d.mj.__version__);atomic(d.out/'acceptance.json',result)
        else:
            if args.app:d.launch_native(args.app)
            while time.monotonic()<d.deadline:d.step();time.sleep(.05)
    except BaseException as error:
        if d:
            progress=d.out/'checks-progress.json';checks=json.loads(progress.read_text()).get('checks',[]) if progress.exists() else []
            atomic(d.out/'acceptance.json',dict(passed=False,error=repr(error),checks=checks,started_unix_s=started,finished_unix_s=time.time()))
        raise
    finally:
        if d:d.close()
if __name__=='__main__':main()
