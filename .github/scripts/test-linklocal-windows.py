#!/usr/bin/env python3
"""Hosted Windows /16 routing through two owned Wintun adapters, never host NICs."""
import argparse
import ctypes
from ctypes import wintypes
import hashlib
import ipaddress
import json
import os
import platform
import signal
from pathlib import Path
import subprocess
import sys
import time
import urllib.request
import uuid
import zipfile

ROOT = Path(__file__).resolve().parents[2]
NETWORK = Path(__file__).with_name('linklocal-windows-network.ps1')
URL = 'https://www.wintun.net/builds/wintun-0.14.1.zip'
SHA256 = '07c256185d6ee3652e09fa55c0b673e2624b565e02c4b9091c79ca7d2f24ef51'
TEST = 'TestWindowsLinkLocalCompetingInterfaces'

def validate_host(snapshot):
    if snapshot['wintun_devices'] or snapshot['wintun_drivers']:
        raise ValueError('Refusing preexisting Wintun devices/drivers, including orphan cleanup targets')
    if any(ipaddress.ip_address(a['IPAddress']).is_link_local for a in snapshot['addresses']):
        raise ValueError('Refusing existing link-local addresses')
    metadata = [r for r in snapshot['routes'] if r['DestinationPrefix'] == '169.254.169.254/32']
    if not metadata:
        raise ValueError('Missing explicit existing metadata /32 route; never synthesize one')
    scope = ipaddress.ip_network('169.254.0.0/16')
    targets = {ipaddress.ip_address(value) for value in ('169.254.100.1','169.254.100.2','169.254.200.1','169.254.200.2')}
    for row in snapshot['routes']:
        network = ipaddress.ip_network(row['DestinationPrefix'])
        if network.prefixlen == 32 and network.network_address in targets:
            raise ValueError('A fixture address already has a foreign host route')
        if network.prefixlen and network.overlaps(scope) and network.prefixlen < 32:
            raise ValueError('Existing non-/32 link-local route could be affected')
    indices = {a['InterfaceIndex'] for a in snapshot['adapters']}
    if any(r['InterfaceIndex'] not in indices for r in metadata):
        raise ValueError('Metadata route does not belong to an inventoried adapter')

def powershell(action, plan=None, *, timeout=20, dll=None):
    args = ['pwsh', '-NoProfile', '-NonInteractive', '-File', str(NETWORK), '-Action', action]
    if plan:
        args += ['-Plan', str(plan)]
    if dll:
        args += ['-DLL', str(dll)]
    result = subprocess.run(args, capture_output=True, text=True, timeout=timeout)
    if result.returncode:
        raise RuntimeError(f'network {action}: {result.stdout}\n{result.stderr}')
    return result.stdout

class JobDrainError(RuntimeError):
    """No network cleanup may race descendants that have not drained."""

class Accounting(ctypes.Structure):
    _fields_ = [(name, ctypes.c_int64) for name in (
        'TotalUserTime', 'TotalKernelTime', 'ThisPeriodTotalUserTime', 'ThisPeriodTotalKernelTime')] + [
        (name, ctypes.c_uint32) for name in ('TotalPageFaultCount', 'TotalProcesses', 'ActiveProcesses', 'TotalTerminatedProcesses')]

