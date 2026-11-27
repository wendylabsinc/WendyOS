"""Exercise real SensorLink framing between virtual cameras and World gateways."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import uuid
from virtual_site import command


def run(image,sensor_image):
    suffix=uuid.uuid4().hex[:10];network='sensorlink-'+suffix
    created=[];made_network=False;user=f'{os.getuid()}:{os.getgid()}'
    with tempfile.TemporaryDirectory(prefix='wendy-sensorlink-') as tmp:
        folder=Path(tmp);capture=folder/'capture';out=folder/'recordings';out.mkdir(mode=0o700)
        try:
            command(['go','run','./cmd/world-lab','--out',str(folder),'--runtime-dir','/state','--containers','--sensors','--run-id','sensorlink-test'])
            command(['docker','run','--rm','--network','none','--user',user,'--mount',f'type=bind,src={folder},dst=/capture',sensor_image,
                'examples/sensor-office/world.json','--out','/capture/capture','--frames','3','--interval-ms','1000','--run-id','sensorlink-test','--alarm','true'])
            samples=[json.loads(line) for line in (capture/'samples.jsonl').read_text().splitlines()]
            command(['docker','network','create','--internal',network]);made_network=True
            for i in range(1,4):
                name=f'sensorlink-node-{i}-{suffix}';state=folder/f'state-{i}';state.mkdir(mode=0o700)
                command(['docker','run','-d','--name',name,'--network',network,'--network-alias',f'world-node-{i}','--user',user,
                    '--mount',f'type=bind,src={folder/f"node-{i}.json"},dst=/config.json,readonly',
                    '--mount',f'type=bind,src={state},dst=/state',image,'--config','/config.json']);created.append(name)
            def until(fn,predicate,timeout=25):
                end=time.monotonic()+timeout;last=None
                while time.monotonic()<end:
                    try:
                        last=fn()
                        if predicate(last):return last
                    except RuntimeError as error:last=str(error)
                    time.sleep(.1)
                raise AssertionError(last)
            def tool(binary,*args):
                return command(['docker','run','--rm','--network',network,'--user',user,'--entrypoint','/'+binary,
                    '--mount',f'type=bind,src={folder},dst=/lab,readonly',image,*args])
            def query(i):return json.loads(tool('world-query','--credentials',f'/lab/client-{i}.json'))
            for i in range(1,4):until(lambda i=i:query(i),lambda s:not s['components'])
            def start(name,binary,args,mounts=(),alias=None):
                options=['docker','run','-d','--name',name,'--network',network,'--user',user,'--entrypoint','/'+binary]
                if alias:options+=['--network-alias',alias]
                command([*options,'--mount',f'type=bind,src={folder},dst=/lab,readonly',*mounts,image,*args]);created.append(name)
            def report(name):
                assert command(['docker','wait',name]).strip()==b'0',name
                # stderr startup diagnostics are excluded from this JSON result.
                result=subprocess.run(['docker','logs',name],capture_output=True,check=True)
                return json.loads(result.stdout)
            recorder=f'sensorlink-recorder-{suffix}'
            record_cfg={'reader':json.loads((folder/'client-3.json').read_text()),'evidence_sources':[json.loads((folder/f'viewer-{i}.json').read_text()) for i in (1,2)]}
            (folder/'record.json').write_text(json.dumps(record_cfg));(folder/'record.json').chmod(0o600)
            start(recorder,'world-record',['--config','/lab/record.json','--out','/recordings/archive','--run-id','sensorlink-test','--duration','12s','--interval','50ms'],
                ['--mount',f'type=bind,src={out},dst=/recordings'])
            fusion=f'sensorlink-fusion-{suffix}'
            start(fusion,'world-fuse',['--config','/lab/fusion.json','--run-id','sensorlink-test'])
            anchor=int(time.time()*1000)+4000
            sources={};bridges={}
            for sensor in ['camera-a','camera-b']:
                alias='sensorlink-'+sensor;name=alias+'-'+suffix;sources[sensor]=name
                args=['--input','/lab/capture','--sensor',sensor,'--run-id','sensorlink-test','--anchor-unix-ms',str(anchor),
                    '--addr',':50060','--allow-network-simulation','--channel','1','--chunk-bytes','512','--jpeg-quality','90']
                if sensor=='camera-a':args+=['--drop-chunk','1:1']
                start(name,'world-sensorlink-sim',args,alias=alias)
                until(lambda name=name:subprocess.run(['docker','logs',name],capture_output=True).stderr.decode(),lambda log:'sensorlink simulator listening on' in log)
                template=next(s for s in samples if s['sensor_id']==sensor)
                cfg={'publisher':json.loads((folder/f'sensor-{sensor}.json').read_text()),'endpoint':alias+':50060','channel_id':1,
                    'sample_template':template,'anchor_unix_us':anchor*1000,'timestamp_clock':'unix-us','clock_error_us':1000,'ttl_ms':15000,'allow_insecure_simulation':True}
                path=folder/f'bridge-{sensor}.json';path.write_text(json.dumps(cfg));path.chmod(0o600)
                bridge=f'sensorlink-bridge-{sensor}-{suffix}';bridges[sensor]=bridge
                start(bridge,'world-sensorlink',['--config',f'/lab/bridge-{sensor}.json','--max-frames','2' if sensor=='camera-a' else '3','--duration','20s'])
            def publish(sensor):return json.loads(tool('world-sense','--credentials',f'/lab/sensor-{sensor}.json','--input','/lab/capture','--sensor',sensor,
                '--run-id','sensorlink-test','--anchor-unix-ms',str(anchor),'--ttl','15s'))
            with ThreadPoolExecutor(max_workers=2) as pool:others=list(pool.map(publish,['gas-1','audio-1']))
            source_reports={sensor:report(name) for sensor,name in sources.items()}
            bridge_reports={sensor:report(name) for sensor,name in bridges.items()}
            assert bridge_reports['camera-a']['published']==2 and bridge_reports['camera-a']['dropped']>0
            assert bridge_reports['camera-b']['published']==3
            assert sum(r['published'] for r in others)==6
            def hazards(snapshot):return [c for c in snapshot['components'] if c['key']['type']=='wendy.hazard.v1']
            def final_hazard(snapshot):
                found=hazards(snapshot)
                if not found:return False
                inputs=found[0]['payload']['inputs']
                return len(inputs)==4 and all(
                    x['sample'].get('sensorlink',{}).get('frame_sequence')==2 if x['kind']=='camera'
                    else x['sample']['sequence']==3 for x in inputs)
            seen=[until(lambda i=i:query(i),final_hazard) for i in (1,2)]
            hazard=hazards(seen[0])[0]
            for item in hazard['payload']['inputs']:
                if item['kind']!='camera':continue
                sample=item['sample'];wire=sample['sensorlink'];sensor=sample['sensor_id']
                sent=next(f for f in source_reports[sensor]['frames'] if f['frame_seq']==2)
                assert wire['frame_sequence']==2 and wire['timestamp_us']==sent['ts_us']
                assert sample['evidence']['content_type']=='image/jpeg' and sample['evidence']['sha256']==sent['jpeg_sha256']==wire['frame_sha256']
                assert sample['sample_ns']==2250000000
                index=1 if sensor=='camera-a' else 2
                media=json.loads(tool('world-evidence','--credentials',f'/lab/viewer-{index}.json','--sha256',sent['jpeg_sha256']))
                assert media['verified'] and media['bytes']==sent['bytes']
            recorded=report(recorder)
            manifest=json.loads((out/'archive'/'recording.json').read_text())
            camera_records=[c for c in manifest['records'] if c['payload']['kind']=='camera']
            assert len(manifest['records'])==11 and len(camera_records)==5
            assert {c['payload']['sensorlink']['frame_sequence'] for c in camera_records if c['payload']['sensor_id']=='camera-a'}=={0,2}
            for c in camera_records:
                ref=c['payload']['evidence'];blob=(out/'archive'/'blobs'/ref['sha256']).read_bytes()
                assert hashlib.sha256(blob).hexdigest()==ref['sha256']
            for i in (1,2):until(lambda i=i:query(i),lambda s:not hazards(s),timeout=25)
            return {'mode':'SensorLink source and bridge containers on existing Docker Linux VM','protocol':'wendylite.v1 SensorLink protobuf over framed TCP',
                'camera_channels':2,'jpeg_observations':5,'gas_audio_observations':6,'protocol_viewers':2,'source_chunk_bytes':512,
                'missing_chunk_frame_not_published':True,'camera_a_wire_sequences_recorded':[0,2],
                'source_jpeg_digests_match_world_evidence':True,'wire_timestamps_preserved':True,'recorded_observations':len(manifest['records']),
                'recording_sha256':recorded['recording_sha256'],'expired_hazard_withdrawn':True,
                'node_image':command(['docker','image','inspect',image,'--format','{{.Id}}']).decode().strip(),
                'sensor_image':command(['docker','image','inspect',sensor_image,'--format','{{.Id}}']).decode().strip(),
                'limits':['synthetic MuJoCo cameras; no physical camera or office data','raw TCP simulation transport; no WendyCom/gRPC pairing or production identity',
                    'configured Unix-microsecond clock profile; no automatic board clock synchronization','MJPEG camera path only; gas/audio use existing simulated publishers','one functional run; no radio or performance acceptance claim']}
        except Exception:
            for name in created:
                log=subprocess.run(['docker','logs','--tail','8',name],capture_output=True)
                # Fusion records include full raw lineage; keep failure logs readable.
                bounded=lambda data:'\n'.join(line[:1200] for line in data.decode().splitlines())
                print(name,bounded(log.stdout),bounded(log.stderr),file=__import__('sys').stderr)
            raise
        finally:
            for name in reversed(created):subprocess.run(['docker','rm','-f',name],capture_output=True)
            if made_network:subprocess.run(['docker','network','rm',network],capture_output=True)


if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('--image',default='wendy-world-node:local');parser.add_argument('--sensor-image',default='wendy-world-sensors:local')
    args=parser.parse_args();print(json.dumps(run(args.image,args.sensor_image),indent=2))
