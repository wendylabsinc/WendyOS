"""Reuse bounded file controls; retain complete original reader observations."""
if __package__:
    from . import libraries, model
else:
    import libraries
    import model

_base = libraries.load('nativesensorfusion_hazard_control', libraries.HERE.parent / 'hazard/control.py',
                       {'libraries': libraries, 'model': model})
for _name in ('ATOMIC_WRITE', 'HTTP', 'LATEST', 'volume_receipt', 'sample_clock', 'MATCH_PUBLICATIONS'):
    globals()[_name] = getattr(_base, _name)
VOLUME_DIRECTORIES = _base.VOLUME_DIRECTORIES.replace('world-hazard-', 'world-nativesensorfusion-')
from integration.distributedfusion.control import READER_OBSERVATIONS

# Aggregate source/bridge receipts contain at most two actual frames. They are
# saved once the app finishes; readers never reinterpret them as current data.
AGGREGATE = r'''
import json,os,stat,sys
try:
 fd=os.open(sys.argv[1],os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK)
 with os.fdopen(fd,'rb') as f:
  s=os.fstat(f.fileno());assert stat.S_ISREG(s.st_mode) and s.st_uid==0 and s.st_nlink==1 and 0<s.st_size<=4<<20
  raw=f.read((4<<20)+1);assert len(raw)<=4<<20
 lines=raw.split(b'\n')[:-1];assert len(lines)==1
 value=json.loads(lines[0]);assert value['version']==1 and value['status']=='complete'
 sys.stdout.buffer.write(lines[0])
except Exception:raise SystemExit(1)
'''
