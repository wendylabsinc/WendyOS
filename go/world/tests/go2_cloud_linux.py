#!/usr/bin/env python3
"""Bounded host/Linux comparison of offline cloud-grid inputs.

One isolated existing Docker image processes exactly fourteen recorded inputs
and nine synthetic fixture inputs. No ROS, runtime API or motion is involved.
Exact JSON equality is required; numerical differences fail and are retained.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import time
import uuid

IMAGE = 'sha256:4857b62ca5fed0175789a298df23a4fe8118ecff0c5e041fd8dca61a9e331294'
LABEL = 'wendy.world.cloud-linux-check'
ROOT = Path(__file__).resolve().parents[1]


def encoded(value):
    return (json.dumps(value, sort_keys=True, indent=2, allow_nan=False)+'\n').encode()


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def read(path, limit):
    path = Path(path).absolute()
    if any(p.is_symlink() for p in (path, *path.parents)):
        raise ValueError('symlink input')
    fd = os.open(path, os.O_RDONLY | os.O_NONBLOCK | os.O_NOFOLLOW)
    with os.fdopen(fd, 'rb') as f:
        s = os.fstat(f.fileno())
        if not stat.S_ISREG(s.st_mode) or s.st_size > limit:
            raise ValueError('regular bounded file required')
        raw = f.read(limit+1)
        end = os.fstat(f.fileno())
    if len(raw) != s.st_size or (s.st_size,s.st_mtime_ns) != (end.st_size,end.st_mtime_ns):
        raise ValueError('input changed or exceeded bound')
    return raw


def strict(raw):
    def pairs(items):
        result = {}
        for k,v in items:
            if k in result: raise ValueError('duplicate field')
            result[k] = v
        return result
    def bad(_): raise ValueError('nonfinite JSON')
    return json.loads(raw, object_pairs_hook=pairs, parse_constant=bad)


def write(path, raw):
    with Path(path).open('xb') as f: f.write(raw)


def differences(a, b, path='$', found=None):
    if found is None: found=[]
    if len(found) >= 128: return found
    if isinstance(a,dict) and isinstance(b,dict):
        if a.keys() != b.keys(): found.append({'path':path,'kind':'keys'})
        for k in sorted(a.keys() & b.keys()): differences(a[k],b[k],path+'.'+k,found)
    elif isinstance(a,list) and isinstance(b,list):
        if len(a)!=len(b): found.append({'path':path,'kind':'length','host':len(a),'linux':len(b)})
        for i,(x,y) in enumerate(zip(a,b)): differences(x,y,path+'['+str(i)+']',found)
    elif type(a) != type(b) or a != b:
        found.append({'path':path,'kind':'value','host':a,'linux':b})
    return found


def worker():
    manifest = strict(read('/work/inputs.json',64<<10))
    if len(manifest['cases']) != 23: raise ValueError('case count')
    rows=[]; start=time.monotonic()
    for row in manifest['cases']:
        if time.monotonic()-start > 110: raise ValueError('worker deadline')
        key=row['key']; raw=read('/work/'+key+'.input.json',1<<20)
        if sha(raw)!=row['input_sha256']: raise ValueError('input hash')
        with open('/results/'+key+'.json','xb') as out, open('/results/'+key+'.stderr','xb') as err:
            p=subprocess.run(['/work/checker'],input=raw,stdout=out,stderr=err,timeout=10,check=False)
        output=read('/results/'+key+'.json',8<<20); error=read('/results/'+key+'.stderr',64<<10)
        rows.append({'key':key,'exit_code':p.returncode,'output_sha256':sha(output),'stderr_sha256':sha(error)})
        if p.returncode!=0: break
    write('/results/worker.json',encoded({'cases':rows,'elapsed_s':time.monotonic()-start}))
    return 0 if len(rows)==23 and all(r['exit_code']==0 for r in rows) else 1


def inspect(name, nonce):
    p=subprocess.run(['docker','inspect',name],capture_output=True,timeout=10)
    if p.returncode:
        e=p.stderr.decode().lower().strip()
        if any(e==prefix+kind+name for prefix in ('','error: ','error response from daemon: ') for kind in ('no such object: ','no such container: ')):
            return None
        raise ValueError('container absence unverified')
    values=strict(p.stdout)
    if len(values)!=1 or values[0]['Config']['Labels'].get(LABEL)!=nonce: raise ValueError('container ownership mismatch')
    return values[0]


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--worker',action='store_true')
    for name in ('checker','recorded-a','recorded-b','fixtures','out'): parser.add_argument('--'+name,type=Path)
    args=parser.parse_args()
    if args.worker: return worker()
    if not all(getattr(args,k) for k in ('checker','recorded_a','recorded_b','fixtures','out')): parser.error('all host paths required')
    out=args.out.absolute()
    if out.exists() or any(p.is_symlink() for p in (out,*out.parents)): raise ValueError('new output directory required')
    out.mkdir(mode=0o700);stage=out/'inputs';stage.mkdir();results=out/'results';results.mkdir()
    cases=[]
    for label,folder,count,pattern,suffix in [('a',args.recorded_a,8,'*.input.json','.output.json'),('b',args.recorded_b,6,'*.input.json','.output.json'),('fixture',args.fixtures,9,'*.request.json','.result.json')]:
        files=sorted(folder.glob(pattern))
        if len(files)!=count: raise ValueError('unexpected '+label+' input count')
        for i,path in enumerate(files):
            host=path.with_name(path.name.replace('.input.json',suffix).replace('.request.json',suffix))
            raw=read(path,1<<20);ref=read(host,8<<20)
            value=strict(ref)
            if value['input_sha256']!=sha(raw) or value['operational_authority'] is not False: raise ValueError('host input binding')
            key=label+'-'+str(i);write(stage/(key+'.input.json'),raw)
            cases.append({'key':key,'input_path':os.path.relpath(path,ROOT),'host_path':os.path.relpath(host,ROOT),'input_sha256':sha(raw),'host_sha256':sha(ref)})
    binary=read(args.checker,32<<20);write(stage/'checker',binary);os.chmod(stage/'checker',0o755)
    source=read(Path(__file__),64<<10);write(stage/'runner.py',source)
    write(stage/'inputs.json',encoded({'cases':cases}))
    nonce=uuid.uuid4().hex;name='world-cloud-linux-'+nonce[:12]
    command=['docker','run','--rm','--pull=never','--name',name,'--label',LABEL+'='+nonce,'--network','none','--read-only','--memory','512m','--cpus','2','--pids-limit','64','--cap-drop','ALL','--security-opt','no-new-privileges','--ulimit','cpu=120:120','--ulimit','fsize=9437184:9437184','--tmpfs','/tmp:rw,noexec,nosuid,size=16m','--user',str(os.getuid())+':'+str(os.getgid()),'-e','PYTHONDONTWRITEBYTECODE=1','--mount','type=bind,src='+str(stage)+',dst=/work,readonly','--mount','type=bind,src='+str(results)+',dst=/results','--entrypoint','python3',IMAGE,'-B','/work/runner.py','--worker']
    receipt={'schema':'wendy.world.go2-cloud-linux-check.v1','command':command,'image':IMAGE,'checker_sha256':sha(binary),'runner_sha256':sha(source),'cases':cases,'success':False,'operational_authority':False,'tolerance':0,'failure':None}
    start=time.monotonic()
    try:
        with (out/'docker.stdout').open('xb') as stdout,(out/'docker.stderr').open('xb') as stderr:
            p=subprocess.run(command,stdout=stdout,stderr=stderr,timeout=130,check=False)
        receipt['exit_code']=p.returncode
        for channel in ('stdout','stderr'):
            raw=read(out/('docker.'+channel),128<<10);receipt[channel]={'sha256':sha(raw),'bytes':len(raw)}
        if p.returncode!=0: raise ValueError('container failed')
        worker_result=strict(read(results/'worker.json',64<<10))
        if len(worker_result['cases'])!=23 or any(r['exit_code']!=0 for r in worker_result['cases']):raise ValueError('worker incomplete')
        for row in cases:
            actual=read(results/(row['key']+'.json'),8<<20);host=read(ROOT/row['host_path'],8<<20)
            if sha(host)!=row['host_sha256']:raise ValueError('host output changed')
            row['linux_sha256']=sha(actual);row['byte_identical']=actual==host
            row['differences']=differences(strict(host),strict(actual))
            row['parsed_equal']=not row['differences']
        receipt['success']=all(c['parsed_equal'] for c in cases)
    except Exception as e:
        receipt['failure']=type(e).__name__+': '+str(e)[:256]
    finally:
        receipt['elapsed_s']=time.monotonic()-start
        try:
            obj=inspect(name,nonce)
            if obj is not None:
                p=subprocess.run(['docker','rm','-f',obj['Id']],capture_output=True,timeout=10)
                receipt['cleanup_remove_exit']=p.returncode
            receipt['owned_container_absent']=inspect(name,nonce) is None
        except Exception as e:
            receipt['cleanup_error']=type(e).__name__;receipt['owned_container_absent']=False
        write(out/'receipt.json',encoded(receipt))
    print(json.dumps({'receipt':str(out/'receipt.json'),'success':receipt['success'],'owned_container_absent':receipt['owned_container_absent']}))
    return 0 if receipt['success'] and receipt['owned_container_absent'] else 1


if __name__=='__main__':raise SystemExit(main())
