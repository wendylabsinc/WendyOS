"""Real three-process CLI acceptance; synthetic spatial claims, no hardware."""
import argparse, hashlib, http.client, json, os, signal, subprocess, time, shutil
from pathlib import Path

def main(a):
 r=a.out.resolve();r.mkdir(mode=0o700,parents=True,exist_ok=False);site=r/'private-site';site.mkdir(mode=0o700)
 processes=[];logs=[];checks=[];commands=[]
 receipt={'driver_sha256':hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),'scope':'Three actual authenticated loopback worldd processes; synthetic spatial claims; CLI controls, no native regional configuration','checks':checks,'processes':processes,'commands':commands,'executables':{str(p):hashlib.sha256(p.read_bytes()).hexdigest() for p in (a.world,a.worldd,a.lab,a.publisher)}}
 def write(p,v):p.write_text(json.dumps(v,indent=2)+'\n');p.chmod(0o600)
 def cmd(argv,ok=True):
  p=subprocess.run(list(map(str,argv)),capture_output=True,timeout=25);commands.append({'argv':[str(x).replace(str(site),'<private-site>') for x in argv],'returncode':p.returncode,'stdout':p.stdout.decode(),'stderr':p.stderr.decode()})
  assert (p.returncode==0)==ok,(argv,p.stderr.decode());return json.loads(p.stdout) if p.stdout.strip().startswith((b'{',b'[')) else None
 active={}
 def stop(i):
  p,rec=active.pop(i);p.kill();p.wait(timeout=5);rec.update(exited_at=time.time(),returncode=p.returncode,signal='SIGKILL')
 def start(i):
  f=(r/f'node-{i}-{len(processes)}.log').open('wb');logs.append(f);p=subprocess.Popen([str(a.worldd),'--config',str(site/f'node-{i}.json')],stdout=f,stderr=f);rec={'pid':p.pid,'node':i,'started_at':time.time()};processes.append(rec);active[i]=(p,rec)
  end=time.monotonic()+8
  while time.monotonic()<end:
   try:
    h=http.client.HTTPConnection('127.0.0.1',a.port+i-1,timeout=.2);h.request('GET','/healthz');q=h.getresponse();q.read();h.close()
    if q.status==200:return
   except OSError:pass
   assert p.poll() is None,'node startup failed';time.sleep(.05)
  raise AssertionError('node readiness')
 def status(i):return cmd([a.world,'replica','status','--credentials',site/f'reader-{i}.json'])
 def rows(i):return cmd([a.world,'query','--credentials',site/f'reader-{i}.json','--selection',r/'keys.json'])['components']
 def awaitrows(i,ids):
  end=time.monotonic()+8
  while time.monotonic()<end:
   got=rows(i)
   if {c['key']['entity'] for c in got}==set(ids):return got
   time.sleep(.15)
  raise AssertionError(('rows',i,ids,got))
 def publish(name,x,unknown=False,ttl=180000):
  payload={'position_m':[x,0,1],'uncertainty_m':0,'label':'synthetic regional movement'}
  if unknown:payload['position_m']=None
  p=r/f'publish-{len(commands)}.json';write(p,[{'id':name,'payload':payload,'ttl_ms':ttl}]);cmd([a.publisher,'--credentials',site/'publisher.json','--records',p])
 try:
  cmd([a.lab,'--out',site,'--run-id','area-workspaces','--base-port',a.port,'--sensors'])
  write(site/'publisher.json',json.loads((site/'client-1.json').read_text()))
  selected={'types':['wendy.hazard.v1'],'region':{'frame':{'id':'office-floor','revision':'office-frame-v1'},'min_xz':[0,0],'max_xz':[5,5]}}
  write(r/'selection.json',selected);write(r/'keys.json',{'types':['wendy.hazard.v1']})
  policy=cmd([a.world,'replica','policy','--tenant','lab','--site','synthetic-office','--selection',r/'selection.json','--unknown-location','retain'])['policy']
  # Use generated scope, never assume a tenant identity.
  publisher=json.loads((site/'publisher.json').read_text());policy['tenant']=publisher['grant']['grant']['tenant'];policy['site']=publisher['grant']['grant']['site']
  for i in (1,2,3):
   cfg=json.loads((site/f'node-{i}.json').read_text());cfg['peers']=[]
   if i>1:cfg['replica_policy']=policy;cfg['peers']=[{'url':f'http://127.0.0.1:{a.port}','audience':'node-1'}]
   write(site/f'node-{i}.json',cfg)
   reader=json.loads((site/'viewer-1.json').read_text());reader['url']=f'http://127.0.0.1:{a.port+i-1}';reader['audience']=f'node-{i}';write(site/f'reader-{i}.json',reader)
   start(i)
  publish('moving-object',1);publish('unlocated-object',0,True)
  for i in (2,3):awaitrows(i,['moving-object','unlocated-object'])
  write(r/'01-inside-and-unknown.json',status(2));checks.append('Two regional replicas retain inside and explicitly unlocated records')
  stop(2);publish('moving-object',20);awaitrows(3,['unlocated-object'])
  write(r/'02-outside-fence.json',status(3));checks.append('Full source repairs outward movement as a payload-free fence')
  stop(1);start(2)
  awaitrows(2,['moving-object','unlocated-object']);checks.append('SIGKILL/reopen while origin partitioned preserves last known inside value')
  start(1);awaitrows(2,['unlocated-object']);write(r/'03-reconnected-fence.json',status(2));checks.append('Reconnect removes stale inside payload using newer authenticated outside version')
  stop(2);start(2);awaitrows(2,['unlocated-object']);s=status(2);assert s['retained_fences']==1 and s['unknown_location_records']==1
  write(r/'04-crash-persistent-fence.json',s);checks.append('Fence and explicit unknown count survive process SIGKILL and reopen')
  cfg=json.loads((site/'node-2.json').read_text());proof=Path(cfg['proof_path']);world=Path(cfg['data_path'])
  for label,path in [('step04-world-checkpoint',world),('step04-proof-store',proof)]:
   saved=r/(label+'.json');saved.write_bytes(path.read_bytes());saved.chmod(0o600)
  assert b'moving-object' not in proof.read_bytes(),'outside proof retained'
  state=json.loads(world.read_text())['state'];assert len(state['world']['fences'])==1 and set(state['world']['fences'][0])=={'key','version','sha256'}
  checks.append('Durable proof store excludes outside original bytes; fence has no payload')
  publish('moving-object',2)
  for i in (2,3):awaitrows(i,['moving-object','unlocated-object'])
  write(r/'05-newer-reentry.json',status(2));checks.append('Newer authenticated inside version replaces fence on both replicas')
  publish('brief-object',1,ttl=3000)
  for i in (2,3):awaitrows(i,['moving-object','unlocated-object','brief-object'])
  time.sleep(3.1)
  for i in (2,3):awaitrows(i,['moving-object','unlocated-object'])
  stop(2);start(2);awaitrows(2,['moving-object','unlocated-object'])
  write(r/'06-expiry-after-restart.json',status(2));checks.append('Original short TTL expires on both replicas and stays expired after process restart')
  receipt['passed']=True
 finally:
  for i in list(active):stop(i)
  for f in logs:f.close()
  receipt['removed_private_file_hashes']={str(p.relative_to(site)):hashlib.sha256(p.read_bytes()).hexdigest() for p in sorted(site.rglob('*')) if p.is_file()}
  shutil.rmtree(site)
  receipt['private_site_removed']=not site.exists()
  receipt['finished_at']=time.time();write(r/'verification.json',receipt)
if __name__=='__main__':
 p=argparse.ArgumentParser();p.add_argument('--out',type=Path,required=True);p.add_argument('--port',type=int,default=19830)
 for n in ('world','worldd','lab','publisher'):p.add_argument('--'+n,type=Path,required=True)
 main(p.parse_args())
