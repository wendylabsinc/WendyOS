"""Explicit test-operator acceptance. Uses real native Review and Sign methods.

Requires an already-built private native app and all five Go binaries. Only this
opt-in test driver invokes operator actions; the product runner never does.
"""
from __future__ import annotations
import argparse,copy,json,math,subprocess,time
from pathlib import Path
import sys
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
from incident_inspection import InspectionDemo
from gas_source import atomic

def check(args):
    d=None;apps=[];logs=[];checks=[];qa={'east':0,'west':0};shots=[]
    def record(label,**details):checks.append(dict(label=label,**details));print(json.dumps(checks[-1]),flush=True)
    def pump():
        d.poll_selection();d.robot.poll_capture();d.poll_status();time.sleep(.06)
    def action(name,op,timeout=18):
        qa[name]+=1;sequence=qa[name];folder=d.out/('viewer-'+name);atomic(folder/'native-qa.json',dict(sequence=sequence,action=op));until=time.monotonic()+timeout
        while time.monotonic()<until:
            path=folder/f'native-receipt-{sequence:04d}.json'
            if path.exists():
                result=json.loads(path.read_text())
                if not result['accepted']:raise AssertionError(dict(action=op,receipt=result))
                return result
            pump()
        raise TimeoutError('native action timed out: '+op)
    def settle(label,state='consistent',timeout=20,gas_state=None):
        until=time.monotonic()+timeout
        while time.monotonic()<until:
            views=d.step(archive=True)
            if all(v['result']['state']==state and (gas_state is None or v['result']['gas']['state']==gas_state) for v in views.values()):record(label,state=state,sequence=d.sequence);return views
        raise AssertionError(dict(label=label,states={n:v['result']['state']for n,v in views.items()}))
    def capture(name,label,mission=False):
        result=action(name,'capture-inspection'if mission else'capture');shots.append(dict(operator=name,capture=result['capture'],title=label,duration=4))
    def no_motion():return not any(math.hypot(*[c['velocity'][i]for i in (0,2)])>0 for c in d.robot.commands)
    try:
        d=InspectionDemo(args);d.deadline=time.monotonic()+300
        for name in ('east','west'):
            log=open(d.out/'logs'/('native-'+name+'.log'),'w');logs.append(log);apps.append(subprocess.Popen([str(args.app/'Contents/MacOS/WendyWorldDemo'),'--incident',str(d.out/('viewer-'+name))],stdout=log,stderr=subprocess.STDOUT))
        settle('three live nodes and two independent consistent readers')
        time.sleep(.4)
        refused=d.readers['east'].call(dict(op='prepare-inspection',input_digest='0'*64),True);assert 'error'in refused;record('changed selected digest refused')
        original=d.last['east']['result']['input_digest'];until=time.monotonic()+4.2
        while time.monotonic()<until:pump()
        refused=d.readers['east'].call(dict(op='prepare-inspection',input_digest=original),True);assert 'error'in refused;record('expired original selection refused without renewing TTL')
        settle('fresh evidence restores selectable association')
        d.command('displace');views=settle('conflicting locations remain separate','conflicting')
        refused=d.readers['east'].call(dict(op='prepare-inspection',input_digest=views['east']['result']['input_digest']),True);assert 'error'in refused;record('conflicting association cannot prepare inspection')
        d.command('reset');d.source=[2.,0.];d.visual_position=d.source.copy()
        views=settle('boundary gas fit leaves only independent visual localization','visual_only',gas_state='ambiguous')
        assert all(v['result']['gas']['state']=='ambiguous' for v in views.values())
        refused=d.readers['east'].call(dict(op='prepare-inspection',input_digest=views['east']['result']['input_digest']),True);assert 'error'in refused;record('ambiguous gas fit cannot prepare inspection')
        d.source=[-.8,-.4];d.command('reset');settle('current consistent association selected')
        assert no_motion();capture('east','Current gas and visual evidence share a location. No motion has been authorized.')
        prepared=action('east','inspection-prepare');assert d.prepared and no_motion();record('prepare creates only proposal',native=prepared,commands=len(d.robot.commands))
        action('east','inspection-pose-expiry-check');record('duplicate snapshot and clock rollback cannot renew local robot marker')
        review=action('east','inspection-review');assert no_motion();record('explicit review creates only local draft',native=review)
        capture('east','Review the retained evidence and fixed nearby destination before signing.',True)
        selected=copy.deepcopy(d.prepared);trigger=selected['mission_input']['trigger_sha256'];destination=selected['navigation_target']
        d.command('displace');settle('live evidence changes after review','conflicting');assert d.prepared==selected and no_motion()
        record('later evidence does not retarget reviewed destination',trigger_sha256=trigger,destination=destination)
        signed=action('east','inspection-sign');task=signed['task_id'];assert task;record('operator explicitly signs one inspection',native=signed)
        until=time.monotonic()+12
        while time.monotonic()<until:
            d.step(archive=True)
            if all(n in d.statuses and d.statuses[n]['record']['spec']['id']==task for n in ('east','west')) and any(math.hypot(c['velocity'][0],c['velocity'][2])>0 for c in d.robot.commands):break
        else:raise AssertionError('mission did not start or replicate')
        assert all(r['record']['spec']['target']==destination and r['record']['spec']['trigger']['sha256']==trigger for r in d.statuses.values())
        capture('east','The explicitly signed planar robot follows the reviewed route.')
        capture('west','The second reader retains the same trigger and assignee status.')
        d.network['mission_partition']=True
        with d.robot.lock:partition_position=d.robot.state()['position_m']
        remote_phase=d.statuses['west']['record']['status']['Phase'];until=time.monotonic()+50
        while time.monotonic()<until:
            d.step(archive=True)
            if d.statuses['east']['record']['status']['Phase']=='succeeded':break
            if d.statuses['east']['record']['status']['Phase']in('failed','canceled'):raise AssertionError(d.statuses['east']['record']['status'])
        else:raise AssertionError('inspection did not complete before deadline')
        with d.robot.lock:end=d.robot.state()['position_m']
        moved=math.hypot(end[0]-partition_position[0],end[2]-partition_position[2]);assert moved>.2
        assert d.statuses['west']['record']['status']['Phase']==remote_phase and d.network['blocked']>0
        record('robot completes with local sensing while remote replica is partitioned',moved_after_partition_m=moved,remote_last_known=remote_phase,blocked=d.network['blocked'])
        capture('west','During the partition, this operator sees last-known status, not invented completion.')
        d.network['mission_partition']=False;until=time.monotonic()+15
        while time.monotonic()<until:
            d.step(archive=True)
            if all(d.statuses[n]['record']['status']['Phase']=='succeeded'and len(d.statuses[n]['verified_media'])==3 for n in ('east','west')):break
        else:raise AssertionError('terminal status or evidence did not reconcile')
        assert d.statuses['east']['record_sha256']==d.statuses['west']['record_sha256']
        for report in d.statuses.values():assert len(report['record']['result']['samples'])==4 and report['record']['spec']['trigger']['sha256']==trigger and report['record']['spec']['target']==destination
        record('both gateways retain identical terminal record and verified evidence',record_sha256=d.statuses['east']['record_sha256'],samples=4,media=3)
        for name in ('east','west'):atomic(d.out/('completed-result-'+name+'.json'),d.statuses[name])
        action('east','inspection-refresh');capture('east','Four robot-local samples and three verified media objects are retained.')
        capture('west','Reconnection brings the exact terminal record to the second operator.')
        # A second review exercises an explicit stop issued before task delivery.
        d.command('reset');settle('fresh selection for stop-before-delivery trial')
        action('east','inspection-prepare');second=action('east','inspection-review');future=second['task_id'];before=sum(math.hypot(c['velocity'][0],c['velocity'][2])>0 for c in d.robot.commands)
        stop=subprocess.run([str(args.world),'task','stop','--credentials',str(d.inspection/'operator.json'),'--id',future,'--json'],capture_output=True,text=True,timeout=5);assert stop.returncode==0,stop.stderr
        action('east','inspection-sign');until=time.monotonic()+4
        while time.monotonic()<until:d.step()
        after=sum(math.hypot(c['velocity'][0],c['velocity'][2])>0 for c in d.robot.commands);assert after==before
        record('explicit stop before delivery prevents new movement',task_id=future,nonzero_commands_before=before,nonzero_commands_after=after)
        atomic(d.out/'acceptance.json',dict(passed=True,checks=checks,shots=shots,node_pids={str(n):p.pid for n,p in d.nodes.items()},executor_pid=d.executor.pid,native_pids=[p.pid for p in apps],mujoco_version=d.mj.__version__,limitations=['Actual planar slide-joint physics and rendered robot-local images; synthetic analytic gas and audio','Private loopback simulation authority; no physical robot or automatic operator approval','Native frames are captured through the actual control/model path']))
    except BaseException as error:
        if d:atomic(d.out/'acceptance.json',dict(passed=False,checks=checks,shots=shots,error=str(error),exception=type(error).__name__))
        raise
    finally:
        for p in reversed(apps):
            if p.poll()is None:
                p.terminate()
                try:p.wait(timeout=4)
                except subprocess.TimeoutExpired:p.kill();p.wait(timeout=2)
        for log in logs:log.close()
        if d:d.close()

if __name__=='__main__':
    p=argparse.ArgumentParser();p.add_argument('--out',type=Path,required=True)
    for n in ('executable','gas-executable','worldd','executor','shared','world','app'):p.add_argument('--'+n,type=Path,required=True)
    p.add_argument('--base-port',type=int,default=19580);p.add_argument('--seed',type=int,default=73)
    check(p.parse_args())
