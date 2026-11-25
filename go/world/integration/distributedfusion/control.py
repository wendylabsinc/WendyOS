"""Reuse exact file guards; add only the B gas acknowledgment selector."""
if __package__:
    from . import libraries, model
else:
    import libraries
    import model

_base = libraries.load('distributedfusion_hazard_control', libraries.HERE.parent / 'hazard/control.py',
                       {'libraries': libraries, 'model': model})
ATOMIC_WRITE, HTTP, LATEST = _base.ATOMIC_WRITE, _base.HTTP, _base.LATEST
VOLUME_DIRECTORIES = _base.VOLUME_DIRECTORIES.replace('world-hazard-', 'world-distributedfusion-')
volume_receipt, sample_clock = _base.volume_receipt, _base.sample_clock
MATCH_PUBLICATIONS = _base.MATCH_PUBLICATIONS

# Complete ordinary reader observations, including a successful live read that
# a later empty tick has superseded. Return original lines, never a new reading.
READER_OBSERVATIONS = r'''
import os,stat,sys
try:
 path=sys.argv[1];fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK)
 with os.fdopen(fd,'rb') as f:
  s=os.fstat(f.fileno());assert stat.S_ISREG(s.st_mode) and s.st_uid==0 and s.st_nlink==1 and 0<s.st_size<=32<<20
  raw=f.read((32<<20)+1);assert len(raw)<=32<<20
 lines=raw.split(b'\n')[:-1];assert len(lines)<=512 and all(0<len(line)<=2<<20 for line in lines)
 sys.stdout.buffer.write(b''.join(line+b'\n' for line in lines))
except Exception:raise SystemExit(1)
'''

MATCH_CAPTURE = r'''
import base64,hashlib,json,os,stat,sys
try:
 path,wanted=sys.argv[1:];assert len(wanted)==64
 fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK)
 with os.fdopen(fd,'rb') as f:
  s=os.fstat(f.fileno());assert stat.S_ISREG(s.st_mode) and s.st_uid==0 and s.st_nlink==1 and 0<s.st_size<=32<<20
  raw=f.read((32<<20)+1);assert len(raw)<=32<<20
 found=[]
 for line in raw.split(b'\n')[:-1]:
  row=json.loads(line)
  if row.get('proof_sha256')!=wanted:continue
  envelope=base64.b64decode(row['signed_record_json'],validate=True)
  assert hashlib.sha256(envelope).hexdigest()==wanted
  found.append(row)
 assert len(found)==1
 output=json.dumps(found[0]).encode();assert len(output)<=384<<10;sys.stdout.buffer.write(output)
except Exception:raise SystemExit(1)
'''
