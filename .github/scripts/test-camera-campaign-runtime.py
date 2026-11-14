#!/usr/bin/env python3
"""Run a real local HF camera campaign with generated scenes and regression controls."""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import signal
import sys
import time

ROOT=Path(__file__).resolve().parents[2]
spec=importlib.util.spec_from_file_location('campaign_owned',ROOT/'.github/scripts/run-owned-media-test.py')
OWNED=importlib.util.module_from_spec(spec);spec.loader.exec_module(OWNED)


def accepted(name,code,log):
    if '--- SKIP:' in log:return False
    if name=='normal':
        return code==0 and 'CAMPAIGN_RUNTIME_VERIFIED ' in log and 'CAMPAIGN_RECONNECT_VERIFIED ' in log and log.count('CAMPAIGN_RUNTIME_EPISODE boot=')==2
    boundary={'lost-presence':'camera reconnection lost presence/cooldown and duplicated episode','lost-input-ledger':'model outputs/inputs missing from real episode'}.get(name)
    return code==1 and boundary is not None and boundary in log and '--- FAIL: TestYAMLCampaignManagedModelRecordsAndRestores ' in log


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output',type=Path,required=True)
    args=parser.parse_args()
    output=args.output.resolve();output.mkdir(parents=True,exist_ok=False,mode=0o700)
    runtime=output/'runtime';runtime.mkdir(mode=0o700)
    (runtime/'home').mkdir(mode=0o700)
    env={**os.environ,'UV_CACHE_DIR':str(runtime/'cache'),'UV_PYTHON_INSTALL_DIR':str(runtime/'python'),'HF_HOME':str(runtime/'models'),'HOME':str(runtime/'home'),'WENDY_CAMPAIGN_RUNTIME_FIXTURE':str(output/'fixture')}
    evidence={'status':'failed','entry':'1890','physical_execution':'not_run','published_model_accuracy':'not_run','scope':'actual YAML/Agent recorder/managed HF worker using generated local DETR and H264 scenes','cases':[],'sources':{},'durations_seconds':{}}
    start=time.monotonic();deadline=start+600;previous={}
    def stop(signum,frame):raise InterruptedError('campaign fixture interrupted by signal '+str(signum))
    def run(name,argv,cap):
        remaining=deadline-time.monotonic()
        if remaining<=0:raise TimeoutError('campaign fixture ten-minute deadline exceeded')
        began=time.monotonic()
        with (output/(name+'.log')).open('w') as log:code=OWNED.run(argv,min(cap,remaining),cwd=ROOT,env=env,stdout=log)
        evidence['durations_seconds'][name]=round(time.monotonic()-began,3)
        return code
    try:
        previous={sig:signal.signal(sig,stop) for sig in (signal.SIGINT,signal.SIGTERM)}
        if shutil.disk_usage(output).free<3*1024**3:raise ValueError('require 3GiB free for the private locked CPU runtime')
        for name in ('go/internal/agent/services/data_campaign_runtime_test.go','go/internal/agent/services/data_inference.go','go/internal/agent/inference/runtime.go','go/internal/agent/inference/worker.py','go/internal/agent/inference/accelerator.py','go/internal/agent/inference/uv.lock','.github/scripts/fixtures/camera-campaign/prepare.py'):
            evidence['sources'][name]=hashlib.sha256((ROOT/name).read_bytes()).hexdigest()
        code=run('prepare',['uv','run','--project',str(ROOT/'go/internal/agent/inference'),'--frozen','--no-dev','--no-build','--managed-python','--python','3.12','python',str(ROOT/'.github/scripts/fixtures/camera-campaign/prepare.py'),'--runtime-root',str(runtime),'--output',str(output/'fixture')],180)
        if code:raise RuntimeError('locked model/scene preparation failed: '+str(code))
        fixture=json.loads((output/'fixture/fixture.json').read_text())
        evidence['fixture']=fixture
        source=ROOT/'go/internal/agent/services/data_inference.go';original=source.read_text()
        before='// Keep presence and cooldown across temporary camera loss.'
        if original.count(before)!=1:raise ValueError('presence control site changed')
        lost_presence=original.replace(before,'delete(presence, id) // historical 6893e5d63 parent behavior')
        before='j.owner.service.manager.RecordModelInput(data.ModelInput{'
        if original.count(before)!=1:raise ValueError('model input control site changed')
        lost_ledger=original.replace(before,'func(data.ModelInput) error { return nil }(data.ModelInput{')
        for name,modified in [('normal',None),('lost-presence',lost_presence),('lost-input-ledger',lost_ledger)]:
            for path,digest in evidence['sources'].items():
                if hashlib.sha256((ROOT/path).read_bytes()).hexdigest()!=digest:raise ValueError('fixture source changed: '+path)
            binary=output/(name+'.test')
            command=['go','test','-race','-tags','regression_campaign_runtime','-c','-o',str(binary)]
            if modified:
                mutant=output/(name+'.go');mutant.write_text(modified)
                overlay=output/(name+'.json');overlay.write_text(json.dumps({'Replace':{str(source):str(mutant)}}));command+=['-overlay',str(overlay)]
            command+=['./go/internal/agent/services']
            code=run('compile-'+name,command,180)
            if code:raise RuntimeError(name+' compile failed: '+str(code))
            code=run(name,[str(binary),'-test.run','^TestYAMLCampaignManagedModelRecordsAndRestores$','-test.v','-test.count=1','-test.timeout=0'],150)
            log=(output/(name+'.log')).read_text()
            if not accepted(name,code,log):raise RuntimeError(name+' failed its specific runtime acceptance, exit='+str(code))
            evidence['cases'].append({'name':name,'exit':code,'status':'passed','binary_sha256':hashlib.sha256(binary.read_bytes()).hexdigest()})
            binary.unlink()
        evidence['status']='passed'
    except BaseException as error:evidence['error']=str(error)
    finally:
        try:
            # Retain the owned runtime on failure. It contains only generated
            # local model/cache bytes; never prune a shared Python or model cache.
            evidence['elapsed_seconds']=round(time.monotonic()-start,3)
            (output/'evidence.json').write_text(json.dumps(evidence,indent=2)+'\n')
        finally:
            for sig,handler in previous.items():signal.signal(sig,handler)
    print(json.dumps(evidence,indent=2))
    return 0 if evidence['status']=='passed' else 1


if __name__=='__main__':sys.exit(main())
