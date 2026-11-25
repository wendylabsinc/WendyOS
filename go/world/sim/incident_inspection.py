"""Explicitly reviewed inspection of a selected gas/visual incident.

Three normal World nodes, separate native operator credentials, and the existing
planar executor. Preparing a proposal cannot sign or deliver a mission. The
optional acceptance driver acts as an explicit simulated operator via the same
native Review and Sign controls.
"""
from __future__ import annotations
import argparse,base64,copy,hashlib,json,os,secrets,signal,subprocess,time,uuid
from pathlib import Path
import sys
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
from incident_association import IncidentDemo
from incident_robot import Robot,inspection_scene
from gas_source import atomic

class InspectionDemo(IncidentDemo):
    def build_scene(self):return inspection_scene()
    def __init__(self,args):
        self.args=args;self.robot=None;self.executor=None;self.executor_log=None;self.request_ids=set();self.prepared={};self.task=None;self.statuses={};self.last_status=0
        super().__init__(args.out,args.executable,args.gas_executable,args.worldd,args.base_port,args.seed,inspection=True)
        try:
            self.inspection=self.runtime/'inspection';read=secrets.token_hex(32);control=secrets.token_hex(32)
            for n,v in [('scene-read-token',read),('scene-control-token',control)]: (self.inspection/n).write_text(v)
            self.robot=Robot(self,args.base_port+6,read,control)
            self.executor_log=open(self.out/'logs/executor.log','w')
            self.executor=subprocess.Popen([str(args.executor),'--credentials',str(self.inspection/'executor.json'),'--adapter',f'http://127.0.0.1:{args.base_port+6}','--adapter-token-file',str(self.inspection/'scene-control-token'),'--adapter-read-token-file',str(self.inspection/'scene-read-token'),'--run-id',self.run,'--frame',self.config['frame']['id'],'--revision',self.config['frame']['revision'],'--planar-config',str(self.inspection/'planar.json')],stdout=self.executor_log,stderr=subprocess.STDOUT)
            ui=json.loads((self.inspection/'ui.json').read_text());ui['handoff_file']=str(self.out/'viewer-east/mission-handoff.json');atomic(self.out/'viewer-east/mission-ui.json',ui)
            # Both readers query their own gateway; only east has a signing UI.
            for n in ('east','west'):self.write_status(n)
        except Exception:self.close();raise
    def step(self,archive=False):
        self.poll_selection()
        if self.robot:self.robot.poll_capture();self.robot.copy_for_render()
        result=super().step(archive)
        if self.robot:self.robot.poll_capture()
        self.poll_status()
        return result
    def poll_selection(self):
        folder=self.out/'viewer-east';path=folder/'selection-request.json'
        try:
            if path.stat().st_size>4096:return
            q=json.loads(path.read_text());id=q['request_id']
            if id in self.request_ids:return
            if len(self.request_ids)>=32:raise ValueError('selection limit reached')
            self.request_ids.add(id)
            if set(q)!={'request_id','run_id','input_digest'}or q['run_id']!=self.run or not isinstance(id,str)or len(id)!=36 or any(c not in '0123456789abcdef-'for c in id):raise ValueError('selection scope differs')
            selected=self.readers['east'].call(dict(op='prepare-inspection',input_digest=q['input_digest']))
            target=self.inspection/(id+'-input.json');handoff=self.inspection/(id+'-handoff.json');atomic(target,selected)
            with self.robot.lock:atomic(self.out/'planning-state.json',self.robot.state())
            command=[str(self.args.shared),'propose','--credentials',str(self.inspection/'proposer.json'),'--mission-input',str(target),'--handoff',str(handoff),'--id',id,'--navigation-directory',str(self.inspection),'--source-read-token-file',str(self.inspection/'scene-read-token')]
            completed=subprocess.run(command,capture_output=True,text=True,timeout=18)
            if completed.returncode:raise ValueError(completed.stderr.strip()[:250])
            prepared=json.loads(handoff.read_text());self.prepared=prepared;self.task=None;self.statuses={};atomic(folder/'mission-handoff.json',prepared)
            atomic(folder/'selection-response.json',dict(request_id=id,envelope_sha256=selected['envelope_sha256']))
            self.write_status('east');self.write_status('west')
        except FileNotFoundError:return
        except Exception as error:
            if 'id'in locals():atomic(folder/'selection-response.json',dict(request_id=id,error=str(error)[:300]))
    def poll_status(self):
        path=self.out/'viewer-east/inspection-task.json'
        try:
            value=json.loads(path.read_text())
            if set(value)=={'task_id','run_id','trigger_sha256'}and value['run_id']==self.run and isinstance(value['task_id'],str)and len(value['task_id'])<=128 and self.prepared and value['trigger_sha256']==self.prepared['mission_input']['trigger_sha256']:self.task=value['task_id']
        except(FileNotFoundError,ValueError):pass
        if not self.task or time.monotonic()-self.last_status<.8:return
        self.last_status=time.monotonic()
        for name in ('east','west'):
            command=[str(self.args.shared),'result','--credentials',str(self.inspection/('operator.json'if name=='east'else'observer.json')),'--id',self.task,'--evidence-credentials',str(self.inspection/'evidence-reader.json'),'--out',str(self.out/('evidence-'+name))]
            if name=='west':command+=['--origin','node-2']
            try:
                result=subprocess.run(command,capture_output=True,text=True,timeout=6)
                if result.returncode:raise ValueError(result.stderr.strip()[:200])
                report=json.loads(result.stdout);record=report['record']
                if record['spec']['id']!=self.task:raise ValueError('task differs')
                if self.prepared and record['spec']['trigger']['sha256']!=self.prepared['mission_input']['trigger_sha256']:raise ValueError('selected trigger changed')
                self.statuses[name]=report;atomic(self.out/('result-'+name+'.json'),report)
                self.write_status(name)
            except Exception as error:self.write_status(name,str(error))
    def write_status(self,name,error=''):
        report=self.statuses.get(name);prepared=self.prepared;record=report['record']if report else None
        value=dict(snapshot_unix_ms=time.time_ns()//1_000_000,run_id=self.run,reader=name,phase='not_requested',knowledge='No inspection has been authorized.',samples=0,media_verified=0)
        if prepared:
            selected=prepared['mission_input'];trigger=json.loads(base64.b64decode(selected['trigger_json']))
            value.update(phase='prepared',knowledge='Retained selection for review. No movement authority.',trigger_sha256=selected['trigger_sha256'],observed=selected['target'],destination=prepared['navigation_target'],source_capture=trigger['component']['timing']['sample_time'],source_valid_until=selected['sample_valid_until'],route=prepared['navigation']['route_world'])
        if record:
            value.update(task_id=record['spec']['id'],phase=record['status']['Phase'],knowledge=('Last-known signed assignee status from node-3.'if name=='west'else'Last-known assignee status from node-2.'),destination=record['spec']['target'],trigger_sha256=record['spec']['trigger']['sha256'],record_sha256=report['record_sha256'],samples=len((record.get('result')or{}).get('samples',[])),media_verified=len(report['verified_media']))
        if error:value['knowledge']='Replica unavailable. Retained status is historical.'
        if self.robot:
            with self.robot.lock:value['robot_position']=self.robot.state()['position_m']
        atomic(self.out/('viewer-'+name)/'inspection-status.json',value)
    def command(self,action,target=''):
        if action=='mission-partition':self.network['mission_partition']=not self.network.get('mission_partition',False);return
        super().command(action,target)
    def close(self):
        if self.executor and self.executor.poll()is None:
            self.executor.terminate()
            try:self.executor.wait(timeout=3)
            except subprocess.TimeoutExpired:self.executor.kill();self.executor.wait(timeout=2)
        if self.executor_log:self.executor_log.close();self.executor_log=None
        if self.robot:
            self.robot.close();atomic(self.out/'robot-receipt.json',dict(commands=self.robot.commands,poses=self.robot.poses));self.robot=None
        super().close()

def main():
    p=argparse.ArgumentParser();p.add_argument('--out',type=Path,required=True)
    for n in ('executable','gas-executable','worldd','executor','shared'):p.add_argument('--'+n,type=Path,required=True)
    p.add_argument('--duration',type=int,default=300);p.add_argument('--base-port',type=int,default=19580);p.add_argument('--seed',type=int,default=73)
    args=p.parse_args()
    if not 1<=args.duration<=1800:raise ValueError('bounded duration1..1800 required')
    d=None
    def stop(*_):raise KeyboardInterrupt
    signal.signal(signal.SIGTERM,stop);signal.signal(signal.SIGINT,stop)
    try:
        d=InspectionDemo(args);d.deadline=time.monotonic()+args.duration
        while time.monotonic()<d.deadline:
            d.poll_commands();d.step(archive=d.sequence==0 or d.sequence%5==0)
            for _ in range(5):
                d.poll_selection();d.robot.poll_capture();time.sleep(.05)
    except KeyboardInterrupt:pass
    finally:
        if d:d.close()
if __name__=='__main__':main()
