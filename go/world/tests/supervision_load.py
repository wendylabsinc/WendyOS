"""Bounded supervision timing comparison on the existing Docker Linux VM.

Eight cells compare host-bind and VM-native durable storage under synthetic
repair load and explicit faults. No physical identity, hardware or new VM is
used. Valid fallback is an availability observation, not a safety failure.
"""
import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import stat
import subprocess
import sys
import tempfile
import time
import uuid

from supervision_site import command, private_json, sanitized_diagnostics
from supervision_load_metrics import metric_delta, validate_snapshot
from virtual_site import ROOT

PROFILES = ("baseline-revocation", "asymmetric", "executor-stall", "disk-delay")
STORAGE = ("host-bind", "vm-volume")
OWNERSHIP_LABEL = "wendy.world.supervision-load.run"

# Runs only in a scoped, network-disabled helper before node restart. It reads
# the recreated container's exact VM bind mount and prints no configuration.
CONFIG_READBACK = r'''
import hashlib,json,os,re,stat,sys
report={'schema':'wendy.world.config-readback.v2','expected_bytes':0,'expected_sha256':'','observed_bytes':0,'observed_sha256':'','exact':False,'error_class':'expected_input_invalid'}
try:
    expected_digest,expected_length=sys.argv[1],int(sys.argv[2])
    if not 0<expected_length<=1<<20 or re.fullmatch('[0-9a-f]{64}',expected_digest) is None:raise ValueError()
    report.update(expected_bytes=expected_length,expected_sha256=expected_digest,error_class='input_unavailable')
    fd=os.open('/config.json',os.O_RDONLY|os.O_NONBLOCK|os.O_NOFOLLOW)
    with os.fdopen(fd,'rb') as source:
        report['error_class']='not_regular'
        info=os.fstat(source.fileno())
        if not stat.S_ISREG(info.st_mode):raise ValueError()
        report['error_class']='read_failed'
        data=source.read((1<<20)+1)
    report['observed_bytes']=len(data)
    report['error_class']='read_bound'
    if len(data)>1<<20:raise ValueError()
    report['observed_sha256']=hashlib.sha256(data).hexdigest()
    report['exact']=len(data)==expected_length and report['observed_sha256']==expected_digest
    report['error_class']='' if report['exact'] else 'byte_mismatch'
except (OSError,ValueError,IndexError):pass
print(json.dumps(report,separators=(',',':')))
sys.exit(0 if report['exact'] else 1)
'''

PROXY = r'''
import http.client, http.server, json, threading, time
from pathlib import Path
slots = threading.BoundedSemaphore(4)
printing = threading.Lock()
class Server(http.server.ThreadingHTTPServer):
    daemon_threads = True
    def process_request(self, request, address):
        if not slots.acquire(False): request.close(); return
        super().process_request(request,address)
    def process_request_thread(self,request,address):
        try: super().process_request_thread(request,address)
        finally: slots.release()
    def get_request(self):
        request,address=super().get_request(); request.settimeout(1); return request,address
class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self,*_): pass
    def do_POST(self):
        if self.path not in ('/v1/tasks/supervision/challenge','/v1/tasks/supervision/ack'):
            self.send_error(404); return
        try:
            n=int(self.headers.get('Content-Length','-1'))
            authorization=self.headers.get('X-World-Authorization','')
            if not 0<n<=65536 or not 0<len(authorization)<=65536: self.send_error(400); return
            body=self.rfile.read(n)
            if len(body)!=n: self.send_error(400); return
            mode=Path('/control/mode').read_text().strip()
            started=time.monotonic_ns()
            connection=http.client.HTTPConnection('world-node-1',8080,timeout=1)
            try:
                connection.request('POST',self.path,body,{'Content-Type':'application/json','X-World-Authorization':authorization})
                response=connection.getresponse(); status=response.status; data=response.read(65537)
                if len(data)>65536: raise ValueError('response bound')
            finally: connection.close()
            delay=.07 if mode=='delay-ack-response' and self.path.endswith('/ack') else 0
            if delay: time.sleep(delay)
            drop=mode=='drop-ack-response' and self.path.endswith('/ack')
            with printing: print(json.dumps({'route':'ack' if self.path.endswith('/ack') else 'challenge','mode':mode,'status':status,'delay_ms':int(delay*1000),'dropped':drop,'elapsed_ns':time.monotonic_ns()-started}),flush=True)
            if drop: self.close_connection=True; return
            self.send_response(status); self.send_header('Content-Type','application/json'); self.send_header('Content-Length',str(len(data))); self.end_headers(); self.wfile.write(data)
        except (OSError,ValueError,http.client.HTTPException): self.close_connection=True
Server(('0.0.0.0',8099),Handler).serve_forever(poll_interval=.1)
'''

SAMPLER = r'''
import datetime,json,math,os,time,urllib.request
from pathlib import Path
token=Path('/token').read_text().strip()
run=Path('/control/run-id').read_text().strip()
preload_path=Path('/control/preload-count')
preload_count=int(preload_path.read_text()) if preload_path.exists() else None
if preload_count is not None:
    if preload_count not in (0,200): raise ValueError('invalid preload count')
    ready={'schema':'wendy.world.sampler-gate.v1','run_id':run,'stage':'ready','ready_at_unix_ms':int(time.time()*1000)}
    with open('/metrics/sampler-ready.json','x') as out: os.chmod(out.name,0o600);json.dump(ready,out)
    deadline=time.monotonic()+30
    while not Path('/control/supervision-release.json').exists():
        if time.monotonic()>deadline: raise ValueError('sampler gate timed out')
        time.sleep(.01)
    release=json.loads(Path('/control/supervision-release.json').read_text())
    if (release.get('schema')!='wendy.world.supervision-gate-release.v1' or release.get('run_id')!=run or
        type(release.get('release_unix_ms')) is not int or type(release.get('window_end_unix_ms')) is not int or
        release['window_end_unix_ms']-release['release_unix_ms']!=24000 or abs(release['release_unix_ms']-int(time.time()*1000))>2000): raise ValueError('invalid sampler release')
    due=time.monotonic()+max(0,(release['release_unix_ms']-time.time()*1000)/1000)
    while time.monotonic()<due: time.sleep(min(.01,max(0,due-time.monotonic())))
    ready.update(stage='sampling',started_at_unix_ms=int(time.time()*1000),release_unix_ms=release['release_unix_ms'])
    with open('/metrics/sampler-ready.json','w') as out: json.dump(ready,out)
expected_source={'device':'sim-load-publisher','app':'wendy-world','service':'sim-load-publisher','stream':'state'}
rows=[]; errors={'physics':0,'task':0,'replica':0}
start=time.monotonic(); last_write=0; last_replica=0; replica={}
reason='running'
def read_json(path,bound):
    with open(path,'rb') as source: data=source.read(bound+1)
    if len(data)>bound: raise ValueError('file bound')
    return json.loads(data)
def output():
    data={'schema':'wendy.world.supervision-load-samples.v1','elapsed_ns':int((time.monotonic()-start)*1e9),'termination_reason':reason,'truncated':reason in ('duration_limit','sample_limit'),'errors':errors,'samples':rows}
    temporary=Path('/metrics/.samples-next')
    with open(temporary,'w') as out: os.chmod(temporary,0o600); json.dump(data,out,separators=(',',':'))
    temporary.replace('/metrics/samples.json')
while time.monotonic()-start<30 and len(rows)<320:
    tick=time.monotonic(); row={'elapsed_ns':int((tick-start)*1e9),'sample_unix_ms':int(time.time()*1000)}
    try:
        request=urllib.request.Request('http://actuator:8091/v1/state',headers={'Authorization':'Bearer '+token})
        with urllib.request.urlopen(request,timeout=.2) as response:
            raw=response.read(65537)
        if len(raw)>65536: raise ValueError('response bound')
        value=json.loads(raw)
        x,v,command=value['position_m'][0],value['velocity_mps'][0],value['commanded_velocity_mps']
        if not all(type(n) in (int,float) and math.isfinite(n) for n in (x,v,command)): raise ValueError('nonfinite state')
        row.update({'position_x':x,'velocity_x':v,'commanded_velocity':command})
    except (OSError,ValueError,KeyError): errors['physics']+=1
    try:
        record=read_json('/data1/node-1.tasks',8<<20)['state']['records'].get('load-mission')
        row['phase']=record['record']['status']['Phase'] if record else 'absent'
    except (OSError,ValueError,KeyError): errors['task']+=1
    if tick-last_replica>=1:
        replica={}
        for index in (1,2,3):
            try:
                saved=read_json('/data%d/node-%d.state'%(index,index),16<<20)
                retained=saved['state']['world']['entries']
                if retained is None: retained=[]
                if not isinstance(retained,list): raise ValueError('invalid entries')
                entries=[v for v in retained if v['component']['key']['type']=='wendy.load.scalar.v1']
                valid=[]; ages=[]; remaining=[]; unexpired=0
                for entry in entries:
                    c=entry['component']; key=c['key']; provenance=c['provenance']
                    matches=(key['tenant']=='lab' and key['site']=='synthetic-office' and key['source']==expected_source and
                        key['entity'].startswith('load/'+run+'/') and c['frame']=={'id':'office-floor','revision':'office-frame-v1'} and
                        c['timing']['clock_domain']=='simulation:'+run and provenance['origin']=='simulated' and
                        provenance['simulation']['run_id']==run and provenance['app_version']=='supervision-load-fixture-v1')
                    if not matches: continue
                    valid.append(c)
                    sampled=datetime.datetime.fromisoformat(c['timing']['sample_time'].replace('Z','+00:00')).timestamp()
                    expires=datetime.datetime.fromisoformat(entry['expires_at'].replace('Z','+00:00')).timestamp()
                    age=int((time.time()-sampled)*1000); ages.append(age)
                    remaining.append(int((expires-time.time())*1000))
                    if age>=0 and time.time()<expires: unexpired+=1
                replica[str(index)]={'retained_entities':len(valid),'unexpired_entities':unexpired,
                    'observed_unix_ms':int(time.time()*1000),
                    'source_run_frame_mismatches':len(entries)-len(valid),
                    'max_sequence':max((c['version']['sequence'] for c in valid),default=0),
                    'oldest_sample_age_ms':max(ages,default=None),'newest_sample_age_ms':min(ages,default=None)}
                if preload_count is not None:
                    replica[str(index)]['minimum_remaining_ms']=min(remaining,default=None)
                    replica[str(index)]['expected_retained_count']=preload_count
            except (OSError,ValueError,KeyError): errors['replica']+=1
        last_replica=tick
        row['replicas']=replica.copy()
    rows.append(row)
    if tick-last_write>=1: output(); last_write=tick
    if Path('/control/stop-sampler').exists(): reason='stop_requested'; break
    time.sleep(max(0,.1-(time.monotonic()-tick)))
if reason=='running': reason='sample_limit' if len(rows)>=320 else 'duration_limit'
output()
'''


def bounded_json(path, limit=256 << 10):
    with open(path, "rb") as source:
        data = source.read(limit + 1)
    if len(data) > limit:
        raise ValueError("diagnostic artifact exceeds bound")
    return json.loads(data)


def write_stage_config(path, value):
    """Create one immutable stage config; never replace an existing mounted inode."""
    if type(value) is not dict:raise ValueError('config object required')
    data=json.dumps(value,separators=(',',':'),allow_nan=False).encode()
    if not 0<len(data)<=1<<20:raise ValueError('config exceeds bound')
    fd=os.open(path,os.O_WRONLY|os.O_NONBLOCK|os.O_NOFOLLOW|os.O_CREAT|os.O_EXCL,0o600)
    try:
        info=os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_mode&0o077:raise ValueError('private regular config required')
        position=0
        while position<len(data):
            wrote=os.write(fd,data[position:])
            if wrote<=0:raise OSError('config write made no progress')
            position+=wrote
        os.fsync(fd)
    finally:os.close(fd)
    parent=os.open(Path(path).parent,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW)
    try:os.fsync(parent)
    finally:os.close(parent)
    return {'schema':'wendy.world.config-readback.v1','bytes':len(data),'sha256':hashlib.sha256(data).hexdigest(),'exact':True}


