#!/usr/bin/env python3
"""Give only the fixture's Docker CLI its pinned original network namespace."""
import hashlib
import os
from pathlib import Path
import re
import stat
import sys


def executable(path,digest):
    path=Path(path)
    info=path.lstat()
    if not path.is_absolute() or not stat.S_ISREG(info.st_mode) or not info.st_mode&0o111 or info.st_size>128<<20:
        raise ValueError('Docker client helper requires an absolute regular executable')
    actual=hashlib.sha256(path.read_bytes()).hexdigest()
    if not re.fullmatch('[0-9a-f]{64}',digest) or actual!=digest:
        raise ValueError('Docker client executable changed after admission')
    return str(path)


def command(arguments,environment,namespace_identity=os.readlink):
    expected=environment['WENDY_FOXGLOVE_PARENT_net']
    match=re.fullmatch(r'net:\[([0-9]+)\]',expected)
    if not match or namespace_identity('/proc/self/ns/net')==expected:
        raise ValueError('Docker client must originate in the private Agent network')
    for kind in ('mnt','pid'):
        parent=environment['WENDY_FOXGLOVE_PARENT_'+kind]
        if not re.fullmatch(kind+r':\[([0-9]+)\]',parent) or namespace_identity('/proc/self/ns/'+kind)==parent:
            raise ValueError('Docker client is outside its owned namespace')
    pinned=Path(environment['WENDY_FOXGLOVE_HOST_NET'])
    if not pinned.is_absolute() or pinned.is_symlink() or pinned.stat().st_ino!=int(match.group(1)):
        raise ValueError('pinned Docker client network namespace changed')
    docker=executable(environment['WENDY_FOXGLOVE_REAL_DOCKER'],environment['WENDY_FOXGLOVE_DOCKER_SHA256'])
    nsenter=executable(environment['WENDY_FOXGLOVE_NSENTER'],environment['WENDY_FOXGLOVE_NSENTER_SHA256'])
    # exec keeps the guardian's owned process group. Enter only the network
    # namespace; the private mount/PID namespaces, HOME/config and exact argv
    # remain unchanged, including paths for the actual production OCI export.
    return [nsenter,'--net='+str(pinned),'--',docker,*arguments]


def main():
    if sys.platform!='linux' or os.geteuid()!=0 or os.environ.get('GITHUB_ACTIONS')!='true' or os.environ.get('RUNNER_ENVIRONMENT')!='github-hosted':
        raise ValueError('Docker network helper requires the owned hosted fixture')
    argv=command(sys.argv[1:],os.environ)
    os.execv(argv[0],argv)


if __name__=='__main__':main()
