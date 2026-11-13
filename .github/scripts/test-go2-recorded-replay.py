#!/usr/bin/env python3
"""Prepare an archived Go2 recording offline, then replay only in a confirmed VM.

No VM creation, module installation/loading, host service changes or robot access.
"""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import signal
import stat
import sys
import time
import uuid

ROOT = Path(__file__).resolve().parents[2]
SCRIPTS = ROOT / '.github/scripts'

def module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    value = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(value)
    return value

ARCHIVE = module('recorded_archive', SCRIPTS / 'go2_recording_archive.py')
GUARDIAN = module('recorded_guardian', SCRIPTS / 'run-owned-media-test.py')


def prepare(args):
    manifest = ARCHIVE.validate(args.archive, args.plan, args.provenance, args.confirm_robot_serial)
    output = args.output.resolve()
    output.mkdir(mode=0o700, parents=False, exist_ok=False)
    corpus = output / 'corpus'; corpus.mkdir()
    provenance = output / 'provenance'; provenance.mkdir()
    shutil.copyfile(args.plan, output / 'plan.json')
    for name in ['recording.json', *(row['file'] for row in manifest['samples'])]:
        shutil.copyfile(args.archive / name, corpus / name)
    for name in manifest['provenance_sha256']:
        shutil.copyfile(args.provenance / name, provenance / name)
    # Verify the bytes actually copied, not only the original path observations.
    copied = ARCHIVE.validate(corpus, output / 'plan.json', provenance, args.confirm_robot_serial)
    if copied != manifest:
        raise ValueError('archive changed during preparation')
    (output / 'replay.json').write_text(json.dumps(manifest, indent=2)+'\n')
    print('Prepared exact archived bytes; physical acceptance remains not_run: '+str(output))