def node_recreation_snapshot(name, run_id, created=False):
    """Inspect only owned stopped containers. Never export their full configuration."""
    raw=command(['docker','inspect',name],timeout=5)
    if len(raw)>128<<10:raise ValueError('container inspection bound')
    parsed=json.loads(raw)
    if type(parsed) is not list or len(parsed)!=1:raise ValueError('container inspection cardinality')
    value=parsed[0]; state=value['State']; config=value['Config']
    if (config.get('Labels',{}).get(OWNERSHIP_LABEL)!=run_id or
        state.get('Status') not in (('created','exited') if created else ('exited',)) or
        state.get('Running') is not False or state.get('Paused') is not False or state.get('Restarting') is not False):
        raise ValueError('owned stopped container required')
    if re.fullmatch('[0-9a-f]{64}',value['Id']) is None or re.fullmatch('sha256:[0-9a-f]{64}',value['Image']) is None:
        raise ValueError('invalid container identity')
    mounts={}
    for mount in value['Mounts']:
        destination=mount['Destination']
        if destination in mounts:raise ValueError('duplicate container mount')
        mounts[destination]={key:mount.get(key,'') for key in ('Type','Name','Source','Destination','RW','Propagation')}
    networks={key:sorted(item.get('Aliases') or []) for key,item in value['NetworkSettings']['Networks'].items()}
    return {'container_id':value['Id'],'image_id':value['Image'],'user':config['User'],
            'entrypoint':config.get('Entrypoint'),'argv':config.get('Cmd'),'mounts':mounts,'networks':networks,
            'process':{key:state.get(key) for key in ('Status','ExitCode','OOMKilled','FinishedAt')}}


def validate_node_recreation(snapshot, plan, original=None):
    """Require exact durable bindings; only the config source and container ID change."""
    if snapshot['user']!=plan['user'] or snapshot['argv']!=['--config','/config.json']:
        raise ValueError('node runtime identity changed')
    expected_destinations={'/state','/config.json','/metrics'}|({'/control'} if plan['control_mount'] else set())
    if snapshot['mounts'].keys()!=expected_destinations or snapshot['mounts']['/config.json']['RW'] is not False:
        raise ValueError('node mount set changed')
    expected_networks={plan['network'],*(name for name,alias in plan['extra_networks'])}
    if snapshot['networks'].keys()!=expected_networks or plan['alias'] not in snapshot['networks'][plan['network']]:
        raise ValueError('node network changed')
    if any(alias not in snapshot['networks'][name] for name,alias in plan['extra_networks']):
        raise ValueError('node network alias changed')
    if original is not None:
        if snapshot['container_id']==original['container_id'] or any(snapshot[key]!=original[key] for key in ('image_id','user','entrypoint','argv')):
            raise ValueError('node recreation identity mismatch')
        for destination in expected_destinations-{'/config.json'}:
            if snapshot['mounts'][destination]!=original['mounts'][destination]:
                raise ValueError('node retained mount changed')
        before=original['mounts']['/config.json']; after=snapshot['mounts']['/config.json']
        if before['Source']==after['Source'] or any(before[key]!=after[key] for key in ('Type','Destination','RW','Propagation')):
            raise ValueError('fresh immutable config binding required')


def recreate_preload_nodes(plans, run_id, configs, result, verify_config, set_stage):
    """Uniformly recreate all three stopped nodes, then verify every new VM mount.

    No start is issued here. Removal uses the inspected immutable container ID,
    without force or volume removal. Failure leaves the cell for owned cleanup.
    """
    if len(plans)!=3 or sorted(plans)!=[1,2,3]:raise ValueError('three node plans required')
    originals={}; expected={}
    lifecycle=result.setdefault('preload_container_recreation',{})
    for index,plan in sorted(plans.items()):
        set_stage('preload_immutable_config_create_'+str(index))
        expected[index]=write_stage_config(plan['measured_config'],configs[index])
        original=node_recreation_snapshot(plan['name'],run_id)
        validate_node_recreation(original,plan)
        originals[index]=original
        lifecycle[str(index)]={'old_container_id':original['container_id'],'image_id':original['image_id'],
            'user':original['user'],'old_process':original['process'],'old_state_mount':original['mounts']['/state'],
            'lifecycle':'owned stopped container replacement; retained durable mounts; no state initialization',
            'state':'prepared'}
    for index,plan in sorted(plans.items()):
        set_stage('preload_owned_container_recreate_'+str(index))
        current=node_recreation_snapshot(plan['name'],run_id)
        if current!=originals[index]:raise ValueError('stopped container changed before removal')
        command(['docker','rm',current['container_id']],timeout=5)
        flags=list(plan['flags'])
        position=flags.index(plan['config_mount'])
        flags[position]=f"type=bind,src={plan['measured_config']},dst=/config.json,readonly"
        created_id=command(['docker','create','--pull=never','--name',plan['name'],'--label',f'{OWNERSHIP_LABEL}={run_id}',
                 '--network',plan['network'],'--user',plan['user']]+flags+[current['image_id'],'--config','/config.json'],timeout=10).decode().strip()
        if re.fullmatch('[0-9a-f]{64}',created_id) is None:raise ValueError('invalid created container identity')
        for network,alias in plan['extra_networks']:
            command(['docker','network','connect','--alias',alias,network,created_id],timeout=5)
        recreated=node_recreation_snapshot(created_id,run_id,created=True)
        if recreated['container_id']!=created_id:raise ValueError('created container identity changed')
        validate_node_recreation(recreated,plan,current)
        lifecycle[str(index)].update({'new_container_id':recreated['container_id'],
            'new_state_mount':recreated['mounts']['/state'],'retained_mounts_equal':True,
            'network_names':sorted(recreated['networks']),'state':'recreated_stopped'})
    for index in sorted(plans):
        set_stage('preload_immutable_config_vm_readback_'+str(index))
        report=verify_config(index,expected[index])
        result.setdefault('preload_restart_config_readback',{})[str(index)]=report
        if not report['exact']:raise RuntimeError('VM config readback refused; see structured result')
        lifecycle[str(index)]['state']='readback_verified_stopped'


def config_readback_report(data, expected, exit_code=0):
    """Validate a bounded success or failure without accepting arbitrary output."""
    if len(data)>1024:raise ValueError('config readback report bound')
    report=json.loads(data)
    fields={'schema','expected_bytes','expected_sha256','observed_bytes','observed_sha256','exact','error_class'}
    errors={'','expected_input_invalid','input_unavailable','not_regular','read_failed','read_bound','byte_mismatch'}
    if (type(report) is not dict or report.keys()!=fields or report['schema']!='wendy.world.config-readback.v2' or
        type(report['expected_bytes']) is not int or report['expected_bytes']!=expected['bytes'] or
        report['expected_sha256']!=expected['sha256'] or type(report['observed_bytes']) is not int or
        not 0<=report['observed_bytes']<=(1<<20)+1 or not isinstance(report['observed_sha256'],str) or
        (report['observed_sha256'] and re.fullmatch('[0-9a-f]{64}',report['observed_sha256']) is None) or
        type(report['exact']) is not bool or report['error_class'] not in errors or type(exit_code) is not int or exit_code not in (0,1)):
        raise ValueError('config readback structure mismatch')
    equal=report['observed_bytes']==expected['bytes'] and report['observed_sha256']==expected['sha256']
    if (report['exact']!=(exit_code==0) or report['exact']!=(report['error_class']=='') or
        (report['exact'] and not equal) or
        (report['error_class']=='byte_mismatch' and (equal or not report['observed_sha256']))):
        raise ValueError('config readback outcome mismatch')
    return report


def execute_config_readback(argv, expected):
    """Retain only fixed schema/numeric exit; discard Docker stderr and tracebacks."""
    try:
        completed=subprocess.run(argv,capture_output=True,timeout=5)
    except subprocess.TimeoutExpired:
        return {'exact':False,'error_class':'docker_helper_timeout','helper_exit_code':None,'report_status':'unavailable'}
    except OSError:
        return {'exact':False,'error_class':'docker_launch_failed','helper_exit_code':None,'report_status':'unavailable'}
    try:
        report=config_readback_report(completed.stdout,expected,completed.returncode)
    except (ValueError,KeyError,TypeError):
        return {'exact':False,'error_class':'docker_launch_failed' if completed.returncode in (125,126,127) else 'helper_report_missing_or_invalid',
            'helper_exit_code':completed.returncode,'report_status':'unavailable','expected_bytes':expected['bytes'],'expected_sha256':expected['sha256']}
    return {**report,'helper_exit_code':completed.returncode,'report_status':'validated'}


def until(fn, predicate, timeout=12):
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        try:
            value = fn()
            if predicate(value):
                return value
        except (RuntimeError, OSError, KeyError, ValueError):
            pass
        time.sleep(.05)
    raise RuntimeError("fixture readiness unavailable within bound")


def failure_metadata(error):
    # Extract only a numeric status from known client/urllib diagnostics. Never
    # retain or print the error text, which can contain an HTTP response body.
    metadata={"failure_class":type(error).__name__}
    matched=re.search(r"\b(?:node HTTP|HTTP Error) ([1-5][0-9]{2})\b",str(error))
    if matched: metadata["failure_http_status"]=int(matched.group(1))
    return metadata


def process_time_ms(value):
    # Docker emits RFC3339Nano. Python 3.9's fromisoformat rejects nine-digit
    # fractions; parse explicitly and keep integer milliseconds without floats.
    if not isinstance(value,str) or not 20<=len(value)<=35:
        raise ValueError("invalid bounded process timestamp")
    matched=re.fullmatch(r"([0-9]{4})-([0-9]{2})-([0-9]{2})T([0-9]{2}):([0-9]{2}):([0-9]{2})(?:\.([0-9]{1,9}))?(Z|[+-][0-9]{2}:[0-9]{2})",value)
    if matched is None:
        raise ValueError("invalid process timestamp format")
    year,month,day,hour,minute,second=(int(part) for part in matched.groups()[:6])
    fraction,zone=matched.groups()[6:]
    offset=0
    if zone!="Z":
        zone_hour,zone_minute=int(zone[1:3]),int(zone[4:6])
        if zone=="-00:00" or zone_hour>23 or zone_minute>59:
            raise ValueError("unknown or invalid process timestamp offset")
        offset=(zone_hour*60+zone_minute)*(1 if zone[0]=="+" else -1)
    instant=datetime.datetime(year,month,day,hour,minute,second,
                              tzinfo=datetime.timezone(datetime.timedelta(minutes=offset)))
    delta=instant-datetime.datetime(1970,1,1,tzinfo=datetime.timezone.utc)
    return (delta.days*86400+delta.seconds)*1000+int(((fraction or "")+"000")[:3])


def process_summary(state):
    return {"running":state["Running"],"exit_code":state["ExitCode"],"oom_killed":state["OOMKilled"],
            "started_at_unix_ms":process_time_ms(state["StartedAt"]),
            "finished_at_unix_ms":None if state["Running"] else process_time_ms(state["FinishedAt"])}


def terminal_diagnostic(data):
    """Classify only static CLI diagnostics; discard all raw log content."""
    if len(data)>1<<20:
        return {"class":"log_budget_exceeded","diagnostics_error_seen":False}
    result={"class":"no_recognized_terminal_error","diagnostics_error_seen":False}
    for line in data.decode(errors="replace").splitlines():
        if line=="supervision diagnostics export failed; supervision continues":
            result["diagnostics_error_seen"]=True
        match=re.fullmatch(r"supervision (challenge|acknowledgement) exchange failed \((timeout|cancelled|transport or response|HTTP [1-5][0-9]{2})\)",line)
        if match:
            result.update({"exchange":match.group(1),"class":"http_refusal" if match.group(2).startswith("HTTP ") else match.group(2).replace(" ","_")})
            if match.group(2).startswith("HTTP "): result["http_status"]=int(match.group(2)[5:])
        elif line in ("supervisor authority no longer valid","untrusted or expired supervision challenge"):
            result["class"]="authority_or_challenge_expired"
        elif line in ("supervision challenge deadline exceeded","supervision receipt arrived after exchange deadline"):
            result["class"]="exchange_deadline"
        elif line=="untrusted, stale or mismatched supervision receipt":
            result["class"]="receipt_refused"
        elif line=="gateway boot or executor generation changed; explicit restart required":
            result["class"]="generation_changed"
    return result


