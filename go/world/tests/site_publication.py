"""Signed site publication over three durable, isolated Linux replicas."""
import argparse
import base64
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import uuid
from virtual_site import command


def run(image, site_path='examples/geography/office.json', compatibility_site=None):
    suffix=uuid.uuid4().hex[:10]
    peer,local='site-peer-'+suffix,'site-local-'+suffix
    nodes=[f'site-node-{i}-{suffix}' for i in range(1,4)]
    created=[];networks=[];user=f'{os.getuid()}:{os.getgid()}'
    with tempfile.TemporaryDirectory(prefix='wendy-site-publication-') as tmp:
        folder=Path(tmp)
        try:
            command(['go','run','./cmd/world-lab','--out',str(folder),'--runtime-dir','/state','--containers','--run-id','site-publication'])
            for network in (peer,local):command(['docker','network','create','--internal',network]);networks.append(network)
            for i,name in enumerate(nodes,1):
                state=folder/f'state-{i}';state.mkdir(mode=0o700)
                command(['docker','run','-d','--name',name,'--network',local if i==1 else peer,'--network-alias',f'world-node-{i}','--user',user,
                    '--mount',f'type=bind,src={folder/f"node-{i}.json"},dst=/config.json,readonly','--mount',f'type=bind,src={state},dst=/state',image,'--config','/config.json']);created.append(name)
            command(['docker','network','connect','--alias','world-node-1',peer,nodes[0]])
            def cli(index,verb,*args,network=peer,trust='site-trust.json'):
                extra=[] if verb=='publish' else ['--trust','/lab/'+trust]
                return json.loads(command(['docker','run','--rm','--network',network,'--user',user,'--entrypoint','/world','--mount',f'type=bind,src={folder},dst=/lab,readonly',image,
                    'site',verb,'--credentials',f'/lab/site-admin-{index}.json',*extra,*args,'--json']))
            def until(fn,predicate,timeout=15):
                deadline=time.monotonic()+timeout;last=None
                while time.monotonic()<deadline:
                    try:
                        last=fn()
                        if predicate(last):return last
                    except RuntimeError as error:last=str(error)
                    time.sleep(.2)
                raise AssertionError(last)
            until(lambda:cli(3,'list'),lambda r:r['verified'] and r['publications']==[])
            compatibility=None
            if compatibility_site:
                (folder/'compatibility.json').write_bytes(Path(compatibility_site).read_bytes())
                compatibility=cli(1,'publish','--site','/lab/compatibility.json','--run-id','site-publication',network=local)['publication']
                until(lambda:cli(3,'fetch','--sha256',compatibility['sha256']),lambda r:r['verified'] and r['publication']==compatibility)
            previous=1 if compatibility else 0
            document=json.loads(Path(site_path).read_text())
            (folder/'site.json').write_text(json.dumps(document))
            first=cli(1,'publish','--site','/lab/site.json','--run-id','site-publication',network=local)['publication']
            digest1=first['sha256']
            remote=until(lambda:cli(3,'fetch','--sha256',digest1),lambda r:r['verified'])
            assert remote['publication']==first
            # Disconnect the origin, publish a distinct placement with the same
            # human revision label, and prove no remote node invents a latest head.
            command(['docker','network','disconnect',peer,nodes[0]])
            document['anchor']['longitude_degrees']+=.001
            (folder/'site.json').write_text(json.dumps(document))
            second=cli(1,'publish','--site','/lab/site.json','--run-id','site-publication',network=local)['publication']
            digest2=second['sha256'];assert digest2!=digest1 and first['revision']==second['revision']
            isolated=cli(3,'list');assert len(isolated['publications'])==1+previous
            command(['docker','network','connect','--alias','world-node-1',peer,nodes[0]])
            until(lambda:cli(3,'list'),lambda r:len(r['publications'])==2+previous)
            assert cli(3,'fetch','--sha256',digest1)['publication']==first
            if compatibility:
                assert cli(3,'fetch','--sha256',compatibility['sha256'])['publication']==compatibility
            assert cli(3,'fetch','--sha256',digest2)['publication']==second
            command(['docker','kill','--signal','KILL',nodes[2]])
            # Keep the restarted reader away from peers, proving local durability.
            command(['docker','network','disconnect',peer,nodes[0]])
            command(['docker','network','disconnect',peer,nodes[1]])
            command(['docker','start',nodes[2]])
            restored=until(lambda:cli(3,'fetch','--sha256',digest2),lambda r:r['verified'])
            assert restored['publication']==second
            assert cli(3,'fetch','--sha256',digest1)['publication']==first
            if compatibility:
                assert cli(3,'fetch','--sha256',compatibility['sha256'])['publication']==compatibility
            # A wrong pinned root cannot turn an HTTP reply into trusted placement.
            trust=json.loads((folder/'site-trust.json').read_text());trust['root_public_key']=base64.b64encode(bytes(32)).decode()
            (folder/'wrong-trust.json').write_text(json.dumps(trust))
            try:cli(3,'fetch','--sha256',digest2,trust='wrong-trust.json')
            except RuntimeError:pass
            else:raise AssertionError('wrong trust root accepted')
            return {'mode':'isolated Linux containers on existing Docker VM','nodes':3,'site_schema':document['schema_version'],'signed_operator_publication':True,
                'origin_partition_holds_remote_state':True,'repair_retains_both_exact_revisions':True,'same_label_distinct_digest':True,
                'reader_sigkill_recovered_without_peers':True,'wrong_pinned_root_rejected':True,
                'publication_digests':[digest1,digest2],'image':command(['docker','image','inspect',image,'--format','{{.Id}}']).decode().strip(),
                'compatible_publication':compatibility,
                'compatible_publication_recovered_without_peers':True if compatibility else None,
                'limits':['simulation-only HTTP transport and lab authority','fresh pinned revocation bundle, no persistent client rollback ledger',
                          'metadata distribution only; no model blob transfer or automatic frame activation','illustrative anchor, no surveyed site or hardware deployment']}
        except Exception:
            for name in created:
                p=subprocess.run(['docker','logs','--tail','8',name],capture_output=True)
                print(name,p.stdout.decode(),p.stderr.decode(),file=__import__('sys').stderr)
            raise
        finally:
            for name in reversed(created):subprocess.run(['docker','rm','-f',name],capture_output=True)
            for network in reversed(networks):subprocess.run(['docker','network','rm',network],capture_output=True)


if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('--image',default='wendy-world-node:local');parser.add_argument('--site',default='examples/geography/office.json');parser.add_argument('--compatibility-site');args=parser.parse_args()
    print(json.dumps(run(args.image,args.site,args.compatibility_site),indent=2))
