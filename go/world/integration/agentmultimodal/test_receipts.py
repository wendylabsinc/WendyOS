"""Fake signatures exercise structural checks only; they never establish trust."""
import base64
import copy
import datetime
import json
import unittest
import receipts as check
from stage import APP, CAPTURE, RESULT, SERVICES, SENSORS, Refused


def raw(x): return json.dumps(x,separators=(',',':')).encode()
def b64(x): return base64.b64encode(x).decode()
def stamp(ns):
    sec,frac=divmod(ns,10**9)
    return datetime.datetime.fromtimestamp(sec,datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%S')+('.%09d'%frac if frac else '')+'Z'
def wrap(x):
    return {'exit.json':dict(version=1,termination='completed',exit_code=0,stdout_bytes=len(raw(x)),stderr_bytes=0,output_truncated=False,succeeded=True,child_started=True),'stdout.json':x}
def sign(plan,states,service,p,seq):
    scope=dict(tenant=plan['tenant'],site=plan['site'],device=plan['device'],app=APP,service=service,deployment_generation=states[service]['generation'],execution_domain='simulation',publish=[dict(component_type=p['component_type'],stream=p['stream'])])
    claims=dict(version=1,authority_domain='physical',scope=scope,epoch=41,issued_at_unix_ms=1000,expires_at_unix_ms=1000000,public_key=b64(b'p'*32))
    record=dict(version=1,authority_domain='physical',delegation_sha256=check.sha(raw(claims)),scope=scope,epoch=41,sequence=seq,issued_at_unix_ms=2000,expires_at_unix_ms=900000,publication=p)
    signed=dict(delegation=dict(claims=b64(raw(claims)),chain_der=[b64(b'public DER fixture')],signature=b64(b'd'*3309)),record=b64(raw(record)),signature=b64(b's'*64))
    return dict(request_json=b64(raw(check.proto_publication(p))),signed_record_json=b64(raw(signed)),proof_sha256=check.sha(raw(signed)),record_sha256=check.sha(raw(record)))
def timing(ns):return dict(sample_time=stamp(ns),clock_domain='simulation:run-a',error_known=True,error_bound_ns=0,age_hint_ns=0)


def fixture():
    anchor=1700000000000000000
    plan=dict(app=APP,tenant='test-tenant',device='spiffe://test/device/one',site='office',frame=dict(id='floor',revision='r1'),run_id='run-a',anchor_unix_ns=anchor,package_sha256='a'*64,scenario_version='scene-v1',seed=7,simulation_only=True)
    states={s:dict(app=APP,service=s,container=APP+'_'+s,pid=100+i,cgroup='/test/'+s,world_socket=s in SENSORS or s=='fusion') for i,s in enumerate(SERVICES)}
    for s in (*SENSORS,'fusion'):states[s].update(generation=(1<<63)+101,socket_directory='/run/private/'+s)
    scene={k:plan[k] for k in ('run_id','site','frame','package_sha256','anchor_unix_ns')};scene.update(boot_id='boot-scene',task=states['scene'])
    media={'camera-a':b'\x89PNG\r\n\x1a\nfixture','sensorlink-bridge':b'\xff\xd8fixture\xff\xd9','audio':b'RIFF0000WAVEfixture'}
    types={'camera-a':'image/png','sensorlink-bridge':'image/jpeg','audio':'audio/wav'}
    refs={s:dict(sha256=check.sha(data),size=len(data),content_type=types[s]) for s,data in media.items()}
    frames=[];collected={};selected={}
    for service,sensor in SENSORS.items():
        rows=[]
        for i in range(1,33 if service=='sensorlink-bridge' else 65):
            kind='camera' if sensor.startswith('camera') else 'gas' if service=='gas' else 'audio'
            sample=dict(schema_version='wendy.sensor.sample.v1',id=f'run-a/{sensor}/{i+100}',site_id=plan['site'],frame=plan['frame'],sensor_id=sensor,kind=kind,sequence=i+100,sample_ns=10**9+i*10**6,run_id='run-a',scenario_version=plan['scenario_version'],seed=7,package_sha256=plan['package_sha256'],sensor_model='model-'+sensor,position_m=[0,0,0],uncertainty_m=0.1,calibration_revision='r1')
            if kind=='camera':sample['camera']=dict(width=1,height=1,fx=1,fy=1,cx=0,cy=0,camera_to_site=[1,0,0,0,0,1,0,0,0,0,1,0,0,0,0,1],calibration_valid_for_ns=10**10)
            elif kind=='audio':sample['audio']=dict(duration_ns=500000000,sample_rate_hz=8000,channels=1)
            else:sample['gas']=dict(value_ppm=80,uncertainty_ppm=1,species='test')
            if service in refs:sample['evidence']=refs[service]
            if service=='sensorlink-bridge':sample['evidence']=dict(sha256='b'*64,size=19,content_type='image/png')
            capture=dict(version=1,scene_boot_id=scene['boot_id'],captured_unix_ns=anchor+sample['sample_ns'],simulation_anchor_unix_ns=anchor,sample_json=b64(raw(sample)))
            if service=='sensorlink-bridge':
                m=dict(version=1,channel_id=4,frame_sequence=i,timestamp_us=capture['captured_unix_ns']//1000,flags=1,jpeg_sha256=refs[service]['sha256'],jpeg_size=refs[service]['size'],capture_json=b64(raw(capture)))
                frames.append(dict(metadata=m,chunks=1,omitted_chunks=0))
                capture['sensorlink']=dict(protocol='wendylite.v1.sensorlink',channel_id=4,frame_sequence=i,timestamp_us=m['timestamp_us'],flags=1,scene_boot_id=scene['boot_id'],capture_sequence=sample['sequence'],captured_unix_ns=capture['captured_unix_ns'],sample_sha256=check.sha(raw(sample)),calibration_sha256='c'*64,evidence=refs[service])
            p=dict(entity='sensor/'+sensor,component_type=CAPTURE,stream=sensor,frame=plan['frame'],timing=timing(anchor+sample['sample_ns']),ttl_ns=3000000000,provenance=dict(origin='simulated',app_version='world-sensor-capture-v1',model_id=sample['sensor_model'],model_version=sample['scenario_version'],simulation=check.simulation(plan)),payload=b64(raw(capture)),tombstone=False)
            receipt=dict(capture_id=sample['id'],native_sequence=sample['sequence'],scene_boot_id=scene['boot_id'],captured_unix_ns=capture['captured_unix_ns'],sample_ns=sample['sample_ns'],**sign(plan,states,service,p,2*i))
            if service in refs:receipt['evidence_sha256']=refs[service]['sha256']
            rows.append(receipt)
            if i==1:selected[service]=check.publisher_receipt(receipt,service,plan,scene,states)
        out=dict(version=1,run_id='run-a',sensor_id=sensor,status='complete',receipts=rows)
        if service=='sensorlink-bridge':out['mode']='bridge'
        else:out['duplicates']=0
        collected[service]=wrap(out)
    collected['sensorlink-source']=wrap(dict(version=1,mode='source',run_id='run-a',sensor_id='camera-b',status='complete',frames_sent=32,chunks_sent=32,frames=frames))
    inputs=[];starts=[];deadlines=[]
    for service,item in selected.items():
        sample=copy.deepcopy(item['sample']);ns=anchor+sample['sample_ns'];span=sample.get('audio',{}).get('duration_ns',0)
        origin=dict(proof_sha256=item['reference']['envelope_sha256'],scene_boot_id=scene['boot_id'],simulation_anchor=stamp(anchor))
        if service=='sensorlink-bridge':origin['original_evidence']=sample['evidence'];sample['evidence']=refs[service]
        used=dict(sample=sample,sample_id=sample['id'],sensor_id=sample['sensor_id'],kind=sample['kind'],source=item['reference']['key']['source'],version=item['reference']['version'],sample_time=stamp(ns),capture_start=stamp(ns-span),valid_until=stamp(ns+3000000000-span),capture=origin)
        if service in refs:used['evidence']=refs[service]
        inputs.append(used);starts.append(ns-span);deadlines.append(ns+3000000000-span)
    hazard=dict(label='possible gas hazard',inputs=inputs,evidence=list(refs.values()),valid_until=stamp(min(deadlines)),input_digest='d'*64)
    references=[v['reference'] for v in selected.values()]
    result=dict(version=1,result_schema='wendy.hazard.v1',result_json=b64(raw(hazard)),inputs=references,retain_until_unix_ms=1000000)
    provenance=dict(origin='inferred',app_version='world-fusion-v1',model_id='synthetic-gas-warning-rule',model_version='1',inputs=[dict(id='sha256:'+r['envelope_sha256'],origin='simulated',simulation=check.simulation(plan)) for r in references])
    p=dict(entity='run-a/possible-gas-hazard',component_type=RESULT,stream='hazard',frame=plan['frame'],timing=timing(min(starts)),ttl_ns=min(deadlines)-min(starts),provenance=provenance,payload=b64(raw(result)),tombstone=False)
    fuser=dict(version=1,run_id='run-a',status='complete',attempts=2,evaluation=dict(state='hazard',reason='fixture',input_digest='d'*64,hazard=hazard),inputs=[dict(reference=x['reference'],envelope_json=b64(x['raw'])) for x in selected.values()],media=[dict(source=dict(app=APP,service=s,component_type=CAPTURE,stream=SENSORS[s]),reference=refs[s],data=b64(media[s])) for s in media],locally_verified=True,**sign(plan,states,'fusion',p,1))
    collected['fusion']=wrap(fuser)
    return plan,scene,states,collected


class ReceiptsTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):cls.valid=fixture()
    def fresh(self):return copy.deepcopy(self.valid)
    def test_complete_run_separates_native_and_agent_sequences(self):
        out=check.validate_receipts(*self.fresh())
        self.assertEqual(out['publisher_receipts'],dict(**{'camera-a':64,'gas':64,'audio':64,'sensorlink-bridge':32}))
        self.assertEqual(out['fusion_input_proofs'],4)
        self.assertFalse(out['retained_proof_retrieval_verified']);self.assertFalse(out['current_authority_verified'])
        self.assertIn('not independently',out['cryptographic_verification'])
    def test_wrapper_failures(self):
        for key,value in (('termination','timeout'),('exit_code',1),('output_truncated',True),('succeeded',False),('child_started',False),('signal',15),('stdout_bytes',33<<20)):
            with self.subTest(key=key):
                a=self.fresh();a[3]['gas']['exit.json'][key]=value
                with self.assertRaises(Refused):check.validate_receipts(*a)
    def test_scope_boot_count_and_integrity(self):
        changes=[lambda a:a[0].update(tenant='wrong'),lambda a:a[0].update(device='wrong'),lambda a:a[2]['gas'].update(generation=102),lambda a:a[1].update(boot_id='wrong'),lambda a:a[0].update(anchor_unix_ns=1),lambda a:a[3]['gas']['stdout.json']['receipts'].pop(),lambda a:a[2]['fusion'].update(socket_directory=a[2]['gas']['socket_directory']),lambda a:a[2]['sensorlink-source'].update(world_socket=True),lambda a:a[3]['gas']['stdout.json']['receipts'][0].update(proof_sha256='e'*64),lambda a:a[3]['gas']['stdout.json']['receipts'][0].update(native_sequence=1)]
        for i,change in enumerate(changes):
            with self.subTest(case=i):
                a=self.fresh();change(a)
                with self.assertRaises(Refused):check.validate_receipts(*a)
    def test_wire_metadata_and_chunk_loss(self):
        for key,value in (('timestamp_us',1),('jpeg_sha256','f'*64),('frame_sequence',2),('capture_json',b64(b'{}'))):
            with self.subTest(key=key):
                a=self.fresh();a[3]['sensorlink-source']['stdout.json']['frames'][0]['metadata'][key]=value
                with self.assertRaises(Refused):check.validate_receipts(*a)
        a=self.fresh();a[3]['sensorlink-source']['stdout.json']['frames'][0]['omitted_chunks']=1
        with self.assertRaises(Refused):check.validate_receipts(*a)
    def test_fusion_proof_media_and_lineage(self):
        changes=[lambda f:f.update(locally_verified=False),lambda f:f.update(status='insufficient_evidence'),lambda f:f['inputs'][0]['reference']['version'].update(sequence=101),lambda f:f['inputs'][0].update(envelope_json=b64(b'{}')),lambda f:f['media'][0].update(data=b64(b'changed')),lambda f:f['media'][0]['source'].update(service='gas'),lambda f:f['media'].pop(),lambda f:f['evaluation']['hazard']['inputs'][0]['version'].update(sequence=101),lambda f:f['evaluation']['hazard']['inputs'][-1]['capture'].pop('original_evidence')]
        for i,change in enumerate(changes):
            with self.subTest(case=i):
                a=self.fresh();change(a[3]['fusion']['stdout.json'])
                with self.assertRaises(Refused):check.validate_receipts(*a)
    def test_latest_value_drop_requires_final_frame_and_exact_gap_count(self):
        a=self.fresh(); bridge=a[3]["sensorlink-bridge"]["stdout.json"]
        bridge["receipts"].pop(28); bridge["dropped_frames"]=1
        out=check.validate_receipts(*a)
        self.assertEqual(out["sensorlink_dropped_frames"],1)
        self.assertEqual(out["publisher_receipts"]["sensorlink-bridge"],31)
        bridge["dropped_frames"]=0
        with self.assertRaises(Refused):check.validate_receipts(*a)
        bridge["dropped_frames"]=2;bridge["receipts"].pop()
        with self.assertRaises(Refused):check.validate_receipts(*a)
    def test_strict_json_and_bounds(self):
        for text in (b'{"a":1,"a":2}',b'{"a":NaN}',b'{"a":"\\ud800"}',b'['*50+b']'*50):
            with self.subTest(text=text):
                with self.assertRaises(Refused):check.strict_json(text,4096)
        with self.assertRaises(Refused):check.binary('A'*100,1)
        with self.assertRaises(Refused):check.strict_json(b'{}',1)
        with self.assertRaises(Refused):check.binary('YQ==\n',8)
        self.assertEqual(check.timestamp('2023-11-14T22:13:20.000000001Z'),1700000000000000001)

if __name__=='__main__':unittest.main()
