#!/usr/bin/env python3
"""Install the hosted NM fixture dependencies without starting host services.

Existing policy-rc.d is never renamed or rewritten. A read-only bind mount
shadows it only inside private mount/PID namespaces; private /run hides host
service sockets. The only possible host policy write is an O_EXCL temporary
mountpoint when no policy exists, removed only if its owned identity survives.
"""
import argparse
import contextlib
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import signal
import stat
import subprocess
import sys
import tempfile

HERE=Path(__file__).resolve().parent
POLICY=b'#!/bin/sh\n# Owned temporary Wendy USB regression package policy.\nexit 101\n'


def snapshot(path):
    entry=path.lstat()
    target=path.resolve(strict=True)
    info=target.stat()
    if not stat.S_ISREG(info.st_mode) or info.st_size>1024*1024:
        raise ValueError('policy-rc.d must resolve to a bounded regular file')
    return {'entry':[entry.st_dev,entry.st_ino,entry.st_mode,entry.st_uid,entry.st_gid,entry.st_mtime_ns],
            'target':str(target),'target_stat':[info.st_dev,info.st_ino,info.st_mode,info.st_uid,info.st_gid,info.st_mtime_ns],
            'sha256':hashlib.sha256(target.read_bytes()).hexdigest()}


@contextlib.contextmanager
def blocked_signals():
    previous=signal.pthread_sigmask(signal.SIG_BLOCK,{signal.SIGINT,signal.SIGTERM})
    try:yield
    finally:signal.pthread_sigmask(signal.SIG_SETMASK,previous)


class PolicyMountpoint:
    def __init__(self,path):
        self.path=Path(path)
        self.created=None
        self.original=None
    def prepare(self):
        # Record ownership before an interrupt can leave a newly created file.
        with blocked_signals():
            try:
                fd=os.open(self.path,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o755)
            except FileExistsError:
                self.original=snapshot(self.path)
            else:
                with os.fdopen(fd,'wb') as target:
                    info=os.fstat(target.fileno())
                    self.created=(info.st_dev,info.st_ino)
                    target.write(POLICY)
                    target.flush()
                    os.fsync(target.fileno())
                self.original=snapshot(self.path)
        return self.original['target']
    def cleanup(self,*,namespace_drained=True):
        if self.created is None:
            if self.original is not None and snapshot(self.path)!=self.original:
                raise RuntimeError('preexisting package policy changed; refusing any repair')
            return
        if not namespace_drained:
            raise RuntimeError("namespace drain unproven; retaining owned exit101 policy for disposable-runner teardown")
        info=self.path.lstat()
        if (info.st_dev,info.st_ino)!=self.created or not stat.S_ISREG(info.st_mode):
            raise RuntimeError('temporary policy was replaced; refusing to remove unrelated state')
        if self.original is not None and snapshot(self.path)!=self.original:
            raise RuntimeError('temporary policy was modified; refusing to remove unrelated state')
        self.path.unlink()


def provision(policy,output,runner,*,work_parent=None,download=None):
    output=Path(output)
    output.mkdir(parents=True,exist_ok=False)
    owner=PolicyMountpoint(policy)
    result={'status':'failed','policy_preexisting':Path(policy).exists() or Path(policy).is_symlink(),
            'packages':['network-manager','dbus'],'cleanup_errors':[]}
    namespace_drained=True
    try:
        # Fetch while the host resolver is visible. This phase only downloads
        # archives; package installation stays behind private service sockets.
        if download is not None:
            with (output/'download.log').open('w') as log:
                result['download_exit_code']=download(log)
            if result['download_exit_code']!=0:
                raise RuntimeError('dependency download failed: '+str(result['download_exit_code']))
        with tempfile.TemporaryDirectory(prefix='wendy-nm-install-',dir=work_parent) as directory:
            work=Path(directory)
            stub=work/'policy-rc.d'
            stub.write_bytes(POLICY);stub.chmod(0o755)
            target=owner.prepare()
            result['policy_before']=owner.original
            plan={'policy':str(target),'stub':str(stub),'completion':str(work/'complete.json'),'nonce':os.urandom(16).hex(),'parent_mnt':os.readlink('/proc/self/ns/mnt') if sys.platform=='linux' else 'offline',
                  'parent_pid':os.readlink('/proc/self/ns/pid') if sys.platform=='linux' else 'offline'}
            plan_path=work/'plan.json';plan_path.write_text(json.dumps(plan));plan_path.chmod(0o600)
            with (output/'install.log').open('w') as log:
                namespace_drained=False
                status,namespace_drained=runner(plan_path,log)
            result['install_exit_code']=status
            result['namespace_drained']=namespace_drained
            if not namespace_drained:raise RuntimeError('dependency namespace completion is unproven')
            if status!=0:raise RuntimeError('dependency installation failed: '+str(status))
            result['status']='passed'
    except BaseException as error:
        result['error']=type(error).__name__+': '+str(error)
        raise
    finally:
        previous={s:signal.signal(s,signal.SIG_IGN) for s in (signal.SIGINT,signal.SIGTERM)}
        try:
            result['namespace_drained']=namespace_drained
            try:owner.cleanup(namespace_drained=namespace_drained)
            except Exception as error:
                result['cleanup_errors'].append(str(error));result['status']='failed'
            (output/'result.json').write_text(json.dumps(result,indent=2)+'\n')
        finally:
            for sig,handler in previous.items():signal.signal(sig,handler)
        if result['cleanup_errors']:
            raise RuntimeError('; '.join(result['cleanup_errors']))