def receipt_log_summary(data, run_id, principal, window_start_ms, window_end_ms):
    """Count bounded public summaries already verified by the CLI, not proofs."""
    result={"complete":True,"successful_receipts":0,"challenge_issued_in_window_count":0,
            "duplicate_receipt_lines":0,"invalid_receipt_lines":0,
            "first_challenge_issued_at_unix_ms":None,"last_receipt_expires_at_unix_ms":None,
            "window_start_unix_ms":window_start_ms,"window_end_unix_ms":window_end_ms,
            "count_semantics":"unique_cli_receipts_grouped_by_signed_challenge_issue_time"}
    if len(data)>1<<20:
        result.update({"complete":False,"status":"log_budget_exceeded"}); return result
    lines=data.decode(errors="replace").splitlines()
    if len(lines)>500:
        result.update({"complete":False,"status":"log_line_budget_exceeded"}); return result
    seen=set()
    for line in lines:
        if not line.startswith("{"): continue
        try: value=json.loads(line)
        except ValueError:
            result["invalid_receipt_lines"]+=1; continue
        if not isinstance(value,dict) or value.get("schema")!="wendy.world.supervision-lease.v1": continue
        issued,expires=value.get("issued_at_unix_ms"),value.get("expires_at_unix_ms")
        challenge=value.get("challenge_id")
        if (value.get("domain")!="simulation" or value.get("run_id")!=run_id or value.get("task_id")!="load-mission" or
                value.get("operator_principal")!=principal or value.get("gateway_principal")!="lab/node-1" or
                type(issued) is not int or type(expires) is not int or not 0<issued<expires<=(1<<63)-1 or expires-issued>500 or
                not isinstance(challenge,str) or re.fullmatch(r"[0-9a-f]{64}",challenge) is None or
                type(value.get("executor_epoch")) is not int or not 0<value["executor_epoch"]<=(1<<64)-1 or
                any(not isinstance(value.get(field),str) or re.fullmatch(r"[0-9a-f]{64}",value[field]) is None for field in ("ack_sha256","spec_sha256"))):
            result["invalid_receipt_lines"]+=1; continue
        if challenge in seen:
            result["duplicate_receipt_lines"]+=1; continue
        seen.add(challenge)
        result["successful_receipts"]+=1
        if window_start_ms<=issued<=window_end_ms: result["challenge_issued_in_window_count"]+=1
        first=result["first_challenge_issued_at_unix_ms"]
        result["first_challenge_issued_at_unix_ms"]=issued if first is None else min(first,issued)
        result["last_receipt_expires_at_unix_ms"]=max(result["last_receipt_expires_at_unix_ms"] or 0,expires)
    if result["invalid_receipt_lines"]: result["complete"]=False
    return result


def supervision_delta_samples_present(measurements):
    # Snapshot intervals can start before the declared fixture window. Require
    # actual exchange samples and avoid calling this window coverage.
    return all(measurements.get(key,{}).get("window_delta_available",False) and
               any(row.get("name")=="supervisor.exchange" and row.get("count",0)>0
                   for row in measurements[key].get("durations",[])) for key in ("a","b"))


def collect_measurement(before, after, state, window_start_ms, read_ms, diagnostic_failed=False):
    """Keep validated lifetime evidence even when a process exited pre-window."""
    validate_snapshot(before)
    validate_snapshot(after)
    finished=None if state["Running"] else process_time_ms(state["FinishedAt"])
    lag=(read_ms if finished is None else finished)-after["captured_at_unix_ms"]
    common={"lifetime_before":before,"lifetime_after":after,
            "snapshot_lag_ms_from_process_end_or_read":lag,"diagnostics_error_seen":diagnostic_failed}
    if before==after and finished is not None and finished<=window_start_ms:
        return {**common,"status":"process_exited_before_window","window_delta_available":False,
                "complete":abs(lag)<=3000 and not diagnostic_failed}
    try:
        delta=metric_delta(before,after)
    except ValueError:
        return {**common,"status":"window_delta_invalid_or_unavailable","window_delta_available":False,"complete":False}
    return {**delta,**common,"status":"window_observed","window_delta_available":True,
            "complete":abs(lag)<=3000 and not diagnostic_failed}


def resource_ownership(kind, name, expected):
    label_path=".Config.Labels" if kind=="container" else ".Labels"
    template='{{index '+label_path+' "'+OWNERSHIP_LABEL+'"}}'
    checked=subprocess.run(["docker",kind,"inspect","--format",template,name],capture_output=True,timeout=4)
    if checked.returncode:
        error=checked.stderr.decode(errors="replace").lower()
        return "missing" if "no such" in error or "not found" in error else "unknown"
    return "owned" if checked.stdout.decode().strip()==expected else "foreign"


def configure_repair(config, enabled):
    """Only the explicit control removes repair peers; transport stays intact."""
    if not enabled:
        config["peers"]=[]
    return config


def preload_verification_report(data,exit_code,run_id,node_index,count):
    if len(data)>16384:raise ValueError("preload verification output bound")
    value=json.loads(data)
    base={"schema","domain","run_id","node_index","count"}
    if (type(value) is not dict or value.get("domain")!="simulation" or value.get("run_id")!=run_id or
        type(value.get("node_index")) is not int or value["node_index"]!=node_index or
        type(value.get("count")) is not int or value["count"]!=count):raise ValueError("preload verification context mismatch")
    if exit_code==0:
        if (value.keys()!=base|{"epoch","verified_at_unix_ms","minimum_remaining_ms","exact_signed_set"} or
            value.get("schema")!="wendy.world.preload-verification.v1" or value.get("exact_signed_set") is not True or
            any(type(value.get(key)) is not int or value[key]<0 for key in ("epoch","verified_at_unix_ms","minimum_remaining_ms")) or value["epoch"]==0):
            raise ValueError("invalid preload verification success")
    else:
        classes={"scope_or_manifest","state_unavailable","state_format","checkpoint_checksum","checkpoint_cardinality",
                 "clock_regression","checkpoint_invalid","proof_cardinality","proof_scope_or_lifetime","origin_signature",
                 "record_mismatch","record_freshness","trust_or_revocation","verification_refused"}
        if (value.keys()!=base|{"failure_class"} or value.get("schema")!="wendy.world.preload-verification-failure.v1" or
            value.get("failure_class") not in classes):raise ValueError("invalid preload verification failure")
    return value


def preload_boundary(metrics):
    result={"read_started_at_unix_ms":int(time.time()*1000),"snapshots":{},"errors":{}}
    for key in ("node1","node2","node3","a","b"):
        try:
            value=bounded_json(metrics/(key+".json"));validate_snapshot(value)
            result["snapshots"][key]=value
        except (OSError,ValueError,KeyError) as error:
            result["errors"][key]=type(error).__name__
    result["read_finished_at_unix_ms"]=int(time.time()*1000)
    return result


def quiet_preload_window(started,metrics,before,clock=time.monotonic,sleep=time.sleep):
    """No subprocess/network/Docker calls in the fixed startup-inclusive window."""
    first={}
    while clock()<started+2:
        for key in ("a","b"):
            if key in first:continue
            try:
                value=bounded_json(metrics/(key+".json"));validate_snapshot(value)
                first[key]={"snapshot":value,"read_at_unix_ms":int(time.time()*1000)}
            except (OSError,ValueError,KeyError):pass
        sleep(min(.01,max(0,started+2-clock())))
    middle=preload_boundary(metrics)
    sleep(max(0,started+24-clock()))
    end=preload_boundary(metrics)
    return {"before_release":before,"first_supervisor_snapshots":first,"at_two_seconds":middle,"at_window_end":end}


def preload_phase_metrics(boundaries):
    start=dict(boundaries["before_release"]["snapshots"])
    for key,value in boundaries["first_supervisor_snapshots"].items():start[key]=value["snapshot"]
    middle=boundaries["at_two_seconds"]["snapshots"];end=boundaries["at_window_end"]["snapshots"]
    output={}
    for phase,before,after in (("startup_0_to_2",start,middle),("later_2_to_24",middle,end)):
        output[phase]={}
        for key in ("node1","node2","node3","a","b"):
            if key not in before or key not in after:
                output[phase][key]={"status":"snapshot_unavailable"};continue
            if before[key]==after[key]:
                output[phase][key]={"status":"identical_snapshot_no_delta","lifetime_snapshot":after[key]};continue
            try:output[phase][key]={"status":"snapshot_interval",**metric_delta(before[key],after[key])}
            except ValueError:output[phase][key]={"status":"invalid_delta"}
        if phase=="startup_0_to_2":
            for key in ("a","b"):
                output[phase][key]["interval_semantics"]="first available supervisor snapshot to two-second snapshot; startup exchanges before the first snapshot are outside this delta"
                if key in after:output[phase][key]["lifetime_at_two_second_snapshot"]=after[key]
    return output


def onset_order(repetitions, storage=None, variant=None):
    """Counterbalance both storage order and fixed/changing order, without retries."""
    if type(repetitions) is not int or not 1<=repetitions<=3:raise ValueError("onset repetition bound")
    cells=[]
    for repetition in range(repetitions):
        stores=(storage,) if storage else (STORAGE if repetition%2==0 else tuple(reversed(STORAGE)))
        variants=(variant,) if variant else (("fixed","changing") if repetition%2==0 else ("changing","fixed"))
        for store in stores:
            for value in variants:cells.append((repetition+1,store,value))
    return cells


def receipt_observations(path,run_id,operator):
    value=bounded_json(path,128<<10)
    fields={"schema","run_id","operator","state","started_at_unix_ms","finished_at_unix_ms","exit_code","receipts","terminal_class","exchange","http_status","invalid_lines","truncated","write_failed"}
    if (type(value) is not dict or value.keys()!=fields or value["schema"]!="wendy.world.supervision-receipts.v1" or
        value["run_id"]!=run_id or value["operator"]!=operator or value["state"] not in ("running","exited") or
        type(value["receipts"]) is not list or len(value["receipts"])>420):raise ValueError("receipt observation scope")
    if (any(type(value[k]) is not int for k in ('started_at_unix_ms','finished_at_unix_ms','exit_code','http_status')) or
        value['started_at_unix_ms']<=0 or value['finished_at_unix_ms']<0 or value['exit_code'] not in range(-1,256) or
        value['http_status'] not in (0,*range(100,600)) or value['exchange'] not in ('','challenge','acknowledgement') or
        value['terminal_class'] not in ('','timeout','cancelled','transport_or_response','http_refusal','diagnostics_export_failed','authority_or_challenge_expired','exchange_deadline','receipt_refused','generation_changed','local_duration_or_cancellation','child_start_failed')):raise ValueError('receipt observation metadata')
    last_elapsed=-1
    for row in value["receipts"]:
        if (type(row) is not dict or row.keys()!={"issued_at_unix_ms","expires_at_unix_ms","observed_at_unix_ms","observed_elapsed_ns"} or
            any(type(v) is not int for v in row.values()) or not 0<row["issued_at_unix_ms"]<row["expires_at_unix_ms"] or
            row["expires_at_unix_ms"]-row["issued_at_unix_ms"]>500 or row["observed_at_unix_ms"]<=0 or row["observed_elapsed_ns"]<0 or row["observed_elapsed_ns"]<last_elapsed):raise ValueError("receipt observation bounds")
        last_elapsed=row["observed_elapsed_ns"]
    if type(value['invalid_lines']) is not int or value['invalid_lines']<0 or type(value['truncated']) is not bool or type(value['write_failed']) is not bool:raise ValueError('receipt status types')
    return value


def onset_live_observation(metrics,run_id,now_ms):
    """Historical CLI success observations, not a gateway authority lookup."""
    result={"observed_at_unix_ms":now_ms,"eligible":False,"operators":{}}
    try:
        rows=bounded_json(metrics/'samples.json')['samples']
        sample=rows[-1]
        result['sample']={key:sample.get(key) for key in ('sample_unix_ms','phase','velocity_x','commanded_velocity')}
        result['sample_age_ms']=now_ms-sample['sample_unix_ms']
        good=sample.get('phase')=='running' and 0<=result['sample_age_ms']<=1200
        for key in ('a','b'):
            observed=receipt_observations(metrics/f'receipts-{key}.json',run_id,key)
            latest=observed['receipts'][-1] if observed['receipts'] else None
            live=(observed['state']=='running' and not observed['invalid_lines'] and not observed['truncated'] and not observed['write_failed'] and
                latest is not None and latest['observed_at_unix_ms']<=now_ms<latest['expires_at_unix_ms'])
            result['operators'][key]={'observer_state':observed['state'],'receipt_count':len(observed['receipts']),'latest':latest,'observed_unexpired':live}
            good=good and live
        result['eligible']=good
    except (OSError,ValueError,KeyError,IndexError) as error:result['read_error']=type(error).__name__
    return result


