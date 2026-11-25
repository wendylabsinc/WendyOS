"""Actual three-node CLI history and native callback/restart acceptance.

No executor, simulator or physical motion. The native window is a neutral
historical timeline; app-owned controls are exercised, not OS mouse input.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import time

p=argparse.ArgumentParser(description=__doc__)
for key in ('bin-dir','app','out'):p.add_argument('--'+key,type=Path,required=True)
p.add_argument('--port',type=int,default=19710);a=p.parse_args();repo=Path(__file__).resolve().parents[1]
os.umask(0o077);out=a.out.resolve();out.mkdir(parents=True,mode=0o700,exist_ok=False)
processes=[];logs=[];checks=[];counter=0;site=out/'site';session=out/'session';session.mkdir(mode=0o700)
def write(path,value):
    tmp=path.with_suffix('.new');tmp.write_text(json.dumps(value,indent=2,sort_keys=True));tmp.replace(path)
def wait(path,seconds=25):
    end=time.monotonic()+seconds
    while time.monotonic()<end:
        if path.exists():return json.loads(path.read_text())
        time.sleep(.04)
    raise TimeoutError(str(path))
def start(args,name,env=None):
    log=(out/(name+'.log')).open('a');logs.append(log);proc=subprocess.Popen([str(x) for x in args],cwd=repo,stdout=log,stderr=log,env=env);processes.append(proc);return proc
def stop(proc,kill=False):
    if proc.poll() is None:
        proc.kill() if kill else proc.terminate()
        try:proc.wait(5)
        except subprocess.TimeoutExpired:proc.kill();proc.wait()
def qa(action,target=None):
    global counter
    counter+=1;key='timeline-'+str(counter);v={'id':key,'action':'history_'+action}
    if target is not None:v['target']=target
    write(session/'qa-request.json',v);result=wait(session/('qa-result-'+key+'.json'))
    assert result['accepted'],result
    return key
def capture(label):
    key=qa('capture');image=session/('history-'+key+'.png');receipt=wait(image.with_suffix('.png.json'))
    (out/(label+'.png')).write_bytes(image.read_bytes());write(out/(label+'.json'),receipt);return receipt
def launch():
    (session/'app-ready.json').unlink(missing_ok=True)
    proc=start([a.app/'Contents/MacOS/WendyWorldDemo','--session',session,'--mission-history-profile',out/'profile.json'],'native')
    wait(session/'app-ready.json');return proc
try:
    env=dict(os.environ,MISSION_HISTORY_KEEP_RUNNING='1')
    fixture=start([sys.executable,repo/'tests/task_history_site.py','--bin-dir',a.bin_dir,'--out',site,'--port',a.port],'site',env)
    wait(site/'ready.json',45)
    write(out/'profile.json',{'version':1,'credentials_file':str(site/'private/viewer-3.json'),'cache_directory':str(out/'cache'),'origin':'node-1','principal':'lab/node-1','assignee':'sim-robot/navigation','run_id':'mission-history-demo'})
    write(session/'geometry.json',{'schema':'wendy.macos-demo.geometry.v1','run_id':'mission-history-demo','model_sha256':'0'*64,'geoms':[],'presentation':{'name':'Historical mission decisions','subtitle':'No live scene or physical execution','labels':[]}})
    write(session/'state.json',{'schema':'wendy.macos-demo.state.v1','run_id':'mission-history-demo','sequence':1,'worker_status':'historical_replay','mode':'historical-mission-history','last_command_sequence':0,'dynamic_geoms':[],'snapshot':{'run_id':'mission-history-demo','time_s':0,'phase':'historical','authorized':False,'stopped':True,'robots':[],'jobs':[],'completed':0,'total':0,'events':[],'voxels':[],'voxel_size':1,'map_node':'historical','network':{'edges':[],'internet':{'id':'offline','a':'local','b':'none','enabled':False,'up':False,'packets':0},'gossip_rounds':0,'peer_voxels':{}},'controls_available':False}})
    app=launch();qa('select','');qa('refresh');allrows=capture('01-origin-timeline');assert len(allrows['events'])==4;checks.append('native authenticated read of actual three-node relayed history')
    qa('select','review-inspection');qa('refresh');selected=capture('02-explicit-stop');assert [x['event'] for x in selected['events']]==['accepted','stop'];checks.append('exact task timeline shows original acceptance and operator stop')
    qa('select','interrupted-inspection');qa('refresh');interrupted=capture('03-assignee-restarted');assert [x['event'] for x in interrupted['events']]==['accepted','restart'];checks.append('assignee SIGKILL/reopen decision appears as retained failure')
    stop(app,True);(site/'finish').touch();fixture.wait(15);assert fixture.returncode==0;wait(site/'cleanup.json')
    app=launch();qa('select','review-inspection');offline=capture('04-viewer-reopened-offline');assert offline['events']==selected['events'] and offline['fetched_unix_ms']==selected['fetched_unix_ms'];checks.append('viewer SIGKILL/reopen retains exact historical timeline while all nodes stopped')
    qa('refresh');failed=capture('05-refresh-unavailable');assert 'Refresh unavailable' in failed['message'] and failed['events']==selected['events'];checks.append('failed refresh keeps saved events and their original retrieval time')
    write(out/'acceptance.json',{'passed':True,'scope':'Actual loopback Go nodes and native SwiftUI callbacks; no OS input, simulator or physical execution','checks':checks,'site_acceptance_sha256':hashlib.sha256((site/'acceptance.json').read_bytes()).hexdigest(),'app_binary_sha256':hashlib.sha256((a.app/'Contents/MacOS/WendyWorldDemo').read_bytes()).hexdigest()})
finally:
    if site.exists():(site/'finish').touch()
    if 'fixture' in globals():
        try:fixture.wait(15)
        except subprocess.TimeoutExpired:stop(fixture)
    for proc in processes:stop(proc)
    for log in logs:log.close()
    write(out/'cleanup.json',{'all_exited':all(x.poll() is not None for x in processes),'owned_processes':[{'pid':x.pid,'exit':x.returncode}for x in processes]})
