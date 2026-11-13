#!/usr/bin/env python3
"""Actual packaged Windows QEMU / WendyOS boot, candidate CLI connection and cleanup.

Hosted Windows only. No machine-wide installer, Hyper-V, driver, or user VM state.
The NSIS package is extracted as data. Every subprocess belongs to a retained Job.
"""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import platform
import shutil
import signal
import socket
import subprocess
import sys
import time
import urllib.request
import uuid

ROOT=Path(__file__).resolve().parents[2]
PINS={
    'qemu':{'url':'https://qemu.weilnetz.de/w64/2026/qemu-w64-setup-20260811.exe',
            'size':206615928,'sha256':'f98a8aeb5f7faea9765b6dee28316c266cd179d80354a2fed8e50176f9a2e59f',
            'version':'11.1.0','manifest_blob':'bb2abbb27f70181c27f27a1ec5ac5ec490000125',
            'manifest_repository':'microsoft/winget-pkgs',
            'manifest_path':'manifests/s/SoftwareFreedomConservancy/QEMU/11.1.0/SoftwareFreedomConservancy.QEMU.installer.yaml'},
    'guest':{'url':'https://storage.googleapis.com/wendyos-images-public/images/vm-arm64/0.20.2/wendyos-image-vm-arm64-wendyos.rootfs-20261102113927.wic.zst',
             'size':582023203,'sha256':'f9f3e3ac18936230d1f72a38a097ba3bae5f7e37ccaba663b20dbfcc53849ea3',
             'version':'0.20.2','device':'vm-arm64','manifest_url':'https://storage.googleapis.com/wendyos-images-public/manifests/vm-arm64.json'}
}


def digest(path):
    value=hashlib.sha256()
    with Path(path).open('rb') as source:
        for chunk in iter(lambda:source.read(1024*1024),b''):value.update(chunk)
    return value.hexdigest()


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self,*args,**kwargs):raise ValueError('artifact redirects are not admitted')


def download(key,path):
    pin=PINS[key]
    opener=urllib.request.build_opener(urllib.request.ProxyHandler({}),NoRedirect())
    size=0;sha=hashlib.sha256();deadline=time.monotonic()+300
    # The outer owned child deadline also bounds headers and slow-progress reads.
    with opener.open(pin['url'],timeout=10) as response,Path(path).open('xb') as output:
        if response.status!=200:raise ValueError('artifact HTTP status is not200')
        while True:
            data=response.read(65536)
            if not data:break
            size+=len(data)
            if size>pin['size'] or time.monotonic()>deadline:raise ValueError('artifact body/deadline exceeded')
            output.write(data);sha.update(data)
    if size!=pin['size'] or sha.hexdigest()!=pin['sha256']:raise ValueError('pinned artifact size/hash mismatch: '+key)


def admitted_child(permit,nonce,command):
    # The owner assigns this Python process to its Job before releasing it.
    # Therefore Go, CLI and QEMU descendants inherit ownership from inception.
    deadline=time.monotonic()+15
    while not Path(permit).exists():
        if time.monotonic()>deadline:raise RuntimeError('Job assignment permit missing')
        time.sleep(.02)
    if Path(permit).read_text()!=nonce:raise RuntimeError('Job assignment nonce mismatch')
    return subprocess.call(command,stdin=sys.stdin.buffer)


class Owned:
    def __init__(self,root,job,check,spawn=subprocess.Popen):
        self.root=root;self.job=job;self.check=check;self.spawn=spawn;self.children=[];self.logs=[]
    def start(self,label,command,env=None):
        self.check();nonce=uuid.uuid4().hex
        permit=self.root/(label+'-'+nonce+'.permit')
        log=(self.root/(label+'.log')).open('wb');self.logs.append(log)
        errors=(self.root/(label+'.stderr.log')).open('wb');self.logs.append(errors)
        child=self.spawn([sys.executable,str(Path(__file__).resolve()),'--child',str(permit),nonce,*map(str,command)],
                         cwd=ROOT,env=env,stdin=subprocess.PIPE,stdout=log,stderr=errors)
        self.children.append(child)
        try:
            self.job.assign(child)
        except BaseException:
            child.kill();child.wait(timeout=5)
            raise
        self.check()
        with permit.open('x') as out:out.write(nonce)
        return child
    def wait(self,child,timeout):
        deadline=time.monotonic()+timeout
        while True:
            self.check()
            remaining=deadline-time.monotonic()
            if remaining<=0:raise subprocess.TimeoutExpired(child.args,timeout)
            try:return child.wait(timeout=min(.2,remaining))
            except subprocess.TimeoutExpired:pass
    def run(self,label,command,timeout=60,env=None,expected=0):
        child=self.start(label,command,env)
        status=self.wait(child,timeout)
        if status!=expected:raise RuntimeError(f'{label}: exit{status}, expected{expected}; see {label}.log')
        return (self.root/(label+'.log')).read_text(errors='replace')
    def close(self):
        try:
            self.job.drain()
            for child in self.children:child.wait(timeout=5)
        finally:
            self.job.close()
            for child in self.children:
                if child.stdin:child.stdin.close()
            for log in self.logs:log.close()