def execute(args):
    prepared = args.prepared.resolve(strict=True)
    manifest = ARCHIVE.validate(prepared / 'corpus', prepared / 'plan.json', prepared / 'provenance', args.confirm_robot_serial)
    if ARCHIVE.document(prepared / 'replay.json')[1] != manifest:
        raise ValueError('prepared replay manifest changed')
    if os.geteuid() == 0:
        raise ValueError('run coordinator as the ordinary VM user; only private kernel execution uses sudo')
    for tool in ['sudo','go','python3','unshare','containerd','ctr','runc','busybox','gst-launch-1.0','gst-inspect-1.0']:
        if not shutil.which(tool):raise ValueError('missing prerequisite: '+tool)
    expected_manifest_hash=ARCHIVE.sha((prepared/'replay.json').read_bytes())
    def verify_prepared():
        if ARCHIVE.sha(ARCHIVE.regular(prepared/'replay.json',2*1024*1024))!=expected_manifest_hash:
            raise ValueError('replay manifest changed after admission')
        observed=ARCHIVE.validate(prepared/'corpus',prepared/'plan.json',prepared/'provenance',args.confirm_robot_serial)
        if observed!=manifest:
            raise ValueError('prepared archive/provenance changed after admission')
    output = prepared / ('run-'+uuid.uuid4().hex)
    output.mkdir(mode=0o700)
    env = dict(os.environ, WENDY_GO2_VM_BOOT_ID=args.confirm_vm_boot_id or '',
               WENDY_GO2_VM_PRODUCT_UUID=args.confirm_vm_product_uuid or '', WENDY_GO2_RECORDING=str(prepared / 'corpus'))
    evidence = {'physical_acceptance':'not_run','recording_sha256':manifest['recording_sha256'],
                'plan_sha256':manifest['plan_sha256'],'replay_manifest_sha256':expected_manifest_hash,'source_hashes':{},'replay_passed':False,'cases':{},'cleanup':'not_run'}
    counter=0
    owned_label=None
    namespace_drained=True
    def interrupted(signum, _frame):
        raise InterruptedError(f'interrupted by signal {signum}')
    handlers={sig:signal.signal(sig,interrupted) for sig in (signal.SIGINT,signal.SIGTERM)}
    def command(argv, timeout=120, check=True):
        nonlocal counter
        counter+=1
        log=output/f'command-{counter:02d}.log'
        with log.open('x') as stream:code=GUARDIAN.run(argv,timeout,cwd=ROOT,env=env,stdout=stream)
        if check and code:raise ValueError(f'command failed({code}): {log}')
        return code,log
    admission_env=['env','PATH='+os.environ['PATH']]
    for key in ('GITHUB_ACTIONS','RUNNER_ENVIRONMENT','WENDY_GO2_VM_BOOT_ID','WENDY_GO2_VM_PRODUCT_UUID','WENDY_GO2_RECORDING'):
        admission_env.append(key+'='+env.get(key,''))
    try:
        _,log=command(['sudo','-n',*admission_env,'python3',str(SCRIPTS/'go2_replay_admission.py')])
        evidence['admission']=json.loads(log.read_text())
        if Path('/dev/video128').exists() or Path('/dev/video128').is_symlink():raise ValueError('reserved video128 is already occupied')
        if not stat.S_ISCHR(Path('/dev/v4l2loopback').lstat().st_mode):raise ValueError('loopback control is not an existing character device')
        if Path('/sys/module/v4l2loopback/version').read_text().strip()!='0.15.4':raise ValueError('already installed loopback0.15.4 is required; no module change is authorized')
        for element in ['h264parse','avdec_h264','videoconvert','fdsrc','fdsink']:command(['gst-inspect-1.0',element])
        _, clean_log=command(['git','status','--porcelain','--untracked-files=all'])
        if clean_log.read_text().strip():raise ValueError('replay requires a clean reviewed checkout; keep archives/output outside Git')
        _, source_log=command(['git','rev-parse','HEAD'])
        evidence['replay_source_commit']=source_log.read_text().strip()
        for name,package in [('capture','services'),('normal','containerd')]:
            command(['env','CGO_ENABLED=0','go','test','-tags','regression_go2_h264','-c','./go/internal/agent/'+package,'-o',str(output/(name+'.test'))],600)
        mutations=[
            ('old-cdr','go/internal/agent/ros2camera/frame.go','return decodeGo2Frame(payload)', 'data, width, height, err := decodeGo2FrontVideo(payload)\n\t\treturn Frame{Data: data, Width: width, Height: height, Codec: CodecMJPEG}, err','Go2 resolution H264 decode failed:'),
            ('wrong-format','go/internal/agent/ros2camera/writer_linux.go','return v4l2PixFmtH264, nil','return v4l2PixFmtMJPEG, nil','Go2 writer kernel format is not H264 640x360'),
            ('empty-camera-label','go/internal/shared/appconfig/annotation.go','value = "enabled=true"','value = ""','Go2 camera entitlement did not retain active consumer')]
        for name,path,before,after,_ in mutations:
            source=(ROOT/path).resolve();raw=source.read_text()
            if raw.count(before)!=1:raise ValueError('mutation site changed: '+name)
            replacement=output/(name+'.go');replacement.write_text(raw.replace(before,after))
            overlay=output/(name+'.json');overlay.write_text(json.dumps({'Replace':{str(source):str(replacement)}}))
            command(['env','CGO_ENABLED=0','go','test','-tags','regression_go2_h264','-overlay',str(overlay),'-c','./go/internal/agent/containerd','-o',str(output/(name+'.test'))],600)
        evidence['binary_sha256']={path.name:ARCHIVE.sha(path.read_bytes()) for path in output.glob('*.test')}
        source_files=['.github/scripts/test-go2-recorded-replay.py','.github/scripts/go2_recording_archive.py','.github/scripts/go2_replay_admission.py','.github/scripts/run-go2-recorded-kernel.sh','.github/scripts/run-owned-media-test.py','.github/scripts/cleanup-owned-go2-camera.py','go/internal/testfixture/go2video/admission.go','go/internal/testfixture/go2video/recorded.go','go/internal/agent/containerd/go2_h264_loopback_linux_test.go','go/internal/agent/services/go2_h264_capture_linux_test.go',*(row[1] for row in mutations)]
        evidence['source_hashes']={path:ARCHIVE.sha((ROOT/path).read_bytes()) for path in source_files}
        host_pid_namespace=os.readlink('/proc/self/ns/pid')
        for name,expected in [('normal',None),*((row[0],row[4]) for row in mutations)]:
            label='ci-go2-'+uuid.uuid4().hex[:12]
            if Path('/dev/video128').exists() or Path('/dev/video128').is_symlink():raise ValueError('reserved camera was occupied before replay')
            verify_prepared()
            _, clean_log=command(['git','status','--porcelain','--untracked-files=all'])
            _, head_log=command(['git','rev-parse','HEAD'])
            if clean_log.read_text().strip() or head_log.read_text().strip()!=evidence['replay_source_commit']:
                raise ValueError('reviewed checkout changed during replay')
            owned_label=label
            namespace_drained=False
            code,log=command(['sudo','-n',*admission_env,'WENDY_GO2_CAMERA_LABEL='+label,'python3',str(SCRIPTS/'run-owned-media-test.py'),'--timeout','180','--','unshare','--mount','--pid','--fork','--kill-child=KILL','--mount-proc','bash',str(SCRIPTS/'run-go2-recorded-kernel.sh'),str(output/(name+'.test')),str(output/'capture.test'),host_pid_namespace],200,False)
            if code>=124:
                evidence['cleanup']='uncertain namespace drain; retained owned node '+label
                raise ValueError(evidence['cleanup'])
            namespace_drained=True
            command(['sudo','-n',*admission_env,'python3',str(SCRIPTS/'cleanup-owned-go2-camera.py'),'--label',label])
            owned_label=None
            evidence['cleanup']='owned namespace drained and exact-label node removed'
            verify_prepared()
            text=log.read_text(errors='replace')
            if '--- SKIP:' in text:raise ValueError('recorded replay skipped')
            if expected is None:
                reported=re.findall(r'GO2_RECORDED_CAPTURE_VERIFIED phase=[01] frames=[0-9]+ manifest=([a-f0-9]{64})',text)
                if reported!=[expected_manifest_hash]*3 or f'GO2_RECORDED_KERNEL_CONTAINER_VERIFIED manifest={expected_manifest_hash};' not in text:
                    raise ValueError('replayed manifest differs from admitted archive')
                if code or '--- PASS: TestGo2H264KernelServiceAndContainerDelivery ' not in text or text.count('GO2_RECORDED_CAPTURE_VERIFIED')!=3 or 'GO2_RECORDED_KERNEL_CONTAINER_VERIFIED' not in text:
                    raise ValueError('recorded kernel/native/OCI comparison did not complete: '+str(log))
            elif code!=1 or expected not in text or '--- FAIL: TestGo2H264KernelServiceAndContainerDelivery ' not in text:
                raise ValueError('recorded regression control failed outside expected boundary: '+str(log))
            evidence['cases'][name]={'passed':True,'log':log.name}
        evidence['replay_passed']=True
    finally:
        for sig in handlers:signal.signal(sig,signal.SIG_IGN)
        try:
            if owned_label is not None:
                evidence['replay_passed']=False
                if namespace_drained:
                    try:
                        command(['sudo','-n',*admission_env,'python3',str(SCRIPTS/'cleanup-owned-go2-camera.py'),'--label',owned_label])
                        owned_label=None
                        evidence['cleanup']='owned namespace drained and exact-label node removed after failure'
                    except BaseException as error:
                        evidence['cleanup']='cleanup failed; retained resources: '+str(error)
                else:
                    evidence['cleanup']='namespace drain uncertain; retained owned node '+owned_label
            (output/'evidence.json').write_text(json.dumps(evidence,indent=2)+'\n')
            if owned_label is None:
                for binary in output.glob('*.test'):binary.unlink()
        finally:
            for sig,handler in handlers.items():signal.signal(sig,handler)
    print('Recorded native/kernel/OCI replay and controls passed; physical acceptance remains operator-reviewed: '+str(output))


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    sub=parser.add_subparsers(dest='command',required=True)
    prep=sub.add_parser('prepare')
    for name in ['archive','plan','provenance','output']:prep.add_argument('--'+name,required=True,type=Path)
    prep.add_argument('--confirm-robot-serial',required=True)
    run=sub.add_parser('run');run.add_argument('--prepared',type=Path,required=True)
    run.add_argument('--confirm-robot-serial',required=True)
    run.add_argument('--confirm-vm-boot-id');run.add_argument('--confirm-vm-product-uuid')
    args=parser.parse_args()
    (prepare if args.command=='prepare' else execute)(args)

if __name__=='__main__':main()