def inside(plan_path):
    plan=json.loads(Path(plan_path).read_text())
    for kind in ('mnt','pid'):
        if os.readlink('/proc/self/ns/'+kind)==plan['parent_'+kind]:
            raise RuntimeError('package install must have private '+kind+' namespace')
    if os.getpid()!=1:
        raise RuntimeError('package helper must own PID namespace init')
    stub,target=Path(plan['stub']),Path(plan['policy'])
    if stub.read_bytes()!=POLICY or not target.is_file():
        raise RuntimeError('invalid owned package policy mount')
    subprocess.run(['mount','--make-rprivate','/'],check=True,timeout=10)
    subprocess.run(['mount','-t','tmpfs','tmpfs','/run'],check=True,timeout=10)
    Path('/run/lock').mkdir(mode=0o755)
    subprocess.run(['mount','--bind',str(stub),str(target)],check=True,timeout=10)
    subprocess.run(['mount','-o','remount,bind,ro',str(target)],check=True,timeout=10)
    if Path('/usr/sbin/policy-rc.d').read_bytes()!=POLICY:
        raise RuntimeError('service-start policy is not active in package namespace')
    # Package scripts cannot contact host systemd/DBus through the private /run.
    # needrestart lists only; do not upgrade already installed requested packages.
    env=dict(os.environ,DEBIAN_FRONTEND='noninteractive',NEEDRESTART_MODE='l')
    for key in ('DBUS_SYSTEM_BUS_ADDRESS','DBUS_SESSION_BUS_ADDRESS','DBUS_STARTER_ADDRESS','DBUS_STARTER_BUS_TYPE','SYSTEMD_BUS_ADDRESS'):
        env.pop(key,None)
    status=subprocess.run(['apt-get','install','-y','--no-download','--no-upgrade','--no-install-recommends','network-manager','dbus'],env=env).returncode
    # This Python process is namespace PID1 and owns/reaps orphaned children.
    # A normal helper exit is insufficient if package scripts left descendants.
    while True:
        try:
            pid,_=os.waitpid(-1,os.WNOHANG)
            if pid==0:break
        except ChildProcessError:break
    remaining=[p.name for p in Path('/proc').iterdir() if p.name.isdecimal() and int(p.name)!=os.getpid()]
    if remaining:raise RuntimeError('package namespace still contains children; no completion proof')
    Path(plan['completion']).write_text(json.dumps({'nonce':plan['nonce'],'exit_code':status}))
    raise SystemExit(status)


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('output',type=Path,nargs='?')
    parser.add_argument('--inside',type=Path)
    args=parser.parse_args()
    if sys.platform!='linux' or os.geteuid()!=0 or os.environ.get('GITHUB_ACTIONS')!='true' or os.environ.get('RUNNER_ENVIRONMENT')!='github-hosted':
        parser.error('requires root in the disposable hosted Linux runner')
    if args.inside:
        inside(args.inside)
        return
    if args.output is None:parser.error('new output directory required')
    spec=importlib.util.spec_from_file_location('nm_owned',HERE/'run-owned-media-test.py')
    owned=importlib.util.module_from_spec(spec);spec.loader.exec_module(owned)
    def interrupted(signum,frame):raise InterruptedError('package fixture interrupted')
    previous={s:signal.signal(s,interrupted) for s in (signal.SIGINT,signal.SIGTERM)}
    try:
        def run(plan,log):
            details=json.loads(plan.read_text())
            status=owned.run(['unshare','--mount','--pid','--fork','--propagation','private','--mount-proc',
                sys.executable,str(Path(__file__).resolve()),'--inside',str(plan)],300,stdout=log)
            try:
                completed=json.loads(Path(details['completion']).read_text())
                drained=completed=={'nonce':details['nonce'],'exit_code':status}
            except (OSError,ValueError):drained=False
            return status,drained
        def download(log):
            return owned.run(['apt-get','install','-y','--download-only','--no-upgrade',
                '--no-install-recommends','network-manager','dbus'],180,stdout=log,
                env=dict(os.environ,DEBIAN_FRONTEND='noninteractive',NEEDRESTART_MODE='l'))
        provision(Path('/usr/sbin/policy-rc.d'),args.output,run,work_parent='/var/tmp',download=download)
    finally:
        for sig,handler in previous.items():signal.signal(sig,handler)

if __name__=='__main__':main()
