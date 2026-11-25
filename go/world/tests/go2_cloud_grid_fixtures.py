#!/usr/bin/env python3
"""Bounded offline fixture generation and independent Go-grid comparison.

Only generate uses Docker, with an existing explicit image, no network and a
unique owned container. Neither mode connects to Wendy or a robot runtime.
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

ROOT=Path(__file__).resolve().parents[1]
sys.path.insert(0,str(ROOT))
from sim.go2_cloud_fixture import SOURCE_FILES, encoded, sha, compare, MAX_BYTES

IMAGE='sha256:dd07d4909b429baf77d136e5edd827ab3f1114e370ee77b3cbb8194f8c56d38d'
LABEL='wendy.world.cloud-grid-fixture'
INPUTS=['sim/__init__.py','sim/go2_cloud.py','sim/go2_cloud_fixture.py','sim/mujoco_compat.py']


def read_regular(path,maximum):
    path=Path(path).absolute()
    for p in [path,*path.parents]:
        if p.is_symlink():raise ValueError('symlink input')
    fd=os.open(path,os.O_RDONLY|os.O_NONBLOCK|os.O_NOFOLLOW)
    with os.fdopen(fd,'rb') as stream:
        before=os.fstat(stream.fileno())
        if not stat.S_ISREG(before.st_mode) or before.st_size>maximum:raise ValueError('input type or size bound')
        raw=stream.read(maximum+1);after=os.fstat(stream.fileno())
    if len(raw)!=before.st_size or (before.st_size,before.st_mtime_ns)!=(after.st_size,after.st_mtime_ns):raise ValueError('input changed while reading')
    return raw


def write_new(path,raw):
    path=Path(path);path.parent.mkdir(parents=True,exist_ok=True)
    with path.open('xb') as stream:stream.write(raw)
    os.chmod(path,0o600)


def output_directory(path):
    path=Path(path).absolute()
    if path.exists() or path.is_symlink() or '..' in path.parts:raise ValueError('new output directory required')
    for parent in path.parents:
        if parent.is_symlink():raise ValueError('symlink output parent')
    path.mkdir(mode=0o700)
    return path


def inspect_owned(name,nonce):
    result=subprocess.run(['docker','inspect',name],capture_output=True,timeout=10)
    if result.returncode!=0:
        # Docker CLI casing differs by installed build. An explicit absence is
        # distinct from daemon/transport/permission errors, which remain unknown.
        error=result.stderr.lower().strip()
        missing=[prefix+kind+name.encode() for prefix in (b'',b'error: ',b'error response from daemon: ')
                 for kind in (b'no such object: ',b'no such container: ')]
        if error in missing:return None
        raise ValueError('container absence unverified')
    values=json.loads(result.stdout)
    if len(values)!=1 or values[0]['Config']['Labels'].get(LABEL)!=nonce:raise ValueError('container ownership mismatch')
    return values[0]


def cleanup_owned(name,nonce):
    container=inspect_owned(name,nonce)
    removed=None
    if container is not None:
        # Immutable ID after ownership inspection; never remove any volume.
        result=subprocess.run(['docker','rm','-f',container['Id']],capture_output=True,timeout=10)
        removed=result.returncode
    return {'cleanup_remove_exit':removed,'owned_container_absent':inspect_owned(name,nonce) is None}


def generate(args):
    if args.image!=IMAGE or os.getuid()==0:raise ValueError('pinned existing image and nonroot host required')
    out=output_directory(args.out);nonce=uuid.uuid4().hex;name='world-cloud-grid-'+nonce[:12]
    stage=out/'inputs';stage.mkdir(mode=0o700);results=out/'results';results.mkdir(mode=0o700)
    inputs={}
    for relative in INPUTS:
        raw=read_regular(ROOT/relative,256<<10);write_new(stage/relative,raw);inputs[relative]=sha(raw)
    for relative,digest in SOURCE_FILES.items():
        raw=read_regular(args.runtime_source/relative,128<<10)
        if sha(raw)!=digest:raise ValueError('cached source identity mismatch')
        write_new(stage/'runtime'/relative,raw);inputs['runtime/'+relative]=digest
    command=['docker','run','--rm','--pull=never','--name',name,'--label',LABEL+'='+nonce,
        '--network','none','--read-only','--memory','1g','--cpus','2','--pids-limit','64',
        '--cap-drop','ALL','--security-opt','no-new-privileges','--ulimit','cpu=60:60',
        '--ulimit','fsize=33554432:33554432','--tmpfs','/tmp:rw,noexec,nosuid,size=64m',
        '--user',str(os.getuid())+':'+str(os.getgid()),'-e','PYTHONDONTWRITEBYTECODE=1',
        '--mount','type=bind,src='+str(stage)+',dst=/work,readonly',
        '--mount','type=bind,src='+str(results)+',dst=/results',
        '--workdir','/work','--entrypoint','python3',args.image,'-B','-m','sim.go2_cloud_fixture',
        '--runtime-source','/work/runtime','--out','/results/fixtures.json']
    receipt={'schema':'wendy.world.go2-cloud-fixture-execution.v1','command':command,'inputs':inputs,
        'image':args.image,'simulation':True,'operational_authority':False,'no_wendy_runtime_calls':True,
        'name':name,'label':nonce,'success':False}
    started=time.monotonic();failure=None
    try:
        with (out/'generate.stdout').open('xb') as stdout,(out/'generate.stderr').open('xb') as stderr:
            try:r=subprocess.run(command,stdout=stdout,stderr=stderr,timeout=120);receipt['exit_code']=r.returncode
            except subprocess.TimeoutExpired:receipt['exit_code']=None;failure='generation_deadline'
        for stream in ('stdout','stderr'):
            raw=read_regular(out/('generate.'+stream),128<<10)
            receipt[stream]={'bytes':len(raw),'sha256':sha(raw)}
        if receipt['exit_code']!=0:failure=failure or 'generator_failed'
        else:
            raw=read_regular(results/'fixtures.json',MAX_BYTES);value=json.loads(raw)
            if value['schema']!='wendy.world.go2-cloud-grid-fixtures.v1' or len(value['cases'])!=9:raise ValueError('fixture schema/count')
            if value['generator_sha256']!=inputs['sim/go2_cloud_fixture.py']:raise ValueError('generator source binding')
            if (value.get('mujoco_version')!='4.0.0' or
                value.get('offline_api_adapter',{}).get('schema')!='wendy.world.offline-multiray-adapter.v1' or
                value['offline_api_adapter'].get('native_version')!='4.0.0' or
                value['offline_api_adapter'].get('source_sha256')!=inputs['sim/mujoco_compat.py']):
                raise ValueError('native engine or compatibility adapter binding')
            receipt['fixture']={'path':'results/fixtures.json','bytes':len(raw),'sha256':sha(raw),'cases':len(value['cases'])}
            receipt['success']=True
    except Exception as error:
        failure=type(error).__name__
    finally:
        try:
            receipt.update(cleanup_owned(name,nonce))
            if not receipt['owned_container_absent']:failure='cleanup_incomplete'
        except Exception:
            receipt['owned_container_absent']=False;failure='cleanup_unverified'
        receipt['elapsed_s']=time.monotonic()-started
        receipt['failure']=failure
        receipt['success']=receipt['success'] and failure is None
        write_new(out/'execution.json',encoded(receipt))
    print(json.dumps(receipt,indent=2))
    return 0 if receipt['success'] else 1


def check(args):
    raw=read_regular(args.fixtures,MAX_BYTES);fixture=json.loads(raw)
    if fixture.get('schema')!='wendy.world.go2-cloud-grid-fixtures.v1' or not 1<=len(fixture['cases'])<=16:raise ValueError('fixture schema/count')
    binary=read_regular(args.checker,32<<20)
    if not os.access(args.checker,os.X_OK):raise ValueError('executable checker required')
    out=output_directory(args.out);started=time.monotonic();deadline=started+180
    receipt={'schema':'wendy.world.go2-cloud-fixture-comparison.v1','fixtures_sha256':sha(raw),
             'checker_sha256':sha(binary),'comparator_sha256':sha(read_regular(ROOT/'sim/go2_cloud_fixture.py',256<<10)),
             'simulation':True,'operational_authority':False,'no_runtime_calls':True,'cases':[],'success':False}
    for case in fixture['cases']:
        name=case['name']
        if not isinstance(name,str) or not name or len(name)>64 or any(c not in 'abcdefghijklmnopqrstuvwxyz0123456789_' for c in name):raise ValueError('case name')
        request=encoded(case['input']);write_new(out/(name+'.request.json'),request)
        row={'name':name,'supported_model':case['supported_model'],'input_sha256':sha(request)}
        t=time.monotonic()
        try:
            remaining=deadline-t
            if remaining<=0:raise subprocess.TimeoutExpired('whole-comparison',180)
            with (out/(name+'.result.json')).open('xb') as stdout,(out/(name+'.stderr')).open('xb') as stderr:
                r=subprocess.run([str(args.checker.absolute())],input=request,stdout=stdout,stderr=stderr,timeout=min(30,remaining))
            row['exit_code']=r.returncode
            output=read_regular(out/(name+'.result.json'),8<<20);error=read_regular(out/(name+'.stderr'),64<<10)
            row['output_sha256']=sha(output);row['stderr_sha256']=sha(error)
            if r.returncode==0:
                row.update(compare(case,json.loads(output),request))
            else:row.update(failure='checker_refused',supported_scene_pass=False)
        except subprocess.TimeoutExpired:row.update(failure='checker_deadline',supported_scene_pass=False)
        except Exception as error:row.update(failure=type(error).__name__,supported_scene_pass=False)
        row['elapsed_s']=time.monotonic()-t;receipt['cases'].append(row)
    receipt['elapsed_s']=time.monotonic()-started
    receipt['success']=all(r.get('supported_scene_pass') for r in receipt['cases'] if r['supported_model']) and not any(r.get('failure') for r in receipt['cases'])
    receipt['qualification']=False
    receipt['limits']=['Oracle is excluded from every Go request and used only after grid output.',
                       'Static supported-scene cell/footprint comparisons do not qualify robot motion.',
                       'Unsupported low obstacles/holes/thin columns retain any free-cell conflicts.']
    write_new(out/'comparison.json',encoded(receipt));print(json.dumps(receipt,indent=2))
    return 0 if receipt['success'] else 1


def main():
    parser=argparse.ArgumentParser(description=__doc__);sub=parser.add_subparsers(dest='mode',required=True)
    gen=sub.add_parser('generate');gen.add_argument('--image',required=True);gen.add_argument('--runtime-source',type=Path,required=True);gen.add_argument('--out',type=Path,required=True)
    compare_parser=sub.add_parser('compare');compare_parser.add_argument('--fixtures',type=Path,required=True);compare_parser.add_argument('--checker',type=Path,required=True);compare_parser.add_argument('--out',type=Path,required=True)
    args=parser.parse_args();return generate(args) if args.mode=='generate' else check(args)


if __name__=='__main__':raise SystemExit(main())
