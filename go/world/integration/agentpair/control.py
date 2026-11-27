"""Bounded controls for an explicitly owned, synthetic two-agent fixture.

The receiver publishes a complete single-link file through Linux renameat2.
There is no hardlink or overwrite fallback for watched command names. Clock
replacement is a serialized fixture operation, not a concurrent compare/swap.
"""
from datetime import datetime, timedelta
import ipaddress
import re
import shlex

import model

ATOMIC_WRITE = r'''
import ctypes,errno,hashlib,json,os,secrets,stat,sys
LIMIT=262144
def checked(s,private=False):
 assert s.st_uid in (0,os.geteuid()) and not s.st_mode & 0o022
 if private: assert s.st_uid==os.geteuid() and not s.st_mode & 0o077
def stable(s):
 return tuple(getattr(s,k) for k in ('st_dev','st_ino','st_mode','st_uid','st_gid','st_nlink','st_size','st_mtime_ns','st_ctime_ns'))
def parent(path):
 assert path.startswith('/') and not path.startswith('//')
 parts=path.split('/')[1:]; assert parts and all(p and p not in ('.','..') for p in parts)
 fd=os.open('/',os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW)
 try:
  checked(os.fstat(fd))
  for part in parts[:-1]:
   child=os.open(part,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=fd)
   os.close(fd);fd=child;checked(os.fstat(fd))
  checked(os.fstat(fd),True)
  return fd,parts[-1]
 except BaseException:os.close(fd);raise
def read(fd,name):
 f=os.open(name,os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK,dir_fd=fd)
 try:
  s=os.fstat(f);checked(s,True)
  assert stat.S_ISREG(s.st_mode) and s.st_nlink==1 and s.st_size<=LIMIT
  raw=b''
  while len(raw)<=LIMIT:
   b=os.read(f,LIMIT+1-len(raw))
   if not b:break
   raw+=b
  assert len(raw)==s.st_size and stable(os.fstat(f))==stable(s)
  return raw,stable(s)
 finally:os.close(f)
def no_replace(fd,source,destination):
 # The supported runtime is Linux. Missing renameat2 fails closed.
 fn=ctypes.CDLL(None,use_errno=True).renameat2
 fn.argtypes=[ctypes.c_int,ctypes.c_char_p,ctypes.c_int,ctypes.c_char_p,ctypes.c_uint]
 fn.restype=ctypes.c_int
 if fn(fd,os.fsencode(source),fd,os.fsencode(destination),1)!=0:
  raise OSError(ctypes.get_errno(),'atomic install refused')
def publish(fd,name,raw,previous):
 old=None
 if previous!='-':
  prior,old=read(fd,name);assert hashlib.sha256(prior).hexdigest()==previous
 temporary='.pending-'+secrets.token_hex(16)
 f=os.open(temporary,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600,dir_fd=fd)
 try:
  with os.fdopen(f,'wb') as stream:
   stream.write(raw);stream.flush();os.fsync(stream.fileno())
  if previous=='-':no_replace(fd,temporary,name)
  else:
   prior,current=read(fd,name)
   assert current==old and hashlib.sha256(prior).hexdigest()==previous
   os.replace(temporary,name,src_dir_fd=fd,dst_dir_fd=fd)
  os.fsync(fd)
  observed,_=read(fd,name);assert observed==raw
 finally:
  try:os.unlink(temporary,dir_fd=fd)
  except FileNotFoundError:pass
def main():
 path,length,digest,previous=sys.argv[1:]
 assert length.isdecimal() and 0<int(length)<=LIMIT
 assert len(digest)==64 and all(c in '0123456789abcdef' for c in digest)
 assert previous=='-' or len(previous)==64 and all(c in '0123456789abcdef' for c in previous)
 raw=sys.stdin.buffer.read(LIMIT+1);assert len(raw)==int(length) and hashlib.sha256(raw).hexdigest()==digest
 fd,name=parent(path)
 try:publish(fd,name,raw,previous)
 finally:os.close(fd)
 print(json.dumps({'bytes':len(raw),'sha256':digest,'atomic':True}))
if __name__=='__main__':
 try:main()
 except Exception:raise SystemExit(1)
'''

