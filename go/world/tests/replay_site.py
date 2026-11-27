"""Record networked sensor history, remove its site, then evaluate a new replay."""
import argparse
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time
import uuid
from virtual_site import command


def run(image, sensor_image):
    suffix=uuid.uuid4().hex[:10]
    networks=[];created=[];user=f'{os.getuid()}:{os.getgid()}'
    sensors=['camera-a','camera-b','gas-1','audio-1']
    with tempfile.TemporaryDirectory(prefix='wendy-replay-') as tmp:
        root=Path(tmp);archive_dir=root/'recordings';archive_dir.mkdir(mode=0o700)
        try:
            def until(fn,predicate,timeout=20):
                deadline=time.monotonic()+timeout;last=None
                while time.monotonic()<deadline:
                    try:
                        last=fn()
                        if predicate(last):return last
                    except RuntimeError as error:last=str(error)
                    time.sleep(.1)
                raise AssertionError(last)
            def start_site(label,replay=False):
                folder=root/label
                args=['go','run','./cmd/world-lab','--out',str(folder),'--runtime-dir','/state','--containers','--sensors','--run-id',label]
                if replay:args+=['--replay']
                command(args)
                network=f'{label}-{suffix}';command(['docker','network','create','--internal',network]);networks.append(network)
                nodes=[]
                for i in range(1,4):
                    name=f'{label}-{i}-{suffix}';state=folder/f'state-{i}';state.mkdir(mode=0o700)
                    command(['docker','run','-d','--name',name,'--network',network,'--network-alias',f'world-node-{i}','--user',user,
                        '--mount',f'type=bind,src={folder/f"node-{i}.json"},dst=/config.json,readonly',
                        '--mount',f'type=bind,src={state},dst=/state',image,'--config','/config.json']);created.append(name);nodes.append(name)
                return folder,network,nodes
            def tool(folder,network,binary,*args,mounts=()):
                return command(['docker','run','--rm','--network',network,'--user',user,'--entrypoint','/'+binary,
                    '--mount',f'type=bind,src={folder},dst=/lab,readonly',*mounts,image,*args])
            def query(folder,network,index):
                return json.loads(tool(folder,network,'world-query','--credentials',f'/lab/viewer-{index}.json'))

            origin,network,nodes=start_site('recorded-run')
            for i in (1,2):until(lambda i=i:query(origin,network,i),lambda x:not x['components'])
            until(lambda:json.loads(tool(origin,network,'world-query','--credentials','/lab/client-3.json')),lambda x:not x['components'])
            command(['docker','run','--rm','--network','none','--user',user,'--mount',f'type=bind,src={root},dst=/capture',sensor_image,
                'examples/sensor-office/world.json','--out','/capture/rendered','--frames','3','--interval-ms','1000','--run-id','recorded-run','--alarm','true'])
            capture=[json.loads(s) for s in (root/'rendered'/'samples.jsonl').read_text().splitlines()]
            config={'reader':json.loads((origin/'client-3.json').read_text()),
                'evidence_sources':[json.loads((origin/f'viewer-{i}.json').read_text()) for i in (1,2)]}
            (origin/'record.json').write_text(json.dumps(config));(origin/'record.json').chmod(0o600)
            recorder=f'recorder-{suffix}'
            command(['docker','run','-d','--name',recorder,'--network',network,'--user',user,'--entrypoint','/world-record',
                '--mount',f'type=bind,src={origin},dst=/lab,readonly','--mount',f'type=bind,src={archive_dir},dst=/output',image,
                '--config','/lab/record.json','--out','/output/archive','--run-id','recorded-run','--duration','7s','--interval','50ms']);created.append(recorder)
            anchor=int(time.time()*1000)+1500
            def publish(sensor):
                return json.loads(tool(origin,network,'world-sense','--credentials',f'/lab/sensor-{sensor}.json','--input','/capture',
                    '--sensor',sensor,'--run-id','recorded-run','--anchor-unix-ms',str(anchor),'--ttl','5s',
                    mounts=['--mount',f'type=bind,src={root/"rendered"},dst=/capture,readonly']))
            with ThreadPoolExecutor(max_workers=4) as pool:published=list(pool.map(publish,sensors))
            assert sum(r['published'] for r in published)==12
            assert command(['docker','wait',recorder]).strip()==b'0'
            archive=archive_dir/'archive';raw=(archive/'recording.json').read_bytes();manifest=json.loads(raw)
            digest=hashlib.sha256(raw).hexdigest();records=manifest['records']
            assert len(records)==12, f'polling missed fixture records: {len(records)}'
            assert {c['payload']['id'] for c in records}=={s['id'] for s in capture}
            for c in records:
                assert c['payload']==next(s for s in capture if s['id']==c['payload']['id'])
            for path in (archive/'blobs').iterdir():assert hashlib.sha256(path.read_bytes()).hexdigest()==path.name
            # The replay must work without the original nodes or their media stores.
            for name in [recorder,*nodes]:command(['docker','rm','-f',name]);created.remove(name)
            command(['docker','network','rm',network]);networks.remove(network)
            latest_expiry=max(datetime.fromisoformat(c['timing']['sample_time'].replace('Z','+00:00')).timestamp()+c['ttl_ns']/1e9 for c in records)
            if latest_expiry>time.time():time.sleep(latest_expiry-time.time()+.1)
            original_expired=all(time.time()>=datetime.fromisoformat(c['timing']['sample_time'].replace('Z','+00:00')).timestamp()+c['ttl_ns']/1e9 for c in records)
            assert original_expired
            shutil.rmtree(origin)
            playback,network,nodes=start_site('replay-run',True)
            for i in (1,2):until(lambda i=i:query(playback,network,i),lambda x:not x['components'])
            mounts=['--mount',f'type=bind,src={archive},dst=/archive,readonly']
            # Digest pinning rejects a substituted archive before publication.
            try:tool(playback,network,'world-replay','--credentials','/lab/sensor-gas-1.json','--input','/archive','--sha256','0'*64,
                '--run-id','replay-run','--anchor-unix-ms',str(int(time.time()*1000)+2000),mounts=mounts)
            except RuntimeError as error:assert 'digest' in str(error)
            else:raise AssertionError('wrong recording digest accepted')
            assert not query(playback,network,1)['components']
            fusion=f'replay-fusion-{suffix}'
            command(['docker','run','-d','--name',fusion,'--network',network,'--user',user,'--entrypoint','/world-fuse',
                '--mount',f'type=bind,src={playback/"fusion.json"},dst=/fusion.json,readonly',image,'--config','/fusion.json',
                '--run-id','replay-run','--replay','--recording-sha256',digest]);created.append(fusion)
            anchor=int(time.time()*1000)+2500
            def replay_sensor(sensor):
                return json.loads(tool(playback,network,'world-replay','--credentials',f'/lab/sensor-{sensor}.json','--input','/archive',
                    '--sha256',digest,'--run-id','replay-run','--anchor-unix-ms',str(anchor),'--ttl','5s',mounts=mounts))
            with ThreadPoolExecutor(max_workers=4) as pool:results=list(pool.map(replay_sensor,sensors))
            assert sum(r['published'] for r in results)==12
            def hazards(snapshot):return [c for c in snapshot['components'] if c['key']['type']=='wendy.hazard.replay.v1']
            snapshots=[until(lambda i=i:query(playback,network,i),lambda s:bool(hazards(s))) for i in (1,2)]
            hazard=hazards(snapshots[0])[0]
            assert not any(c['key']['type']=='wendy.hazard.v1' for s in snapshots for c in s['components'])
            assert all(p['origin']=='replayed' and p['simulation']['run_id']=='recorded-run' for p in hazard['provenance']['inputs'])
            for item in hazard['payload']['inputs']:
                replay=item['replay'];original=replay['original']
                assert original in records and replay['recording_sha256']==digest and replay['run_id']=='replay-run'
                assert original['timing']['sample_time']!=item['sample_time']
            media=hazard['payload']['evidence'][0]
            # Replay bytes remain available from the fresh replay site.
            verified=json.loads(tool(playback,network,'world-evidence','--credentials','/lab/viewer-1.json','--sha256',media['sha256']))
            assert verified['verified']
            for i in (1,2):until(lambda i=i:query(playback,network,i),lambda s:not hazards(s),timeout=15)
            return {'mode':'recorded and replay sites in isolated Linux containers on existing Docker VM','recording_sha256':digest,
                'captured_records':len(records),'replayed_records':sum(r['published'] for r in results),'protocol_viewers':2,
                'original_site_removed':True,'original_observations_expired_before_replay':original_expired,
                'original_samples_and_media_preserved':True,'wrong_recording_digest_rejected':True,
                'hazard_type':hazard['key']['type'],'replayed_ancestry_preserved':True,'expired_replay_hazard_withdrawn':True,
                'node_image':command(['docker','image','inspect',image,'--format','{{.Id}}']).decode().strip(),
                'sensor_image':command(['docker','image','inspect',sensor_image,'--format','{{.Id}}']).decode().strip(),
                'limits':['synthetic recorded cameras, analytic gas and tone; no real office capture','polling recorder can miss intermediate values',
                    'archive integrity is pinned by digest; no independent historical signature proof','one-timescale replay with original viewpoints; no counterfactual rendering or hardware authorization']}
        except Exception:
            for name in created:
                log=subprocess.run(['docker','logs','--tail','12',name],capture_output=True)
                print(name,log.stdout.decode(),log.stderr.decode(),file=__import__('sys').stderr)
            raise
        finally:
            for name in reversed(created):subprocess.run(['docker','rm','-f',name],capture_output=True)
            for network in networks:subprocess.run(['docker','network','rm',network],capture_output=True)


if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('--image',default='wendy-world-node:local');parser.add_argument('--sensor-image',default='wendy-world-sensors:local')
    args=parser.parse_args();print(json.dumps(run(args.image,args.sensor_image),indent=2))
