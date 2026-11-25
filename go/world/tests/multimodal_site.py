"""Run rendered sensors, remote media retrieval and hazard fusion on isolated nodes."""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import uuid
from concurrent.futures import ThreadPoolExecutor
from virtual_site import command


def run(node_image, sensor_image):
    suffix=uuid.uuid4().hex[:10]
    network=f"multimodal-{suffix}"
    nodes=[f"sensor-node-{i}-{suffix}" for i in range(1,4)]
    fusion_name=f"sensor-fusion-{suffix}"
    created=[];network_created=False
    user=f"{os.getuid()}:{os.getgid()}"
    with tempfile.TemporaryDirectory(prefix="wendy-multimodal-") as tmp:
        folder=Path(tmp)
        try:
            command(["go","run","./cmd/world-lab","--out",str(folder),"--runtime-dir","/state","--containers","--sensors","--run-id","multimodal-test"])
            command(["docker","run","--rm","--network","none","--user",user,"--mount",f"type=bind,src={folder},dst=/capture",sensor_image,
                     "examples/sensor-office/world.json","--out","/capture/warning","--frames","3","--interval-ms","100","--run-id","multimodal-test","--seed","7","--alarm","true"])
            samples=[json.loads(line) for line in (folder/"warning"/"samples.jsonl").read_text().splitlines()]
            assert len(samples)==12
            camera_hashes={s['sensor_id']:s['evidence']['sha256'] for s in samples if s['kind']=='camera'}
            assert camera_hashes['camera-a']!=camera_hashes['camera-b']
            command(["docker","network","create","--internal",network]);network_created=True
            for i,name in enumerate(nodes,1):
                state=folder/f"state-{i}";state.mkdir(mode=0o700)
                command(["docker","run","-d","--name",name,"--network",network,"--network-alias",f"world-node-{i}","--user",user,
                         "--mount",f"type=bind,src={folder/f'node-{i}.json'},dst=/config.json,readonly",
                         "--mount",f"type=bind,src={state},dst=/state",node_image,"--config","/config.json"]);created.append(name)

            def tool(binary,credential,*args):
                return json.loads(command(["docker","run","--rm","--network",network,"--user",user,"--entrypoint","/"+binary,
                    "--mount",f"type=bind,src={folder/credential},dst=/client.json,readonly",node_image,"--credentials","/client.json",*args]))
            def query(i):return tool("world-query",f"viewer-{i}.json")
            def until(fn,predicate,timeout=20):
                end=time.monotonic()+timeout;last=None
                while time.monotonic()<end:
                    try:
                        last=fn()
                        if predicate(last):return last
                    except RuntimeError as e:last=str(e)
                    time.sleep(.1)
                raise AssertionError(f"condition not met: {last}")
            def hazards(snapshot):return [c for c in snapshot['components'] if c['key']['type']=='wendy.hazard.v1']
            for i in (1,2):until(lambda:query(i),lambda s:not s['components'])
            command(["docker","run","-d","--name",fusion_name,"--network",network,"--user",user,"--entrypoint","/world-fuse",
                     "--mount",f"type=bind,src={folder/'fusion.json'},dst=/fusion.json,readonly",node_image,"--config","/fusion.json","--run-id","multimodal-test"]);created.append(fusion_name)
            anchor=int(time.time()*1000)+2500
            def publish(sensor):
                return json.loads(command(["docker","run","--rm","--network",network,"--user",user,"--entrypoint","/world-sense",
                     "--mount",f"type=bind,src={folder/f'sensor-{sensor}.json'},dst=/client.json,readonly",
                     "--mount",f"type=bind,src={folder/'warning'},dst=/capture,readonly",node_image,"--credentials","/client.json",
                     "--input","/capture","--sensor",sensor,"--run-id","multimodal-test","--anchor-unix-ms",str(anchor),"--ttl","15s"]))
            with ThreadPoolExecutor(max_workers=3) as workers:publish_results=list(workers.map(publish,['camera-a','camera-b','gas-1']))
            for i in (1,2):
                incomplete=until(lambda i=i:query(i),lambda s:len(s['components'])>=3)
                assert not hazards(incomplete), 'hazard accepted without required audio'
            publish_results.append(publish('audio-1'))
            assert all(r['published']==3 for r in publish_results),publish_results
            seen=[until(lambda i=i:query(i),lambda s:len(hazards(s))==1) for i in (1,2)]
            hazard=hazards(seen[0])[0]
            assert hazard['provenance']['origin']=='inferred'
            assert len(hazard['provenance']['inputs'])==4
            assert all(i['origin']=='simulated' for i in hazard['provenance']['inputs'])
            # Media stays at capture nodes. Metadata replication does not copy bytes.
            assert not list((folder/'state-3').rglob('*.blob'))
            digest=camera_hashes['camera-b']
            first=tool('world-evidence','viewer-2.json','--sha256',digest,'--offset','0','--length','1024')
            data=base64.b64decode(first['data']);assert len(data)==1024
            command(['docker','kill','--signal','KILL',nodes[1]])
            command(['docker','start',nodes[1]])
            offset=len(data)
            while offset<first['size']:
                chunk=until(lambda:tool('world-evidence','viewer-2.json','--sha256',digest,'--offset',str(offset),'--length','49152'),lambda c:c['offset']==offset)
                assert chunk['sha256']==digest and chunk['size']==first['size']
                block=base64.b64decode(chunk['data']);assert block
                data+=block;offset+=len(block)
            assert hashlib.sha256(data).hexdigest()==digest
            assert data==(folder/'warning'/'blobs'/digest).read_bytes()
            try:tool('world-evidence','client-1.json','--sha256',camera_hashes['camera-a'])
            except RuntimeError as error:assert '401' in str(error),error
            else:raise AssertionError('ordinary world read fetched sensitive evidence')
            # With all publishers stopped, the derived hazard must expire or be
            # withdrawn. Lack of a live hazard does not assert the floor is safe.
            for i in (1,2):until(lambda i=i:query(i),lambda s:not hazards(s) and not s['components'],timeout=25)
            retained=tool('world-evidence','viewer-2.json','--sha256',digest)
            assert retained['verified'] and retained['bytes']==len(data)
            # Actual received payload supports both clients and keeps four inputs.
            return {'mode':'isolated Linux containers on existing Docker VM','rendered_camera_sources':2,
                    'published_observations':sum(x['published'] for x in publish_results),'media_uploaded':sum(x['evidence_uploaded'] for x in publish_results),
                    'protocol_viewers':2,'hazard_inputs':len(hazard['provenance']['inputs']),
                    'hazard_payload':hazard['payload'],'evidence_resume_after_capture_restart':True,
                    'evidence_digest_verified':digest,'unauthorized_evidence_read_rejected':True,
                    'missing_audio_prevents_hazard':True,'expired_inputs_withdraw_hazard':True,'evidence_outlives_observations':True,
                    'limits':['synthetic red-indicator, analytic gas and generated tone fixtures; no validated leak detector',
                              'not SensorLink wire emulation or physical camera/audio hardware','no native mobile clients','one run; no performance acceptance claim']}
        except Exception:
            for name in created:
                p=subprocess.run(['docker','logs','--tail','20',name],capture_output=True)
                print(name,p.stdout.decode(),p.stderr.decode(),file=__import__('sys').stderr)
            raise
        finally:
            for name in reversed(created):subprocess.run(['docker','rm','-f',name],capture_output=True)
            if network_created:subprocess.run(['docker','network','rm',network],capture_output=True)

if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--node-image',default='wendy-world-node:local');parser.add_argument('--sensor-image',default='wendy-world-sensors:local')
    args=parser.parse_args();print(json.dumps(run(args.node_image,args.sensor_image),indent=2))
