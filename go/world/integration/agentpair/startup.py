"""Classify only fixed private-agent startup failures; never export log text."""
import inspect
import json


def classify(raw):
    """Absence of a known failure is not World readiness or remote readiness."""
    if not isinstance(raw, bytes) or len(raw) > 1 << 20:
        raise ValueError('startup_log_limit')
    messages = {"Private World publication unavailable": "publication_unavailable",
                "Private World replication unavailable": "replication_unavailable"}
    found = set()
    for line in raw.splitlines():
        if not any(message.encode() in line for message in messages):
            continue
        if len(line) > 65536:
            raise ValueError('recognized_startup_log_invalid')
        def pairs(items):
            value = {}
            for key, item in items:
                if key in value:
                    raise ValueError('recognized_startup_log_invalid')
                value[key] = item
            return value
        try:
            value = json.loads(line, object_pairs_hook=pairs)
        except (ValueError, UnicodeError, RecursionError):
            raise ValueError('recognized_startup_log_invalid') from None
        if not isinstance(value, dict) or value.get('msg') not in messages:
            continue
        reason = messages[value['msg']]
        if reason == 'publication_unavailable' and value.get('error') == 'fresh authenticated quorum required':
            reason = 'publication_fresh_quorum_missing'
        found.add(reason)
    return {'known_startup_failure': bool(found), 'reasons': sorted(found),
            'world_ready': False, 'scope_behavior_check_pending': True}


# This code runs inside the exact owned container. Only its reconstructed,
# fixed-schema result goes through the process collector. Raw logs stay there.
PROBE_SCRIPT = 'import hashlib,json,os,stat,sys\n' + inspect.getsource(classify) + r'''
def protected(path, maximum, private=False):
 parts=path.split('/'); assert parts[0]=='' and all(x not in ('','.','..') for x in parts[1:])
 parent=os.open('/',os.O_RDONLY|os.O_DIRECTORY)
 try:
  for part in parts[1:-1]:
   child=os.open(part,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=parent);os.close(parent);parent=child
   info=os.fstat(parent); assert info.st_uid==0 and not info.st_mode&0o022
  fd=os.open(parts[-1],os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK,dir_fd=parent)
 finally:os.close(parent)
 with os.fdopen(fd,'rb') as f:
  info=os.fstat(f.fileno());assert stat.S_ISREG(info.st_mode) and info.st_uid==0 and info.st_nlink==1 and not info.st_mode&0o022 and info.st_size<=maximum
  if private:assert not info.st_mode&0o077
  raw=f.read(maximum+1);assert len(raw)<=maximum
 return raw
result={'known_startup_failure':False,'reasons':[],'world_ready':False,'scope_behavior_check_pending':True,'inspection_complete':False,'config_hash_matches':False}
try:
 config,expected,log,pid=sys.argv[1:];assert len(expected)==64 and all(c in '0123456789abcdef' for c in expected)
 assert int(pid)>1 and os.readlink('/proc/'+pid+'/exe')=='/var/lib/agentpair/tools/agent'
 before=protected(config,262144,True); assert hashlib.sha256(before).hexdigest()==expected
 raw=protected(log,1048576)
 result.update(classify(raw))
 assert os.readlink('/proc/'+pid+'/exe')=='/var/lib/agentpair/tools/agent'
 after=protected(config,262144,True); assert after==before
 result.update(inspection_complete=True,config_hash_matches=True)
except ValueError as failure:
 reason=str(failure)
 result['reasons']=[reason if reason in ('startup_log_limit','recognized_startup_log_invalid') else 'startup_inspection_refused']
except Exception:result['reasons']=['startup_inspection_refused']
print(json.dumps(result,separators=(',',':'),sort_keys=True))
'''
