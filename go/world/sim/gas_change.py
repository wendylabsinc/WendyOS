"""Bounded original-history change detection across three real World nodes.

MuJoCo supplies actual placements/rendering; concentrations are explicitly
analytic synthetic fixtures. Neither inferred source truth nor motion is used.
"""
from __future__ import annotations
import argparse
import concurrent.futures
import hashlib
import json
import math
import os
from pathlib import Path
import signal
import subprocess
import time
import threading
from gas_replicas import ReplicaDemo, RPC
from gas_source import GasDemo, atomic, png

class ChangeDemo(ReplicaDemo):
    def __init__(self,*args,**kwargs):
        super().__init__(*args,**kwargs)
        self.mode='steady';self.level=80.;self.level_at=time.monotonic();self.dropout=False
        self.native={};self.native_logs={};self.qa_seq={'east':0,'west':0}
        self.last_history={};self.last_samples=[];self.deadline=time.monotonic()+600
        self.sample_lock=threading.RLock()
        self.sample_sequence=0;self.last_publication={};self.templates=[GasDemo.sample(self,s) for s in self.config['sensors'][:4]]
    def command(self,action,target=''):
        if action in ('ramp','decline','steady','spike','opposing'): self.mode=action
        elif action=='dropout':self.dropout=not self.dropout
        elif action in ('partition','duplicates','bias','restart_east'):super().command(action,target)
        else:raise ValueError('unknown gas-change control')
        self.events.append(dict(time_s=time.time()-self.anchor,action=action,target=target));self.events=self.events[-12:]
    def restart_east(self):
        with self.sample_lock:super().restart_east()
        self.restart_warmup=self.fusers['east'].call({'op':'tick'})
        atomic(self.out/'restart-warmup.json',self.restart_warmup)
    def sample_once(self):
        start=time.monotonic();dt=min(2,start-self.level_at);self.level_at=start
        if self.mode in ('ramp','opposing'):self.level=min(9000,self.level+15*dt)
        elif self.mode=='decline':self.level=max(20,self.level-15*dt)
        if self.duplicates:
            self.last_publication=self.sensors.call({'op':'duplicates'});return None
        captured=time.time()-self.anchor;self.sample_sequence+=1;observations=[]
        for template,sensor in zip(self.templates,self.config['sensors'][:4]):
            if self.dropout and sensor['id']=='d':continue
            o=json.loads(json.dumps(template));p=sensor['position_xz'];background=4*math.exp(-((p[0]+.8)**2+(p[1]+.4)**2)/6)
            value=self.level+background
            if self.mode=='opposing' and sensor['id']=='a':value=600-self.level+background
            if self.mode=='spike':value+=30
            if self.bias and sensor['id']=='a':value+=40
            o.update(version=dict(epoch=1,sequence=self.sample_sequence),id=f"{sensor['id']}/capture/{self.sample_sequence}",captured_s=captured,valid_until_s=captured+4.,value_ppm=value+self.rng.uniform(-.03,.03),uncertainty_ppm=.1)
            observations.append(o)
        self.last_samples=observations;atomic(self.out/'sensor-input'/f'{self.sample_sequence:05d}.json',observations)
        self.last_publication=self.sensors.call(dict(op='publish',observations=observations))
        atomic(self.out/'sensor-input'/f'{self.sample_sequence:05d}-receipt.json',self.last_publication)
        if self.mode=='spike':self.mode='steady'
        return captured
    def step(self,archive=True,capture=True):
        if time.monotonic()>self.deadline:raise TimeoutError('bounded simulation deadline')
        self.poll_commands();self.sequence+=1
        self.data.time=time.time()-self.anchor
        for _ in range(10):self.mj.mj_step(self.model,self.data)
        self.mj.mj_forward(self.model,self.data)
        captured=self.sample_once() if capture else None
        publication=self.last_publication
        raw_end=time.monotonic()+.55
        while True:
            fusers=self.parallel(self.fusers,{'op':'tick'})
            for name,value in fusers.items():self.last_fusers[name]={**self.last_fusers.get(name,{}),**value}
            def complete(value):
                return all(t['points'] and abs(t['points'][-1]['captured_s']-captured)<1e-7 for t in value['result']['window']['traces'] if not(self.dropout and t['sensor']=='d'))
            if captured is None or self.network['partition'] or all(complete(v) for v in fusers.values()) or time.monotonic()>=raw_end:break
            time.sleep(.04)
        expected={name:value.get('sha256') if value['result']['window']['state'] not in ('unknown','warming') else None for name,value in self.last_fusers.items()}
        repair_end=time.monotonic()+.55
        while True:
            self.last=self.parallel(self.readers,{'op':'tick'})
            def delivered(view):
                return all(any(c['gateway']==name and ((digest is None and not c.get('window')) or c.get('sha256')==digest) for c in view['selection']['candidates']) for name,digest in expected.items())
            if all(delivered(v) for v in self.last.values()) or self.network['partition'] or time.monotonic()>=repair_end:break
            time.sleep(.04)
        for s in self.config['sensors']:
            gid=self.mj.mj_name2id(self.model,self.mj.mjtObj.mjOBJ_GEOM,'lamp-'+s['id']);self.model.geom_rgba[gid]=[.4,.43,.45,1] if s['id'] not in 'abcd' or self.dropout and s['id']=='d' else [.07,.58,.51,1]
        self.model.geom_rgba[self.mj.mj_name2id(self.model,self.mj.mjtObj.mjOBJ_GEOM,'source-marker'),3]=0
        self.renderer.update_scene(self.data,camera='overview');raw=png(self.renderer.render());image=f'scene-{self.sequence%2}.png';digest=hashlib.sha256(raw).hexdigest()
        written=time.time();run_now=written-self.anchor
        for name,value in self.last.items():
            # Remaining lifetimes were calculated before rendering. Conservatively
            # subtract elapsed host time when crossing this local UI bridge.
            import datetime
            received=datetime.datetime.fromisoformat(value['received_at'].replace('Z','+00:00')).timestamp()
            for c in value['selection']['candidates']:
                if c.get('window'):
                    c['remaining_s']=max(0,min(c['remaining_s']-(written-received),c['window']['valid_until_s']-run_now))
                    if c['remaining_s']<=0:c.update(state='unknown',reason='Original support expired during delivery',remaining_s=0);c.pop('window',None)
            live_states={c['state'] for c in value['selection']['candidates'] if c.get('window')}
            value['selection']['state']=next(iter(live_states)) if len(live_states)==1 else 'mixed' if live_states else 'unknown'
            snap=dict(schema='wendy.gas-change-demo.v1',run_id=self.run,reader=name,node=value['node'],sequence=self.sequence,last_command_sequence=self.command_sequences[name],now_s=run_now,written_unix_s=written,selection=value['selection'],image=image,image_sha256=digest,mode=self.mode,partition=self.network['partition'],duplicates=self.duplicates,dropout=self.dropout,bias=self.bias,controls=name=='east')
            folder=self.out/('viewer-'+name);tmp=folder/(image+'.tmp');tmp.write_bytes(raw);os.replace(tmp,folder/image);atomic(folder/'snapshot.json',snap)
            if archive:atomic(self.out/'captures'/f'{self.sequence:05d}-{name}.json',snap)
        if archive:
            atomic(self.out/'captures'/f'{self.sequence:05d}-transport.json',dict(publication=publication,estimators=fusers,readers=self.last,blocked=self.network['blocked']))
            (self.out/'captures'/f'{self.sequence:05d}.png').write_bytes(raw)
        return self.last
    def launch_native(self,app):
        for name in ('east','west'):
            folder=self.out/('viewer-'+name);log=open(self.out/'logs'/f'native-{name}.log','a');self.native_logs[name]=log
            self.native[name]=subprocess.Popen([str(Path(app)/'Contents/MacOS/WendyWorldDemo'),'--gas-change',str(folder)],stdout=log,stderr=log)
        time.sleep(.4)
    def qa(self,action,name='east'):
        self.qa_seq[name]+=1;seq=self.qa_seq[name];folder=self.out/('viewer-'+name)
        atomic(folder/'native-qa.json',dict(sequence=seq,action=action))
        end=time.monotonic()+7
        while time.monotonic()<end:
            self.step();path=folder/f'native-receipt-{seq:04d}.json'
            if path.exists():
                value=json.loads(path.read_text())
                if not value['accepted']:raise AssertionError(dict(native_control=action,receipt=value))
                return value
            time.sleep(.12)
        raise TimeoutError('native action receipt unavailable '+action)
    def close(self):
        for p in getattr(self,'native',{}).values():
            if p.poll() is None:
                p.terminate()
                try:p.wait(timeout=3)
                except subprocess.TimeoutExpired:p.kill();p.wait(timeout=2)
        for f in getattr(self,'native_logs',{}).values():f.close()
        super().close()