def mutant_overlay(output):
    source=ROOT/'go/internal/cli/vm/spec.go';text=source.read_text()
    before='''\tif runtime.GOOS == "windows" {
\t\tif path, err := FindQEMU(runtime.GOOS); err == nil {
\t\t\treturn path
\t\t}
\t}
'''
    if text.count(before)!=1:raise ValueError('Windows launch resolution production site changed')
    # Keep runtime imported, as it is used only at the historical launch site.
    mutant=output/'path-only.go';mutant.write_text(text.replace(before,'\t_ = runtime.GOOS\n'))
    overlay=output/'path-only.json';overlay.write_text(json.dumps({'Replace':{str(source):str(mutant)}}))
    return overlay


def private_environment(output,program_files):
    env={key:value for key,value in os.environ.items() if not key.upper().startswith('WENDY_') and key.upper() not in ('PROGRAMFILES','PATH','CI','NO_COLOR')}
    system=Path(os.environ['SystemRoot'])
    env.update(PROGRAMFILES=str(program_files),PATH=os.pathsep.join([str(output/'empty-path'),str(system/'System32'),str(system)]),
               WENDY_CONFIG_DIR=str(output/'config'),WENDY_ANALYTICS='false',WENDY_SECRET_STORE='file',WENDY_NO_UPDATE_CHECK='1',CI='true',NO_COLOR='1')
    if shutil.which('qemu-system-aarch64',path=env['PATH']):raise ValueError('QEMU is unexpectedly present on isolated PATH')
    return env


def free_port_span():
    for _ in range(50):
        with socket.socket() as first,socket.socket() as second:
            first.bind(('127.0.0.1',0));port=first.getsockname()[1]
            if port==65535:continue
            try:second.bind(('127.0.0.1',port+1))
            except OSError:continue
            return port
    raise RuntimeError('no free adjacent private agent ports')


def validate_guest(info):
    if info.get('cpuArchitecture')!='arm64' or info.get('deviceType')!='vm-arm64' or not info.get('version'):
        raise ValueError('CLI did not observe an actual ARM64 WendyOS VM agent')
    if info.get('os')!='wendyos' or info.get('osVersion')!=PINS['guest']['version']:
        raise ValueError('guest OS/version does not match pinned image')


def validate_listener(record,state,qemu,port):
    if record.get('pid')!=state.get('pid') or record.get('port')!=port:
        raise ValueError('forwarded listener does not belong to recorded QEMU PID')
    if os.path.normcase(record.get('path',''))!=os.path.normcase(str(qemu)):
        raise ValueError('listener executable is not the pinned private QEMU')
    if state.get('accel')!='tcg' or state.get('netMode')!='user' or state.get('agentPort')!=port:
        raise ValueError('unexpected VM acceleration/network/run state')