class Job:
    """Retain the job until every owned process has terminated, then close it."""
    def __init__(self):
        class Basic(ctypes.Structure):
            _fields_ = [('PerProcessUserTimeLimit', ctypes.c_int64), ('PerJobUserTimeLimit', ctypes.c_int64),
                        ('LimitFlags', wintypes.DWORD), ('MinimumWorkingSetSize', ctypes.c_size_t),
                        ('MaximumWorkingSetSize', ctypes.c_size_t), ('ActiveProcessLimit', wintypes.DWORD),
                        ('Affinity', ctypes.c_size_t), ('PriorityClass', wintypes.DWORD), ('SchedulingClass', wintypes.DWORD)]
        class IO(ctypes.Structure):
            _fields_ = [(name, ctypes.c_uint64) for name in ('ReadOperationCount','WriteOperationCount','OtherOperationCount','ReadTransferCount','WriteTransferCount','OtherTransferCount')]
        class Extended(ctypes.Structure):
            _fields_ = [('BasicLimitInformation', Basic), ('IoInfo', IO), ('ProcessMemoryLimit', ctypes.c_size_t),
                        ('JobMemoryLimit', ctypes.c_size_t), ('PeakProcessMemoryUsed', ctypes.c_size_t), ('PeakJobMemoryUsed', ctypes.c_size_t)]
        self.api = ctypes.WinDLL('kernel32', use_last_error=True)
        self.api.CreateJobObjectW.argtypes = [ctypes.c_void_p, wintypes.LPCWSTR]
        self.api.CreateJobObjectW.restype = wintypes.HANDLE
        self.api.SetInformationJobObject.argtypes = [wintypes.HANDLE, ctypes.c_int, ctypes.c_void_p, wintypes.DWORD]
        self.api.SetInformationJobObject.restype = wintypes.BOOL
        self.api.AssignProcessToJobObject.argtypes = [wintypes.HANDLE, wintypes.HANDLE]
        self.api.AssignProcessToJobObject.restype = wintypes.BOOL
        self.api.TerminateJobObject.argtypes = [wintypes.HANDLE, wintypes.UINT]
        self.api.TerminateJobObject.restype = wintypes.BOOL
        self.api.QueryInformationJobObject.argtypes = [wintypes.HANDLE, ctypes.c_int, ctypes.c_void_p, wintypes.DWORD, ctypes.c_void_p]
        self.api.QueryInformationJobObject.restype = wintypes.BOOL
        self.api.CloseHandle.argtypes = [wintypes.HANDLE]
        self.api.CloseHandle.restype = wintypes.BOOL
        self.handle = self.api.CreateJobObjectW(None, None)
        if not self.handle:
            raise ctypes.WinError(ctypes.get_last_error())
        limits = Extended()
        limits.BasicLimitInformation.LimitFlags = 0x2000  # JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
        if not self.api.SetInformationJobObject(self.handle, 9, ctypes.byref(limits), ctypes.sizeof(limits)):
            self.close()
            raise ctypes.WinError(ctypes.get_last_error())
    def assign(self, process):
        if not self.api.AssignProcessToJobObject(self.handle, wintypes.HANDLE(int(process._handle))):
            raise ctypes.WinError(ctypes.get_last_error())
    def active(self):
        accounting = Accounting()
        if not self.api.QueryInformationJobObject(self.handle, 1, ctypes.byref(accounting), ctypes.sizeof(accounting), None):
            raise JobDrainError('Cannot observe owned Windows job process count')
        return accounting.ActiveProcesses
    def drain(self, timeout=8):
        if not self.active():
            return
        if not self.api.TerminateJobObject(self.handle, 125):
            raise JobDrainError('Cannot terminate owned Windows job')
        until = time.monotonic() + timeout
        while self.active():
            if time.monotonic() >= until:
                raise JobDrainError('Owned Windows job descendants did not drain; network cleanup refused')
            time.sleep(0.05)
    def close(self):
        if self.handle:
            self.api.CloseHandle(self.handle)
            self.handle = None

def execute(binary, plan, output):
    # The Go process waits for admission before loading the driver or starting
    # children. Job assignment therefore precedes any adapter mutation.
    job = Job()
    process = None
    assigned = False
    try:
        with (output / 'runtime.log').open('w') as log:
            process = subprocess.Popen([str(binary), '-test.run=^'+TEST+'$', '-test.v', '-test.count=1', '-test.timeout=150s'],
                cwd=ROOT, env=dict(os.environ, WENDY_WINDOWS_LINKLOCAL_PLAN=str(plan)), stdout=log, stderr=subprocess.STDOUT)
            job.assign(process)
            assigned = True
            details = json.loads(plan.read_text())
            Path(details['permit']).write_text(details['nonce'])
            return process.wait(timeout=170)
    finally:
        # Ignore repeated interruption only while draining. Terminating a job is
        # asynchronous; waiting for the leader alone does not drain its children.
        previous = {sig: signal.signal(sig, signal.SIG_IGN) for sig in (signal.SIGINT, signal.SIGTERM)}
        try:
            if process is not None and not assigned:
                # No permit was written, so this child cannot have started any
                # network mutation or descendants. Popen retains its handle.
                process.kill()
                process.wait(timeout=5)
            job.drain()
            if process is not None:
                process.wait(timeout=5)
        except (subprocess.TimeoutExpired, OSError) as error:
            raise JobDrainError('Owned Windows processes did not finish; network cleanup refused') from error
        finally:
            job.close()
            for sig, handler in previous.items():
                signal.signal(sig, handler)

