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
import struct
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
    env={key:value for key,value in os.environ.items() if not key.upper().startswith('WENDY_') and key.upper() not in ('PROGRAMFILES','PROGRAMFILES(X86)','PROGRAMW6432','PATH','CI','NO_COLOR')}
    system=Path(os.environ['SystemRoot'])
    env.update(PROGRAMFILES=str(program_files),PATH=os.pathsep.join([str(output/'empty-path'),str(system/'System32'),str(system)]),
               WENDY_CONFIG_DIR=str(output/'config'),WENDY_ANALYTICS='false',WENDY_SECRET_STORE='file',WENDY_NO_UPDATE_CHECK='1',CI='true',NO_COLOR='1')
    # Keep architecture-specific directory aliases consistent in every owned
    # Python/Go descendant. Retain the real resolver probe: this is not boot proof.
    env['PROGRAMFILES(X86)']=str(program_files)
    env['PROGRAMW6432']=str(program_files)
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
    if info.get('os')!='wendyos' or info.get('osVersion')!='WendyOS-'+PINS['guest']['version']:
        raise ValueError('guest OS/version does not match pinned image')


def validate_identity(record,info,port,nonce):
    if record.get('address')!='127.0.0.1:'+str(port) or record.get('nonce')!=nonce:
        raise ValueError('identity receipt did not use the owned loopback forward')
    for key in ('hostname',):
        if not isinstance(record.get(key),str) or not record[key].strip():
            raise ValueError('actual RPC persistent hostname missing')
    try:boot_id=uuid.UUID(record.get('bootId',''))
    except (ValueError,TypeError,AttributeError):raise ValueError('actual kernel boot identity missing or invalid')
    if boot_id.int==0 or str(boot_id)!=record['bootId'] or type(record.get('bootTimeNs')) is not int or record['bootTimeNs']<=0:
        raise ValueError('actual kernel boot identity/time missing or invalid')
    for key in ('version','os','osVersion','cpuArchitecture','deviceType'):
        if record.get(key)!=info.get(key):
            raise ValueError('identity RPC disagrees with CLI device info: '+key)


def validate_restart(state,identity,disk,first_state,first_identity,first_disk):
    if not state.get('startedAt') or state['startedAt']==first_state.get('startedAt'):
        raise ValueError('restart reused the previous VM run record')
    if identity['hostname']!=first_identity['hostname'] or disk!=first_disk:
        raise ValueError('same-disk restart changed hostname or owned disk identity')
    if identity['bootId']==first_identity['bootId']:
        raise ValueError('restart did not produce a new kernel boot ID')


def validate_listener(record,state,qemu,port):
    if record.get('pid')!=state.get('pid') or record.get('port')!=port:
        raise ValueError('forwarded listener does not belong to recorded QEMU PID')
    if os.path.normcase(record.get('path',''))!=os.path.normcase(str(qemu)):
        raise ValueError('listener executable is not the pinned private QEMU')
    if state.get('accel')!='tcg' or state.get('netMode')!='user' or state.get('agentPort')!=port:
        raise ValueError('unexpected VM acceleration/network/run state')



def validate_path_only_control(code,stderr):
    # Missing executables use the CLI's documented machine error exit6, rather
    # than a generic Cobra exit1. Demand the exact launch boundary as well.
    try:record=json.loads(stderr)['error']
    except (ValueError,KeyError,TypeError):raise ValueError('control did not emit a structured CLI error')
    if not isinstance(record,dict):raise ValueError('control error is not an object')
    message=record.get('message','')
    if code!=6 or record.get('exit')!=6 or record.get('code')!='tool_not_found' or record.get('retryable') is not False:
        raise ValueError('control did not reach the missing-tool contract')
    if not isinstance(message,str) or not message.startswith('starting qemu-system-aarch64: exec: "qemu-system-aarch64": ') or 'executable file not found in %PATH%' not in message:
        raise ValueError('control failed outside the historical bare-QEMU launch')
    return record


