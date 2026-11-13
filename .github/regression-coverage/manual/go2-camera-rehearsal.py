#!/usr/bin/env python3
"""Generate an exact-target physical Go2 camera plan without contacting devices."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import uuid

ROOT=Path(__file__).resolve().parents[3]

def artifact(value):
    p=Path(value).resolve(strict=True)
    if not p.is_file() or p.stat().st_size==0:raise ValueError('need nonempty provenance file')
    digest=hashlib.sha256()
    with p.open('rb') as stream:
        for chunk in iter(lambda:stream.read(1024*1024),b''):digest.update(chunk)
    return {'path':str(p),'sha256':digest.hexdigest()}

def line(value):
    if not isinstance(value,str) or not value.strip() or any(ord(c)<32 for c in value):raise ValueError('need nonempty single-line identity')
    return value

def generate(inventory,output):
    raw=json.loads(Path(inventory).read_text());cli=artifact(raw['cli_binary']);agent=artifact(raw['agent_binary'])
    if not os.access(cli['path'],os.X_OK):raise ValueError('CLI must be executable')
    target,robot=raw['target'],raw['robot']
    for section,keys in [(target,['selector','serial','owner','board','kernel','camera_stable_id']),
                         (robot,['serial','model','firmware','firmware_record','wired_interface'])]:
        for key in keys:line(section[key])
    if target['selector'].startswith('-') or not target['camera_stable_id'].startswith('ros2:'):raise ValueError('need exact selector and observed ROS camera identity')
    if robot['model']!='Unitree Go2' or robot['topic']!='/frontvideostream' or robot['resolution']!=360:raise ValueError('plan targets the original Go2 front360p firmware case')
    if not re.fullmatch(r'[0-9a-f]{40}',raw['source_commit']):raise ValueError('pin full source commit')
    if not re.fullmatch(r'[^\s]+@sha256:[0-9a-f]{64}',raw['consumer_image']):raise ValueError('pin the reviewed GStreamer consumer image digest')
    records={key:artifact(raw['records'][key]) for key in ['device_info','installed_agent','camera_list','kernel_module','ros_interfaces','baseline_apps','recovery']}
    firmware=artifact(robot['firmware_record'])
    cameras=json.loads(Path(records['camera_list']['path']).read_text())
    matching=[c for c in cameras if c.get('stable_id')==target['camera_stable_id']]
    if len(matching)!=1 or not 128<=matching[0].get('id',0)<=199:raise ValueError('exact ROS camera must appear once in observed list')
    target=dict(target,node='/dev/video'+str(matching[0]['id']))
    out=Path(output).resolve();out.mkdir(mode=0o700,parents=True,exist_ok=False)
    nonce='go2-camera-'+uuid.uuid4().hex[:12]
    base=[cli['path'],'--device',target['selector']]
    guardian=ROOT/'.github/scripts/run-owned-media-test.py'
    capture='#!/bin/sh\nset -eu\numask 077\ncd '+shlex.quote(str(out))+'\nset -C\n'
    capture+=shlex.join(['python3',str(guardian),'--timeout','35','--',*base,'device','camera','view','--stable-id',target['camera_stable_id'],'--stdout','--non-interactive'])+' > physical.h264 2> physical.stderr\n'
    (out/'capture-cli.sh').write_text(capture)
    files=['.github/regression-coverage/manual/go2-record-cdr.py',
           'go/internal/agent/ros2camera/frame.go','go/internal/agent/ros2camera/writer_linux.go',
           'go/internal/agent/services/video_service.go','go/internal/shared/appconfig/annotation.go',
           '.github/regression-coverage/manual/go2-camera-rehearsal.py',
           '.github/regression-coverage/manual/go2-camera-rehearsal.txt',
           '.github/regression-coverage/manual/go2-container-consumer.py']
    plan={'schema_version':1,'nonce':nonce,'target':target,'robot':robot,'source_commit':raw['source_commit'],
          'cli':cli,'agent':agent,'firmware_record':firmware,'records':records,
          'consumer_image':raw['consumer_image'],'source_sha256':{f:artifact(ROOT/f)['sha256'] for f in files},
          'cases':{k:'not_run' for k in ['physical_identity','raw_cdr_recording','native_cli_decode','camera_container_delivery','recorded_service_replay','cleanup']}}
    (out/'plan.json').write_text(json.dumps(plan,indent=2)+'\n')
    (out/'go2-record-cdr.py').write_bytes((ROOT/files[0]).read_bytes())
    (out/'go2-recorded-replay.txt').write_bytes((ROOT/'.github/regression-coverage/manual/go2-recorded-replay.txt').read_bytes())
    app=out/'consumer';app.mkdir(mode=0o700)
    app_id='sh.wendy.tests.'+nonce.replace('-','_')
    (app/'wendy.json').write_text(json.dumps({'appId':app_id,'version':'1','entitlements':[{'type':'camera'}],
        'env':{'GO2_CAMERA_NODE':target['node'],'GO2_CAMERA_NONCE':nonce}},indent=2)+'\n')
    (app/'Dockerfile').write_text('FROM '+raw['consumer_image']+'\nCOPY go2-container-consumer.py /go2-container-consumer.py\nENTRYPOINT ["python3", "/go2-container-consumer.py"]\n')
    (app/'go2-container-consumer.py').write_bytes((ROOT/'.github/regression-coverage/manual/go2-container-consumer.py').read_bytes())
    plan['consumer_app_id']=app_id
    plan['generated_sha256']={str(f.relative_to(out)):artifact(f)['sha256'] for f in app.iterdir()}
    (out/'plan.json').write_text(json.dumps(plan,indent=2)+'\n')
    commands={'device_info':base+['--json','device','info'], 'cameras':base+['--json','device','camera','list'],
              'apps_before_and_after':base+['--json','device','apps','list'],
              'deploy_owned_consumer':base+['--json','run','--prefix',str(app),'--detach','--yes','--no-restart'],
              'owned_consumer_logs':base+['--json','device','logs','--app',app_id,'--tail','100','--no-follow'],
              'remove_owned_consumer':base+['device','apps','remove',app_id,'--force']}
    (out/'commands.json').write_text(json.dumps(commands,indent=2)+'\n')
    guide=Path(__file__).with_suffix('.txt').read_text().replace('@PLAN@',shlex.quote(str(out))).replace('@NODE@',target['node']).replace('@NONCE@',nonce)
    guide+='\nExact read-only commands:\n'+'\n'.join(name+': '+shlex.join(cmd) for name,cmd in commands.items())+'\n'
    (out/'procedure.txt').write_text(guide)
    plan['generated_sha256'].update({name:artifact(out/name)['sha256'] for name in
        ['capture-cli.sh','go2-record-cdr.py','go2-recorded-replay.txt','commands.json','procedure.txt']})
    (out/'plan.json').write_text(json.dumps(plan,indent=2)+'\n')
    return plan

def main():
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('inventory');p.add_argument('output');a=p.parse_args();generate(a.inventory,a.output)
if __name__=='__main__':main()
