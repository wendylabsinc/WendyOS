#!/usr/bin/env python3
"""Explicit reserved Windows serial command, owned by the existing retained Job.

A zero CLI exit is recorded only as command_completed, never physical acceptance.
"""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import time
import uuid


def load(name,path):
    spec=importlib.util.spec_from_file_location(name,path)
    module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module);return module


def verify(record):
    path=Path(record['path']);digest=hashlib.sha256()
    with path.open('rb') as f:
        for chunk in iter(lambda:f.read(1024*1024),b''):digest.update(chunk)
    if digest.hexdigest()!=record['sha256'] or path.stat().st_size!=record['size_bytes']:raise ValueError('changed serial artifact: '+str(path))


def admit(snapshot,inventory,transport):
    host=inventory['host'];board=inventory['board']
    for key in ('machine_uuid','sid','os_build'):
        if snapshot[key]!=host[key]:raise ValueError('Windows host/account changed: '+key)
    if snapshot['elevated'] is not False:raise ValueError('run as reserved ordinary account; driver installation is separate')
    ports=snapshot['ports']
    supported=[]
    for port in ports:
        identity=port['instance_id'].upper()
        if any(pair in identity for pair in ('VID_10C4&PID_EA60','VID_303A&PID_1001','VID_303A&PID_0020','VID_303A&PID_4002')):supported.append(port)
    expected=[] if transport=='absent' else [board['native' if transport=='replug' else transport]]
    if len(supported)!=len(expected):raise ValueError('unexpected supported serial candidate count')
    if not expected:return
    wanted=expected[0];actual=supported[0]
    expected_port=board['native_replug_path'] if transport=='replug' else wanted['path']
    expected_location=board['native_replug_location'] if transport=='replug' else wanted['location']
    if actual['port']!=expected_port or actual['instance_id'].casefold()!=wanted['instance_id'].casefold():raise ValueError('COM or PnP identity changed')
    if expected_location not in actual['locations']:raise ValueError('USB location differs')
    if actual['status']!='OK' or actual['error_code']!=0 or actual['driver_signed'] is not True:raise ValueError('unhealthy or unsigned serial driver')
    for key in ('driver_version','driver_provider','driver_inf'):
        if actual[key]!=wanted[key]:raise ValueError('serial driver changed: '+key)


def child(permit,nonce,command_file):
    with Path(str(permit)+'.ready').open('x') as ready:ready.write(nonce+'\n')
    deadline=time.monotonic()+15
    while not Path(permit).exists():
        if time.monotonic()>deadline:raise ValueError('owned Windows Job permit missing')
        time.sleep(.02)
    if Path(permit).read_text()!=nonce+'\n':raise ValueError('owned Windows Job nonce mismatch')
    command=json.loads(Path(command_file).read_text())
    # Only after assignment does the launcher create CLI/PowerShell descendants.
    return subprocess.call(command)


def execute(command,timeout,output,label,env,check,owner,job_factory,stdout=None):
    nonce=uuid.uuid4().hex;permit=output/(label+'.permit')
    command_file=output/(label+'.argv.json')
    with command_file.open('x') as f:json.dump(command,f)
    launcher=[sys.executable,str(Path(__file__).resolve()),'--child',str(permit),nonce,str(command_file)]
    def spawn(argv,env):return subprocess.Popen(argv,env=env,stdout=stdout)
    return owner.windows_probe(launcher,env,permit,nonce,job_factory,spawn=spawn,check=check,timeout=timeout)


def windows_host():return os.name=='nt'


def run(evidence_path,action,output,confirmation):
    if not windows_host():raise ValueError('reserved Windows host required; no local serial action')
    evidence_path=Path(evidence_path).resolve(strict=True);evidence=json.loads(evidence_path.read_text())
    data=evidence['inventory'];board=data['board']
    if data['host']['os']!='windows' or confirmation!=board['serial']:raise ValueError('confirm exact reserved physical board serial')
    for record in [evidence['inventory_record'],*evidence['records'].values(),*evidence['sources'],*evidence['generated'],*evidence['app_sources']]:verify(record)
    verify({'path':evidence['firmware']['cache'],'sha256':evidence['firmware']['sha256'],'size_bytes':evidence['records']['firmware_binary']['size_bytes']})
    if Path(sys.executable).resolve()!=Path(evidence['records']['python_binary']['path']).resolve():raise ValueError('run with the pinned Python interpreter')
    owner=load('serial_owned_job',evidence['windows']['supervisor'])
    if action not in evidence['windows']['actions']:raise ValueError('unknown physical action')
    plan=evidence['windows']['actions'][action]
    output=Path(output).resolve();output.mkdir(parents=True,exist_ok=False)
    env={k:v for k,v in os.environ.items() if not k.upper().startswith('WENDY_') and k.upper() not in ('HOME','USERPROFILE','LOCALAPPDATA','XDG_CACHE_HOME')}
    env.update(evidence['windows']['environment'])
    report={'action':action,'status':'failed','hardware_acceptance':'pending_operator_review','cli':evidence['records']['cli_binary'],'started':time.time()}
    stopped=0;previous={}
    def stop(sig,frame):
        nonlocal stopped
        stopped=sig
    def check():
        if stopped:raise InterruptedError('serial operation interrupted')
    job=load('serial_job_type',evidence['windows']['job_helper'])
    try:
        signals=[signal.SIGINT,signal.SIGTERM]
        if hasattr(signal,'SIGBREAK'):signals.append(signal.SIGBREAK)
        for sig in signals:previous[sig]=signal.signal(sig,stop)
        snapshot=output/'inventory.json'
        with snapshot.open('w',encoding='utf-8') as log:
            status=execute([evidence['records']['powershell_binary']['path'],'-NoProfile','-NonInteractive','-File',evidence['windows']['snapshot']],30,output,'inventory',env,check,owner,job.Job,stdout=log)
        check()
        if status:raise ValueError('Windows inventory failed')
        if snapshot.stat().st_size>1024*1024:raise ValueError('oversized Windows inventory')
        value=json.loads(snapshot.read_text(encoding='utf-8-sig'));admit(value,data,plan['transport'])
        status=execute(plan['argv'],plan['timeout'],output,'cli',env,check,owner,job.Job)
        check()
        for record in (evidence['records']['cli_binary'],evidence['records']['firmware_binary']):owner.verify(record)
        cache=Path(evidence['firmware']['cache'])
        owner.verify({'path':str(cache),'sha256':evidence['firmware']['sha256'],'size_bytes':evidence['records']['firmware_binary']['size_bytes']})
        report.update(status='command_completed',cli_exit=status)
        return status
    except BaseException as error:
        report['error']=type(error).__name__+': '+str(error);raise
    finally:
        try:
            report['finished']=time.time();(output/'result.json').write_text(json.dumps(report,indent=2)+'\n')
        finally:
            for sig,handler in previous.items():signal.signal(sig,handler)


if __name__=='__main__':
    if len(sys.argv)==5 and sys.argv[1]=='--child':raise SystemExit(child(*sys.argv[2:]))
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('evidence',type=Path);p.add_argument('action');p.add_argument('--output',type=Path,required=True);p.add_argument('--confirm-board',required=True);a=p.parse_args();raise SystemExit(run(a.evidence,a.action,a.output,a.confirm_board))