def execute_and_cleanup(binary, plan, output):
    drained = True
    old_term = signal.signal(signal.SIGTERM, lambda *_: (_ for _ in ()).throw(InterruptedError('fixture interrupted')))
    try:
        return execute(binary, plan, output)
    except JobDrainError:
        drained = False
        raise
    finally:
        old_int = signal.signal(signal.SIGINT, signal.SIG_IGN)
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
        try:
            if drained:
                (output/'cleanup.log').write_text(powershell('Cleanup', plan, timeout=30))
            else:
                (output/'cleanup.log').write_text('REFUSED: job descendants not proven drained; disposable runner destruction required.\n')
        finally:
            signal.signal(signal.SIGINT, old_int)
            signal.signal(signal.SIGTERM, old_term)

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('output', type=Path)
    args = parser.parse_args()
    if os.name != 'nt' or os.environ.get('GITHUB_ACTIONS') != 'true' or os.environ.get('RUNNER_ENVIRONMENT') != 'github-hosted':
        parser.error('requires the disposable hosted Windows runner')
    if platform.machine().lower() not in ('amd64','x86_64'):
        parser.error('pinned fixture DLL targets Windows amd64')
    if not ctypes.windll.shell32.IsUserAnAdmin():
        parser.error('the disposable runner must already be elevated')
    args.output.mkdir(parents=True, exist_ok=False)
    output = args.output.resolve()
    baseline = json.loads(powershell('Snapshot'))
    validate_host(baseline)
    (output/'baseline.json').write_text(json.dumps(baseline, indent=2))
    archive = output/'wintun.zip'
    # Only this fixed official HTTPS release is consumed; SHA pin is verified
    # before extraction or loading. No driver or binary enters Git.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open(URL, timeout=20) as response, archive.open('xb') as target:
        data = response.read(4*1024*1024+1)
        if len(data)>4*1024*1024:
            raise ValueError('oversized Wintun release')
        target.write(data)
    if hashlib.sha256(data).hexdigest()!=SHA256:
        raise ValueError('Wintun release digest mismatch')
    dll = output/'wintun.dll'
    with zipfile.ZipFile(archive) as bundle:
        dll.write_bytes(bundle.read('wintun/bin/amd64/wintun.dll'))
    (output/'signature.json').write_text(powershell('VerifySignature', dll=dll))
    original = ROOT/'go/internal/cli/linklocal/bind_windows.go'
    listener_source = ROOT/'go/internal/cli/linklocal/linklocal.go'
    variants = {'normal': None,
        'unbound-dials': (original, 'ipUnicastIF, int(bits.ReverseBytes32(uint32(ifi.Index))))', 'ipUnicastIF, int(bits.ReverseBytes32(uint32(0))))'),
        'unbound-listeners': (listener_source, 'lc := net.ListenConfig{Control: pinTo(l)}', 'lc := net.ListenConfig{}'),
        'wrong-byte-order': (original, 'bits.ReverseBytes32(uint32(ifi.Index))', 'bits.ReverseBytes32(bits.ReverseBytes32(uint32(ifi.Index)))')}
    for name, mutation in variants.items():
        current = output/name
        current.mkdir()
        binary=current/'linklocal.test.exe'
        command=['go','test','-tags=regression_windows_linklocal','-c','-o',str(binary)]
        if mutation:
            source,before,after=mutation
            text=source.read_text()
            if text.count(before)!=1:
                raise ValueError('Windows binding mutation site changed: '+name)
            replacement=current/source.name
            replacement.write_text(text.replace(before,after))
            overlay=current/'overlay.json'
            overlay.write_text(json.dumps({'Replace':{str(source):str(replacement)}}))
            command+=['-overlay',str(overlay)]
        subprocess.run(command+['./go/internal/cli/linklocal'],cwd=ROOT,check=True,timeout=180)
        nonce=uuid.uuid4().hex
        plan=current/'plan.json'
        plan.write_text(json.dumps({'nonce':nonce,'dll':str(dll),'network_script':str(NETWORK),
            'permit':str(current/'admit'),'binary':str(binary),'firewall_name':'wendy-ll-'+nonce,'baseline':baseline,
            'adapters':[{'role':role,'name':'wendy-ll-'+nonce[:12]+'-'+role,'guid':'{'+str(uuid.uuid4())+'}',
                         'host':f'169.254.{subnet}.1','peer':f'169.254.{subnet}.2','metric':metric}
                        for role,subnet,metric in [('a',100,5000),('b',200,6000)]]},indent=2))
        code=execute_and_cleanup(binary,plan,current)
        log=(current/'runtime.log').read_text()
        print(log,flush=True)
        if '--- SKIP:' in log:
            raise ValueError('Windows interface test skipped')
        if name=='normal':
            if code!=0 or '--- PASS: '+TEST+' ' not in log or 'WINDOWS_LINKLOCAL_FULL_16_VERIFIED' not in log:
                raise ValueError('Windows full /16 interface proof failed')
        else:
            marker='wrong-route Windows listener reply' if name=='unbound-listeners' else 'wrong-route Windows device b'
            if code==0 or '--- FAIL: '+TEST+' ' not in log or marker not in log:
                raise ValueError('Windows control failed at an unrelated boundary: '+name)
    (output/'result.json').write_text(json.dumps({'passed':True,'topology':'two owned Windows Wintun /16 interfaces','physical_usb':False},indent=2))

if __name__=='__main__':
    main()
