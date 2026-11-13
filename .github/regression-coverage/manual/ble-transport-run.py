#!/usr/bin/env python3
"""Explicit physical probe supervisor; never invoked by plan generation."""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import signal
import subprocess
import time


def verify(record):
    path=Path(record['path'])
    digest=hashlib.sha256()
    with path.open('rb') as source:
        for chunk in iter(lambda:source.read(1024*1024),b''):digest.update(chunk)
    if digest.hexdigest()!=record['sha256'] or path.stat().st_size!=record['size_bytes']:
        raise ValueError('changed physical plan artifact: '+str(path))


def windows_probe(command,env,permit,trial,job_factory,spawn=None,check=lambda:None,timeout=100):
    spawn=spawn or subprocess.Popen
    job=job_factory();child=None;assigned=False
    try:
        check()
        child=spawn(command,env=env)
        job.assign(child);assigned=True
        check()
        # Probe's pre-radio barrier requires this exact fresh nonce file.
        with Path(permit).open('x') as out:out.write(trial+'\n')
        deadline=time.monotonic()+timeout
        while True:
            check()
            remaining=deadline-time.monotonic()
            if remaining<=0:raise subprocess.TimeoutExpired(command,timeout)
            try:return child.wait(timeout=min(.1,remaining))
            except subprocess.TimeoutExpired:pass
    finally:
        # Signal handlers record cancellation without raising through cleanup.
        # Keep the Job handle until the bounded drain finishes. Kill-on-close
        # remains a final containment measure if drain cannot be established.
        try:
            if child is not None and not assigned:child.kill()
            job.drain()
            if child is not None:child.wait(timeout=5)
        finally:job.close()


def completed(status,plan):
    if status != 0:return status
    if Path(plan['SuccessFile']).read_text()!=plan['Trial']+'\n':
        raise ValueError('missing exact completed physical probe nonce')
    return 0


def load_helper(name,path):
    spec=importlib.util.spec_from_file_location(name,path)
    helper=importlib.util.module_from_spec(spec);spec.loader.exec_module(helper)
    return helper


def run(evidence_path):
    evidence=json.loads(Path(evidence_path).read_text())
    for record in [*evidence['records'].values(),*evidence['sources'],*evidence['generated'],evidence['inventory_record']]:verify(record)
    plan=json.loads(Path(evidence['probe_plan']).read_text())
    for name in ('RestoreFile','PermitFile','SuccessFile'):
        if Path(plan[name]).exists():raise ValueError('fresh physical invocation required; acknowledgment already exists')
    env={k:v for k,v in os.environ.items() if not k.startswith('WENDY_BLE_')}
    env['WENDY_BLE_PHYSICAL_PLAN']=evidence['probe_plan']
    command=[evidence['records']['probe_binary']['path'],'-test.run','^'+evidence['test']+'$','-test.v','-test.count=1','-test.timeout=240s']
    if plan['Platform']!='windows':
        # Run in this process so TERM/INT reaches the guardian's owned cleanup,
        # rather than terminating a wrapper and leaving its supervisor alive.
        helper=load_helper('physical_ble_guardian',evidence['guardian'])
        status=helper.run(command,250,env=env)
        return completed(status,plan)
    helper=load_helper('physical_ble_owned_job',evidence['job_helper'])
    previous={}
    stopped=0
    def stop(signum,frame):
        nonlocal stopped
        stopped=signum
    def check():
        if stopped:raise KeyboardInterrupt('physical probe interrupted')
    try:
        for signum in (signal.SIGINT,signal.SIGTERM):previous[signum]=signal.signal(signum,stop)
        status=windows_probe(command,env,plan['PermitFile'],plan['Trial'],helper.Job,check=check)
        check()
        return completed(status,plan)
    finally:
        for signum,handler in previous.items():signal.signal(signum,handler)


if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('evidence',type=Path)
    args=parser.parse_args();raise SystemExit(run(args.evidence))