def check(d,app=None):
    checks=[]
    def record(value):
        checks.append(value);atomic(d.out/'checks-progress.json',dict(checks=checks,captures=d.sequence))
    def settle(label,state,minimum=1.3,timeout=12):
        start=time.monotonic();observed=[]
        while time.monotonic()-start<timeout:
            views=d.step();states={n:v['selection']['state'] for n,v in views.items()};observed.append(states)
            if time.monotonic()-start>=minimum and all(v==state for v in states.values()):
                record(dict(label=label,state=state,sequence=d.sequence,observed=observed));return views
            time.sleep(.06)
        raise AssertionError(dict(label=label,expected=state,observed=observed,last=d.last_fusers))
    settle('steady original history on two independent readers','steady')
    if app:d.launch_native(app);d.qa('capture');d.qa('capture','west')
    def control(action):
        if app:d.qa(action)
        else:d.command(action)
    control('ramp');settle('sustained multi-sensor rise','rising',2)
    if app:d.qa('capture');d.qa('capture','west')
    # Connected comparison checks exact per-source published bytes, not a claim
    # of consensus about past episodes after different reader histories.
    until=time.monotonic()+6
    while True:
        views=d.last;hashes={n:{c['gateway']:c.get('sha256') for c in v['selection']['candidates'] if c.get('window') and c['remaining_s']>0} for n,v in views.items()}
        if hashes['east']==hashes['west'] and len(hashes['east'])==2:break
        if time.monotonic()>until:raise AssertionError(dict(hash_comparison=hashes))
        d.step();time.sleep(.05)
    record(dict(label='both readers retain identical complete claim hashes per gateway',hashes=hashes))
    control('steady');settle('steady after rise is not a clearance assertion','steady',2)
    control('bias');settle('fixed single-sensor bias does not create sustained rise','steady',2)
    control('bias');settle('bias removal settles','steady',2)
    control('spike');views=d.step();record(dict(label='isolated transient retained without sustained episode',sequence=d.sequence,states={n:v['selection']['state'] for n,v in views.items()}));settle('spike does not persist as rise','steady',2)
    control('opposing');settle('opposing sensor changes remain mixed','mixed',2)
    if app:d.qa('capture')
    control('decline');settle('sustained decline is distinct from safety','falling',2)
    if app:d.qa('capture')
    control('dropout');settle('missing commissioned sensor becomes unknown','unknown',2)
    if app:d.qa('capture')
    control('dropout');control('ramp');settle('fresh captures rebuild history after dropout','rising',2)
    control('partition');settle('partition cannot manufacture a complete window','unknown',2)
    assert d.network['blocked']>0
    if app:d.qa('capture')
    control('partition');settle('repair requires fresh chronological samples','rising',2)
    control('duplicates');settle('duplicate original packets cannot extend dwell or expiry','unknown',2)
    if app:d.qa('capture');d.qa('capture','west')
    control('duplicates');settle('new originals restore rising window','rising',2)
    before=d.last_fusers['east'].get('component',{}).get('version',{}).get('epoch')
    control('restart_east');views=d.step();assert d.restart_warmup['result']['window']['state'] in ('warming','unknown'),d.restart_warmup
    record(dict(label='restart clears inferred dwell instead of reconstructing history',sequence=d.sequence))
    settle('fresh post-restart sequence restores rise','rising',2)
    epoch=d.last_fusers['east'].get('component',{}).get('version',{}).get('epoch');assert before and epoch>before,(before,epoch)
    record(dict(label='publisher restart uses durable newer epoch',before=before,after=epoch))
    if app:d.qa('capture')
    return dict(passed=True,checks=checks,captures=d.sequence,blocked_repairs=d.network['blocked'],run_id=d.run,simulation='MuJoCo placements/rendering; analytic synthetic concentrations; no motion',limits=['Window rule is not calibrated confidence, exposure clearance or physical leak diagnosis.','Nested originals are gateway-attested under existing World trust.','Latest-value repair cannot reconstruct missing history; reader episodes are local historical observations.'])

def main():
    p=argparse.ArgumentParser();p.add_argument('--out',required=True);p.add_argument('--executable',required=True);p.add_argument('--worldd',required=True);p.add_argument('--base-port',type=int,default=29480);p.add_argument('--app');p.add_argument('--check',action='store_true');p.add_argument('--seconds',type=int,default=120);a=p.parse_args();os.umask(0o077)
    d=None;result=None
    try:
        d=ChangeDemo(a.out,Path(a.executable),Path(a.worldd),a.base_port)
        if a.check:result=check(d,a.app)
        else:
            d.step()
            if a.app:d.launch_native(a.app)
            end=time.monotonic()+min(600,a.seconds)
            while time.monotonic()<end:d.step();time.sleep(.06)
            result=dict(passed=True,captures=d.sequence,run_id=d.run)
    except BaseException as e:
        result=dict(passed=False,error=repr(e),captures=getattr(d,'sequence',0));raise
    finally:
        if d:d.close()
        if result:atomic(Path(a.out)/'acceptance.json',result)
        runtime=Path(a.out)/'runtime'
        if runtime.exists():
            for f in runtime.glob('*.json'):
                if f.name!='public.json':f.unlink()
if __name__=='__main__':main()