# Root-owned parents can predate this fixture because the normal agent also
# uses /var/lib/wendy. Never change their permissions to make a guard pass.
VOLUME_DIRECTORIES = r'''
import json,os,re,stat,sys
def safe_directory(fd):
 s=os.fstat(fd)
 assert stat.S_ISDIR(s.st_mode) and s.st_uid in (0,os.geteuid()) and not s.st_mode & 0o022
 return s
def row(fd,component,created,private=False):
 s=safe_directory(fd)
 assert s.st_uid==os.geteuid()
 mode=stat.S_IMODE(s.st_mode)
 if private or created:assert mode==0o700
 return {'component':component,'uid':s.st_uid,'mode':mode,'created':created}
def prepare(root,names):
 assert root.startswith('/') and not root.startswith('//')
 parts=root.split('/')[1:]
 assert len(parts)>=2 and parts[-2:]==['wendy','volumes'] and all(p and p not in ('.','..') for p in parts)
 assert len(names)==3 and len(set(names))==3
 assert all(re.fullmatch(r'world-pair-[a-z0-9-]{1,100}-'+kind,name) for name,kind in zip(names,('config','control','proofs')))
 assert len({name.rsplit('-',1)[0] for name in names})==1
 fd=os.open('/',os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW)
 result=[]
 try:
  safe_directory(fd)
  for index,part in enumerate(parts):
   created=False
   try:child=os.open(part,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=fd)
   except FileNotFoundError:
    assert index>=len(parts)-2
    os.mkdir(part,0o700,dir_fd=fd);os.fsync(fd);created=True
    child=os.open(part,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=fd)
   os.close(fd);fd=child;safe_directory(fd)
   if index>=len(parts)-2:result.append(row(fd,part,created))
  # Refuse any reused name, including a symlink, before creating phase dirs.
  for name in names:
   try:os.stat(name,dir_fd=fd,follow_symlinks=False)
   except FileNotFoundError:continue
   raise FileExistsError('phase volume already exists')
  for name in names:
   os.mkdir(name,0o700,dir_fd=fd)
   child=os.open(name,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=fd)
   try:
    result.append(row(child,name,True,True));assert not os.listdir(child)
   finally:os.close(child)
  os.fsync(fd)
 finally:os.close(fd)
 return {'version':1,'directories':result}
if __name__=='__main__':
 try:
  assert os.geteuid()==0
  print(json.dumps(prepare('/var/lib/wendy/volumes',sys.argv[1:])))
 except Exception:raise SystemExit(1)
'''


def volume_receipt(raw, names):
    """Accept only fixed, public metadata for the roots and new phase dirs."""
    model.require(isinstance(names, list) and len(names) == 3 and len(set(names)) == 3, 'three exact phase directories required')
    value = model.strict_json(raw, 4096)
    model.require(isinstance(value, dict) and set(value) == {'version', 'directories'} and
                  type(value['version']) is int and value['version'] == 1 and isinstance(value['directories'], list) and
                  len(value['directories']) == 5, 'bounded volume directory receipt required')
    for index, (item, component) in enumerate(zip(value['directories'], ['wendy', 'volumes', *names])):
        model.require(isinstance(item, dict) and set(item) == {'component', 'uid', 'mode', 'created'} and
                      item['component'] == component and type(item['uid']) is int and item['uid'] == 0 and
                      type(item['mode']) is int and 0 <= item['mode'] <= 0o777 and not item['mode'] & 0o022 and
                      type(item['created']) is bool, 'volume directory metadata differs')
        if index >= 2 or item['created']:
            model.require(item['mode'] == 0o700 and item['created'], 'fresh private directory required')
    return value


