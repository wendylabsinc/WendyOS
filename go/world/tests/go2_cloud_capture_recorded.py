"""Replay retained candidates through conversion and the existing Go checker.

All checker uncertainty is an explicit synthetic fixture. This tests format and
builder interoperability only, never current freshness or permission to move.
Reads existing recordings and writes a new output directory; no runtime calls.
"""
import argparse, gzip, hashlib, json, math, pathlib, subprocess, sys, time
root=pathlib.Path(__file__).resolve().parents[1]
sys.path.insert(0,str(root))
from sim.go2_cloud_stream import Assembler
from sim.go2_cloud import SOURCE_DIGEST
from sim.go2_cloud_capture import convert
from sim.test_go2_cloud_capture import enriched_scan
from sim.test_go2_tilt import tilted_configuration
from go2_cloud_shadow_verify import verify as verify_shadow
from go2_cloud_stream_recorded import read_capture, replay as verify_recorded
parser=argparse.ArgumentParser(description=__doc__)
parser.add_argument('--shadow',required=True,type=pathlib.Path)
parser.add_argument('--recorded',required=True,action='append',type=pathlib.Path)
parser.add_argument('--checker',required=True,type=pathlib.Path)
parser.add_argument('--out',required=True,type=pathlib.Path)
args=parser.parse_args()
if len(args.recorded)!=2:
    parser.error('Exactly two original A/B recordings are required for this regression probe.')
if not args.checker.is_file():
    parser.error('An existing compiled world-go2-cloud-check executable is required.')
if args.out.exists():
    parser.error('Output directory already exists; preserve prior evidence.')
