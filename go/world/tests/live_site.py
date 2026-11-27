"""Run hazard, autonomous inspection and retained result in one live MuJoCo scene."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import uuid
from virtual_site import command


def run(node_image, scene_image):
    suffix=uuid.uuid4().hex[:10]
    peer,local,capture=[f"live-{part}-{suffix}" for part in ('peer','local','capture')]
    nodes=[f"live-node-{i}-{suffix}" for i in range(1,4)]
    scene,executor,fuser=[f"live-{part}-{suffix}" for part in ('scene','executor','fusion')]
    created=[];networks=[];user=f"{os.getuid()}:{os.getgid()}"
    with tempfile.TemporaryDirectory(prefix='wendy-live-') as tmp:
        folder=Path(tmp)
        try:
            command(['go','run','./cmd/world-lab','--out',str(folder),'--runtime-dir','/state','--containers','--missions','--sensors','--run-id','live-test'])
            for network in (peer,local,capture):command(['docker','network','create','--internal',network]);networks.append(network)
            for i,name in enumerate(nodes,1):
                state=folder/f'state-{i}';state.mkdir(mode=0o700)
                command(['docker','run','-d','--name',name,'--network',local if i==1 else peer,'--network-alias',f'world-node-{i}','--user',user,
                    '--mount',f'type=bind,src={folder/f"node-{i}.json"},dst=/config.json,readonly','--mount',f'type=bind,src={state},dst=/state',node_image,'--config','/config.json']);created.append(name)
            command(['docker','network','connect','--alias','world-node-1',peer,nodes[0]])
            command(['docker','run','-d','--name',scene,'--network',local,'--network-alias','scene','--user',user,
                '--mount',f'type=bind,src={folder/"adapter.token"},dst=/token,readonly',
                '--mount',f'type=bind,src={folder/"sensor.token"},dst=/sensor-token,readonly',scene_image,
                'examples/live-office/world.json','--token-file','/token','--sensor-token-file','/sensor-token','--run-id','live-test',
                '--host','0.0.0.0','--allow-network-simulation','--allow-test-faults']);created.append(scene)
            command(['docker','network','connect','--alias','scene',capture,scene])
            def until(fn,predicate,timeout=20):
                end=time.monotonic()+timeout;last=None
                while time.monotonic()<end:
                    try:
                        last=fn()
                        if predicate(last):return last
                    except (RuntimeError,OSError,KeyError) as error:last=str(error)
                    time.sleep(.1)
                raise AssertionError(f'condition not met: {last}')
            def physics(path='state',data=None):
                script="""import json,urllib.request,sys