def validate_lifecycle_port(phase,port,metadata,first_state):
    if type(port) is not int or metadata.get('agentPort')!=port:
        raise ValueError('automatic start did not persist actual agent port')
    if phase=='collision':
        if port<=50051 or port>50051+7*2 or (port-50051)%2:
            raise ValueError('occupied default port did not select a bounded later span')
    elif phase=='restart':
        if not first_state or port!=first_state.get('agentPort'):
            raise ValueError('restart discarded saved fallback port')
    else:raise ValueError('unknown VM lifecycle phase')


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
        lookup=output/'qemu-lookup.exe'
        owner.run('build-lookup',[go,'build','-o',str(lookup),'./go/internal/testfixture/windowsqemulookup'],120,build_env)
        identity_probe=output/'vm-identity.exe'
        owner.run('build-identity',[go,'build','-o',str(identity_probe),'./go/internal/testfixture/windowsvmidentity'],120,build_env)
        evidence['identity_probe_sha256']=digest(identity_probe)
        evidence['identity_probe_source_sha256']=digest(ROOT/'go/internal/testfixture/windowsvmidentity/main.go')
        evidence['lookup_sha256']=digest(lookup)
        evidence['lookup_source_sha256']=digest(ROOT/'go/internal/testfixture/windowsqemulookup/main.go')
        evidence['python']={'executable':sys.executable,'pointer_bits':struct.calcsize('P')*8,'machine':platform.machine()}
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
        # Run the unchanged production lookup in the same owned process chain
        # and environment. A preflight failure is not the historical launch bug.
        owner.run('qemu-lookup',[lookup,qemu],20,env)
        # Restore historical bare executable launch. Private ProgramFiles is
        # populated, but PATH deliberately lacks QEMU. It must fail before boot.
        owner.run('path-only-control',[control,'vm','start',name,'--port',str(port),'--memory','2048','--cpus','2'],30,env,expected=6)
        evidence['control_error']=validate_path_only_control(6,(output/'path-only-control.stderr.log').read_text(errors='replace'))
        control_listing=json.loads(owner.run('control-stopped',[cli,'vm','list','--json'],20,env))
        if len(control_listing)!=1 or control_listing[0].get('name')!=name or control_listing[0].get('state')!='stopped':
            raise RuntimeError('historical launch failure left an active VM')
        evidence['control']='passed_expected_failure'
        # Hold only our own default port while starting without --port. This
        # exercises actual foreground port fallback, not a mocked allocator.
        blocker=socket.socket()
        try:
            blocker.bind(('127.0.0.1',50051));blocker.listen(1)
        except BaseException:
            blocker.close();raise
        try:
            first_state=None;first_identity=None;first_disk=None
            identity_nonce=uuid.uuid4().hex
            for phase in ('collision','restart'):
                boot=owner.start(phase+'-console',[cli,'vm','start',name,'--memory','2048','--cpus','2'],env)
                info=None;boot_deadline=time.monotonic()+600;attempt=0
                while time.monotonic()<boot_deadline:
                    check()
                    if boot.poll() is not None:raise RuntimeError('foreground QEMU/CLI exited before agent readiness: '+phase)
                    attempt+=1
                    label=f'{phase}-info-{attempt}'
                    probe=owner.start(label,[cli,'--device','vm:'+name,'device','info','--read-only','--json'],env)
                    status=owner.wait(probe,min(30,max(.1,boot_deadline-time.monotonic())))
                    if status==0:
                        info=json.loads((output/(label+'.log')).read_text());validate_guest(info);break
                    time.sleep(1)
                if info is None:raise RuntimeError('guest agent did not become ready within10minutes: '+phase)
                vm_dir=Path(env['WENDY_CONFIG_DIR'])/'vms'/name
                state=json.loads((vm_dir/'state.json').read_text())
                metadata=json.loads((vm_dir/'meta.json').read_text())
                port=state.get('agentPort')
                validate_lifecycle_port(phase,port,metadata,first_state)
                # Observe actual kernel TCP ownership, not only private records.
                query=("$ErrorActionPreference='Stop'; "
                       +f"$r=@(Get-NetTCPConnection -LocalAddress 127.0.0.1 -LocalPort {port} -State Listen); "
                       +"if($r.Count -ne 1){throw 'ambiguous listener'}; "
                       +"$p=Get-Process -Id $r[0].OwningProcess; "
                       +"@{pid=$p.Id;path=$p.Path;port=$r[0].LocalPort}|ConvertTo-Json -Compress")
                listener=json.loads(owner.run(phase+'-listener',[powershell,'-NoProfile','-NonInteractive','-Command',query],20,env))
                validate_listener(listener,state,qemu,port)
                # device info has no hostname field. Read it and the actual
                # kernel boot ID from read-only RPCs, only after kernel ownership
                # has bound this literal loopback port to our packaged QEMU.
                identity=json.loads(owner.run(phase+'-identity',[identity_probe,'127.0.0.1:'+str(port),identity_nonce],15,env))
                validate_identity(identity,info,port,identity_nonce)
                confirmed=json.loads(owner.run(phase+'-listener-after-identity',[powershell,'-NoProfile','-NonInteractive','-Command',query],20,env))
                validate_listener(confirmed,state,qemu,port)
                disk=vm_dir/'disk.img'
                if disk.is_symlink():raise RuntimeError('owned VM disk was replaced by a symlink')
                disk_stat=disk.stat()
                if not disk.is_file() or disk_stat.st_ino<=0:raise RuntimeError('owned disk file identity unavailable')
                disk_identity={'path':str(disk.resolve()),'device':disk_stat.st_dev,'inode':disk_stat.st_ino,'size':disk_stat.st_size}
                if phase=='collision':
                    first_state=state;first_identity=identity;first_disk=disk_identity
                else:
                    validate_restart(state,identity,disk_identity,first_state,first_identity,first_disk)
                evidence.setdefault('lifecycle',[]).append({'phase':phase,'guest':info,'identity':identity,'state':state,'metadata':metadata,'listener':listener,'listener_after_identity':confirmed,'disk_identity':disk_identity})
                # Documented foreground console exit, only for this disposable
                # owned disk. Stop/join before probing ports or restarting.
                boot.stdin.write(b'\x01x');boot.stdin.flush()
                if owner.wait(boot,30)!=0:raise RuntimeError('foreground console exit failed: '+phase)
                listing=json.loads(owner.run(phase+'-stopped',[cli,'vm','list','--json'],20,env))
                if len(listing)!=1 or listing[0].get('name')!=name or listing[0].get('state')!='stopped':raise RuntimeError('CLI retained VM run lock/state: '+phase)
                for reclaimed_port in (port,port+1):
                    with socket.socket() as reclaimed:reclaimed.bind(('127.0.0.1',reclaimed_port))
                # Release the original obstruction before restart. The saved
                # fallback must win even though50051 is now available again.
                if phase=='collision':blocker.close()
            evidence.update(normal_boot='passed',collision_fallback='passed',same_disk_restart='passed',guest=first_info,run_state=first_state)
        finally:
            blocker.close()
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
    print('WINDOWS_VM_BOOT_VERIFIED packaged QEMU outside PATH, actual WendyOS agent, historical control, default-port collision, sticky same-disk restart, owned foreground cleanup')


def main():
    if len(sys.argv)>1 and sys.argv[1]=='--child':return admitted_child(sys.argv[2],sys.argv[3],sys.argv[4:])
    if len(sys.argv)>1 and sys.argv[1]=='--download':download(sys.argv[2],sys.argv[3]);return 0
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('output',type=Path)
    args=parser.parse_args();execute(args.output)

if __name__=='__main__':raise SystemExit(main())
