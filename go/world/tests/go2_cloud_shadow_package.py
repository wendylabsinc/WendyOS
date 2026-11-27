#!/usr/bin/env python3
"""Stage the passive app from exact private source and an explicitly pinned helper.

This does not deploy, start a capture, renew credentials or contact a registry.
The base image is an existing private ROS runtime and must be selected by digest.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import struct

ROOT = Path(__file__).resolve().parents[1]
TEMPLATE = ROOT/'deploy/go2-cloud-shadow'
MODULES = ('__init__.py', 'go2_cloud.py', 'go2_cloud_observer.py',
           'go2_cloud_recording.py', 'go2_cloud_stream.py', 'go2_cloud_shadow.py')


def require(ok, reason):
    if not ok:
        raise ValueError(reason)


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def read(path, maximum):
    fd = os.open(path, os.O_RDONLY | os.O_NONBLOCK | os.O_NOFOLLOW)
    try:
        info = os.fstat(fd)
        require(stat.S_ISREG(info.st_mode) and info.st_size <= maximum, 'bounded regular input required')
        with os.fdopen(fd, 'rb', closefd=False) as stream:
            raw = stream.read(maximum+1)
        require(len(raw) <= maximum, 'input grew beyond bound')
        return raw
    finally:
        os.close(fd)


def stage(helper, expected_sha, base, out):
    require(re.fullmatch(r'[0-9a-f]{64}', expected_sha) is not None, 'helper digest required')
    require(re.fullmatch(r'localhost:5000/[a-z0-9][a-z0-9._/-]*@sha256:[0-9a-f]{64}', base) is not None,
            'existing private local registry image digest required')
    out = out.absolute()
    require('..' not in out.parts and not out.exists() and not out.is_symlink(), 'new output directory required')
    require(all(p.is_dir() and not p.is_symlink() for p in out.parents), 'output parents must be regular directories')
    native = read(helper, 16 << 20)
    require(sha(native) == expected_sha, 'helper digest mismatch')
    require(len(native) >= 64 and native[:7] == b'\x7fELF\x02\x01\x01'
            and struct.unpack_from('<H', native, 18)[0] == 183, 'Linux AArch64 helper required')
    files = {'world-go2-cloud-shadow': native}
    sources = {}
    for name in MODULES:
        path = ROOT/'sim'/name
        raw = read(path, 128 << 10)
        files['sim/'+name] = raw
        sources[str(path.relative_to(ROOT))] = sha(raw)
    for name in ('wendy.json', 'entrypoint.sh', 'capture.sh', 'export_report.py', 'launch.py', 'Dockerfile'):
        raw = read(TEMPLATE/name, 64 << 10)
        sources[str((TEMPLATE/name).relative_to(ROOT))] = sha(raw)
        if name == 'Dockerfile':
            require(raw.count(b'__WORLD_BASE_IMAGE__') == 1, 'base marker count')
            raw = raw.replace(b'__WORLD_BASE_IMAGE__', base.encode())
        files[name] = raw
    manifest = {'schema':'wendy.world.go2-cloud-shadow-package.v1', 'simulation':True,
                'operational_authority':False, 'captures_on_startup':False,
                'base_image':base, 'helper_sha256':expected_sha, 'source_sha256':sources,
                'files':{n:{'bytes':len(raw),'sha256':sha(raw)} for n,raw in files.items()}}
    out.mkdir(mode=0o700)
    (out/'sim').mkdir(mode=0o700)
    for name, raw in files.items():
        path=out/name
        with path.open('xb') as stream: stream.write(raw)
        path.chmod(0o755 if name in ('world-go2-cloud-shadow','capture.sh','entrypoint.sh') else 0o600)
    (out/'package-manifest.json').write_text(json.dumps(manifest,sort_keys=True,indent=2)+'\n')
    return manifest


def main(argv=None):
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--helper',type=Path,required=True)
    p.add_argument('--helper-sha256',required=True)
    p.add_argument('--base-image',required=True)
    p.add_argument('--out',type=Path,required=True)
    a=p.parse_args(argv)
    result=stage(a.helper,a.helper_sha256,a.base_image,a.out)
    print(json.dumps({'out':str(a.out),'helper_sha256':result['helper_sha256'],'files':len(result['files']),
                      'operational_authority':False,'captures_on_startup':False}))


if __name__=='__main__':
    main()