def onset_receipt_summary(value,begin,end):
    rows=value['receipts']
    return {'complete':not value['truncated'] and not value['invalid_lines'] and not value['write_failed'],
        'successful_receipts':len(rows),'receipt_observed_in_window_count':sum(begin<=r['observed_at_unix_ms']<=end for r in rows),
        'challenge_issued_in_window_count':sum(begin<=r['issued_at_unix_ms']<=end for r in rows),
        'first_challenge_issued_at_unix_ms':rows[0]['issued_at_unix_ms'] if rows else None,
        'last_receipt_expires_at_unix_ms':rows[-1]['expires_at_unix_ms'] if rows else None,
        'first_receipt_observed_at_unix_ms':rows[0]['observed_at_unix_ms'] if rows else None,
        'last_receipt_observed_at_unix_ms':rows[-1]['observed_at_unix_ms'] if rows else None,
        'window_start_unix_ms':begin,'window_end_unix_ms':end,
        'count_semantics':'CLI verified receipt lines, with wrapper observation completion timestamps separate from signed issue time'}


def trigger_request_trace(control,nonce):
    # Publish a complete file once. Existing final names are never overwritten.
    private_json(control/'trace-trigger-next.json',{'version':1,'run_nonce':nonce})
    os.link(control/'trace-trigger-next.json',control/'trace-trigger.json')
    (control/'trace-trigger-next.json').unlink()


def collect_fixed_trace(path,nonce,wait_seconds=.75,clock=time.monotonic,sleep=time.sleep):
    """Bounded local postwindow collection; never changes the workload window."""
    started=clock();limit=started+wait_seconds;value=None;errors=0;status='unavailable';attempts=0
    last_start=None;last_end=None;late=False
    while True:
        now=clock()
        if now>=limit:break
        last_start=now-started;attempts+=1
        try:
            current=bounded_json(path,8<<20)
            if (type(current) is not dict or current.get('schema')!='wendy.world.request-trace.v1' or
                current.get('run_nonce')!=nonce or current.get('state') not in ('waiting','recording','draining','complete') or
                type(current.get('armed_at_unix_ms')) is not int or
                (current['state']=='waiting' and current['armed_at_unix_ms']!=0) or
                (current['state']!='waiting' and current['armed_at_unix_ms']<=0)):
                status='scope_or_state_invalid'
            else:value=current;status=current['state']
        except (OSError,ValueError):errors+=1
        finally:
            finished=clock();last_end=finished-started;late=finished>limit
        if status in ('complete','scope_or_state_invalid') or clock()>=limit:break
        sleep(min(.01,max(0,limit-clock())))
    return value,{'status':status,'complete':status=='complete','read_errors':errors,
                  'wait_seconds':clock()-started,'wait_limit_seconds':wait_seconds,
                  'wait_limit_semantics':'retry budget; an already-started filesystem read is not interruptible',
                  'read_attempts':attempts,'last_read_started_after_collection_seconds':last_start,
                  'last_read_completed_after_collection_seconds':last_end,
                  'read_completed_after_retry_budget':late,'complete_observed_within_retry_budget':status=='complete' and not late,
                  'measurement_window_extended':False}


def quiet_onset_window(started,metrics,control,run_id,variant,before,trace_nonce=None,clock=time.monotonic,sleep=time.sleep,wall=lambda:int(time.time()*1000),trace_at_seconds=None,supervisor_release_unix_ms=None):
    """Only bounded local file reads/writes; no subprocess or network operations."""
    stable_since=None;released=False;observations=[];read_errors=0;first={};middle=None
    result={'status':'onset_not_exercised','warmup_cap_seconds':6,'required_observed_live_seconds':2,'observations':observations,
        'docker_operations_in_window':False,'sampled_evidence_not_continuous_guarantee':True}
    fixed=None;ack_deadline=None
    if trace_at_seconds is not None:
        if type(trace_at_seconds) is not int or trace_at_seconds not in (0,6,18) or not trace_nonce:
            raise ValueError('fixed trace offset requires explicit trace and bounded offset')
        release_wall=supervisor_release_unix_ms if supervisor_release_unix_ms is not None else wall()+int((started-clock())*1000)
        fixed={'requested_offset_seconds':trace_at_seconds,'supervisor_release_unix_ms':release_wall,
               'scheduled_trigger_unix_ms':release_wall+trace_at_seconds*1000,'status':'scheduled',
               'ack_read_errors':0,'trigger_attempts':0,'publication_gate_independent':True,
               'zero_offset_does_not_cover_requests_before_gateway_arming':trace_at_seconds==0}
        result['fixed_trace']=fixed
    while clock()<started+24:
        now=clock()
        if now>=started+24:break
        if fixed is not None and fixed['status']=='scheduled' and started+trace_at_seconds<=now<started+24:
            fixed['at_trigger_observation']=onset_live_observation(metrics,run_id,wall())
            trigger_now=clock()
            if trigger_now>=started+24:
                fixed['status']='scheduled_offset_missed_window'
                break
            fixed.update(trigger_attempts=1,trigger_attempted_at_unix_ms=wall(),trigger_attempted_offset_seconds=trigger_now-started)
            try:
                trigger_request_trace(control,trace_nonce)
                fixed.update(status='awaiting_ack',trigger_created_at_unix_ms=wall(),trigger_created_offset_seconds=clock()-started)
                ack_deadline=clock()+.1
            except OSError:
                fixed.update(status='trigger_failed',error_class='local_trigger_io')
        if fixed is not None and fixed['status']=='awaiting_ack' and clock()>ack_deadline:
            fixed['status']='ack_not_observed_within_bound'
        if fixed is not None and fixed['status']=='awaiting_ack':
            try:
                trace=bounded_json(metrics/'request-trace.json',8<<20)
                if (type(trace) is dict and trace.get('schema')=='wendy.world.request-trace.v1' and trace.get('run_nonce')==trace_nonce and
                    trace.get('state') in ('recording','draining','complete') and type(trace.get('armed_at_unix_ms')) is int and trace['armed_at_unix_ms']>0):
                    difference=trace['armed_at_unix_ms']-fixed['supervisor_release_unix_ms']
                    within_bound=clock()<=ack_deadline
                    fixed.update(status='armed_observed' if within_bound else 'armed_observed_after_bound',
                        ack_observed_within_bound=within_bound,ack_observed_at_unix_ms=wall(),ack_observed_offset_seconds=clock()-started,
                        armed_at_unix_ms=trace['armed_at_unix_ms'],armed_minus_supervisor_release_ms=difference,
                        at_ack_observation=onset_live_observation(metrics,run_id,wall()))
                    if trace_at_seconds==0:fixed['startup_prefix_before_arming_ms']=difference if difference>=0 else None
            except (OSError,ValueError):fixed['ack_read_errors']+=1
            if fixed['status']=='awaiting_ack' and clock()>=ack_deadline:fixed['status']='ack_not_observed_within_bound'
        for key in ('a','b'):
            if key not in first:
                try:
                    snapshot=bounded_json(metrics/(key+'.json'));validate_snapshot(snapshot)
                    first[key]={'snapshot':snapshot,'read_at_unix_ms':wall()}
                except (OSError,KeyError,ValueError):pass
        if middle is None and now>=started+2:middle=preload_boundary(metrics)
        if not released and now>=started:
            observed=onset_live_observation(metrics,run_id,wall());observations.append(observed)
            if 'read_error' in observed:read_errors+=1
            if observed['eligible']:
                if stable_since is None:stable_since=now
            else:stable_since=None
            ready=stable_since is not None and now-stable_since>=2 and now<=started+6
            if ready or now>=started+6:
                result.update(status='released' if ready else 'onset_not_exercised',baseline_observed_live=ready,
                    baseline_started_at_seconds=stable_since-started if stable_since is not None else None,
                    decision_at_unix_ms=wall(),decision_at_seconds=now-started,baseline_evidence=observed)
                if trace_nonce and ready and fixed is None:
                    result['trace_trigger_created_at_unix_ms']=wall()
                    # O_EXCL trigger is single-use; no existing experiment is rearmed.
                    trigger_request_trace(control,trace_nonce)
                    trace_deadline=clock()+.1
                    result['trace_ready']=False
                    while clock()<trace_deadline:
                        try:
                            trace=bounded_json(metrics/'request-trace.json',8<<20)
                            if trace.get('schema')=='wendy.world.request-trace.v1' and trace.get('state')=='recording' and trace.get('run_nonce')==trace_nonce and trace.get('armed_at_unix_ms',0)>0:
                                result['trace_ready']=True;result['trace_armed_at_unix_ms']=trace['armed_at_unix_ms'];break
                        except (OSError,ValueError):pass
                        sleep(.005)
                release_ms=wall()+50
                if ready:
                    final_observation=onset_live_observation(metrics,run_id,wall())
                    result['after_trace_live_evidence']=final_observation
                    ready=(final_observation['eligible'] and
                        all(item.get('latest') and item['latest']['expires_at_unix_ms']>release_ms for item in final_observation['operators'].values()) and
                        (not trace_nonce or fixed is not None or result.get('trace_ready',False)))
                    if not ready:result.update(status='onset_not_exercised',release_refusal='trace_not_armed_or_receipt_not_live_at_planned_release')
                result['baseline_live_at_planned_release']=ready
                release={'schema':'wendy.world.publication-onset-release.v1','run_id':run_id,'variant':variant,
                    'release_unix_ms':release_ms,'window_end_unix_ms':wall()+max(0,int((started+24-clock())*1000)),
                    'publish':ready and variant=='changing'}
                private_json(control/'publication-next.json',release);(control/'publication-next.json').replace(control/'publication-release.json')
                result['release']=release;result['release_written_at_unix_ms']=wall();result['release_at_seconds']=clock()-started
                result['at_publication_release']=preload_boundary(metrics);released=True
        sleep(min(.05,max(0,started+24-clock())))
    result['read_errors']=read_errors
    if fixed is not None and fixed['status']=='scheduled':fixed['status']='scheduled_offset_missed_window'
    result['boundaries']={'before_release':before,'first_supervisor_snapshots':first,'at_two_seconds':middle or preload_boundary(metrics),'at_window_end':preload_boundary(metrics)}
    return result


def stop_timing_report(data, exit_code, host_elapsed_ns):
    """Accept only the fixed public helper report; never preserve raw output."""
    if len(data)>16384:
        raise ValueError("stop report exceeds bound")
    value=json.loads(data)
    fields={"schema","domain","request_attempted","response_matches_task","outcome","returned_phase",
            "error_class","http_status","initialization_ns","instruction_ns","request_ns",
            "response_validation_ns","total_ns","request_deadline_ns"}
    if type(value) is not dict or value.keys()!=fields:
        raise ValueError("invalid stop report fields")
    if (value["schema"]!="wendy.world.stop-timing.v1" or value["domain"]!="simulation" or
            type(value["request_attempted"]) is not bool or type(value["response_matches_task"]) is not bool or
            value["outcome"] not in ("terminal_response","unknown","not_attempted") or
            value["returned_phase"] not in ("","canceled","failed","succeeded","rejected") or
            value["error_class"] not in ("","input_refused","instruction_refused","timeout","cancelled","http_refusal","transport_or_response","response_mismatch") or
            type(value["http_status"]) is not int or value["http_status"] not in (0,*range(100,600))):
        raise ValueError("invalid stop report context")
    for key in ("initialization_ns","instruction_ns","request_ns","response_validation_ns","total_ns","request_deadline_ns"):
        if type(value[key]) is not int or not 0<=value[key]<=10_000_000_000:
            raise ValueError("invalid stop timing")
    if value["request_deadline_ns"]!=2_000_000_000 or value["total_ns"]<sum(value[key] for key in ("initialization_ns","instruction_ns","request_ns","response_validation_ns")):
        raise ValueError("invalid stop duration accounting")
    confirmed=value["response_matches_task"]
    if confirmed != (value["outcome"]=="terminal_response") or confirmed != bool(value["returned_phase"]):
        raise ValueError("inconsistent stop result")
    if (confirmed and (exit_code!=0 or not value["request_attempted"] or value["error_class"] or value["http_status"])) or (not confirmed and (exit_code==0 or not value["error_class"])):
        raise ValueError("inconsistent stop exit")
    if value["request_attempted"] != (value["outcome"]!="not_attempted"):
        raise ValueError("inconsistent stop attempt")
    return {**value,"host_docker_elapsed_ns":host_elapsed_ns,"helper_exit_code":exit_code,
            "timing_semantics":"request includes outer signing, HTTP and bounded response read; host includes Docker launch; no physical stop assertion"}


