#!/usr/bin/env python3
"""Boot a private published WendyOS VM through actual CLI/Agent/QMP paths.

The hosted route requires Intel macOS. An independently confirmed bare-metal
ARM64 reservation can run the same production HVF path. Generation/admission
never means a VM was executed. No process is signalled by a saved PID.
"""
import argparse
import fcntl
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import platform
import shutil
import signal
import socket
import stat
import subprocess
import sys
import tempfile
import time
import uuid

ROOT=Path(__file__).resolve().parents[2]

def load(name,path):
    spec=importlib.util.spec_from_file_location(name,path); module=importlib.util.module_from_spec(spec); spec.loader.exec_module(module); return module

GUARD=load('vm_guard',ROOT/'.github/scripts/run-owned-media-test.py')
ARTIFACT=load('vm_artifact',ROOT/'.github/scripts/test-windows-vm-boot.py')


def sha(path):
    return ARTIFACT.digest(path)


def output(command):
    return subprocess.check_output(command,text=True,timeout=10).strip()


def host_identity():
    import plistlib
    rows=plistlib.loads(subprocess.check_output(['/usr/sbin/ioreg','-a','-rd1','-c','IOPlatformExpertDevice'],timeout=10))
    return {'platform_uuid':rows[0]['IOPlatformUUID'],'uid':os.getuid(),'hostname':socket.gethostname(),
            'boot':output(['/usr/sbin/sysctl','-n','kern.boottime']),'machine':platform.machine(),'system':platform.system()}


def admit_host(identity,reservation=None,env=os.environ):
    if identity['system']!='Darwin' or identity['uid']==0:raise ValueError('ordinary macOS account required')
    if reservation is None:
        if identity['machine']!='x86_64' or env.get('GITHUB_ACTIONS')!='true' or env.get('RUNNER_ENVIRONMENT')!='github-hosted':
            raise ValueError('hosted entrypoint requires the disposable Intel macOS runner')
        return 'hosted_tcg'
    if identity['machine']!='arm64' or reservation.get('identity')!=identity:
        raise ValueError('reserved Apple Silicon host identity changed')
    if reservation.get('owner_confirmation')!='I reserve this bare-metal Mac for the owned WendyOS VM lifecycle':
        raise ValueError('exact owner reservation missing')
    if reservation.get('hardware_execution')!='not_run':raise ValueError('reservation must begin not_run')
    return 'reserved_hvf'


def qmp(path,command):
    deadline=time.monotonic()+3
    with socket.socket(socket.AF_UNIX,socket.SOCK_STREAM) as conn:
        conn.settimeout(3);conn.connect(str(path));pending=bytearray();received=0
        def read():
            nonlocal received
            while b'\n' not in pending:
                remaining=deadline-time.monotonic()
                if remaining<=0:raise TimeoutError('absolute QMP deadline')
                conn.settimeout(remaining)
                chunk=conn.recv(4096)
                received+=len(chunk)
                if not chunk or received>65536:raise ValueError('QMP missing or oversized exchange')
                pending.extend(chunk)
            line,_,rest=pending.partition(b'\n');pending[:]=rest
            return json.loads(line)
        if 'QMP' not in read():raise ValueError('missing QMP greeting')
        for cmd in ('qmp_capabilities',command):
            remaining=deadline-time.monotonic()
            if remaining<=0:raise TimeoutError('absolute QMP deadline')
            conn.settimeout(remaining);conn.sendall(json.dumps({'execute':cmd}).encode()+b'\n')
            while True:
                if time.monotonic()>=deadline:raise TimeoutError('absolute QMP event deadline')
                response=read()
                if 'event' in response:continue
                if 'return' not in response:raise ValueError('QMP refused '+cmd)
                break


def drain(config,name,nonce,timeout=20):
    # No generic process sweep: the nonce-owned private socket talks only to
    # this VM. Its inherited production flock proves when the QEMU writer ends.
    if (config/'fixture-owner').read_text()!=nonce:raise ValueError('cleanup ownership mismatch')
    vm=config/'vms'/name
    if not vm.exists():return True
    if vm.is_symlink() or vm.stat().st_uid!=os.getuid():raise ValueError('unsafe VM directory')
    lock=vm/'run.lock'
    if not lock.exists():return not (vm/'state.json').exists()
    if lock.is_symlink() or not stat.S_ISREG(lock.stat().st_mode):raise ValueError('unsafe run lock')
    with lock.open('rb') as stream:
        deadline=time.monotonic()+timeout;sent=False
        while True:
            try:fcntl.flock(stream.fileno(),fcntl.LOCK_EX|fcntl.LOCK_NB);return True
            except BlockingIOError:pass
            if not sent:
                try:qmp(vm/'qmp.sock','quit');sent=True
                except (OSError,ValueError):pass
            if time.monotonic()>=deadline:return False
            time.sleep(.1)