def command(action, sequence, fields):
    model.require(type(sequence) is int and 1 <= sequence <= 32, 'bounded command number required')
    required = {'publish': {'value', 'ttl_ms'}, 'tombstone': {'ttl_ms'}, 'query': set(), 'finish': set()}
    if action == 'await_query':
        expect = fields.get('expect')
        model.require(expect in ('ready', 'present', 'absent'), 'fixed query expectation required')
        required[action] = {'expect', 'envelope_sha256'} if expect == 'present' else {'expect'}
        if expect == 'present':
            model.require(model.digest_shape(fields.get('envelope_sha256')), 'exact expected envelope required')
    model.require(action in required and set(fields) == required[action], 'fixed command fields required')
    if action in ('publish', 'tombstone'):
        model.require(type(fields['ttl_ms']) is int and 100 <= fields['ttl_ms'] <= 30000, 'bounded sample lifetime required')
    raw = model.json_bytes({'version': 1, 'sequence': sequence, 'action': action, **fields})
    model.require(len(raw) <= 1024, 'command exceeds probe bound')
    return raw


def sample_clock(run_id, elapsed_seconds):
    model.require(isinstance(run_id, str) and re.fullmatch('[a-z0-9][a-z0-9-]{0,39}', run_id), 'fixed run domain required')
    model.require(type(elapsed_seconds) is int and 0 <= elapsed_seconds <= 600, 'bounded synthetic sample time required')
    start = datetime.fromisoformat(model.SAMPLE_START.replace('Z', '+00:00'))
    instant = start + timedelta(seconds=elapsed_seconds)
    return model.json_bytes({'domain': model.sample_domain(run_id),
                             'now': instant.isoformat().replace('+00:00', 'Z')}), int(instant.timestamp()) * 1_000_000_000


def peer_ipv4(address):
    try:
        parsed = ipaddress.IPv4Address(address)
    except (ValueError, TypeError) as error:
        raise model.Refused('exact private peer IPv4 required') from error
    model.require(str(parsed) == address and parsed.is_private and not parsed.is_loopback and not parsed.is_unspecified,
                  'exact private peer IPv4 required')
    return address


def partition_rules(owner, address):
    model.require(isinstance(owner, str) and re.fullmatch('world-agentpair-[a-f0-9]{12}', owner), 'owned scenario identity required')
    address = peer_ipv4(address)
    prefix = 'WWP_' + owner[-12:]
    result = []
    for direction, flag, suffix in (('INPUT', '-s', 'I'), ('OUTPUT', '-d', 'O')):
        chain = prefix + suffix
        rules = [[flag, address + '/32', '-p', 'tcp', '-m', 'tcp', port, str(model.PORT), '-j', 'DROP']
                 for port in ('--sport', '--dport')]
        result.append({'direction': direction, 'chain': chain, 'rules': rules})
    return result


def validate_chain(raw, row):
    model.require(isinstance(raw, bytes) and len(raw) <= 4096, 'bounded private rule listing required')
    try:
        lines = [shlex.split(line) for line in raw.decode('ascii').splitlines()]
    except (ValueError, UnicodeError) as error:
        raise model.Refused('invalid private rule listing') from error
    expected = [['-N', row['chain']], *[['-A', row['chain'], *rule] for rule in row['rules']]]
    model.require(lines == expected, 'partition rule membership changed')


def chain_drops(raw, row):
    model.require(isinstance(raw, bytes) and len(raw) <= 4096, 'bounded private counters required')
    try:
        lines = raw.decode('ascii').splitlines()
        model.require(len(lines) == 4 and lines[0].startswith('Chain ' + row['chain'] + ' '), 'unexpected partition counter shape')
        totals = [line.split() for line in lines[2:]]
        model.require(all(len(parts) >= 9 and parts[0].isdecimal() and parts[1].isdecimal() and parts[2:4] == ['DROP', 'tcp'] for parts in totals), 'unexpected partition counters')
        return sum(int(parts[0]) for parts in totals)
    except UnicodeError as error:
        raise model.Refused('invalid partition counters') from error


def validate_jump(raw, row):
    model.require(isinstance(raw, bytes) and len(raw) <= 64 << 10, 'bounded namespace filter listing required')
    try:
        lines = [shlex.split(line) for line in raw.decode('ascii').splitlines()]
    except (ValueError, UnicodeError) as error:
        raise model.Refused('invalid namespace filter listing') from error
    rules = [line for line in lines if line and line[0] == '-A']
    expected = ['-A', row['direction'], '-j', row['chain']]
    model.require(rules and rules[0] == expected and rules.count(expected) == 1, 'owned partition jump is not uniquely first')