def run_cell(args, storage, profile, diagnostics=True, repair=True, preload_count=None, onset_variant=None):
    startup_control=profile=="startup-control"
    repair_control=profile=="repair-control"
    onset_control=profile=="onset-control"
    preload_control=profile in ("preload-control","onset-control")
    window_seconds=8 if startup_control else 24
    suffix = uuid.uuid4().hex[:10]
    run_id = "load-" + suffix
    names = {key:"supervision-load-"+key+"-"+suffix for key in
             ("node1","node2","node3","actuator","executor","proxy","sampler","publisher","preloader","a","b","control","peer")}
    containers, networks, volumes = [], [], []
    paused = []
    stage="initialize_fixture"
    user = f"{os.getuid()}:{os.getgid()}"
    result = {"storage":storage,"profile":profile,"run_id":run_id,"duration_seconds":window_seconds,"events":[],
              "diagnostics_enabled":diagnostics,"target":{"entities":preload_count if preload_control else 0 if startup_control else 200,"hz_per_entity":0 if startup_control or preload_control else 2},"measurements":{}}
    trace_nonce=uuid.uuid4().hex if onset_control and args.onset_trace else None
    if onset_control:
        result["onset_control"]={"variant":onset_variant,"retained_records":200,"repair_enabled":True,"record_ttl_seconds":300,"trace_enabled":bool(trace_nonce)}
        if args.onset_trace_at_seconds is not None:result['onset_control']['trace_at_seconds']=args.onset_trace_at_seconds
    if preload_control:
        result["preload_control"]={"retained_records":preload_count,"repair_enabled":repair,"uniform_node_restart":True,
            "node_lifecycle":"uniform stopped container recreation with immutable stage configs and retained state mounts",
            "record_ttl_seconds":300,"publisher_running_in_window":False,"docker_operations_in_window":False}
    if onset_control:
        result["preload_control"]["publisher_running_in_window"]=True
        result["target"]={"entities":200,"hz_per_entity":2 if onset_variant=="changing" else 0}
    if repair_control:
        result["repair_control"]={"enabled":repair,"meaning":"configured peer repair enabled" if repair else
            "all node peers empty; configured peer sync including world/task/status repair disabled; node2 publication remains local",
            "network_connected":True,"injected_faults":False}
    with tempfile.TemporaryDirectory(prefix="wendy-supervision-load-") as temporary:
        folder = Path(temporary)
        metrics = folder / "metrics"; metrics.mkdir(mode=0o700)
        control = folder / "control"; control.mkdir(mode=0o700)
        (control / "mode").write_text("pass")
        (control / "run-id").write_text(run_id)
        if preload_control:(control/"preload-count").write_text(str(preload_count))
        data_mounts = {}
        node_plans = {}

        def set_stage(value):
            nonlocal stage
            stage=value

        def verify_preload(minimum=60,deadline=None):
            verified={}
            for index in (1,2,3):
                name="supervision-load-verify-"+uuid.uuid4().hex[:10];containers.append(name)
                limit=5 if deadline is None else min(5,deadline-time.monotonic())
                if limit<=0:raise RuntimeError("preload verification deadline exceeded")
                verify_started=time.monotonic_ns()
                invocation=subprocess.run(["docker","run","--rm","--pull=never","--name",name,"--label",f"{OWNERSHIP_LABEL}={run_id}",
                    "--network","none","--user",user,"--entrypoint","/load-publisher",
                    "--mount",f"type=bind,src={args.publisher_binary},dst=/load-publisher,readonly",
                    "--mount",f"type=bind,src={control/'preload-manifest.json'},dst=/manifest.json,readonly",
                    "--mount",f"type=bind,src={folder/'site-trust.json'},dst=/trust.json,readonly",
                    "--mount",data_mounts[index]+",readonly",args.node_image,
                    "--verify-preload","--trust","/trust.json","--run-id",run_id,"--record-count",str(preload_count),
                    "--manifest","/manifest.json","--state-dir","/state","--node-index",str(index),"--minimum-fresh-seconds",str(minimum)],capture_output=True,timeout=limit)
                value=preload_verification_report(invocation.stdout,invocation.returncode,run_id,index,preload_count)
                attempts=result.setdefault("preload_verification_attempts",[])
                if len(attempts)<128:attempts.append({"stage":stage,"exit_code":invocation.returncode,"host_elapsed_ns":time.monotonic_ns()-verify_started,**value})
                else:result["preload_verification_attempts_truncated"]=True
                if invocation.returncode:raise RuntimeError("preload verification refused; see fixed failure class")
                verified[str(index)]=value
            return verified

        def process(name):
            return json.loads(command(["docker","inspect","--format","{{json .State}}",name],timeout=5))

        def verify_vm_config(index, expected):
            name='supervision-load-config-'+uuid.uuid4().hex[:10]
            containers.append(name)
            begin=time.monotonic_ns()
            response=execute_config_readback(['docker','run','--rm','--pull=never','--name',name,'--label',f'{OWNERSHIP_LABEL}={run_id}',
                '--network','none','--user',user,'--entrypoint','python',
                '--volumes-from',result['preload_container_recreation'][str(index)]['new_container_id']+':ro',
                args.actuator_image,'-c',CONFIG_READBACK,expected['sha256'],str(expected['bytes'])],expected)
            return {**response,'node_index':index,'host_elapsed_ns':time.monotonic_ns()-begin,
                'method':'VM read-only volumes-from the recreated stopped node, new immutable /config.json mount, after exclusive file and parent fsync; all three verified before any node start'}

        def launch(name, image, flags, argv, network="control", entrypoint=None):
            containers.append(name)
            cmd=["docker","run","-d","--pull=never","--name",name,"--label",f"{OWNERSHIP_LABEL}={run_id}","--network",names[network],"--user",user]
            if entrypoint: cmd += ["--entrypoint",entrypoint]
            command(cmd+flags+[image]+argv)

        def metric_file(name): return metrics / (name+".json")

        def read_receipts(key, window_start_ms, window_end_ms):
            if onset_control:
                value=receipt_observations(metrics/f'receipts-{key}.json',run_id,key)
                return onset_receipt_summary(value,window_start_ms,window_end_ms),{'class':value['terminal_class'] or 'no_recognized_terminal_error','exchange':value['exchange'],'http_status':value['http_status'],'diagnostics_error_seen':value['write_failed']}
            begin=int(time.time()*1000)
            logged=subprocess.run(["docker","logs","--tail","500",names[key]],capture_output=True,timeout=5)
            summary=receipt_log_summary(logged.stdout,run_id,"lab/supervisor-"+key,window_start_ms,window_end_ms)
            summary["read_started_at_unix_ms"]=begin
            summary["read_finished_at_unix_ms"]=int(time.time()*1000)
            if logged.returncode: summary.update({"complete":False,"status":"log_read_failed"})
            return summary,terminal_diagnostic(logged.stdout+logged.stderr)

        def start_sampler():
            mounts=["--mount",f"type=bind,src={folder/'sampler.py'},dst=/sampler.py,readonly",
                "--mount",f"type=bind,src={folder/'adapter.token'},dst=/token,readonly","--mount",f"type=bind,src={metrics},dst=/metrics",
                "--mount",f"type=bind,src={control},dst=/control,readonly"]
            for index,mount in data_mounts.items(): mounts += ["--mount",mount.replace("dst=/state",f"dst=/data{index}")+",readonly"]
            launch(names["sampler"],args.actuator_image,mounts,["/sampler.py"],entrypoint="python")

        def cli(verb, *extra):
            name="supervision-load-cli-"+uuid.uuid4().hex[:10]
            containers.append(name)
            return json.loads(command(["docker","run","--rm","--pull=never","--name",name,"--label",f"{OWNERSHIP_LABEL}={run_id}","--network",names["control"],"--user",user,
                "--entrypoint","/world","--mount",f"type=bind,src={folder/'operator-1.json'},dst=/client.json,readonly",
                "--mount",f"type=bind,src={folder/'mission.json'},dst=/mission.json,readonly",args.node_image,
                "task",verb,"--credentials","/client.json","--json",*extra],timeout=8))

        def measured_stop():
            name="supervision-load-stop-"+uuid.uuid4().hex[:10]
            containers.append(name)
            begin=time.monotonic_ns()
            argv=["docker","run","--rm","--pull=never","--name",name,"--label",f"{OWNERSHIP_LABEL}={run_id}",
                "--network",names["control"],"--user",user,"--entrypoint","/stop-timing",
                "--mount",f"type=bind,src={args.stop_binary},dst=/stop-timing,readonly",
                "--mount",f"type=bind,src={folder/'supervisor-a.json'},dst=/client.json,readonly",
                "--mount",f"type=bind,src={folder/'site-trust.json'},dst=/trust.json,readonly",
                "--mount",f"type=bind,src={folder/'mission.json'},dst=/mission.json,readonly",args.node_image,
                "--credentials","/client.json","--trust","/trust.json","--spec","/mission.json","--run-id",run_id]
            try:
                stopped=subprocess.run(argv,capture_output=True,timeout=8)
            except subprocess.TimeoutExpired:
                result["stop_client_timing"]={"outcome":"unknown","report_status":"helper_did_not_finish_within_host_bound",
                    "host_docker_elapsed_ns":time.monotonic_ns()-begin,"request_attempted":None,"response_matches_task":False}
                raise RuntimeError("bounded stop outcome unknown")
            elapsed=time.monotonic_ns()-begin
            try:
                result["stop_client_timing"]=stop_timing_report(stopped.stdout,stopped.returncode,elapsed)
            except (ValueError,TypeError,KeyError):
                result["stop_client_timing"]={"outcome":"unknown","report_status":"helper_report_missing_or_invalid",
                    "host_docker_elapsed_ns":elapsed,"helper_exit_code":stopped.returncode,
                    "request_attempted":None,"response_matches_task":False}
                raise RuntimeError("bounded stop outcome unknown")
            if not result["stop_client_timing"]["response_matches_task"]:
                raise RuntimeError("bounded stop was not confirmed")

        def physics():
            script="import json,urllib.request; r=urllib.request.Request('http://127.0.0.1:8091/v1/state',headers={'Authorization':'Bearer '+open('/token').read()}); response=urllib.request.urlopen(r,timeout=1); print(response.read(65537).decode())"
            return json.loads(command(["docker","exec",names["actuator"],"python","-c",script],timeout=3))

        def mode(value):
            path=control/"next-mode"; path.write_text(value); path.replace(control/"mode")

        def stop(name): command(["docker","stop","--time","2",name],timeout=5)

        try:
            stage="generate_lab"
            command(["go","run","./cmd/world-lab","--out",str(folder),"--runtime-dir","/state","--containers","--missions","--run-id",run_id])
            if not startup_control and not repair_control and not preload_control:
                stage="prepare_revocation"
                command(["go","run","./tests/supervision_fixture","--issuer",str(folder/"issuer.json"),"--credentials",str(folder/"supervisor-b.json"),"--out",str(folder/"revocation.json"),"--revoke"])
            (folder/"proxy.py").write_text(PROXY)
            (folder/"sampler.py").write_text(SAMPLER)
            stage="create_networks"
            for key in ("control","peer"):
                networks.append(names[key]); command(["docker","network","create","--internal","--label",f"{OWNERSHIP_LABEL}={run_id}",names[key]])
            for index in (1,2,3):
                stage=f"start_node_{index}"
                if storage == "host-bind":
                    state=folder/f"state-{index}"; state.mkdir(mode=0o700)
                    mount=f"type=bind,src={state},dst=/state"
                else:
                    volume=f"supervision-load-state-{index}-{suffix}"
                    volumes.append(volume); command(["docker","volume","create","--label",f"{OWNERSHIP_LABEL}={run_id}",volume])
                    if resource_ownership("volume",volume,run_id)!="owned":
                        raise RuntimeError("new volume ownership was not confirmed")
                    initializer=f"supervision-load-init-{index}-{suffix}"
                    containers.append(initializer)
                    command(["docker","run","--rm","--pull=never","--name",initializer,"--label",f"{OWNERSHIP_LABEL}={run_id}","--network","none","--user","0:0",
                        "--entrypoint","python","--mount",f"type=volume,src={volume},dst=/state",args.actuator_image,"-c",
                        "import os,sys; os.chown('/state',int(sys.argv[1]),int(sys.argv[2])); os.chmod('/state',0o700)",str(os.getuid()),str(os.getgid())])
                    mount=f"type=volume,src={volume},dst=/state"
                data_mounts[index]=mount
                config=bounded_json(folder/f"node-{index}.json")
                if repair_control: configure_repair(config,repair)
                if diagnostics: config["diagnostics"]={"path":f"/metrics/{'preparation-' if preload_control else ''}node{index}.json"}
                if profile == "disk-delay" and index == 1:
                    config["simulation_faults"]={"replay_commit_delay_ms":10,"task_commit_delay_ms":20}
                (folder/f"node-{index}.json").write_text(json.dumps(config))
                image=args.fault_node_image if profile == "disk-delay" and index == 1 else args.node_image
                config_mount=f"type=bind,src={folder/f'node-{index}.json'},dst=/config.json,readonly"
                node_flags=["--network-alias",f"world-node-{index}","--mount",mount,
                    "--mount",config_mount,"--mount",f"type=bind,src={metrics},dst=/metrics"]+(["--mount",f"type=bind,src={control},dst=/control,readonly"] if onset_control else [])
                primary_network="control" if index==1 else "peer"
                node_plans[index]={'name':names[f'node{index}'],'flags':node_flags,'config_mount':config_mount,
                    'measured_config':folder/f'node-{index}-measured.json','user':user,'network':names[primary_network],
                    'alias':f'world-node-{index}','control_mount':onset_control,
                    'extra_networks':[(names['peer'],'world-node-1')] if index==1 else []}
                launch(names[f"node{index}"],image,node_flags,["--config","/config.json"],network=primary_network)
            command(["docker","network","connect","--alias","world-node-1",names["peer"],names["node1"]])
            if preload_control:
                stage="preparation_node_readiness"
                for index in (1,2,3):
                    until(lambda index=index:bounded_json(metrics/f"preparation-node{index}.json"),lambda value:value.get("enabled") is True,timeout=6)
                stage="exact_preload_publication"
                launch(names["preloader"] if onset_control else names["publisher"],args.node_image,["--mount",f"type=bind,src={args.publisher_binary},dst=/load-publisher,readonly",
                    "--mount",f"type=bind,src={folder/'load-publisher.json'},dst=/client.json,readonly",
                    "--mount",f"type=bind,src={folder/'site-trust.json'},dst=/trust.json,readonly",
                    "--mount",f"type=bind,src={control},dst=/control"],
                    ["--preload","--credentials","/client.json","--trust","/trust.json","--run-id",run_id,"--duration","40s",
                     "--record-count",str(preload_count),"--manifest","/control/preload-manifest.json"],network="peer",entrypoint="/load-publisher")
                state=until(lambda:process(names["preloader"] if onset_control else names["publisher"]),lambda value:not value["Running"],timeout=45)
                logged=command(["docker","logs","--tail","1",names["preloader"] if onset_control else names["publisher"]],timeout=5)
                if len(logged)>16384:raise ValueError("preload report bound")
                result["preload_publication"]=json.loads(logged)
                prepared=result["preload_publication"]
                if state["ExitCode"]!=0 or prepared.get("complete") is not True or prepared.get("acknowledged")!=preload_count or prepared.get("count")!=preload_count or prepared.get("run_id")!=run_id:
                    raise RuntimeError("exact preload failed or has unknown publication outcomes")
                result["preload_publisher_process"]=process_summary(state)
                stage="preload_all_replicas"
                verification_deadline=time.monotonic()+20
                result["preload_before_restart"]=until(lambda:verify_preload(60,verification_deadline),lambda value:len(value)==3,timeout=20)
                stage="uniform_preload_node_restart"
                for index in (1,2,3):stop(names[f"node{index}"])
                measured_configs={}
                for index in (1,2,3):
                    config=bounded_json(folder/f"node-{index}.json")
                    configure_repair(config,repair)
                    config["diagnostics"]={"path":f"/metrics/node{index}.json"}
                    if onset_control and trace_nonce and index==1:
                        config['request_trace']={'path':'/metrics/request-trace.json','trigger_path':'/control/trace-trigger.json','run_nonce':trace_nonce,'window_ms':3000,'grace_ms':3000,'max_requests':2048,'max_events':32768}
                    measured_configs[index]=config
                recreate_preload_nodes(node_plans,run_id,measured_configs,result,verify_vm_config,set_stage)
                stage='uniform_preload_node_restart'
                for index in (1,2,3):command(["docker","start",result['preload_container_recreation'][str(index)]['new_container_id']],timeout=5)
                for index in (1,2,3):until(lambda index=index:bounded_json(metric_file(f"node{index}")),lambda value:value.get("enabled") is True,timeout=6)
            stage="actuator_start_and_readiness"
            launch(names["actuator"],args.actuator_image,["--network-alias","actuator","--mount",f"type=bind,src={folder/'adapter.token'},dst=/token,readonly"],
                ["examples/mission-office/world.json","--token-file","/token","--host","0.0.0.0","--allow-network-simulation","--allow-test-faults"])
            until(physics,lambda v:v.get("synthetic") is True)
            stage="executor_start_and_readiness"
            launch(names["executor"],args.node_image,["--mount",f"type=bind,src={folder/'executor-1.json'},dst=/client.json,readonly",
                "--mount",f"type=bind,src={folder/'adapter.token'},dst=/token,readonly"],
                ["--credentials","/client.json","--adapter","http://actuator:8091","--adapter-token-file","/token"],entrypoint="/world-executor")
            until(physics,lambda v:v["sequence"]>0)
            stage="proxy_start"
            launch(names["proxy"],args.actuator_image,["--network-alias","supervision-proxy","--mount",f"type=bind,src={folder/'proxy.py'},dst=/proxy.py,readonly",
                "--mount",f"type=bind,src={control},dst=/control,readonly"],["/proxy.py"],entrypoint="python")
            proxy_profile=bounded_json(folder/"supervisor-a.json")
            proxy_profile["url"]="http://supervision-proxy:8099"
            private_json(folder/"supervisor-proxy.json",proxy_profile); del proxy_profile
            mission={"id":"load-mission","type":"wendy.nav.go_to.v1","site":"synthetic-office","assignee":"sim-robot/navigation",
                "domain":"simulation","class":"MOVE","mode":"supervised","frame":{"id":"office-floor","revision":"office-frame-v1"},
                "not_after_unix_ms":int(time.time()*1000)+120000,"area":{"min":[-2.1,.1,-.1],"max":[2.1,.3,.1]},
                "required_sensors":["gas","local-obstacles"],"minimum_energy":.2,"fallback":"hold","target":[1.8,.2,0],
                "speed_mps":.025,"tolerance_m":.03,"stop_speed_mps":.01}
            private_json(folder/"mission.json",mission)
            if startup_control or preload_control:
                stage="sampler_start"
                start_sampler()
            stage="mission_admission"
            until(lambda:cli("list"),lambda v:not v["tasks"])
            admitted=cli("send","--spec","/mission.json")
            result["mission_admission_phase"]=admitted["status"]["Phase"]
            if onset_control:
                stage='prepare_onset_publisher'
                launch(names['publisher'],args.node_image,['--mount',f'type=bind,src={args.publisher_binary},dst=/load-publisher,readonly',
                    '--mount',f'type=bind,src={folder/"load-publisher.json"},dst=/client.json,readonly',
                    '--mount',f'type=bind,src={folder/"site-trust.json"},dst=/trust.json,readonly',
                    '--mount',f'type=bind,src={metrics},dst=/metrics','--mount',f'type=bind,src={control},dst=/control,readonly'],
                    ['--credentials','/client.json','--trust','/trust.json','--run-id',run_id,'--duration','24s','--entities','200','--hz','2',
                    '--onset-variant',onset_variant,'--manifest','/control/preload-manifest.json','--release-file','/control/publication-release.json','--report-file','/metrics/publisher-gate.json'],network='peer',entrypoint='/load-publisher')
                result['onset_control']['publisher_ready']=until(lambda:bounded_json(metrics/'publisher-gate.json'),lambda v:v.get('stage')=='ready' and v.get('run_id')==run_id,timeout=6)
            for key in ("a","b"):
                stage=f"supervisor_{key}_start"
                credential="supervisor-proxy.json" if key=="a" else "supervisor-b.json"
                argv=["task","supervise","--credentials","/client.json","--trust","/trust.json","--spec","/mission.json","--run-id",run_id,
                     "--assignee","sim-robot/navigation","--gateway-principal","lab/node-1","--duration","40s","--json"]
                if diagnostics: argv += ["--diagnostics-file",f"/metrics/{key}.json"]
                mounts=["--mount",f"type=bind,src={folder/credential},dst=/client.json,readonly",
                    "--mount",f"type=bind,src={folder/'site-trust.json'},dst=/trust.json,readonly",
                    "--mount",f"type=bind,src={folder/'mission.json'},dst=/mission.json,readonly",
                    "--mount",f"type=bind,src={metrics},dst=/metrics"]
                if preload_control:
                    mounts += ["--mount",f"type=bind,src={args.gate_binary},dst=/supervision-gate,readonly",
                        "--mount",f"type=bind,src={control},dst=/control,readonly"]
                    argv=["--credentials","/client.json","--trust","/trust.json","--spec","/mission.json","--run-id",run_id,
                        "--operator",key,"--release-file","/control/supervision-release.json","--report-file",f"/metrics/gate-{key}.json",
                        "--diagnostics-file",f"/metrics/{key}.json"]
                    if onset_control:argv += ["--observations-file",f"/metrics/receipts-{key}.json"]
                launch(names[key],args.node_image,mounts,argv,entrypoint="/supervision-gate" if preload_control else "/world")
            if startup_control:
                started=time.monotonic()
                window_start_ms=int(time.time()*1000)
                result["window_started_at_unix_ms"]=window_start_ms
                result["initial_receipt_log_reads"]={key:read_receipts(key,window_start_ms,int(time.time()*1000))[0] for key in ("a","b")}
            before={}
            baseline_reads={}
            if preload_control:
                stage="preload_gates_ready"
                result["preload_gate_ready"]={key:until(lambda key=key:bounded_json(metrics/f"gate-{key}.json"),
                    lambda value:value.get("run_id")==run_id and value.get("operator")==key and value.get("stage")=="ready",timeout=6) for key in ("a","b")}
                until(lambda:bounded_json(metrics/"sampler-ready.json"),lambda value:value.get("run_id")==run_id and value.get("stage")=="ready",timeout=6)
                result["preload_before_release"]=verify_preload(60)
                result["initial_velocity_x"]=physics()["velocity_mps"][0]
                result["supervisor_preload_state"]={key:process_summary(process(names[key])) for key in ("a","b")}
                result["supervisor_preload_state_observed_at_unix_ms"]=int(time.time()*1000)
                initial_boundary=preload_boundary(metrics)
                if any(key not in initial_boundary["snapshots"] for key in ("node1","node2","node3")):raise RuntimeError("preload node metrics unavailable before release")
                release_ms=int(time.time()*1000)+500
                for value in result["preload_before_release"].values():
                    if preload_count and value["minimum_remaining_ms"]-(release_ms-value["verified_at_unix_ms"])<60000:
                        raise RuntimeError("preload will not remain fresh through the fixed window")
                release={"schema":"wendy.world.supervision-gate-release.v1","run_id":run_id,
                    "release_unix_ms":release_ms,"window_end_unix_ms":release_ms+24000}
                private_json(control/"release-next.json",release)
                (control/"release-next.json").replace(control/"supervision-release.json")
                started=time.monotonic()+max(0,(release_ms-time.time()*1000)/1000)
                window_start_ms=release_ms
                result["window_started_at_unix_ms"]=window_start_ms
                result["initial_supervisor_successful_rounds"]={"a":0,"b":0}
                stage="preload_quiet_window"
                if onset_control:
                    result['onset_window']=quiet_onset_window(started,metrics,control,run_id,onset_variant,initial_boundary,trace_nonce,
                        trace_at_seconds=args.onset_trace_at_seconds,supervisor_release_unix_ms=window_start_ms)
                    result['preload_metric_boundaries']=result['onset_window']['boundaries']
                else:
                    result["preload_metric_boundaries"]=quiet_preload_window(started,metrics,initial_boundary)
                result["preload_phase_metrics"]=preload_phase_metrics(result["preload_metric_boundaries"])
                result["preload_phase_metrics_complete"]={phase:all(value["status"] in ("snapshot_interval","identical_snapshot_no_delta") for value in entries.values())
                    for phase,entries in result["preload_phase_metrics"].items()}
            if not startup_control and not preload_control:
                # Launch both before waiting for either once-per-second exporter.
                for key in ("a","b"):
                    stage=f"supervisor_{key}_readiness"
                    until(lambda key=key:bounded_json(metric_file(key)),lambda v:any(d["name"]=="supervisor.exchange" and d["count"]>0 for d in v["durations"]),timeout=6)
                stage="baseline_metrics"
                for key in ("node1","node2","node3","a","b"):
                    baseline_reads[key]={"started_at_unix_ms":int(time.time()*1000)}
                    before[key]=bounded_json(metric_file(key))
                    baseline_reads[key]["finished_at_unix_ms"]=int(time.time()*1000)
                result["initial_supervisor_successful_rounds"]={key:
                    next(d["count"] for d in before[key]["durations"] if d["name"]=="supervisor.exchange")-
                    next(c["value"] for c in before[key]["counters"] if c["name"]=="supervisor.exchange_failures") for key in ("a","b")}
            if not preload_control:
                result["initial_velocity_x"]=physics()["velocity_mps"][0]
                result["supervisor_preload_state"]={key:process_summary(process(names[key])) for key in ("a","b")}
                result["supervisor_preload_state_observed_at_unix_ms"]=int(time.time()*1000)
            if not startup_control and not preload_control:
                stage="sampler_start"
                start_sampler()
                stage="publisher_start"
                launch(names["publisher"],args.node_image,["--mount",f"type=bind,src={args.publisher_binary},dst=/load-publisher,readonly",
                    "--mount",f"type=bind,src={folder/'load-publisher.json'},dst=/client.json,readonly",
                    "--mount",f"type=bind,src={folder/'site-trust.json'},dst=/trust.json,readonly"],
                    ["--credentials","/client.json","--trust","/trust.json","--run-id",run_id,"--duration","24s","--entities","200","--hz","2"],network="peer",entrypoint="/load-publisher")
                started=time.monotonic()
                window_start_ms=int(time.time()*1000)
                result["window_started_at_unix_ms"]=window_start_ms

            def event(label, action):
                begin=time.monotonic()
                before_fault={"observed_at_unix_ms":int(time.time()*1000),"supervisors":{}}
                try:
                    observed=bounded_json(metrics/"samples.json")["samples"][-1]
                    before_fault["sample_age_ms"]=before_fault["observed_at_unix_ms"]-observed["sample_unix_ms"]
                    before_fault["sample"]={key:observed.get(key) for key in ("sample_unix_ms","phase","position_x","velocity_x","commanded_velocity")}
                except (OSError,ValueError,KeyError,IndexError): before_fault["sample_status"]="unavailable"
                for key in ("a","b"):
                    try:
                        state=process(names[key])
                        if onset_control:
                            leases=receipt_observations(metrics/f'receipts-{key}.json',run_id,key)['receipts']
                        else:
                            logged=subprocess.run(["docker","logs","--tail","2",names[key]],capture_output=True,timeout=3)
                            if len(logged.stdout)>16384: raise ValueError("lease log bound")
                            leases=[json.loads(line) for line in logged.stdout.decode().splitlines() if line.startswith("{")]
                            leases=[value for value in leases if value.get("schema")=="wendy.world.supervision-lease.v1" and value.get("run_id")==run_id and value.get("task_id")=="load-mission"]
                        expires=max((value["expires_at_unix_ms"] for value in leases),default=0)
                        before_fault["supervisors"][key]={"process_running":state["Running"],"latest_receipt_expires_at_unix_ms":expires or None,
                            "observed_at_unix_ms":int(time.time()*1000),
                            "receipt_unexpired_at_observation":state["Running"] and expires>int(time.time()*1000)}
                    except (RuntimeError,OSError,ValueError,KeyError,subprocess.TimeoutExpired): before_fault["supervisors"][key]={"status":"unavailable"}
                action_started=time.monotonic()
                failure={}
                try: action(); status="applied"
                except (RuntimeError,subprocess.TimeoutExpired) as error:
                    status="operation_failed"; failure=failure_metadata(error)
                result["events"].append({"event":label,"at_seconds":round(begin-started,3),"action_started_at_seconds":round(action_started-started,3),
                    "operation_seconds":round(time.monotonic()-action_started,3),"status":status,"before_fault":before_fault,**failure})

            def revoke():
                script="import urllib.request; r=urllib.request.Request('http://world-node-1:8080/v1/revocations',data=open('/revocation.json','rb').read(),headers={'Content-Type':'application/json'}); response=urllib.request.urlopen(r,timeout=2); print(response.status)"
                name="supervision-load-revoke-"+suffix; containers.append(name)
                command(["docker","run","--rm","--pull=never","--name",name,"--label",f"{OWNERSHIP_LABEL}={run_id}","--network",names["control"],"--user",user,"--entrypoint","python",
                    "--mount",f"type=bind,src={folder/'revocation.json'},dst=/revocation.json,readonly",args.actuator_image,"-c",script],timeout=5)

            def stall():
                paused.append(names["executor"])
                command(["docker","kill","--signal","STOP",names["executor"]],timeout=3)
                time.sleep(.7)
                command(["docker","kill","--signal","CONT",names["executor"]],timeout=3)
                paused.remove(names["executor"])

            schedule=[(8,"repair_partition",lambda:command(["docker","network","disconnect",names["peer"],names["node1"]],timeout=4)),
                      (10,"repair_reconnect",lambda:command(["docker","network","connect","--alias","world-node-1",names["peer"],names["node1"]],timeout=4)),
                      (16,"repair_partition",lambda:command(["docker","network","disconnect",names["peer"],names["node1"]],timeout=4)),
                      (18,"repair_reconnect",lambda:command(["docker","network","connect","--alias","world-node-1",names["peer"],names["node1"]],timeout=4))]
            if profile=="baseline-revocation": schedule += [(12,"revoke_b",revoke),(20,"stop_last_operator",lambda:stop(names["a"]))]
            if profile=="asymmetric": schedule += [(6,"ack_response_delay_70ms",lambda:mode("delay-ack-response")),(11,"drop_a_ack_responses",lambda:mode("drop-ack-response")),(20,"stop_last_operator",lambda:stop(names["b"]))]
            if profile=="executor-stall": schedule += [(12,"executor_pause_700ms",stall)]
            if startup_control or repair_control or preload_control: schedule=[]
            stage="measurement_window"
            for at,label,action in sorted(schedule,key=lambda item:item[0]):
                time.sleep(max(0,started+at-time.monotonic())); event(label,action)
            time.sleep(max(0,started+window_seconds-time.monotonic()))
            result["actual_window_seconds"]=round(time.monotonic()-started,3)
            window_end_ms=int(time.time()*1000)
            result["window_ended_at_unix_ms"]=window_end_ms
            stage="operator_stop_and_process_completion"
            event("operator_stop",measured_stop if args.stop_binary else lambda:cli("stop","--id","load-mission"))
            for key in ("a","b"):
                if process(names[key])["Running"]: stop(names[key])
            if not startup_control:
                until(lambda:process(names["publisher"]),lambda value:not value["Running"],timeout=3)
            time.sleep(.7)
            (control/"stop-sampler").write_text("stop")
            until(lambda:process(names["sampler"]),lambda value:not value["Running"],timeout=3)
            stage="collect_samples"
            samples=bounded_json(metrics/"samples.json")
            result["samples"]=samples
            stage="collect_metrics"
            process_keys=("node1","node2","node3","a","b","executor")+(() if startup_control else ("publisher",))
            final_states={key:process(names[key]) for key in process_keys}
            supervisor_diagnostics={}
            receipt_summaries={}
            for key in ("a","b"):
                receipt_summaries[key],supervisor_diagnostics[key]=read_receipts(key,window_start_ms,window_end_ms)
            result["supervisor_terminal_diagnostics"]=supervisor_diagnostics
            result["receipt_log_summaries"]=receipt_summaries
            result["diagnostic_files_present"]=[key for key in ("node1","node2","node3","a","b") if metric_file(key).exists()]
            if not diagnostics and result["diagnostic_files_present"]:
                raise RuntimeError("diagnostics output unexpectedly enabled in off control")
            for key in ("node1","node2","node3","a","b"):
                if not diagnostics:
                    result["measurements"][key]={"enabled":False,"complete":None,"status":"disabled_for_control"}
                    continue
                try:
                    final_read_begin=int(time.time()*1000)
                    after=bounded_json(metric_file(key))
                    final_read_end=int(time.time()*1000)
                    if startup_control or preload_control:
                        validate_snapshot(after)
                        state=final_states[key]
                        lag=(final_read_end if state["Running"] else process_time_ms(state["FinishedAt"]))-after["captured_at_unix_ms"]
                        result["measurements"][key]={"status":"process_lifetime_only","lifetime_after":after,
                            "window_delta_available":False,"complete":abs(lag)<=3000 and not supervisor_diagnostics.get(key,{}).get("diagnostics_error_seen",False),
                            "snapshot_lag_ms_from_process_end_or_read":lag}
                    else:
                        result["measurements"][key]=collect_measurement(before[key],after,final_states[key],window_start_ms,
                            final_read_end,supervisor_diagnostics.get(key,{}).get("diagnostics_error_seen",False))
                    result["measurements"][key]["baseline_file_read"]=baseline_reads.get(key)
                    result["measurements"][key]["final_file_read"]={"started_at_unix_ms":final_read_begin,"finished_at_unix_ms":final_read_end}
                except (OSError,KeyError,ValueError):
                    result["measurements"][key]={"complete":False,"status":"snapshot_missing_or_invalid"}
            result["diagnostics_complete"]=all(value["complete"] for value in result["measurements"].values()) if diagnostics else None
            stage="collect_publisher_and_process_results"
            if preload_control:
                if onset_control:
                    result['publisher']=bounded_json(metrics/'publisher-gate.json',65536)
                    publication=result['publisher']
                    if (publication.get('schema')!='wendy.world.publication-onset-publisher.v1' or publication.get('run_id')!=run_id or
                        publication.get('variant')!=onset_variant or publication.get('stage')!='finished' or publication.get('error_class') or
                        publication.get('epoch')!=result['preload_publication'].get('epoch',result['preload_before_release']['2']['epoch']) or
                        publication.get('first_sequence')!=201 or
                        (not result['onset_window']['release']['publish'] and (publication.get('attempted')!=0 or publication.get('published')!=0))):
                        raise RuntimeError('onset publisher gate failed or mismatched')
                    result['receipt_observations']={key:receipt_observations(metrics/f'receipts-{key}.json',run_id,key) for key in ('a','b')}
                    if trace_nonce:
                        if args.onset_trace_at_seconds is None:
                            result['request_trace']=bounded_json(metrics/'request-trace.json',8<<20)
                            result['onset_control']['trace_complete']=result['request_trace'].get('state')=='complete' and result['request_trace'].get('run_nonce')==trace_nonce
                        else:
                            trace,collection=collect_fixed_trace(metrics/'request-trace.json',trace_nonce)
                            result['trace_collection']=collection
                            result['onset_control']['trace_complete']=collection['complete']
                            if trace is not None:
                                result['request_trace']=trace
                                result['onset_window']['fixed_trace']['final_observed_state']=trace['state']
                                result['onset_window']['fixed_trace']['final_observed_armed_at_unix_ms']=trace.get('armed_at_unix_ms')
                else:
                    result["publisher"]={"enabled":False,"status":"exact_preload_finished_before_operators","prepared_records":preload_count}
                result["preload_gate_reports"]={key:bounded_json(metrics/f"gate-{key}.json") for key in ("a","b")}
                result["preload_control"]["gate_execution_complete"]=all(value.get("stage")=="exec" for value in result["preload_gate_reports"].values())
                if onset_control:
                    # The wrapper remains alive to sanitize output. A child
                    # timeout is measured availability, not failed fixture exec.
                    result['preload_control']['gate_execution_complete']=all(
                        value.get('exec_at_unix_ms') is not None and
                        result['receipt_observations'][key]['terminal_class']!='child_start_failed'
                        for key,value in result['preload_gate_reports'].items())
                    release_at=result['publisher'].get('first_request_attempt_at_unix_ms')
                    result['onset_control']['first_attempt_receipt_evidence']={}
                    if release_at:
                        for key,observed in result['receipt_observations'].items():
                            prior=[row for row in observed['receipts'] if row['observed_at_unix_ms']<=release_at]
                            latest=prior[-1] if prior else None
                            result['onset_control']['first_attempt_receipt_evidence'][key]={
                                'first_request_attempt_at_unix_ms':release_at,'latest_observed_receipt':latest,
                                'receipt_unexpired_at_attempt':latest is not None and release_at<latest['expires_at_unix_ms'],
                                'observer_exited_before_attempt':observed['state']=='exited' and observed['finished_at_unix_ms']<=release_at}
                result["sampler_gate_report"]=bounded_json(metrics/"sampler-ready.json")
                stage="preload_after_window_verification"
                if not onset_control or onset_variant=='fixed' or result['publisher']['attempted']==0:
                    result["preload_after_window"]=verify_preload(1)
                else:
                    result['preload_after_window']={'status':'changing_records_expected; exact initial signed set no longer applies'}
                stage="collect_publisher_and_process_results"
                result["phase_receipt_summaries"]={phase:{key:read_receipts(key,begin,end)[0] for key in ("a","b")}
                    for phase,begin,end in (("startup_0_to_2",window_start_ms,window_start_ms+1999),("later_2_to_24",window_start_ms+2000,window_end_ms))}
            elif startup_control:
                result["publisher"]={"enabled":False,"status":"no_offered_load_in_startup_control"}
            else:
                published=command(["docker","logs","--tail","1",names["publisher"]],timeout=5)
                if len(published)>65536: raise ValueError("publisher diagnostic bound")
                result["publisher"]=json.loads(published)
            result["processes"]={key:process_summary(final_states[key]) for key in ("a","b","executor")+(() if startup_control else ("publisher",))}
            result["proxy_events"]=[json.loads(line) for line in command(["docker","logs","--tail","12",names["proxy"]],timeout=5).decode().splitlines() if line.startswith("{")]
            phases={}
            for row in samples["samples"]:
                phase=row.get("phase","unknown"); phases[phase]=phases.get(phase,0)+1
            result["phase_sample_counts"]=phases
            result["availability"]={"first_observed_phase":next((row["phase"] for row in samples["samples"] if "phase" in row),"unknown"),
                "running_samples":phases.get("running",0),"held_samples":phases.get("held",0),
                "supervisors_exited_before_window":[key for key in ("a","b") if not final_states[key]["Running"] and process_time_ms(final_states[key]["FinishedAt"])<=window_start_ms],
                "supervision_delta_samples_present":supervision_delta_samples_present(result["measurements"])}
            window_rows=[row for row in samples["samples"] if window_start_ms<=row["sample_unix_ms"]<=window_end_ms]
            result["availability"]["declared_window_samples"]={"count":len(window_rows),
                "running":sum(row.get("phase")=="running" for row in window_rows),
                "held":sum(row.get("phase")=="held" for row in window_rows),
                "motion":sum(abs(row.get("velocity_x",0))>.001 for row in window_rows)}
            if preload_control:
                result["phase_availability"]={}
                for phase,begin,end in (("startup_0_to_2",window_start_ms,window_start_ms+1999),("later_2_to_24",window_start_ms+2000,window_end_ms)):
                    rows=[row for row in samples["samples"] if begin<=row["sample_unix_ms"]<=end]
                    counts={}
                    for row in rows:
                        name=row.get("phase","unknown");counts[name]=counts.get(name,0)+1
                    result["phase_availability"][phase]={"start_unix_ms":begin,"end_unix_ms":end,"phase_samples":counts,
                        "motion_samples":sum(abs(row.get("velocity_x",0))>.001 for row in rows)}
            result["fault_application_complete"]=all(event["status"]=="applied" for event in result["events"])
            result["replica_progress"]={}
            for index in ("1","2","3"):
                observed=[row["replicas"][index] for row in samples["samples"] if index in row.get("replicas",{})]
                result["replica_progress"][index]={"observations":len(observed),
                    "first_max_sequence":observed[0]["max_sequence"] if observed else None,
                    "last_max_sequence":observed[-1]["max_sequence"] if observed else None,
                    "maximum_unexpired_entities":max((item["unexpired_entities"] for item in observed),default=0),
                    "source_run_frame_mismatches":sum(item["source_run_frame_mismatches"] for item in observed)}
            result["result"]="fixture_failed" if preload_control and not result["preload_control"]["gate_execution_complete"] else "measured"
        except Exception as error:
            result["result"]="fixture_failed"
            result["failure_stage"]=stage
            result.update(failure_metadata(error))
            sanitized_diagnostics(containers)
        finally:
            for name in paused:
                try:
                    if resource_ownership("container",name,run_id)=="owned": subprocess.run(["docker","kill","--signal","CONT",name],capture_output=True,timeout=3)
                except (OSError,subprocess.TimeoutExpired): pass
            failures=[]; conflicts=[]
            for kind,items in (("container",containers),("network",networks),("volume",volumes)):
                for name in reversed(items):
                    try:
                        ownership=resource_ownership(kind,name,run_id)
                        if ownership=="missing": continue
                        if ownership!="owned":
                            failures.append(name)
                            if ownership=="foreign": conflicts.append(name)
                            continue
                        argv=["docker",kind,"rm"]+(["-f"] if kind=="container" else [])+[name]
                        removed=subprocess.run(argv,capture_output=True,timeout=10)
                        if removed.returncode and resource_ownership(kind,name,run_id)!="missing": failures.append(name)
                    except (OSError,subprocess.TimeoutExpired): failures.append(name)
            result["cleanup"]={"complete":not failures,"retained_resources":failures,"ownership_conflicts":conflicts}
            if failures: result["result"]="fixture_failed"
    return result


