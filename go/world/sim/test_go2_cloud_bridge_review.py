"""Independent cloud bridge fences with original CDR and fake runtime clocks."""
import copy
import dataclasses
import hashlib
import inspect
import unittest
from types import MappingProxyType
from unittest.mock import patch

from sim.go2_bridge.core import Bridge
from sim.go2_bridge.protocol import Configuration, Refused, encoded
from sim.go2_cloud import SOURCE_DIGEST
from sim.go2_cloud_capture import convert
from sim.go2_cloud_stream import Assembler
from sim.test_go2_bridge import Time, Transport
from sim.test_go2_cloud_stream import Feeder, epoch_wire, mount_wire, scan_wire, odom_wire, cloud_wire
from sim.test_go2_tilt import tilted_configuration


def cloud_configuration():
    old,alignment,_=tilted_configuration()
    profile=copy.deepcopy(old.profile)
    profile.update(motion_envelope='isotropic-gait-v1',max_measured_lateral_mps=.15,
                   cloud_experiment={'model':'coherent-multiring-slab-v1',
                                     'scope':'contained-simulation','collector_duration_ms':60000})
    raw=encoded(profile);binding=dict(old.binding,profile_sha256=hashlib.sha256(raw).hexdigest())
    return Configuration(binding,alignment,raw)


def original_capture(stamp,sequence,with_original=False):
    a=Assembler(SOURCE_DIGEST,2,start_ns=0,startup_timeout_ns=500_000_000,
                run_timeout_ns=1_000_000_000,companion_timeout_ns=100_000_000)
    f=Feeder(a);f.send('/simulation/epoch',epoch_wire(2));f.send('/tf_static',mount_wire())
    f.send('/scan',scan_wire(stamp));f.send('/odom',odom_wire(stamp))
    original=f.send('/utlidar/cloud',cloud_wire(stamp))[0].candidate
    value=convert(original,capture_sequence=sequence)
    out=value.bridge_capture();out['cloud']=value.cloud_payload()
    return (out,value.original) if with_original else out


class CloudTransport(Transport):
    def __init__(self,t,cfg):
        super().__init__(t,cfg)
        self.latest,original=original_capture(t.w,1,True)
        self.originals={1:original}
        self.capture_error=False

    def capture(self):
        if self.capture_error:raise Refused('cloud_input_lost')
        return copy.deepcopy(self.latest)

    def newer(self):
        self.capture_seq+=1
        self.latest,original=original_capture(self.t.w,self.capture_seq,True)
        self.originals[self.capture_seq]=original

    def original_capture(self,sequence):
        return self.originals.get(sequence)


