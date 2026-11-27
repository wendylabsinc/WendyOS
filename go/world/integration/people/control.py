"""Protected local fixture writes and bounded scene requests; no runtime on import."""
from datetime import datetime, timezone
import libraries
import model

ATOMIC_WRITE = libraries.control.ATOMIC_WRITE
# Same descriptor-anchored creation, fresh-volume refusal and fixed safe modes.
# Only this fixture's two explicit volume names replace the pair's three names.
VOLUME_DIRECTORIES = libraries.control.VOLUME_DIRECTORIES.replace('len(names)==3', 'len(names)==2').replace('len(set(names))==3', 'len(set(names))==2').replace("('config','control','proofs')", "('config','proofs')").replace('world-pair-', 'world-people-')


def volume_receipt(raw, names):
    value = model.strict_json(raw, 4096)
    model.require(set(value) == {'version', 'directories'} and value['version'] == 1 and len(value['directories']) == 4, 'fixed volume metadata required')
    for index, (row, name) in enumerate(zip(value['directories'], ['wendy', 'volumes', *names])):
        model.require(set(row) == {'component', 'uid', 'mode', 'created'} and row['component'] == name and row['uid'] == 0 and type(row['mode']) is int and not row['mode'] & 0o022 and type(row['created']) is bool, 'protected volume metadata required')
        if index >= 2 or row['created']:
            model.require(row['mode'] == 0o700 and row['created'], 'fresh private volume required')
    return value


def sample_clock(run_id, anchor, ns):
    model.require(type(ns) is int and 0 <= ns <= 120_000_000_000 and anchor == model.ANCHOR, 'bounded original sample clock required')
    seconds, nanos = divmod(anchor + ns, 1_000_000_000)
    now = datetime.fromtimestamp(seconds, timezone.utc).strftime('%Y-%m-%dT%H:%M:%S') + ('.%09d' % nanos) + 'Z'
    return model.json_bytes({'domain': model.sample_domain(run_id), 'now': now})


HTTP = r'''
import json,sys,urllib.request
class NoRedirect(urllib.request.HTTPRedirectHandler):
 def redirect_request(self,*args,**kwargs):return None
try:
 endpoint,path,tokenfile,body=sys.argv[1:]
 assert path in ('/v1/identity','/v1/presentation','/v1/cameras/camera-a','/v1/cameras/camera-b','/v1/step')
 token=open(tokenfile).read().strip(); data=None if body=='-' else body.encode()
 request=urllib.request.Request(endpoint+path,data=data,headers={'Authorization':'Bearer '+token,'Content-Type':'application/json'})
 opener=urllib.request.build_opener(urllib.request.ProxyHandler({}),NoRedirect())
 with opener.open(request,timeout=4) as response:
  raw=response.read((4<<20)+1);assert response.status==200 and 0<len(raw)<=4<<20
  json.loads(raw);sys.stdout.buffer.write(raw)
except Exception:raise SystemExit(1)
'''

# Read only complete NDJSON lines from the ordinary app's bounded stdout. This
# is a current prefix observation; final exact stdout and exit receipts remain
# separately retained. No trace/private agent log is read by this helper.
LATEST = r'''
import json,os,stat,sys
try:
 path,maximum=sys.argv[1:];fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK)
 with os.fdopen(fd,'rb') as f:
  s=os.fstat(f.fileno());assert stat.S_ISREG(s.st_mode) and s.st_uid==0 and s.st_nlink==1 and 0<s.st_size<=32<<20
  raw=f.read((32<<20)+1);assert len(raw)<=32<<20
 lines=raw.split(b'\n')[:-1];assert lines
 chosen=next(line for line in reversed(lines) if 'summary' not in json.loads(line))
 assert len(chosen)<=int(maximum);sys.stdout.buffer.write(chosen+b'\n')
except Exception:raise SystemExit(1)
'''

MATCH_PUBLICATIONS = r'''
import base64,hashlib,json,os,stat,sys
try:
 path,wanted=sys.argv[1:];wanted=set(json.loads(wanted));assert 0<len(wanted)<=48
 fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK)
 with os.fdopen(fd,'rb') as f:
  s=os.fstat(f.fileno());assert stat.S_ISREG(s.st_mode) and s.st_uid==0 and s.st_nlink==1 and 0<s.st_size<=32<<20
  raw=f.read((32<<20)+1);assert len(raw)<=32<<20
 found={}
 for line in raw.split(b'\n')[:-1]:
  row=json.loads(line)
  for p in row.get('publications',[]):
   envelope=base64.b64decode(p.get('signed_record_json',''),validate=True);digest=hashlib.sha256(envelope).hexdigest()
   if digest in wanted:
    assert digest not in found and p.get('locally_verified') is True
    found[digest]={'tick':row['tick'],'run_id':row['run_id'],'sample_time':row['sample_time'],'publication':p}
 assert set(found)==wanted
 output=json.dumps(found).encode();assert len(output)<=4<<20;sys.stdout.buffer.write(output)
except Exception:raise SystemExit(1)
'''