args.out.mkdir(parents=False,exist_ok=False)
out=args.out.resolve()
checker=args.checker.resolve()
keys=('kind','topic','publisher_gid','source_timestamp_ns','received_timestamp_ns','helper_elapsed_ns','payload_hex')
def sha(b):return hashlib.sha256(b).hexdigest()
def encoded(x):return json.dumps(x,sort_keys=True,separators=(',',':'),allow_nan=False).encode()+b'\n'
summary={'schema':'wendy.world.go2-cloud-capture-conversion-probe.v1','simulation':True,'logical_replay_only':True,'operational_authority':False,'new_capture':False,'commands_published':0,'cases':[], 'checker_scope':'First and last converted candidate from each recording, with explicit synthetic uncertainty/profile for wire and builder interoperability only.'}
paths={'live':args.shadow,'a':args.recorded[0],'b':args.recorded[1]}
for name,path in paths.items():
    if name=='live':
        if not path.is_file() or path.stat().st_size>48<<20:raise ValueError('bounded shadow report required')
        with path.open('rb') as stream:stored=stream.read((48<<20)+1)
        if len(stored)>48<<20:raise ValueError('shadow report grew')
        if path.suffix=='.gz':
            import io
            with gzip.GzipFile(fileobj=io.BytesIO(stored)) as stream:raw=stream.read((48<<20)+1)
        else:raw=stored
        checked=verify_shadow(raw)
        if not checked['integrity_valid'] or not checked['capture_complete'] or checked['refused_candidates']:
            raise ValueError('complete verified shadow capture without refused candidates required')
    else:
        stored,raw=read_capture(path)
        checked=verify_recorded(raw)
        if not checked['success']:raise ValueError('recorded arrival verification failed')
    p=json.loads(raw)
    if name=='live':
        start=p['start_clock']['monotonic_after_ns'];opts=dict(start_ns=start,startup_timeout_ns=p['startup_deadline_ns']-start,run_timeout_ns=p['run_deadline_ns']-start,companion_timeout_ns=p['companion_timeout_ns'])
        rows=[(m['row'],m['arrival_clock']['monotonic_after_ns']) for m in p['messages']];end=p['last_clock']['monotonic_after_ns']
    else:
        opts=dict(start_ns=0,startup_timeout_ns=10**9,run_timeout_ns=2*10**9,companion_timeout_ns=100_000_000)
        rows=[({k:m[k] for k in keys},m['parent_elapsed_ns']) for m in p['messages']];end=p['parent_elapsed_ns']
    a=Assembler(SOURCE_DIGEST,p['expected_epoch'],**opts);events=[]
    for row,now in rows:events.extend(a.ingest(row,now))
    events.extend(a.poll(end));events.extend(a.close(end))
    candidates=[e.candidate for e in events if e.kind=='ready'];assert not any(e.kind=='fatal' for e in events)
    converted=[];timing=[]
    for sequence,c in enumerate(candidates,1001):
        t=time.perf_counter_ns();v=convert(c,capture_sequence=sequence);timing.append(time.perf_counter_ns()-t);converted.append(v)
        assert v.original == c and v.original.cloud.raw_cdr==c.cloud.raw_cdr
        assert v.bridge_capture()['cloud_captured_unix_ns']==c.cloud.value.capture_unix_ns
    record={'recording':name,'input_sha256':sha(raw),'input_bytes':len(raw),'clouds':p['cloud_count'],'converted':len(converted),'unavailable':[{'capture_id':e.capture_id,'reason':e.reason} for e in events if e.kind=='unavailable'],'conversion_ns':{'min':min(timing),'max':max(timing),'mean':sum(timing)/len(timing)},'checker':[],'input_path':str(path)}
    for suffix,value in [('first',converted[0]),('last',converted[-1])]:
        scan=enriched_scan(value);cfg,_,_=tilted_configuration();profile=cfg.profile
        tilt=profile['tilted_scan'];age=1.;skew=abs(value.original.odom_minus_cloud_ns)/1e9;drift=math.hypot(.001,-.0005)*age
        pose=scan['TiltedPose'];he=tilt['height_calibration_error_m']+.05+drift+tilt['max_translation_m_per_wall_s']*skew;h=pose['odom_position_xyz'][2]-tilt['odom_floor_z_m']
        pose.update(pair_translation_bound_m=tilt['max_translation_m_per_wall_s']*skew,pair_attitude_bound_rad=tilt['max_rotation_rad_per_wall_s']*skew,base_height_min_m=h-he,base_height_max_m=h+he)
        scan.update(PositionUncertaintyM=.02+.05+drift,YawUncertaintyRad=.01+.0002*age)
        nav={'TiltedScan':tilt,'MaxAbsRollRad':profile['max_abs_roll_rad'],'MaxAbsPitchRad':profile['max_abs_pitch_rad'],'MotionEnvelope':profile['motion_envelope'],'FlatFloor':True,'HalfLengthM':profile['half_length_m'],'HalfWidthM':profile['half_width_m'],'ClearanceM':profile['clearance_m'],'MaxPositionUncertaintyM':profile['max_position_uncertainty_m'],'MaxYawUncertaintyRad':profile['max_yaw_uncertainty_rad']}
        req={'schema':'wendy.world.go2-cloud-check.v1','simulation':True,'observation':value.cloud_observation(scan),'bounds':{'Min':{'X':-4.,'Y':-4.},'Max':{'X':4.,'Y':4.}},'profile':nav,'continuation':None};data=encoded(req)
        proc=subprocess.run([str(checker)],input=data,stdout=subprocess.PIPE,stderr=subprocess.PIPE,timeout=15)
        if len(proc.stdout)>8<<20 or len(proc.stderr)>65536:raise ValueError('checker output exceeded its expected bound')
        (out/f'{name}-{suffix}.request.json.gz').write_bytes(gzip.compress(data,mtime=0));(out/f'{name}-{suffix}.response.json.gz').write_bytes(gzip.compress(proc.stdout,mtime=0));(out/f'{name}-{suffix}.stderr').write_bytes(proc.stderr)
        assert proc.returncode==0,(name,suffix,proc.stderr)
        result=json.loads(proc.stdout);assert result['input_sha256']==sha(data) and result['operational_authority'] is False
        record['checker'].append({'candidate_id':value.original.capture_id,'served_sequence':value.sequence,'request_bytes':len(data),'request_sha256':sha(data),'response_sha256':sha(proc.stdout),'exit_code':proc.returncode,'cloud':result['cloud'],'horizontal':result['horizontal']})
    summary['cases'].append(record)
assert [(r['converted'],len(r['unavailable'])) for r in summary['cases']]==[(64,0),(4,1),(3,2)]
summary['sources']={str(p):sha((root/p).read_bytes()) for p in ['sim/go2_cloud_capture.py','sim/test_go2_cloud_capture.py','sim/test_go2_tilt.py','sim/test_go2_bridge.py','sim/go2_cloud_stream.py','sim/go2_cloud.py','sim/go2_cloud_recording.py','tests/go2_cloud_shadow_verify.py','tests/go2_cloud_stream_recorded.py','tests/go2_cloud_capture_recorded.py','internal/go2nav/cloud.go','cmd/world-go2-cloud-check/main.go']}
summary['checker_sha256']=sha(checker.read_bytes());(out/'summary.json').write_bytes(encoded(summary));print(json.dumps(summary,indent=2))