class CloudBridgeReviewTests(unittest.TestCase):
    def setUp(self):
        self.cfg=cloud_configuration();self.time=Time();self.transport=CloudTransport(self.time,self.cfg)
        self.bridge=Bridge(self.cfg,self.transport,self.time.clock(),fault_reporter=lambda _:None)

    def session(self):
        self.state=self.bridge.dispatch('/v1/session',{'binding':self.cfg.binding})
        self.bridge.dispatch('/v1/begin',{'session':self.state['session'],'executor_epoch':7,
            'task_id':'cloud-review','spec_digest':'a'*64,'binding_sha256':self.cfg.binding_sha256})
        self.time.advance(10)
        return self.state

    def command(self,sequence=1,capture=1):
        return {'session':self.state['session'],'sequence':sequence,'executor_epoch':7,
                'task_id':'cloud-review','spec_digest':'a'*64,'capture_sequence':capture,
                'body_vx':.1,'body_vy':0.,'yaw_rate':0.,
                'not_after_unix_ms':self.time.w//1_000_000+500,'decision_remaining_ms':500}

    def positive_publications(self):
        return [x for x in self.transport.sent if any(x[0])]

    def test_original_cloud_reaches_state_without_timestamp_or_range_fill(self):
        original=copy.deepcopy(self.transport.latest);state=self.session()
        self.assertEqual(state['cloud'],original['cloud'])
        self.assertEqual(state['cloud']['captured_unix_ns'],state['scan_captured_unix_ns'])
        self.assertEqual(state['cloud']['sequence'],state['capture_sequence'])
        self.assertEqual(sum(x is None for x in state['cloud']['ranges']),8999)
        self.assertIsNone(state['scan']['ranges'][1])
        state['cloud']['ranges'][4320]=9.
        self.assertEqual(self.bridge.dispatch('/v1/state',{'session':state['session']})['cloud'],original['cloud'])

    def test_cloud_requires_explicit_profile_and_presence(self):
        original=self.transport.latest.pop('cloud')
        with self.assertRaises(Refused):self.session()
        self.assertEqual(self.transport.arms,0)
        old,_,_=tilted_configuration();t=Time();tr=CloudTransport(t,old)
        self.assertEqual(tr.latest['cloud'],original)
        b=Bridge(old,tr,t.clock(),fault_reporter=lambda _:None)
        with self.assertRaises(Refused):b.dispatch('/v1/session',{'binding':old.binding})
        self.assertEqual(tr.arms,0)

    def test_added_publishers_are_pinned_before_dispatch(self):
        for name in ('publisher_gid','epoch_publisher','mount_publisher','cloud_mount_publisher'):
            with self.subTest(field=name):
                self.setUp();self.session();self.time.advance(10);self.transport.newer()
                self.transport.latest['cloud'][name]='e'*48
                with self.assertRaises(Refused):self.bridge.dispatch('/v1/command',self.command())
                self.assertEqual(self.positive_publications(),[])
                self.assertEqual(self.transport.arms,1)

    def test_cloud_original_identity_and_pair_are_required_before_arm(self):
        changes=[lambda c:c['cloud'].update(sequence=2),
                 lambda c:c['cloud'].update(observed_epoch=3),
                 lambda c:c['cloud'].update(captured_unix_ns=c['scan_captured_unix_ns']+1),
                 lambda c:c['cloud'].update(scan_sha256='invalid'),
                 lambda c:c['cloud'].update(publisher_gid='0'*48),
                 lambda c:c['cloud']['ranges'].__setitem__(4320,True)]
        for change in changes:
            with self.subTest(change=change):
                self.setUp();change(self.transport.latest)
                with self.assertRaises(Refused):self.session()
                self.assertEqual(self.transport.arms,0)

    def test_exact_original_age_limit_is_not_refreshed_by_new_cloud(self):
        self.session();self.time.advance(240);self.transport.newer()
        with self.assertRaisesRegex(Refused,'served_capture_stale'):
            self.bridge.dispatch('/v1/command',self.command(capture=1))
        self.assertEqual(self.positive_publications(),[])
        self.assertEqual(self.transport.arms,1)

    def test_healthy_newer_cloud_does_not_replace_served_command_capture(self):
        state=self.session();original=copy.deepcopy(state['cloud']);self.time.advance(10);self.transport.newer()
        self.bridge.dispatch('/v1/state',{'session':state['session']})
        self.assertEqual(self.bridge.served_captures[1]['cloud'],original)
        self.bridge.dispatch('/v1/command',self.command(capture=1))
        self.assertEqual(self.bridge.used_capture,1)
        self.assertEqual(len(self.positive_publications()),1)
        with self.assertRaises(Refused):self.bridge.dispatch('/v1/command',self.command(sequence=2,capture=1))
        self.assertEqual(len(self.positive_publications()),1)

    def test_collector_failure_latches_and_does_not_rearm_when_data_returns(self):
        self.session();self.transport.capture_error=True
        with self.assertRaises(Refused):self.bridge.dispatch('/v1/command',self.command())
        self.transport.capture_error=False;self.time.advance(10);self.transport.newer()
        with self.assertRaises(Refused):self.bridge.dispatch('/v1/state',{'session':self.state['session']})
        with self.assertRaises(Refused):self.bridge.dispatch('/v1/session',{'binding':self.cfg.binding})
        self.assertEqual(self.transport.arms,1);self.assertEqual(self.positive_publications(),[])

    def test_current_runtime_delay_cannot_extend_original_command_budget(self):
        self.session();request=self.command();self.transport.delay=500
        with self.assertRaises(Refused):self.bridge.dispatch('/v1/command',request)
        self.assertEqual(self.positive_publications(),[])
        self.assertEqual(self.transport.arms,1)

    def test_same_served_sequence_cannot_change_cloud_or_companion_tuple(self):
        changes=[lambda c:c['cloud'].update(serialized_sha256='e'*64),
                 lambda c:c['cloud'].update(odom_sha256='e'*64),
                 lambda c:c['cloud']['ranges'].__setitem__(4321,2.),
                 lambda c:c['pose_covariance'].__setitem__(0,.02),
                 lambda c:c['pose'].update(x=1.1)]
        for change in changes:
            with self.subTest(change=change):
                self.setUp();self.session();change(self.transport.latest)
                with self.assertRaisesRegex(Refused,'cloud_capture_changed'):
                    self.bridge.dispatch('/v1/command',self.command())
                self.assertEqual(self.positive_publications(),[])

    def test_clock_after_capture_work_controls_freshness(self):
        original=self.transport.capture
        def delayed():
            c=original();self.time.advance(250);return c
        self.transport.capture=delayed
        with self.assertRaisesRegex(Refused,'sensor_stale'):self.session()
        self.assertEqual(self.transport.arms,0)

    def test_shared_clock_fault_prevents_command_and_latches(self):
        self.session();self.time.w+=6_000_000
        with self.assertRaises(Refused):self.bridge.dispatch('/v1/command',self.command())
        self.time.w-=6_000_000
        with self.assertRaises(Refused):self.bridge.dispatch('/v1/state',{'session':self.state['session']})
        self.assertEqual(self.positive_publications(),[]);self.assertEqual(self.transport.arms,1)

    def test_original_evidence_publishers_and_stamps_match_served_fields(self):
        changes=[lambda o:dataclasses.replace(o,cloud=dataclasses.replace(o.cloud,publisher_gid='e'*48)),
                 lambda o:dataclasses.replace(o,scan=dataclasses.replace(o.scan,publisher_gid='e'*48)),
                 lambda o:dataclasses.replace(o,epoch_sample=dataclasses.replace(o.epoch_sample,publisher_gid='e'*48)),
                 lambda o:dataclasses.replace(o,odom=dataclasses.replace(o.odom,
                     value=MappingProxyType(dict(o.odom.value,capture_unix_ns=o.odom.value['capture_unix_ns']+1)))),
                 lambda o:dataclasses.replace(o,epoch_sample=dataclasses.replace(o.epoch_sample,
                     value=MappingProxyType({'epoch':3})))]
        for change in changes:
            with self.subTest(change=change):
                self.setUp();self.transport.originals[1]=change(self.transport.originals[1])
                with self.assertRaisesRegex(Refused,'cloud_evidence_unavailable'):self.session()
                self.assertEqual(self.positive_publications(),[])
                self.assertNotIn(1,self.bridge.served_cloud_originals)

    def test_original_evidence_rechecks_raw_hash_size_and_metadata_types(self):
        changes=[lambda s:dataclasses.replace(s,raw_cdr=s.raw_cdr+b'\x00'),
                 lambda s:dataclasses.replace(s,raw_cdr=b'\x00'*110001),
                 lambda s:dataclasses.replace(s,source_timestamp_ns=True),
                 lambda s:dataclasses.replace(s,received_timestamp_ns=-1)]
        for change in changes:
            with self.subTest(change=change):
                self.setUp();o=self.transport.originals[1]
                self.transport.originals[1]=dataclasses.replace(o,cloud=change(o.cloud))
                with self.assertRaisesRegex(Refused,'cloud_evidence_unavailable'):self.session()
                self.assertEqual(self.positive_publications(),[])

    def test_evidence_reads_exact_original_after_fault_without_control_or_clock(self):
        self.session();original=self.transport.originals[1]
        self.transport.capture_error=True
        with self.assertRaises(Refused):self.bridge.dispatch('/v1/state',{'session':self.state['session']})
        before=(self.bridge.fault,self.bridge.sequence,self.bridge.used_capture,len(self.transport.sent),self.transport.arms)
        with patch.object(self.bridge,'_clock',side_effect=AssertionError('clock')), \
             patch.object(self.transport,'status',side_effect=AssertionError('runtime')), \
             patch.object(self.transport,'capture',side_effect=AssertionError('sensor')), \
             patch.object(self.transport,'publish',side_effect=AssertionError('publish')):
            evidence=self.bridge.dispatch('/v1/cloud-evidence',{'capture_sequence':1})
        self.assertEqual(before,(self.bridge.fault,self.bridge.sequence,self.bridge.used_capture,len(self.transport.sent),self.transport.arms))
        self.assertLessEqual(len(encoded(evidence)),512<<10)
        self.assertFalse(evidence['operational_authority'])
        expected=(original.epoch_sample,original.cloud,original.scan,original.odom,*original.transforms)
        for saved,sample in zip(evidence['samples'],expected):
            self.assertEqual(bytes.fromhex(saved['payload_hex']),sample.raw_cdr)
            self.assertEqual(hashlib.sha256(bytes.fromhex(saved['payload_hex'])).hexdigest(),saved['serialized_sha256'])
            self.assertEqual(saved['publisher_gid'],sample.publisher_gid)
        evidence['samples'][1]['payload_hex']=''
        self.assertEqual(self.bridge.dispatch('/v1/cloud-evidence',{'capture_sequence':1})['samples'][1]['payload_hex'],original.cloud.raw_cdr.hex())

    def test_evidence_is_bounded_to_served_captures_and_invalid_reads_do_not_latch(self):
        self.session()
        for _ in range(9):
            self.time.advance(1);self.transport.newer()
            self.bridge.dispatch('/v1/state',{'session':self.state['session']})
        self.assertEqual(len(self.bridge.served_cloud_originals),8)
        self.assertEqual(len(self.bridge.served_captures),8)
        before=(self.bridge.fault,len(self.transport.sent),self.transport.arms)
        for request in ({'capture_sequence':1},{'capture_sequence':True},{'capture_sequence':0},
                        {'capture_sequence':10,'session':'extra'}):
            with self.subTest(request=request),self.assertRaises(Refused):
                self.bridge.dispatch('/v1/cloud-evidence',request)
        self.assertEqual(before,(self.bridge.fault,len(self.transport.sent),self.transport.arms))


class CloudProfileReviewTests(unittest.TestCase):
    def test_cloud_opt_in_has_exact_scope_model_and_duration(self):
        cfg=cloud_configuration()
        for value,valid in [(100,True),(60000,True),(99,False),(60001,False),(True,False),(None,False)]:
            profile=copy.deepcopy(cfg.profile);profile['cloud_experiment']['collector_duration_ms']=value
            raw=encoded(profile);binding=dict(cfg.binding,profile_sha256=hashlib.sha256(raw).hexdigest())
            with self.subTest(duration=value):
                if valid:Configuration(binding,encoded(cfg.alignment),raw)
                else:
                    with self.assertRaises(Refused):Configuration(binding,encoded(cfg.alignment),raw)
        changes=[lambda p:p.update(cloud_experiment=None),
                 lambda p:p['cloud_experiment'].update(scope='physical'),
                 lambda p:p['cloud_experiment'].update(model='unknown'),
                 lambda p:p['cloud_experiment'].update(extra=True),
                 lambda p:p['cloud_experiment'].pop('scope')]
        for change in changes:
            profile=copy.deepcopy(cfg.profile);change(profile);raw=encoded(profile)
            binding=dict(cfg.binding,profile_sha256=hashlib.sha256(raw).hexdigest())
            with self.subTest(change=change),self.assertRaises(Refused):
                Configuration(binding,encoded(cfg.alignment),raw)


class AcquisitionDelay:
    """Model waiting for the publication/delivery lock after processing work."""
    def __init__(self,lock,clock,caller,occurrence,delay_ns):
        self.lock=lock;self.clock=clock;self.caller=caller
        self.remaining=occurrence;self.delay_ns=delay_ns

    def __enter__(self):
        if inspect.currentframe().f_back.f_code.co_name==self.caller:
            self.remaining-=1
            if self.remaining==0:self.clock.advance(self.delay_ns)
        return self.lock.__enter__()

    def __exit__(self,*args):return self.lock.__exit__(*args)


class LivePublicationReviewTests(unittest.TestCase):
    def make(self):
        from sim.go2_cloud_live import LiveCloudSensors
        from sim.test_go2_cloud_live import Clock, Process, Feed, profile
        clock=Clock();live=LiveCloudSensors(SOURCE_DIGEST,31,profile(),clock=clock,
                                          process_factory=Process,start_thread=False)
        self.addCleanup(live.close)
        return live,clock,Feed(live)

    def test_publication_lock_delay_cannot_cross_original_companion_cutoff(self):
        live,clock,feed=self.make();feed.base()
        live.lock=AcquisitionDelay(live.lock,clock,'_events',1,100_000_000)
        feed.complete()
        self.assertIsNone(live.latest);self.assertEqual(live.sequence,0)
        self.assertEqual(live.unavailable_reason,'sensor_cloud_companion_deadline')

    def test_snapshot_delivery_lock_delay_cannot_cross_original_age_limit(self):
        live,clock,feed=self.make();feed.base();feed.complete()
        live.lock=AcquisitionDelay(live.lock,clock,'snapshot',2,240_000_000)
        with self.assertRaisesRegex(Refused,'sensor_cloud_stale'):live.snapshot()
        self.assertIsNone(live.latest);self.assertEqual(live.sequence,1)

    def test_first_bad_native_row_preserved_and_later_rows_cannot_replace_it(self):
        live,clock,feed=self.make();feed.base();feed.complete()
        original=live.original_capture(1);bad=b'{"kind":"sample","bad":true}\n'
        live._ingest_line(bad);live._ingest_line(b'not the original failure\n')
        self.assertEqual(live.rejected_native_row(),bad)
        metadata=live.diagnostics()['rejected_native_row']
        self.assertEqual(metadata,{'bytes':len(bad),'sha256':hashlib.sha256(bad).hexdigest(),
                                   'retained_bytes':len(bad),'truncated':False})
        self.assertIs(live.original_capture(1),original)
        metadata['sha256']='changed'
        self.assertEqual(live.diagnostics()['rejected_native_row']['sha256'],hashlib.sha256(bad).hexdigest())

    def test_oversize_native_rejection_retains_only_bounded_prefix(self):
        live,clock,feed=self.make();bad=b'x'*((256<<10)+1)
        live._ingest_line(bad)
        self.assertEqual(len(live.rejected_native_row()),256<<10)
        metadata=live.diagnostics()['rejected_native_row']
        self.assertEqual(metadata['bytes'],len(bad));self.assertTrue(metadata['truncated'])
        self.assertEqual(metadata['sha256'],hashlib.sha256(bad).hexdigest())

    def test_clock_failure_does_not_invent_a_rejected_input_row(self):
        live,clock,feed=self.make();feed.base();feed.complete();clock.bad=True
        with self.assertRaises(Refused):live.snapshot()
        self.assertIsNone(live.rejected_native_row())
        self.assertIsNone(live.diagnostics()['rejected_native_row'])

    def test_admission_metadata_work_is_inside_companion_deadline(self):
        live,clock,feed=self.make();feed.base();advanced=False
        builtin_len=len
        def slow_reference_length(value):
            nonlocal advanced
            if type(value) is bytes and not advanced:
                advanced=True;clock.advance(100_000_000)
            return builtin_len(value)
        # The live module computes original-byte lengths while constructing
        # admission metadata. Charge that work to the original candidate.
        with patch('sim.go2_cloud_live.len',side_effect=slow_reference_length,create=True):
            feed.complete()
        self.assertTrue(advanced);self.assertEqual(live.sequence,0)
        self.assertIsNone(live.latest)
        self.assertEqual(live.unavailable_reason,'sensor_cloud_companion_deadline')


class LiveProbeReportReviewTests(unittest.TestCase):
    def make(self):
        from sim.go2_cloud_live import LiveCloudSensors
        from sim.test_go2_cloud_live import Clock, Process, Feed, profile
        clock=Clock();live=LiveCloudSensors(SOURCE_DIGEST,31,profile(),clock=clock,
                                          process_factory=Process,start_thread=False)
        self.addCleanup(live.close)
        feed=Feed(live);feed.base();feed.complete()
        return live,clock,profile()

    def run_probe(self,live,clock,profile):
        from sim.go2_cloud_live import probe
        with patch('sim.go2_cloud_live.LiveCloudSensors',return_value=live), \
             patch('sim.go2_cloud_live.time.monotonic_ns',side_effect=lambda:clock.mono), \
             patch('sim.go2_cloud_live.time.sleep',return_value=None):
            return probe(SOURCE_DIGEST,31,profile,1)

    def test_final_requested_sample_cannot_override_deadline_error(self):
        live,clock,profile=self.make();get=live.capture_diagnostic
        def delayed(sequence):
            result=get(sequence);clock.advance(60_000_000_000);return result
        with patch.object(live,'capture_diagnostic',side_effect=delayed):
            result=self.run_probe(live,clock,profile)
        self.assertFalse(result['complete'])
        self.assertEqual(result['error'],'sensor_cloud_probe_deadline')
        self.assertEqual(result['before_close']['deadline_ns'],60_000_000_000)
        self.assertTrue(result['after_close']['cleanup']['reaped'])

    def test_helper_fault_before_close_prevents_complete_report(self):
        live,clock,profile=self.make();diagnostics=live.diagnostics;first=True
        def faulting_diagnostic():
            nonlocal first
            if first:first=False;live._fail('sensor_cloud_helper_exit')
            return diagnostics()
        with patch.object(live,'diagnostics',side_effect=faulting_diagnostic):
            result=self.run_probe(live,clock,profile)
        self.assertFalse(result['complete'])
        self.assertEqual(result['error'],'sensor_cloud_helper_exit')
        self.assertTrue(result['after_close']['cleanup']['reaped'])


if __name__=='__main__':unittest.main()