request=urllib.request.Request('http://127.0.0.1:8091/v1/'+sys.argv[1],data=None if sys.argv[2]=='null' else sys.argv[2].encode(),headers={'Authorization':'Bearer '+open('/token').read(),'Content-Type':'application/json'})
with urllib.request.urlopen(request,timeout=2) as response: print(response.read().decode())
"""
                return json.loads(command(['docker','exec',scene,'python','-c',script,path,json.dumps(data)]))
            def cli(binary,credential,*args,network=peer,mounts=()):
                extra=[]
                for source,dest in mounts:extra+=['--mount',f'type=bind,src={source},dst={dest},readonly']
                return json.loads(command(['docker','run','--rm','--network',network,'--user',user,'--entrypoint','/'+binary,
                    '--mount',f'type=bind,src={folder/credential},dst=/client.json,readonly',*extra,node_image,*args,'--credentials','/client.json']))
            def query(i):return cli('world-query',f'viewer-{i}.json',network=local if i==1 else peer)
            def tasks(network=peer):return cli('world','operator-1.json','task','list','--json',network=network)['tasks']
            def task(task_id,network=peer):return next(r for r in tasks(network) if r['spec']['id']==task_id)
            def disk_task(task_id):return json.loads((folder/'state-1'/'node-1.tasks').read_text())['state']['records'][task_id]['record']
            def hazards(s):return [c for c in s['components'] if c['key']['type']=='wendy.hazard.v1']
            until(physics,lambda s:s['synthetic'])
            until(lambda:physics('samples'),lambda s:len(s['observations'])==4)
            until(lambda:tasks(local),lambda t:not t)
            command(['docker','run','-d','--name',executor,'--network',local,'--user',user,'--entrypoint','/world-executor',
                '--mount',f'type=bind,src={folder/"executor-1.json"},dst=/client.json,readonly','--mount',f'type=bind,src={folder/"adapter.token"},dst=/token,readonly',
                node_image,'--credentials','/client.json','--adapter','http://scene:8091','--adapter-token-file','/token','--run-id','live-test']);created.append(executor)
            until(physics,lambda s:s['sequence']>0)
            for sensor in ('camera-a','camera-b','gas-1','audio-1'):
                name=f'live-publisher-{sensor}-{suffix}'
                command(['docker','run','-d','--name',name,'--network',peer if sensor=='camera-b' else local,'--user',user,'--entrypoint','/world-live-sense',
                    '--mount',f'type=bind,src={folder/f"sensor-{sensor}.json"},dst=/client.json,readonly','--mount',f'type=bind,src={folder/"sensor.token"},dst=/sensor-token,readonly',
                    node_image,'--credentials','/client.json','--adapter','http://scene:8091','--sensor-token-file','/sensor-token','--sensor',sensor,'--run-id','live-test','--ttl','3s']);created.append(name)
                if sensor=='camera-b':command(['docker','network','connect',capture,name])
            command(['docker','run','-d','--name',fuser,'--network',peer,'--user',user,'--entrypoint','/world-fuse',
                '--mount',f'type=bind,src={folder/"fusion.json"},dst=/fusion.json,readonly',node_image,'--config','/fusion.json','--run-id','live-test']);created.append(fuser)
            observed=[until(lambda i=i:query(i),lambda s:len(hazards(s))==1,timeout=30) for i in (1,2)]
            trigger=hazards(observed[0])[0]
            before=physics('samples')
            def camera_hash(batch,sensor):return next(s['evidence']['sha256'] for s in batch['observations'] if s['sensor_id']==sensor)
            def gas(batch):return next(s for s in batch['observations'] if s['sensor_id']=='gas-1')
            trigger_path=folder/'trigger.json';trigger_path.write_text(json.dumps(trigger))
            def submit(task_id,target,inspect=False):
                spec={'id':task_id,'type':'wendy.nav.inspect.v1' if inspect else 'wendy.nav.go_to.v1','site':'synthetic-office','assignee':'sim-robot/navigation',
                    'domain':'simulation','class':'MOVE','mode':'autonomous','frame':{'id':'office-floor','revision':'office-frame-v1'},
                    'not_after_unix_ms':int(time.time()*1000)+120000,'area':{'min':[-2.1,.1,-.1],'max':[2.1,.3,.1]},
                    'required_sensors':['gas','local-obstacles'],'minimum_energy':.2,'fallback':'hold','target':[target,.2,0],
                    'speed_mps':.4,'tolerance_m':.03,'stop_speed_mps':.03}
                path=folder/(task_id+'.json');path.write_text(json.dumps(spec));args=['task','send','--spec','/spec.json','--json'];mounts=[(path,'/spec.json')]
                if inspect:args+=['--trigger-file','/trigger.json'];mounts.append((trigger_path,'/trigger.json'))
                return cli('world','operator-1.json',*args,network=local,mounts=mounts)
            submitted=submit('inspect-live',-.6,True)
            until(physics,lambda s:abs(s['velocity_mps'][0])>.1)
            command(['docker','network','disconnect',peer,nodes[0]])
            partition_start=time.monotonic()
            partition_pose=physics()
            assert disk_task('inspect-live')['status']['Phase']=='running'
            assert partition_pose['velocity_mps'][0]>.1 and partition_pose['position_m'][0]<-.95
            # No operator process runs during this wait. Read the durable assignee
            # file as a test oracle, not as a product client or network connection.
            completed=until(lambda:disk_task('inspect-live'),lambda r:r['status']['Phase']=='succeeded',timeout=25)
            assert completed.get('result') and completed['result']['task_id']=='inspect-live'
            result=completed['result'];assert result['trigger']==submitted['spec']['trigger']
            assert abs(result['position_m'][0]+.6)<=.031 and len(result['samples'])==4
            assert result['position_m'][0]-partition_pose['position_m'][0]>.3
            assert all(s['package_sha256']==before['state']['provenance']['package_sha256'] for s in result['samples'])
            during=physics('samples');assert during['boot_id']==before['boot_id']==result['boot_id']
            assert abs(during['state']['position_m'][0]+.6)<=.031
            assert camera_hash(during,'camera-b')!=camera_hash(before,'camera-b')
            assert gas(during)['position_m'][0]>gas(before)['position_m'][0]+1
            assert abs(gas(during)['gas']['value_ppm']-gas(before)['gas']['value_ppm'])>.1
            # The disconnected fusion replica must stop showing its old hazard.
            until(lambda:query(2),lambda s:not hazards(s),timeout=10)
            partition_seconds=time.monotonic()-partition_start
            command(['docker','network','connect','--alias','world-node-1',peer,nodes[0]])
            returned=until(lambda:task('inspect-live'),lambda r:r['status']['Phase']=='succeeded')
            assert returned['result']==result
            until(lambda:query(2),lambda s:len(hazards(s))==1,timeout=20)
            refs=[s['evidence'] for s in result['samples'] if 'evidence' in s]
            for ref in refs:
                verified=cli('world-evidence','viewer-1.json','--sha256',ref['sha256'],network=peer)
                assert verified['verified'] and verified['bytes']==ref['size']
            command(['docker','kill','--signal','KILL',nodes[0]])
            command(['docker','start',nodes[0]])
            recovered=until(lambda:task('inspect-live'),lambda r:r['status']['Phase']=='succeeded')
            assert recovered['result']==result
            for ref in refs:
                verified=cli('world-evidence','viewer-1.json','--sha256',ref['sha256'],network=peer)
                assert verified['verified'] and verified['bytes']==ref['size']
            after=physics();assert abs(after['velocity_mps'][0])<.001 and abs(after['position_m'][0]+.6)<=.031
            # Local prerequisites still stop this same live scene under a new task.
            submit('live-sensor-loss',1.2)
            until(physics,lambda s:abs(s['velocity_mps'][0])>.1)
            physics('fault',{'sensor':'gas','available':False})
            held=until(lambda:task('live-sensor-loss'),lambda r:r['status']['Phase']=='held')
            assert not held.get('result')
            physics('fault',{'sensor':'gas','available':True});time.sleep(.5)
            assert task('live-sensor-loss')['status']['Phase']=='held' and abs(physics()['velocity_mps'][0])<.001
            cli('world','operator-1.json','task','stop','--id','live-sensor-loss','--json')
            return {'mode':'isolated Linux containers on existing Docker VM','single_live_scene':True,'scene_boot_id':result['boot_id'],
                'initial_robot_x':before['state']['position_m'][0],'inspected_robot_x':result['position_m'][0],
                'robot_camera_changed':True,'robot_gas_position_and_value_changed':True,'protocol_viewers':2,
                'completed_while_peer_disconnected':True,'partition_seconds':round(partition_seconds,3),
                'partition_start_x':partition_pose['position_m'][0],
                'durable_result_survived_gateway_sigkill':True,'result_media_verified':len(refs),'local_sensor_loss_holds':True,
                'result_media_verified_after_gateway_sigkill':len(refs),
                'inspection_result':result,'limits':['synthetic one-axis robot and heuristic hazard fixture','direct task delivery and result retrieval; no distributed task status plane',
                'no SensorLink wire adapter, native clients or physical deployment','single functional run, not performance or safety certification']}
        except Exception:
            for name in created:
                p=subprocess.run(['docker','logs','--tail','12',name],capture_output=True)
                print(name,p.stdout.decode(),p.stderr.decode(),file=__import__('sys').stderr)
            raise
        finally:
            for name in reversed(created):subprocess.run(['docker','rm','-f',name],capture_output=True)
            for network in reversed(networks):subprocess.run(['docker','network','rm',network],capture_output=True)

if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('--node-image',default='wendy-world-node:local');parser.add_argument('--scene-image',default='wendy-world-live:local')
    args=parser.parse_args();print(json.dumps(run(args.node_image,args.scene_image),indent=2))
