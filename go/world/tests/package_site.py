"""Fetch a signed asset package, then run its MuJoCo model disconnected."""
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


def run(image,sim_image):
    suffix=uuid.uuid4().hex[:10];network='package-'+suffix
    nodes=[f'package-node-{i}-{suffix}' for i in range(1,4)]
    created=[];made_network=False;user=f'{os.getuid()}:{os.getgid()}'
    fixture=Path('examples/asset-office').resolve()
    root_bytes=(fixture/'world.json').read_bytes();package=json.loads(root_bytes)
    package_hash=hashlib.sha256(root_bytes).hexdigest()
    with tempfile.TemporaryDirectory(prefix='wendy-package-') as tmp:
        folder=Path(tmp);output=folder/'downloads';output.mkdir(mode=0o700)
        try:
            command(['go','run','./cmd/world-lab','--out',str(folder),'--runtime-dir','/state','--containers','--sensors','--run-id','package-transfer'])
            command(['docker','network','create','--internal',network]);made_network=True
            for i,name in enumerate(nodes,1):
                state=folder/f'state-{i}';state.mkdir(mode=0o700)
                command(['docker','run','-d','--name',name,'--network',network,'--network-alias',f'world-node-{i}','--user',user,
                    '--mount',f'type=bind,src={folder/f"node-{i}.json"},dst=/config.json,readonly','--mount',f'type=bind,src={state},dst=/state',image,'--config','/config.json']);created.append(name)
            def cli(index,group,verb,*args):
                return json.loads(command(['docker','run','--rm','--network',network,'--user',user,'--entrypoint','/world',
                    '--mount',f'type=bind,src={folder},dst=/lab,readonly','--mount',f'type=bind,src={fixture},dst=/fixture,readonly',
                    '--mount',f'type=bind,src={output},dst=/output',image,group,verb,'--credentials',f'/lab/site-admin-{index}.json',*args,'--json']))
            def until(fn,predicate,timeout=20):
                deadline=time.monotonic()+timeout;last=None
                while time.monotonic()<deadline:
                    try:
                        last=fn()
                        if predicate(last):return last
                    except RuntimeError as e:last=str(e)
                    time.sleep(.2)
                raise AssertionError(last)
            until(lambda:cli(3,'site','list','--trust','/lab/site-trust.json'),lambda r:r['verified'])
            until(lambda:cli(1,'site','list','--trust','/lab/site-trust.json'),lambda r:r['verified'])
            site=json.loads(Path('examples/geography/office.json').read_text());site['package_sha256']=package_hash
            assert site['site']==package['site_id']
            (folder/'site.json').write_text(json.dumps(site))
            published=cli(1,'package','publish','--site','/lab/site.json','--package','/fixture/world.json','--run-id','package-transfer')
            digest=published['publication']['sha256'];assert published['package_sha256']==package_hash
            until(lambda:cli(3,'site','fetch','--trust','/lab/site-trust.json','--sha256',digest),lambda r:r['verified'])
            # Metadata repair does not silently copy bulk blobs to every node.
            try:cli(3,'package','fetch','--trust','/lab/site-trust.json','--sha256',digest,'--out','/output/missing')
            except RuntimeError:pass
            else:raise AssertionError('reader claimed unavailable blobs')
            assert not (output/'missing').exists()
            installed=cli(3,'package','fetch','--trust','/lab/site-trust.json','--sha256',digest,'--blob-credentials','/lab/site-admin-1.json','--out','/output/installed')
            assert installed['verified'] and installed['installed'] and installed['package_sha256']==package_hash
            files=['world.json',package['mujoco']['path']]+[a['path'] for a in package.get('assets',[])]
            assert len(files)>2
            for name in files:assert (output/'installed'/name).read_bytes()==(fixture/name).read_bytes()
            try:cli(3,'package','fetch','--trust','/lab/site-trust.json','--sha256',digest,'--blob-credentials','/lab/site-admin-1.json','--out','/output/installed')
            except RuntimeError:pass
            else:raise AssertionError('existing installation overwritten')
            for name in files:assert (output/'installed'/name).read_bytes()==(fixture/name).read_bytes()
            # Remove origin access before explicitly starting the installed model.
            for name in nodes:command(['docker','network','disconnect',network,name])
            observations=[json.loads(line) for line in command(['docker','run','--rm','--network','none','--user',user,
                '--mount',f'type=bind,src={output/"installed"},dst=/installed,readonly',sim_image,
                '/installed/world.json','--steps','20','--every','5','--run-id','fetched-package']).splitlines()]
            assert len(observations)==8 and all(o['provenance']['package_sha256']==package_hash for o in observations)
            assert {o['kind'] for o in observations}=={'pose','gas'}
            return {'mode':'isolated Linux nodes and offline MuJoCo on existing Docker VM','files_verified':len(files),
                'site_publication_sha256':digest,'package_sha256':package_hash,'metadata_read_from_replica':True,'blobs_read_from_explicit_origin':True,
                'missing_blob_fetch_left_no_installation':True,'existing_installation_preserved':True,'offline_observations':len(observations),
                'node_image':command(['docker','image','inspect',image,'--format','{{.Id}}']).decode().strip(),
                'sim_image':command(['docker','image','inspect',sim_image,'--format','{{.Id}}']).decode().strip(),
                'limits':['synthetic OBJ/include fixture, no real office data or physical deployment','explicit blob endpoint, no automatic cache discovery',
                          'no automatic model execution or frame activation on fetch','no persistent partial-download ledger or production key lifecycle']}
        except Exception:
            for name in created:
                p=subprocess.run(['docker','logs','--tail','8',name],capture_output=True)
                print(name,p.stdout.decode(),p.stderr.decode(),file=__import__('sys').stderr)
            raise
        finally:
            for name in reversed(created):subprocess.run(['docker','rm','-f',name],capture_output=True)
            if made_network:subprocess.run(['docker','network','rm',network],capture_output=True)


if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('--image',default='wendy-world-node:local');p.add_argument('--sim-image',default='wendy-world-sim:local');a=p.parse_args()
    print(json.dumps(run(a.image,a.sim_image),indent=2))