def execute(output):
    if os.name!='nt' or platform.machine().lower() not in ('amd64','x86_64') or os.environ.get('GITHUB_ACTIONS')!='true':
        raise RuntimeError('run only on the existing disposable x64 Windows CI runner')
    temporary=Path(os.environ['RUNNER_TEMP']).resolve();output=output.resolve()
    if temporary not in output.parents or output.exists():raise ValueError('require a fresh owned directory below RUNNER_TEMP')
    if shutil.disk_usage(temporary).free<12*1024**3:raise RuntimeError('need12GiB free for pinned image/private sparse VM')
    seven=shutil.which('7z') or str(Path(os.environ['ProgramFiles'])/'7-Zip/7z.exe')
    if not Path(seven).is_file():raise RuntimeError('existing Windows runner7-Zip missing; do not install machine-wide')
    go=shutil.which('go');git=shutil.which('git')
    if not go or not git:raise RuntimeError('existing Go/Git toolchain missing')
    powershell=Path(os.environ['SystemRoot'])/'System32/WindowsPowerShell/v1.0/powershell.exe'
    output.mkdir();(output/'empty-path').mkdir()
    spec=importlib.util.spec_from_file_location('owned_windows_vm_job',Path(__file__).with_name('test-linklocal-windows.py'))
    helper=importlib.util.module_from_spec(spec);spec.loader.exec_module(helper)
    stopped=0;deadline=time.monotonic()+1500
    def stop(signum,frame):
        nonlocal stopped
        stopped=signum
    def check():
        if stopped:raise KeyboardInterrupt('Windows VM gate interrupted')
        if time.monotonic()>deadline:raise TimeoutError('Windows VM gate exceeded25minute wall deadline')
    previous={sig:signal.signal(sig,stop) for sig in (signal.SIGINT,signal.SIGTERM)}
    owner=None;evidence={'status':'failed','pins':PINS,'normal_boot':'not_run','control':'not_run','cleanup':'not_run'}
    try:
        owner=Owned(output,helper.Job(),check)
        evidence['source_revision']=owner.run('source',[git,'rev-parse','HEAD']).strip()
        evidence['source_hashes']={str(p.relative_to(ROOT)):digest(p) for p in [Path(__file__).resolve(),ROOT/'go/internal/cli/vm/host.go',ROOT/'go/internal/cli/vm/spec.go',ROOT/'go/internal/cli/vm/launch.go',ROOT/'go/internal/cli/commands/vm.go',ROOT/'.github/scripts/test-linklocal-windows.py']}
        for key,name in (('qemu','qemu-installer.exe'),('guest','guest.wic.zst')):
            owner.run('download-'+key,[sys.executable,__file__,'--download',key,str(output/name)],330)
        extracted=output/'extracted';extracted.mkdir()
        owner.run('extract',[seven,'x','-y','-o'+str(extracted),str(output/'qemu-installer.exe')],120)
        binaries=list(extracted.rglob('qemu-system-aarch64.exe'))
        if len(binaries)!=1:raise RuntimeError('expected one actual packaged ARM64 emulator')
        program_files=output/'Private Program Files';program_files.mkdir();qemu_dir=program_files/'qemu'
        shutil.move(str(binaries[0].parent),qemu_dir)
        qemu=qemu_dir/'qemu-system-aarch64.exe'
        firmware=next((p for p in [qemu_dir/'share/edk2-aarch64-code.fd',qemu_dir/'edk2-aarch64-code.fd'] if p.is_file() and p.stat().st_size==64*1024**2),None)
        if firmware is None:raise RuntimeError('actual QEMU package lacks expected64MiB ARM64 firmware')
        evidence['emulator']={'path':str(qemu),'sha256':digest(qemu),'firmware':str(firmware),'firmware_sha256':digest(firmware)}
        build_env=dict(os.environ,CGO_ENABLED='0',GOOS='windows',GOARCH='amd64',GOFLAGS='',GOENV='off')
        cli=output/'wendy.exe';control=output/'wendy-path-only.exe'
        owner.run('build',[go,'build','-o',str(cli),'./go/cmd/wendy'],300,build_env)
        owner.run('build-control',[go,'build','-overlay='+str(mutant_overlay(output)),'-o',str(control),'./go/cmd/wendy'],300,build_env)
        evidence['cli_sha256']=digest(cli);evidence['control_sha256']=digest(control)
        evidence['control_source_sha256']=digest(output/'path-only.go')
        env=private_environment(output,program_files)
        qemu_version=owner.run('qemu-version',[qemu,'--version'],20,env)
        if 'QEMU emulator version 11.1.0' not in qemu_version:raise RuntimeError('unexpected packaged QEMU version')
        evidence['qemu_version']=qemu_version.strip()
        name='reg2325-'+uuid.uuid4().hex[:12]
        port=free_port_span()
        evidence.update(name=name,port=port)
        owner.run('create',[cli,'vm','create',name,'--image',output/'guest.wic.zst','--disk','16'],300,env)
        # Restore historical bare executable launch. Private ProgramFiles is
        # populated, but PATH deliberately lacks QEMU. It must fail before boot.
        before=owner.run('path-only-control',[control,'vm','start',name,'--port',str(port),'--memory','2048','--cpus','2'],30,env,expected=1)
        before+=(output/'path-only-control.stderr.log').read_text(errors='replace')
        if 'starting qemu-system-aarch64' not in before or 'executable file not found' not in before:raise RuntimeError('control failed outside historical executable lookup')
        evidence['control']='passed_expected_failure'
        boot=owner.start('console',[cli,'vm','start',name,'--port',str(port),'--memory','2048','--cpus','2'],env)
        info=None;boot_deadline=time.monotonic()+600;attempt=0
        while time.monotonic()<boot_deadline:
            check()
            if boot.poll() is not None:raise RuntimeError('foreground QEMU/CLI exited before agent readiness')
            attempt+=1
            probe=owner.start(f'info-{attempt}',[cli,'--device','vm:'+name,'device','info','--read-only','--json'],env)
            status=owner.wait(probe,min(30,max(.1,boot_deadline-time.monotonic())))
            if status==0:
                info=json.loads((output/f'info-{attempt}.log').read_text());validate_guest(info);break
            time.sleep(1)
        if info is None:raise RuntimeError('guest agent did not become ready within10minutes')
        state_path=Path(env['WENDY_CONFIG_DIR'])/'vms'/name/'state.json'
        state=json.loads(state_path.read_text())
        # Observe actual kernel TCP ownership, not only the private run record.
        query=("$ErrorActionPreference='Stop'; "
               +f"$r=@(Get-NetTCPConnection -LocalAddress 127.0.0.1 -LocalPort {port} -State Listen); "
               +"if($r.Count -ne 1){throw 'ambiguous listener'}; "
               +"$p=Get-Process -Id $r[0].OwningProcess; "
               +"@{pid=$p.Id;path=$p.Path;port=$r[0].LocalPort}|ConvertTo-Json -Compress")
        listener=json.loads(owner.run('listener',[powershell,'-NoProfile','-NonInteractive','-Command',query],20,env))
        validate_listener(listener,state,qemu,port)
        evidence.update(normal_boot='passed',guest=info,run_state=state,listener=listener)
        # The documented foreground console exit is Ctrl-A x. This is an
        # intentional power-off of this disposable copy, not guest clean shutdown.
        boot.stdin.write(b'\x01x');boot.stdin.flush()
        if owner.wait(boot,30)!=0:raise RuntimeError('foreground console exit was unsuccessful')
        listing=json.loads(owner.run('stopped',[cli,'vm','list','--json'],20,env))
        if len(listing)!=1 or listing[0].get('name')!=name or listing[0].get('state')!='stopped':raise RuntimeError('CLI did not release its VM run lock/state')
        for reclaimed_port in (port,port+1):
            with socket.socket() as reclaimed:reclaimed.bind(('127.0.0.1',reclaimed_port))
        owner.run('remove',[cli,'vm','rm',name],20,env)
        if (Path(env['WENDY_CONFIG_DIR'])/'vms'/name).exists():raise RuntimeError('owned stopped VM directory survived removal')
        evidence['status']='passed'
    finally:
        try:
            if owner is not None:owner.close();evidence['cleanup']='passed'
            if stopped:evidence['status']='failed_interrupted'
        except BaseException as error:
            evidence['status']='failed_cleanup';evidence['cleanup']=str(error);raise
        finally:
            try:(output/'evidence.json').write_text(json.dumps(evidence,indent=2)+'\n')
            finally:
                for sig,handler in previous.items():signal.signal(sig,handler)
    if stopped:raise KeyboardInterrupt('Windows VM gate interrupted during cleanup')
    print('WINDOWS_VM_BOOT_VERIFIED packaged QEMU outside PATH, actual WendyOS agent, historical control, owned foreground cleanup')


def main():
    if len(sys.argv)>1 and sys.argv[1]=='--child':return admitted_child(sys.argv[2],sys.argv[3],sys.argv[4:])
    if len(sys.argv)>1 and sys.argv[1]=='--download':download(sys.argv[2],sys.argv[3]);return 0
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('output',type=Path)
    args=parser.parse_args();execute(args.output)

if __name__=='__main__':raise SystemExit(main())