def main(argv=None):
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--node-image",default="wendy-world-node:local")
    parser.add_argument("--fault-node-image")
    parser.add_argument("--actuator-image",default="wendy-world-actuator:local")
    parser.add_argument("--publisher-binary",type=Path)
    parser.add_argument("--stop-binary",type=Path,help="optional simulation-only one-shot stop timing helper")
    parser.add_argument("--gate-binary",type=Path,help="fixed-command simulation supervisor gate for preload control")
    parser.add_argument("--storage",choices=STORAGE)
    parser.add_argument("--profile",choices=PROFILES)
    parser.add_argument("--startup-control",action="store_true",help="matched8s diagnostics on/off startup, no offered load or injected faults")
    parser.add_argument("--repair-control",action="store_true",help="matched24s publication load with peer repair on/off, no injected faults")
    parser.add_argument("--preload-control",action="store_true",help="fixed retained0/200 records, no publisher or Docker calls during24s supervision")
    parser.add_argument("--preload-records",type=int,choices=(0,200))
    parser.add_argument('--onset-control',action='store_true',help='gated publication onset after observed dual supervision; no injected faults')
    parser.add_argument('--onset-repetitions',type=int,choices=(1,2,3),default=2)
    parser.add_argument('--onset-variant',choices=('fixed','changing'))
    parser.add_argument('--onset-trace',action='store_true',help='explicit bounded gateway request trace at publication onset')
    parser.add_argument('--onset-trace-at-seconds',type=int,choices=(0,6,18),help='one fixed trace trigger offset from supervisor release; independent of publication readiness')
    parser.add_argument("--repair-mode",choices=("both","on","off"),default="both")
    parser.add_argument("--control-diagnostics",choices=("both","on","off"),default="both")
    args=parser.parse_args(argv)
    if sum((args.startup_control,args.repair_control,args.preload_control,args.onset_control))>1: parser.error("controls are separate experiments")
    if not (args.repair_control or args.preload_control) and args.repair_mode!="both": parser.error("repair-mode requires repair-control or preload-control")
    if not args.startup_control and args.control_diagnostics!="both": parser.error("control-diagnostics requires startup-control")
    if not (args.preload_control or args.onset_control) and (args.preload_records is not None or args.gate_binary):parser.error("preload-records/gate-binary require gated control")
    if not args.onset_control and (args.onset_variant or args.onset_trace or args.onset_repetitions!=2):parser.error('onset options require onset-control')
    if args.onset_trace_at_seconds is not None and not (args.onset_control and args.onset_trace):parser.error('fixed trace offset requires onset-control and onset-trace')
    if args.onset_control and (args.profile or args.preload_records is not None or not args.gate_binary or not args.stop_binary):parser.error('onset-control requires gate/stop binaries and refuses fault/cardinality overrides')
    if args.preload_control and (args.profile or not args.gate_binary or not args.stop_binary):parser.error("preload-control requires gate-binary/stop-binary and refuses fault profiles")
    if args.gate_binary:
        info=args.gate_binary.lstat()
        if not stat.S_ISREG(info.st_mode) or info.st_size>32<<20 or info.st_mode&0o111==0:parser.error("gate-binary must be a bounded regular executable")
        args.gate_binary=args.gate_binary.resolve()
    if args.stop_binary:
        info=args.stop_binary.lstat()
        if not stat.S_ISREG(info.st_mode) or info.st_size>32<<20 or info.st_mode&0o111==0:
            parser.error("stop-binary must be a bounded regular executable, not a symlink")
        args.stop_binary=args.stop_binary.resolve()
    if args.startup_control:
        if args.profile: parser.error("startup-control does not accept a fault profile")
        image_names=(args.node_image,args.actuator_image)
    else:
        if args.repair_control and (args.profile or not args.stop_binary):
            parser.error("repair-control requires stop-binary and refuses injected fault profiles")
        if (not (args.repair_control or args.preload_control or args.onset_control) and not args.fault_node_image) or not args.publisher_binary:
            parser.error("load matrix requires fault-node-image and publisher-binary")
        info=args.publisher_binary.lstat()
        if not stat.S_ISREG(info.st_mode) or info.st_size>32<<20 or info.st_mode&0o111==0:
            parser.error("publisher-binary must be a bounded regular executable, not a symlink")
        args.publisher_binary=args.publisher_binary.resolve()
        image_names=(args.node_image,args.actuator_image) if args.repair_control or args.preload_control or args.onset_control else (args.node_image,args.fault_node_image,args.actuator_image)
    images={name:command(["docker","image","inspect","--format","{{.Id}}",name],timeout=5).decode().strip() for name in image_names}
    cells=[]
    if args.onset_control:
        for repetition,storage,variant in onset_order(args.onset_repetitions,args.storage,args.onset_variant):
            print(json.dumps({'load_cell_started':{'storage':storage,'profile':'onset-control','variant':variant,'repetition':repetition}}),file=sys.stderr,flush=True)
            cell=run_cell(args,storage,'onset-control',True,True,200,variant);cell['repetition']=repetition;cells.append(cell)
    for storage in (() if args.onset_control else (args.storage,) if args.storage else STORAGE):
        if args.preload_control:
            for count in (0,200) if args.preload_records is None else (args.preload_records,):
                for repair in (True,False) if args.repair_mode=="both" else (args.repair_mode=="on",):
                    print(json.dumps({"load_cell_started":{"storage":storage,"profile":"preload-control","retained_records":count,"repair_enabled":repair}}),file=sys.stderr,flush=True)
                    cells.append(run_cell(args,storage,"preload-control",True,repair,count))
        elif args.repair_control:
            for repair in (True,False) if args.repair_mode=="both" else (args.repair_mode=="on",):
                print(json.dumps({"load_cell_started":{"storage":storage,"profile":"repair-control","repair_enabled":repair}}),file=sys.stderr,flush=True)
                cells.append(run_cell(args,storage,"repair-control",True,repair))
        else:
            variants=(("startup-control",value) for value in ((True,False) if args.control_diagnostics=="both" else (args.control_diagnostics=="on",))) if args.startup_control else ((profile,True) for profile in ((args.profile,) if args.profile else PROFILES))
            for profile,diagnostics in variants:
                print(json.dumps({"load_cell_started":{"storage":storage,"profile":profile,"diagnostics_enabled":diagnostics}}),file=sys.stderr,flush=True)
                cells.append(run_cell(args,storage,profile,diagnostics))
    schema="wendy.world.supervision-onset-control.v1" if args.onset_control else "wendy.world.supervision-preload-control.v1" if args.preload_control else "wendy.world.supervision-repair-control.v1" if args.repair_control else "wendy.world.supervision-startup-control.v1" if args.startup_control else "wendy.world.supervision-load.v1"
    print(json.dumps({"schema":schema,"domain":"simulation","physical_hardware":False,
        "images":images,"cells":cells,"result":"measured" if all(c["result"]=="measured" for c in cells) else "incomplete",
        "limits":["bounded synthetic load on one existing Docker VM, not fleet capacity or physical safety acceptance",
                  "faults never change lease deadlines; valid HOLD and supervisor failure remain availability observations",
                  "histogram bins are fixed; reported maxima span process lifetime, not only the measurement window",
                  "host command timing includes Docker overhead; observer polls are not real-time safety measurements",
                  "startup control is one ordered on/off pair per storage, not statistical attribution of diagnostic overhead",
                  "repair control disables configured peers, not local publication, authority checks or persistence; one ordered pair is not a causal performance estimate",
                  "preload control keeps an exact signed set on every node, stops publication and gates supervisors after uniform restart; five-minute record TTL is not a grant or lease extension"]},indent=2))


if __name__=="__main__":
    signal.signal(signal.SIGTERM,lambda *_:(_ for _ in ()).throw(KeyboardInterrupt()))
    main()
