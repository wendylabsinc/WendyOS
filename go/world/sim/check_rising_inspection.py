"""Explicit simulated operator acceptance using real native review/sign controls."""
import base64,copy,datetime,hashlib,json,math,subprocess,time,zlib
from gas_source import atomic

def read_journal(path):
    wrapper=json.loads(path.read_text())
    assert wrapper['version']==1
    raw=base64.b64decode(wrapper['payload'],validate=True)
    assert hashlib.sha256(raw).hexdigest()==wrapper['sha256']
    return json.loads(raw)

def check(d,args):
    checks=[];shots=[];actions=[];qa={'east':0,'west':0}
    def record(label,**details):
        checks.append(dict(label=label,**details));atomic(d.out/'checks-progress.json',dict(checks=checks));print(json.dumps(checks[-1]),flush=True)
    def pump():d.poll_selection();d.poll_status();time.sleep(.05)
    def settle(label,want,minimum=.1,remaining=None):
        start=time.monotonic()
        while time.monotonic()-start<14:
            views=d.step()
            budget={name:datetime.datetime.fromisoformat(v['result']['valid_until'].replace('Z','+00:00')).timestamp()-time.time() for name,v in views.items()}
            if all(v['result']['state']==want for v in views.values()) and minimum<=time.monotonic()-start<14 and (remaining is None or min(budget.values())>=remaining):record(label,state=want,sequence=d.sequence,original_remaining_s=budget);return views
        raise AssertionError(dict(label=label,states={n:v['result']['state']for n,v in d.last.items()}))
    def action(name,op):
        qa[name]+=1;sequence=qa[name];folder=d.out/('viewer-'+name);current=d.last.get(name,{}).get('result',{});entry=dict(operator=name,sequence=sequence,action=op,clicked_unix_s=time.time(),selected_snapshot_sequence=d.sequence,selected_input_digest=current.get('input_digest'),original_valid_until=current.get('valid_until'));actions.append(entry);atomic(d.out/'native-actions.json',actions);atomic(folder/'native-qa.json',dict(sequence=sequence,action=op));end=time.monotonic()+18
        while time.monotonic()<end:
            path=folder/f'native-receipt-{sequence:04d}.json'
            if path.exists():
                value=json.loads(path.read_text());entry.update(received_unix_s=time.time(),receipt=value);atomic(d.out/'native-actions.json',actions);assert value['accepted'],value;return value
            pump()
        raise TimeoutError('native action '+op)
    def capture(name,title,mission=False):
        value=action(name,'capture-inspection'if mission else'capture');shots.append(dict(operator=name,capture=value['capture'],title=title,duration=4));atomic(d.out/'shots.json',shots)
    def no_motion():return not any(math.hypot(c['velocity'][0],c['velocity'][2])>0 for c in d.robot.commands)
    def refuse_selected(label,digest):
        result=d.readers['east'].call(dict(op='prepare-inspection',input_digest=digest),True);assert 'error'in result,result;record(label,response=result)
    assert args.app,'actual native app required'
    settle('steady spatial evidence alone cannot prepare a rising inspection','non_rising')
    d.launch_native(args.app);refuse_selected('non-rising preparation refused',d.last['east']['result']['input_digest'])
    d.command('ramp');settle('both independent readers share a current exact-original rise','supported_rise',2)
    refuse_selected('changed selected digest refused','0'*64)
    selected=d.last['east']['result']['input_digest'];until=time.monotonic()+4.2
    while time.monotonic()<until:pump()
    refuse_selected('original expiry cannot be renewed by preparation',selected)
    settle('fresh chronology restores selectable rise','supported_rise',1.5)
    d.command('displace');settle('displaced visual evidence does not join','unsupported');refuse_selected('conflicting spatial support refused',d.last['east']['result']['input_digest'])
    d.command('displace');settle('matching current originals restore preparation','supported_rise')
    assert no_motion();capture('east','Current rise, exact original witnesses and a spatial location. No motion approval.')
    # Refresh once immediately before the scripted click, using the same live path.
    settle('current selected evidence before explicit preparation','supported_rise',remaining=.5)
    prepared=action('east','inspection-prepare');assert d.prepared and no_motion();record('preparation created only a proposal',native=prepared)
    selection=copy.deepcopy(d.prepared);triggerRaw=base64.b64decode(selection['mission_input']['trigger_json']);trigger=json.loads(triggerRaw);payload=trigger['component']['payload'];assert payload['temporal_codec']=='deflate-json-v1';claimRaw=zlib.decompress(base64.b64decode(payload['temporal_json']),wbits=-15);claim=json.loads(claimRaw)
    assert payload['schema']=='wendy.rising-incident.inspection.v1' and len(claim['payload']['inputs'])==12 and hashlib.sha256(claimRaw).hexdigest()==payload['temporal_sha256']
    assert len(triggerRaw)<=64<<10;record('exact temporal originals and immutable joined bytes retained',trigger_bytes=len(triggerRaw),temporal_originals=12,trigger_sha256=trigger['sha256'])
    # A read-only identity cannot submit even a well-formed proposal to the real origin.
    denied=dict(version=1,id='denied-rise',tenant='lab',site=d.config['site'],run_id=d.run,assignee='atlas/navigation',type='wendy.nav.inspect.v1',parameters={'target':selection['navigation_target']},summary='Denied fixture proposal',not_after_unix_ms=time.time_ns()//1_000_000+30000)
    atomic(d.out/'denied-proposal.json',denied)
    denied_call=subprocess.run([str(args.world),'task','propose','--credentials',str(d.inspection/'evidence-reader.json'),'--proposal',str(d.out/'denied-proposal.json')],capture_output=True,text=True,timeout=8)
    assert denied_call.returncode!=0 and ('node HTTP 401'in denied_call.stderr or 'node HTTP 403'in denied_call.stderr),(denied_call.stdout,denied_call.stderr)
    record('real origin refuses a reader without proposal scope',response=denied_call.stderr.strip()[:300])
    tampered=copy.deepcopy(selection['mission_input']);tampered['target'][0]+=1;atomic(d.out/'tampered-input.json',tampered)
    refused=subprocess.run([str(args.shared),'propose','--credentials',str(d.inspection/'proposer.json'),'--mission-input',str(d.out/'tampered-input.json'),'--handoff',str(d.out/'unexpected-handoff.json'),'--id','tampered-rise','--navigation-directory',str(d.inspection),'--source-read-token-file',str(d.inspection/'scene-read-token')],capture_output=True,text=True,timeout=8)
    assert refused.returncode!=0 and not(d.out/'unexpected-handoff.json').exists();record('changed fixed target cannot prepare a proposal',response=refused.stderr.strip()[:300])
    reviewed=action('east','inspection-review');assert no_motion() and 0<reviewed['reviewed_payload_bytes']<=32<<10,reviewed;record('explicit review saved only an unsigned draft',native=reviewed)
    capture('east','Review the retained rising episode and fixed destination before signing.',True)
    # A later change cannot replace the retained episode or destination.
    d.command('decline');settle('later observations do not establish a sustained rise','non_rising',2)
    assert d.prepared==selection and no_motion();record('new evidence cannot retarget the retained review',destination=selection['navigation_target'])
    signed=action('east','inspection-sign');task=signed['task_id'];assert task and signed['error']=='Inspection submitted. Check its status below.',signed;record('operator explicitly signed one retained rising inspection',native=signed)
    end=time.monotonic()+12
    while time.monotonic()<end:
        d.step()
        if all(n in d.statuses and d.statuses[n]['record']['spec']['id']==task for n in ('east','west')) and not no_motion():break
    else:raise AssertionError('signed task did not start and replicate')
    assert all(r['record']['spec']['target']==selection['navigation_target'] and r['record']['spec']['trigger']['sha256']==trigger['sha256'] for r in d.statuses.values())
    proof=d.statuses['east']['record']['proof'];approved_bytes=len(json.dumps(proof,separators=(',',':'),ensure_ascii=False).encode());signed_payload_bytes=len(json.dumps(proof['payload'],separators=(',',':'),ensure_ascii=False).encode());assert approved_bytes<=64<<10 and signed_payload_bytes<=32<<10;record('complete accepted signed instruction fits64KiB',signed_instruction_bytes=approved_bytes,signed_payload_bytes=signed_payload_bytes)
    capture('east','Explicitly approved planar motion follows the fixed destination.');capture('west','Independent reader retains the same original rise and task.')
    d.network['mission_partition']=True
    with d.robot.lock:start=d.robot.state()['position_m']
    end=time.monotonic()+50
    while time.monotonic()<end:
        d.step()
        phase=d.statuses['east']['record']['status']['Phase']
        if phase=='succeeded':break
        if phase in ('failed','canceled'):raise AssertionError(d.statuses['east']['record']['status'])
    else:raise AssertionError('inspection did not complete')
    with d.robot.lock:position=d.robot.state()['position_m']
    assert math.hypot(position[0]-start[0],position[2]-start[2])>.1
    assert d.statuses['west']['record']['status']['Phase']!='succeeded';record('authorized mission completed locally during replica partition',terminal=d.statuses['east']['record']['status'],partition_distance_m=math.hypot(position[0]-start[0],position[2]-start[2]))
    d.network['mission_partition']=False;end=time.monotonic()+14
    while time.monotonic()<end:
        d.step()
        if d.statuses.get('west',{}).get('record',{}).get('status',{}).get('Phase')=='succeeded':break
    else:raise AssertionError('terminal evidence did not repair')
    for name in ('east','west'):
        result=d.statuses[name];assert result['record']['spec']['trigger']['sha256']==trigger['sha256'];assert result['record']['spec']['trigger']['component']['payload']==payload;assert len(result['verified_media'])==3 and len(result['record']['result']['samples'])==4
    assert d.statuses['east']['record']==d.statuses['west']['record'];record('both independent readers retain identical terminal result, twelve original gas samples and three verified media objects',record_sha256=d.statuses['east']['record_sha256'])
    capture('east','Completed inspection retains the original rise, fixed target and verified evidence.');capture('west','Repair restores the same completed result at the second operator.')
    # Exact task retry must not initiate another execution.
    before=sum(math.hypot(c['velocity'][0],c['velocity'][2])>0 for c in d.robot.commands);refreshed=action('east','inspection-refresh');pump();assert refreshed['error']=='Status refreshed.',refreshed;assert d.statuses['east']['record']['status']['Phase']=='succeeded';after=sum(math.hypot(c['velocity'][0],c['velocity'][2])>0 for c in d.robot.commands);assert after==before
    journals=[]
    for path in (d.inspection/'journal').rglob('*.json'):
        saved=read_journal(path)
        for entry in saved.get('entries',[]):
            if entry.get('draft',{}).get('id')==task and entry.get('record',{}).get('status',{}).get('Phase')=='succeeded':journals.append((path,entry))
    assert len(journals)==1,journals
    assert journals[0][1]['record']==d.statuses['east']['record']
    record('native SDK validated the complete typed terminal proof without another execution',nonzero_commands_before=before,nonzero_commands_after=after,journal=str(journals[0][0].relative_to(d.out)))
    for name in ('east','west'):atomic(d.out/('completed-result-'+name+'.json'),d.statuses[name])
    d.command('ramp');settle('fresh rise for stop-before-delivery trial','supported_rise',1.5,remaining=.5)
    action('east','inspection-prepare');second=action('east','inspection-review');future=second['task_id'];assert future and future!=task
    before=sum(math.hypot(c['velocity'][0],c['velocity'][2])>0 for c in d.robot.commands)
    stop=subprocess.run([str(args.world),'task','stop','--credentials',str(d.inspection/'operator.json'),'--id',future,'--json'],capture_output=True,text=True,timeout=5);assert stop.returncode==0,stop.stderr
    action('east','inspection-sign');until=time.monotonic()+3
    while time.monotonic()<until:d.step()
    after=sum(math.hypot(c['velocity'][0],c['velocity'][2])>0 for c in d.robot.commands);assert after==before
    record('stop before signed task delivery prevents another movement',task_id=future,commands_before=before,commands_after=after)
    return dict(passed=True,checks=checks,run_id=d.run,captures=d.sequence,simulation='Actual MuJoCo two-slide-joint robot,96x72 rendered cameras/LiDAR,analytic gas; native scripted callbacks',limits=['Historical rising context may be explicitly approved after its original support expires; preparation requires current support.','No autonomous approval, retargeting, clearance, calibrated gas probability or physical robot guarantee.'])
