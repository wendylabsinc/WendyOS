"""Replay genuine completed inspection evidence into disposable capture stores.

Records actual native SwiftUI/model callbacks, never OS mouse input. This does
not claim the disposable retention stores were the original capture owners.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import time

p = argparse.ArgumentParser(description=__doc__)
for name in ('record','media_manifest','geometry','state','app','out'):
    p.add_argument('--'+name.replace('_','-'), required=True, type=Path)
p.add_argument('--port',type=int,default=19450)
a=p.parse_args();repo=Path(__file__).resolve().parents[1];out=a.out.resolve()
if out.exists():raise ValueError('fresh output required')
os.umask(0o077);out.mkdir(mode=0o700,parents=True)
processes=[];logs=[];history=[];app=None

def write(path,value):
    tmp=path.with_suffix('.new');tmp.write_text(json.dumps(value,sort_keys=True,indent=2));tmp.replace(path)
def wait(path,seconds=15):
    until=time.monotonic()+seconds
    while time.monotonic()<until:
        if path.exists():return json.loads(path.read_text())
        time.sleep(.04)
    raise TimeoutError(str(path))
def run(args,**kwargs):return subprocess.run(args,cwd=repo,check=True,**kwargs)
def start(args,name):
    log=(out/(name+'.log')).open('a');logs.append(log)
    proc=subprocess.Popen(args,cwd=repo,stdout=log,stderr=log);processes.append(proc);return proc
def stop(proc,kill=False):
    if proc.poll() is None:
        proc.kill() if kill else proc.terminate()
        try:proc.wait(timeout=5)
        except subprocess.TimeoutExpired:proc.kill();proc.wait()
    history.append({'pid':proc.pid,'exit':proc.returncode,'killed':kill})
count=0
session=out/'session'
def qa(action,target=None,seconds=None,await_result=True):
    global count
    count+=1;identifier='investigation-'+str(count)
    value={'id':identifier,'action':'investigation_'+action}
    if target is not None:value['target']=target
    if seconds is not None:value['delta_x']=seconds
    write(session/'qa-request.json',value)
    if await_result:
        result=wait(session/('qa-result-'+identifier+'.json'))
        if not result['accepted']:raise AssertionError(result)
    return identifier
def capture(label):
    identifier=qa('capture')
    source=session/('investigation-'+identifier+'.png')
    receipt=wait(source.with_suffix('.png.json'))
    (out/(label+'.png')).write_bytes(source.read_bytes())
    write(out/(label+'.json'),receipt)
    return receipt
def launch():
    (session/'app-ready.json').unlink(missing_ok=True)
    proc=start([str(a.app/'Contents/MacOS/WendyWorldDemo'),'--session',str(session),'--investigation-profile',str(private/'profile.json')],'native-app')
    wait(session/'app-ready.json');return proc
try:
    envelope=json.loads(a.record.read_text());record=envelope.get('record',envelope)
    if record.get('status',{}).get('Phase')!='succeeded':raise ValueError('genuine terminal succeeded task record required')
    result=record['result'];spec=record['spec'];grant=record['proof']['grant']['grant']
    assert result['task_id']==spec['id'] and result['run_id'] and result['captured_unix_ms']>0
    tenant,site=grant['tenant'],spec['site'];assert grant['site']==site and grant['domain']=='simulation'
    manifest=json.loads(a.media_manifest.read_text());imports=[]
    for sample in result['samples']:
        ref=sample.get('evidence')
        if not ref:continue
        media=Path(manifest[ref['sha256']]).resolve();raw=media.read_bytes()
        assert len(raw)==ref['size'] and hashlib.sha256(raw).hexdigest()==ref['sha256']
        destination='capture-a' if sample['sensor_id']=='camera-a' else 'capture-b'
        imports.append({'original_sensor':sample['sensor_id'],'sha256':ref['sha256'],'size':len(raw),'content_type':ref['content_type'],'source_path':str(media),'standalone_import_audience':destination})
    assert len(imports)>=2 and len({x['standalone_import_audience'] for x in imports})==2
    write(out/'import-binding.json',{'scope':'Replay import of exact completed simulated inspection bytes into new standalone stores; these were not the original capture owners','original_record_path':str(a.record),'original_wrapper_sha256':hashlib.sha256(a.record.read_bytes()).hexdigest(),'import_record_json_sha256':hashlib.sha256(json.dumps(record,sort_keys=True,separators=(',',':')).encode()).hexdigest(),'original_task':result['task_id'],'original_run':result['run_id'],'original_capture_unix_ms':result['captured_unix_ms'],'tenant':tenant,'site':site,'imports':imports})
    run(['go','build','-o',str(out/'fixture'),'./tests/investigationfixture']);run(['go','build','-o',str(out/'worldd'),'./cmd/worldd'])
    private=out/'private'
    run([str(out/'fixture'),'--out',str(private),'--port',str(a.port),'--tenant',tenant,'--site',site,'--run',result['run_id']])
    nodes={}
    for i,name in enumerate(('capture-a','capture-b')):
        nodes[name]=start([str(out/'worldd'),'--config',str(private/(name+'.json'))],name)
        proxy=private/(name+'-proxy');proxy.mkdir(mode=0o700)
        start([sys.executable,str(repo/'tests/investigation_proxy.py'),'--port',str(a.port+2+i),'--upstream',str(a.port+i),'--directory',str(proxy)],name+'-proxy')
    time.sleep(.3)
    for item in imports:
        with (out/(item['original_sensor']+'-import.json')).open('w') as receipt:
            run([str(out/'fixture'),'--mode','upload','--out',str(private),'--node',item['standalone_import_audience'],'--file',item['source_path'],'--mime',item['content_type'],'--ttl','600'],stdout=receipt)
    profile={'version':1,'tenant':tenant,'site':site,'journal_directory':str(private/'journal'),'origins':[]}
    for name,label in [('capture-a','West standalone store'),('capture-b','East standalone store')]:
        profile['origins'].append({'id':name,'label':label,'backend':'standalone-worldd','pin_credentials_file':str(private/(name+'-pin.json')),'read_credentials_file':str(private/(name+'-read.json')),'sensor_ids':[x['original_sensor'] for x in imports if x['standalone_import_audience']==name]})
    write(private/'profile.json',profile)
    session.mkdir(mode=0o700);(session/'geometry.json').write_bytes(a.geometry.read_bytes())
    state=json.loads(a.state.read_text());state['worker_status']='historical_replay';state['mode']='historical-inspection-replay'
    write(session/'state.json',state);write(session/'inspection-result.json',result)
    write(out/'scene-replay.json',{'geometry_sha256':hashlib.sha256(a.geometry.read_bytes()).hexdigest(),'state_sha256':hashlib.sha256(a.state.read_bytes()).hexdigest(),'scope':'Neutral result-only shell; no scene geometry, simulator, current observations or controls; native investigation callbacks remain usable offline'})
    app=launch();qa('prepare','inspection-result.json');capture('01-review')
    write(private/'capture-b-proxy/mode.json',{'partition':True})
    qa('retain','Inspection · retained review',seconds=180)
    initial=capture('02-partition');assert 'Protected at last check' in initial['states'] and 'Create outcome unknown' in initial['states']
    stop(app,True);app=launch();qa('open','Inspection · retained review');reopened=capture('03-reopened');assert reopened['states']==initial['states']
    write(private/'capture-b-proxy/mode.json',{})
    qa('retry','capture-b');connected=capture('04-reconnected');assert connected['states']==['Protected at last check']*2
    stop(nodes['capture-a'],True);nodes['capture-a']=start([str(out/'worldd'),'--config',str(private/'capture-a.json')],'capture-a');time.sleep(.2)
    qa('retry','capture-a');assert capture('05-store-restarted')['states']==['Protected at last check']*2
    proxy=private/'capture-a-proxy';write(proxy/'mode.json',{'drop_unpin':True,'wait_after_delivery':True})
    qa('release','capture-a',await_result=False);wait(proxy/'delivered.json');stop(app,True)
    write(proxy/'mode.json',{});app=launch();qa('open','Inspection · retained review')
    uncertain=capture('06-release-interrupted');assert 'Release outcome unknown' in uncertain['states']
    qa('release','capture-a');qa('release','capture-b');released=capture('07-released');assert released['states']==['Release acknowledged']*2
    qa('prepare','inspection-result.json');qa('retain','Short fixed deadline',seconds=4);time.sleep(4.2)
    expired=capture('08-expired');assert expired['states']==['Expired']*2
    # Retained exact body traces prove the uncertain release did not create a new identity.
    events=[json.loads(line) for line in (proxy/'events.jsonl').read_text().splitlines()]
    first_id=next(x['body']['id'] for x in events if x['path'].endswith('/pin'))
    same=[x for x in events if x.get('body',{}).get('id')==first_id]
    pins=[x['body'] for x in same if x['path'].endswith('/pin')]
    assert all(x==pins[0] for x in pins)
    assert len([x for x in same if x['path'].endswith('/unpin')])==2
    write(out/'acceptance.json',{'passed':True,'scope':'Actual native SwiftUI panels and scripted application callbacks, not OS mouse input; genuine historical inspection bytes replay-imported into two standalone stores','task_id':result['task_id'],'run_id':result['run_id'],'checks':['actual selected camera/audio digest reads','two-store independent outcomes','partition/reconnect','viewer SIGKILL/reopen','capture-store SIGKILL/reopen','lost release acknowledgement and exact retry','explicit release','fixed expiry'],'import_binding_sha256':hashlib.sha256((out/'import-binding.json').read_bytes()).hexdigest(),'app_binary_sha256':hashlib.sha256((a.app/'Contents/MacOS/WendyWorldDemo').read_bytes()).hexdigest()})
finally:
    for proc in processes:stop(proc)
    for log in logs:log.close()
    write(out/'cleanup.json',{'owned_processes':history,'all_exited':all(x.poll() is not None for x in processes)})
