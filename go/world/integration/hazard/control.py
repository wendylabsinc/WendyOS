"""Use the qualified local-file guards with this fixture's fixed source paths."""
if __package__:
    from . import libraries, model
else:
    import libraries
    import model

_people = libraries.base.load('hazard_people_control', libraries.HERE.parent / 'people/control.py',
                             {'libraries': libraries.base, 'model': model})
ATOMIC_WRITE = _people.ATOMIC_WRITE
VOLUME_DIRECTORIES = _people.VOLUME_DIRECTORIES.replace('world-people-', 'world-hazard-')
volume_receipt = _people.volume_receipt
sample_clock = _people.sample_clock

HTTP = _people.HTTP.replace("'/v1/cameras/camera-a','/v1/cameras/camera-b'",
    ','.join(repr('/v1/captures/' + source) for source in model.SENSORS))

# Fusion's terminal summary has no tick. Only actual app observations belong
# in the live readiness/acceptance predicate; completed files are also retained.
LATEST = _people.LATEST.replace("'summary' not in json.loads(line)",
                               "type(json.loads(line).get('tick')) is int and json.loads(line)['tick'] > 0")

MATCH_PUBLICATIONS = r'''
import base64,hashlib,json,os,stat,sys
try:
 path,wanted=sys.argv[1:];wanted=set(json.loads(wanted));assert 0<len(wanted)<=8
 fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK)
 with os.fdopen(fd,'rb') as f:
  s=os.fstat(f.fileno());assert stat.S_ISREG(s.st_mode) and s.st_uid==0 and s.st_nlink==1 and 0<s.st_size<=32<<20
  raw=f.read((32<<20)+1);assert len(raw)<=32<<20
 found={}
 for line in raw.split(b'\n')[:-1]:
  row=json.loads(line)
  if row.get('status')!='complete' or row.get('locally_verified') is not True:continue
  envelope=base64.b64decode(row.get('signed_record_json',''),validate=True);digest=hashlib.sha256(envelope).hexdigest()
  if digest in wanted:
   assert digest not in found and digest==row['proof_sha256'] and type(row.get('tick')) is int and row['tick']>0
   found[digest]={'tick':row['tick'],'run_id':row['run_id'],'publication':row}
 assert set(found)==wanted
 output=json.dumps(found).encode();assert len(output)<=4<<20;sys.stdout.buffer.write(output)
except Exception:raise SystemExit(1)
'''