def validate_log(text,code,control=False):
    if any(x in text for x in ('--- SKIP:','WARNING: DATA RACE','panic:','VM_DRAIN_UNKNOWN')):
        raise ValueError('incomplete/skipped/raced VM evidence')
    if control:
        if code!=1 or not all(marker in text for marker in ('VM_GUEST_APP_STARTED','VM_CANCEL_RECOVERY_VERIFIED')) or 'VM_APP_FORWARD: actual guest application never became reachable:' not in text or '--- FAIL: TestVMOwnedLifecycle ' not in text or 'VM_LIFECYCLE_VERIFIED' in text:
            raise ValueError('missing-forwarding control did not fail the actual app transport boundary')
    elif code!=0 or not all(x in text for x in ('VM_CANCEL_RECOVERY_VERIFIED','VM_APP_FORWARD_VERIFIED','VM_RESTART_FORWARD_VERIFIED','VM_LIFECYCLE_VERIFIED','--- PASS: TestVMOwnedLifecycle ')):
        raise ValueError('actual VM lifecycle evidence incomplete')
    if 'VM_OWNED_DRAINED' not in text:raise ValueError('owned VM drain was not observed')


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--cli',type=Path,required=True)
    parser.add_argument('--test-binary',type=Path,required=True,help='existing commands test binary from this exact source')
    parser.add_argument('--output',type=Path)
    parser.add_argument('--write-reservation',type=Path,help='only record source/binary/host pins; never create or boot a VM')
    parser.add_argument('--reserved-host',type=Path)
    args=parser.parse_args()
    identity=host_identity();reservation=json.loads(args.reserved_host.read_text()) if args.reserved_host else None
    if args.write_reservation:
        if reservation or identity['system']!='Darwin' or identity['machine']!='arm64' or identity['uid']==0:
            raise ValueError('reservation generation requires the intended ordinary-account Apple Silicon host')
        mode='prepare_only'
    else:
        if not args.output:parser.error('--output is required for execution')
        mode=admit_host(identity,reservation)
    cli=args.cli.resolve(strict=True);normal=args.test_binary.resolve(strict=True)
    qemu=Path(shutil.which('qemu-system-aarch64') or '').resolve(strict=True)
    prefix=output(['brew','--prefix']);firmware=Path(prefix)/'share/qemu/edk2-aarch64-code.fd'
    if not qemu.is_file() or not os.access(qemu,os.X_OK) or not firmware.is_file() or firmware.stat().st_size!=64*1024*1024:
        raise ValueError('installed native QEMU and padded ARM64 firmware required')
    sources=['go/internal/cli/commands/vm_lifecycle_darwin_test.go','go/internal/cli/commands/vm_ports.go','go/internal/cli/commands/vm_ensure.go','go/internal/testfixture/vmhttp/main.go','.github/scripts/test-macos-vm-lifecycle.py','.github/scripts/run-owned-media-test.py']
    pins={'cli_sha256':sha(cli),'test_binary_sha256':sha(normal),'qemu_sha256':sha(qemu),'firmware_sha256':sha(firmware),
          'python_sha256':sha(Path(sys.executable).resolve()),'revision':output(['git','-C',str(ROOT),'rev-parse','HEAD']),
          'sources':{p:sha(ROOT/p) for p in sources}}
    if args.write_reservation:
        with args.write_reservation.open('x') as stream:
            json.dump({'identity':identity,'pins':pins,'owner_confirmation':'','hardware_execution':'not_run'},stream,indent=2);stream.write('\n')
        print('Recorded admission only; owner must confirm bare-metal reservation before a separate execution.')
        return 0
    if reservation and reservation.get('pins')!=pins:raise ValueError('reserved source/binary pins changed')
    args.output.mkdir(parents=True,exist_ok=False)
    evidence={'status':'failed','mode':mode,'host':identity,'pins':pins,'qemu_version':output([str(qemu),'--version']),
              'guest_pin':ARTIFACT.PINS['guest'],'phases':[],'hardware_execution':'not_run' if mode=='reserved_hvf' else 'not_applicable'}
    if shutil.disk_usage('/tmp').free<12*1024**3:raise ValueError('12GiB free required for one owned sparse guest and build outputs')
    work=Path(tempfile.mkdtemp(prefix='wv-',dir='/tmp'));work.chmod(0o700);evidence['owned_work']=str(work)
    deadline=time.monotonic()+32*60;active=None;drained=True
    def stop(signum,frame):raise InterruptedError('VM fixture interrupted by signal '+str(signum))
    previous={sig:signal.signal(sig,stop) for sig in (signal.SIGINT,signal.SIGTERM)}
    def run(label,command,env=None,bound=300):
        remaining=min(bound,deadline-time.monotonic())
        if remaining<=0:raise TimeoutError('VM lifecycle total deadline')
        with (args.output/(label+'.log')).open('xb') as stream:
            return GUARD.run(list(map(str,command)),remaining,cwd=ROOT,env=env,stdout=stream)
    try:
        guest=work/'guest.wic.zst';ARTIFACT.download('guest',guest)
        app=work/'guest-http'
        build_env={**os.environ,'GOOS':'linux','GOARCH':'arm64','CGO_ENABLED':'0'}
        if run('guest-build',['go','build','-trimpath','-o',app,'./go/internal/testfixture/vmhttp'],build_env)!=0:raise RuntimeError('static guest app build failed')
        source=ROOT/'go/internal/cli/commands/vm_ports.go';text=source.read_text()
        needle='return forwardVMPorts(ctx, name, ports)'
        if text.count(needle)!=1:raise ValueError('historical forwarding boundary changed')
        mutant=work/'vm_ports.go';mutant.write_text(text.replace(needle,'return nil // pre-0a3c57ff89c6c8e VM app forwarding behavior'))
        overlay=work/'overlay.json';overlay.write_text(json.dumps({'Replace':{str(source):str(mutant)}}))
        control=work/'commands-control.test'
        if run('control-build',['go','test','-overlay',overlay,'-c','-o',control,'./go/internal/cli/commands'])!=0:raise RuntimeError('forwarding control compile failed')
        evidence['guest_application_sha256']=sha(app);evidence['control_binary_sha256']=sha(control)
        for phase,binary in (('normal',normal),('missing-forwarding',control)):
            for path,key in ((cli,'cli_sha256'),(normal,'test_binary_sha256'),(qemu,'qemu_sha256'),(firmware,'firmware_sha256')):
                if sha(path)!=pins[key]:raise ValueError('runtime pin changed: '+key)
            nonce=uuid.uuid4().hex;name='vm-'+nonce[:16];config=work/phase;config.mkdir(mode=0o700)
            (config/'fixture-owner').write_text(nonce);home=work/(phase+'-home');home.mkdir(mode=0o700)
            env={**os.environ,'HOME':str(home),'WENDY_CONFIG_DIR':str(config),'WENDY_SECRET_STORE':'file','WENDY_ANALYTICS':'false',
                 'WENDY_NO_UPDATE_CHECK':'1','CI':'true','WENDY_VM_LIFECYCLE_NAME':name,'WENDY_VM_LIFECYCLE_NONCE':nonce,'WENDY_VM_LIFECYCLE_APP':str(app)}
            active=(config,name,nonce);drained=False
            if run(phase+'-create',[cli,'vm','create',name,'--image',guest,'--disk','16'],env)!=0:raise RuntimeError('actual CLI VM creation failed')
            code=run(phase,[binary,'-test.run','^TestVMOwnedLifecycle$','-test.v','-test.count=1','-test.timeout=0'],env,14*60 if phase=='normal' else 8*60)
            drained=drain(*active)
            vm=config/'vms'/name
            for file in ('console.log','meta.json','state.json'):
                if (vm/file).exists():shutil.copyfile(vm/file,args.output/(phase+'-'+file))
            if not drained:raise RuntimeError('unknown owned QEMU drain; retaining work')
            validate_log((args.output/(phase+'.log')).read_text(errors='replace'),code,phase!='normal')
            evidence['phases'].append({'name':phase,'exit_code':code,'vm':name,'nonce':nonce,'drained':True})
            if run(phase+'-remove',[cli,'vm','rm',name,'--yes'],env,30)!=0:raise RuntimeError('actual stopped VM removal failed')
            if vm.exists():raise RuntimeError('removed VM directory remains')
            active=None
        evidence['status']='passed';evidence['hardware_execution']='executed_pending_operator_review' if mode=='reserved_hvf' else 'not_applicable'
    except BaseException as error:
        evidence['error']=str(error)
        raise
    finally:
        try:
            if active:
                try:drained=drain(*active)
                except BaseException as error:drained=False;evidence['cleanup_error']=str(error)
            evidence['owned_vm_drained']=drained
            if drained:
                try:shutil.rmtree(work)
                except OSError as error:evidence['status']='failed';evidence['cleanup_error']=str(error);evidence['retained_work']=str(work)
            else:evidence['status']='failed';evidence['retained_work']=str(work)
            (args.output/'evidence.json').write_text(json.dumps(evidence,indent=2)+'\n')
        finally:
            for sig,handler in previous.items():signal.signal(sig,handler)
    if evidence['status']!='passed':raise RuntimeError('VM lifecycle or owned cleanup incomplete')
    return 0

if __name__=='__main__':sys.exit(main())
