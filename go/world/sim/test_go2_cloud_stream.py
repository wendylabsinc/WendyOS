"""Deterministic incremental joins over independently encoded original CDR."""
import dataclasses
import hashlib
import itertools
import math
import unittest

from sim.go2_cloud import SOURCE_DIGEST
from sim.go2_cloud_stream import Assembler, MAX_PENDING, MAX_SCANS, MAX_ODOMS, MAX_INT
from sim.test_go2_cloud import wire, ray
from sim.test_go2_cloud_recording import Writer

STAMP = 123_000_000_456
MS = 1_000_000


def scan_wire(stamp=STAMP, **options):
    w=Writer();w.header('lidar_link',stamp)
    scalars=[-math.pi,math.pi-2*math.pi/360,2*math.pi/360,0,.1,.1,12]
    if 'scalar' in options:scalars[options['scalar'][0]]=options['scalar'][1]
    w.vector(scalars,'f');ranges=[math.inf]*360;ranges[0]=options.get('range',1)
    w.sequence(ranges);w.sequence([]);return bytes(w.data)


def epoch_wire(epoch=31):
    w=Writer();w.n('Q',epoch);return bytes(w.data)


def mount_wire(children=('lidar_link','utlidar_lidar'), parent='base_link', x=.28):
    w=Writer();w.n('I',len(children))
    for child in children:
        w.header(parent,0);w.string(child);w.vector([x,0,.08]);w.vector([0,0,0,1])
    return bytes(w.data)


def odom_wire(stamp=STAMP, quaternion=(0,0,0,1)):
    w=Writer();w.header('odom',stamp);w.string('base_link')
    w.vector([1,2,.33]);w.vector(quaternion);w.vector([.01]*36)
    w.vector([.02,.03,.04]);w.vector([.05,.06,.07]);w.vector([.02]*36)
    return bytes(w.data)


def cloud_wire(stamp=STAMP):
    return wire([ray(4320)],sec=stamp//10**9,nanosec=stamp%10**9)


def assembler(**kwargs):
    options=dict(start_ns=0,startup_timeout_ns=500*MS,run_timeout_ns=2_000*MS,companion_timeout_ns=100*MS)
    options.update(kwargs)
    return Assembler(SOURCE_DIGEST,31,**options)


class Feeder:
    def __init__(self, target):self.a=target;self.n=0
    def row(self,topic,payload,**kwargs):
        self.n+=1
        row={'kind':'sample','topic':topic,'publisher_gid':'12'*24,
             'source_timestamp_ns':STAMP+self.n,'received_timestamp_ns':0,
             'helper_elapsed_ns':self.n,'payload_hex':payload.hex()}
        row.update(kwargs);return row
    def send(self,topic,payload,now=None,**kwargs):
        row=self.row(topic,payload,**kwargs)
        return self.a.ingest(row,self.n if now is None else now)
    def base(self):
        self.send('/simulation/epoch',epoch_wire());self.send('/tf_static',mount_wire())
    def complete(self,stamp=STAMP,now=None):
        self.send('/scan',scan_wire(stamp),now)
        self.send('/odom',odom_wire(stamp),now)
        return self.send('/utlidar/cloud',cloud_wire(stamp),now)


class StreamTests(unittest.TestCase):
    def test_original_identity_bytes_and_immutable_candidate(self):
        a=assembler();f=Feeder(a);f.base();event=f.complete()[0];c=event.candidate
        self.assertEqual(event.kind,'ready');self.assertEqual(c.capture_id,1)
        self.assertEqual(c.observed_epoch,31);self.assertEqual(c.odom_minus_cloud_ns,0)
        self.assertEqual(c.cloud.value.capture_unix_ns,STAMP)
        self.assertEqual(c.scan.value['capture_unix_ns'],STAMP)
        self.assertEqual(c.odom.value['position_xyz'],(1,2,.33))
        self.assertEqual(c.odom.value['linear_velocity_xyz'],(.02,.03,.04))
        for s in [c.cloud,c.scan,c.odom,c.epoch_sample,*c.transforms]:
            self.assertEqual(s.serialized_sha256,hashlib.sha256(s.raw_cdr).hexdigest())
            self.assertEqual(s.publisher_gid,'12'*24);self.assertEqual(s.received_timestamp_ns,0)
        with self.assertRaises(dataclasses.FrozenInstanceError):c.capture_id=99
        with self.assertRaises(TypeError):c.odom.value['position_xyz'][0]=9
        with self.assertRaises(TypeError):c.odom.value['new']=3
        before=a.status();before['counters']['ready']=99;before['publisher_gids'].clear()
        self.assertEqual(a.status()['counters']['ready'],1)
        self.assertEqual(a.poll(50),())
        f.send('/odom',odom_wire(STAMP+MS),60)
        self.assertEqual(a.poll(61),());self.assertEqual(c.odom.value['capture_unix_ns'],STAMP)

    def test_cross_topic_delivery_permutations(self):
        parts=[('/scan',scan_wire()),('/odom',odom_wire()),('/utlidar/cloud',cloud_wire()),('/tf_static',mount_wire())]
        for ordering in itertools.permutations(parts):
            with self.subTest(order=[t for t,_ in ordering]):
                a=assembler();f=Feeder(a);f.send('/simulation/epoch',epoch_wire());events=[]
                for topic,payload in ordering:events.extend(f.send(topic,payload))
                self.assertEqual([e.kind for e in events],['ready'])

    def test_late_matching_scan_uses_original_stamp(self):
        a=assembler();f=Feeder(a);f.base();f.send('/odom',odom_wire())
        self.assertEqual(f.send('/utlidar/cloud',cloud_wire(),10*MS),())
        event=f.send('/scan',scan_wire(),35*MS)[0]
        self.assertEqual(event.candidate.scan.arrived_ns,35*MS)
        self.assertEqual(event.candidate.scan.value['capture_unix_ns'],STAMP)
        self.assertEqual(event.candidate.companion_deadline_ns,110*MS)

    def test_odom_waits_for_watermark_and_never_revises(self):
        a=assembler();f=Feeder(a);f.base()
        f.send('/odom',odom_wire(STAMP-30*MS));f.send('/scan',scan_wire())
        self.assertEqual(f.send('/utlidar/cloud',cloud_wire()),())
        self.assertEqual(a.poll(20),())
        result=f.send('/odom',odom_wire(STAMP+10*MS),21)[0].candidate
        self.assertEqual(result.odom_minus_cloud_ns,10*MS)
        self.assertEqual(f.send('/odom',odom_wire(STAMP+11*MS),22),())
        self.assertEqual(result.odom_minus_cloud_ns,10*MS)

    def test_nearest_tie_and_outside_bound_unavailable(self):
        for offset,reason in [(20*MS,'ambiguous_nearest_odom'),(51*MS,'odom_outside_pair_bound')]:
            a=assembler();f=Feeder(a);f.base()
            f.send('/odom',odom_wire(STAMP-offset));f.send('/scan',scan_wire())
            f.send('/utlidar/cloud',cloud_wire())
            events=f.send('/odom',odom_wire(STAMP+offset))
            self.assertEqual([(e.kind,e.reason) for e in events],[('unavailable',reason)])
            self.assertEqual(a.status()['counters']['ready'],0)

    def test_pair_bound_inclusive(self):
        a=assembler();f=Feeder(a);f.base();f.send('/odom',odom_wire(STAMP+50*MS))
        f.send('/scan',scan_wire());self.assertEqual(f.send('/utlidar/cloud',cloud_wire())[0].candidate.odom_minus_cloud_ns,50*MS)

    def test_early_positive_original_stamp_has_no_false_eviction(self):
        a=assembler();f=Feeder(a);f.base()
        self.assertEqual(f.complete(stamp=1)[0].kind,'ready')

    def test_epoch_must_precede_cloud(self):
        a=assembler();f=Feeder(a)
        e=f.send('/utlidar/cloud',cloud_wire())[0]
        self.assertEqual(e.reason,'epoch_not_observed_before_cloud')
        f.base();f.send('/scan',scan_wire());f.send('/odom',odom_wire())
        self.assertEqual(a.poll(10),());self.assertEqual(a.status()['counters']['ready'],0)

    def test_exclusive_companion_deadline_no_resurrection(self):
        a=assembler();f=Feeder(a);f.base();f.send('/odom',odom_wire())
        f.send('/utlidar/cloud',cloud_wire(),10*MS)
        self.assertEqual(a.poll(110*MS-1),())
        events=f.send('/scan',scan_wire(),110*MS)
        self.assertEqual([(e.kind,e.reason) for e in events],[('unavailable','companion_deadline')])
        self.assertEqual(a.poll(110*MS),())

    def test_run_and_startup_deadlines_are_fixed(self):
        a=assembler(startup_timeout_ns=50*MS);f=Feeder(a);f.base()
        events=f.send('/odom',odom_wire(),50*MS)
        self.assertEqual(events[0].reason,'startup_deadline');self.assertEqual(a.status()['odoms'],0)
        self.assertEqual(f.send('/scan',scan_wire(),51*MS),())
        a=assembler();f=Feeder(a);f.base();f.complete()
        self.assertEqual(a.poll(2_000*MS)[0].reason,'run_deadline')
        self.assertEqual(a.poll(2_001*MS),())
        self.assertEqual(a.status()['run_deadline_ns'],2_000*MS)

    def test_close_clears_all_slots_and_no_further_output(self):
        a=assembler();f=Feeder(a);f.base();f.send('/utlidar/cloud',cloud_wire())
        events=a.close(10)
        self.assertEqual([e.kind for e in events],['unavailable','closed'])
        self.assertEqual(a.close(11),());self.assertEqual(a.ingest({},12),())
        self.assertEqual([a.status()[k] for k in ['pending','scans','odoms','mounts']],[0]*4)

    def test_source_pin_and_constructor_bounds(self):
        with self.assertRaises(ValueError):Assembler('wrong',31,start_ns=0,startup_timeout_ns=1,run_timeout_ns=2,companion_timeout_ns=1)
        for options in [dict(start_ns=-1),dict(run_timeout_ns=True),dict(run_timeout_ns=60_000*MS+1),dict(companion_timeout_ns=100*MS+1),dict(startup_timeout_ns=5_001*MS),dict(start_ns=MAX_INT)]:
            with self.subTest(options=options),self.assertRaises(ValueError):assembler(**options)

    def test_malformed_rows_latch_and_clear(self):
        mutations=[lambda r:r.update(extra=1),lambda r:r.update(kind='other'),lambda r:r.update(topic='/other'),
                   lambda r:r.update(publisher_gid='00'*24),lambda r:r.update(source_timestamp_ns=0),
                   lambda r:r.update(received_timestamp_ns=-1),lambda r:r.update(helper_elapsed_ns=True),
                   lambda r:r.update(payload_hex='0'),lambda r:r.update(payload_hex='z0'),
                   lambda r:r.update(payload_hex='00'*110001),lambda r:r.update(payload_hex='00010000')]
        for change in mutations:
            a=assembler();f=Feeder(a);f.base();row=f.row('/utlidar/cloud',cloud_wire());change(row)
            self.assertEqual(a.ingest(row,10)[0].kind,'fatal')
            self.assertEqual(a.poll(11),());self.assertEqual(a.status()['mounts'],0)
        a=assembler();self.assertEqual(a.ingest({'kind':'complete'},0)[0].reason,'sample_schema')

    def test_clock_regression_and_invalid_clock_latch(self):
        for now,reason in [(9,'caller_clock_regression'),(True,'caller_clock_invalid')]:
            a=assembler();a.poll(10)
            self.assertEqual(a.poll(now)[0].reason,reason);self.assertEqual(a.poll(20),())
        a=assembler();f=Feeder(a);f.base()
        self.assertEqual(f.send('/odom',odom_wire(),10,helper_elapsed_ns=0)[0].reason,'helper_clock_regression')

    def test_publisher_epoch_and_stamp_changes_fatal(self):
        for mode in ['gid','epoch','capture','dds']:
            a=assembler();f=Feeder(a);f.base();f.send('/odom',odom_wire())
            if mode=='gid':e=f.send('/odom',odom_wire(STAMP+1),publisher_gid='34'*24)
            elif mode=='epoch':e=f.send('/simulation/epoch',epoch_wire(32))
            elif mode=='capture':e=f.send('/odom',odom_wire())
            else:e=f.send('/odom',odom_wire(STAMP+1),source_timestamp_ns=1)
            self.assertEqual(e[0].kind,'fatal');self.assertEqual(a.status()['state'],'fault')

    def test_transform_duplicates_conflicts_and_partial_mounts(self):
        for raw in [mount_wire(('lidar_link','lidar_link')),mount_wire(parent='world'),mount_wire(x=.29)]:
            a=assembler();f=Feeder(a)
            self.assertEqual(f.send('/tf_static',raw)[0].kind,'fatal')
        a=assembler();f=Feeder(a);f.send('/simulation/epoch',epoch_wire())
        f.send('/tf_static',mount_wire(('lidar_link',)))
        self.assertEqual(f.complete(),())
        event=f.send('/tf_static',mount_wire(('utlidar_lidar',)))[0]
        self.assertEqual(event.kind,'ready')
        self.assertNotEqual(event.candidate.transforms[0].raw_cdr,event.candidate.transforms[1].raw_cdr)

    def test_horizontal_mismatch_is_fatal_not_unknown(self):
        a=assembler();f=Feeder(a);f.base();f.send('/scan',scan_wire(range=2));f.send('/odom',odom_wire())
        self.assertEqual(f.send('/utlidar/cloud',cloud_wire())[0].reason,'horizontal_range_mismatch')
        self.assertEqual(a.status()['counters']['ready'],0)

    def test_scan_source_geometry_cannot_change(self):
        for index,value in enumerate([-3.1, math.pi, .018, .01, .11, .2, 11]):
            a=assembler();f=Feeder(a)
            self.assertEqual(f.send('/scan',scan_wire(scalar=(index,value)))[0].reason,'scan_layout')

    def test_missing_scan_watermark_refuses(self):
        a=assembler();f=Feeder(a);f.base();f.send('/scan',scan_wire(STAMP+MS))
        self.assertEqual(f.send('/utlidar/cloud',cloud_wire())[0].reason,'scan_watermark_passed')

    def test_overload_explicitly_retires_and_keeps_watermarks(self):
        a=assembler();f=Feeder(a);f.base()
        for i in range(MAX_PENDING):self.assertEqual(f.send('/utlidar/cloud',cloud_wire(STAMP+i)),())
        events=f.send('/utlidar/cloud',cloud_wire(STAMP+2))
        self.assertEqual(events[0].reason,'pending_capacity');self.assertEqual(events[0].capture_id,1)
        self.assertEqual(a.status()['pending'],2)
        self.assertEqual(f.send('/utlidar/cloud',cloud_wire(STAMP))[0].reason,'capture_stamp_nonadvancing')
        a=assembler();f=Feeder(a)
        for i in range(40):
            f.send('/scan',scan_wire(STAMP+i));f.send('/odom',odom_wire(STAMP+i))
        status=a.status();self.assertEqual(status['scans'],MAX_SCANS);self.assertEqual(status['odoms'],MAX_ODOMS)
        self.assertEqual(status['counters']['evicted_scans'],32);self.assertEqual(status['counters']['evicted_odoms'],24)
        self.assertEqual(f.send('/scan',scan_wire(STAMP))[0].reason,'capture_stamp_nonadvancing')

    def test_evicted_odom_history_never_invents_nearest(self):
        a=assembler();f=Feeder(a);f.base()
        for i in range(MAX_ODOMS+1):f.send('/odom',odom_wire(STAMP+i))
        f.send('/scan',scan_wire())
        self.assertEqual(f.send('/utlidar/cloud',cloud_wire())[0].reason,'odom_history_evicted')

    def test_duplicate_does_not_extend_candidate_deadline(self):
        a=assembler();f=Feeder(a);f.base();row=f.row('/utlidar/cloud',cloud_wire())
        a.ingest(row,10*MS)
        self.assertEqual(a.ingest(row,90*MS)[0].kind,'fatal')
        self.assertEqual(a.poll(109*MS),())

    def test_input_row_mutation_does_not_change_output(self):
        a=assembler();f=Feeder(a);f.base();f.send('/scan',scan_wire());f.send('/odom',odom_wire())
        row=f.row('/utlidar/cloud',cloud_wire());c=a.ingest(row,10)[0].candidate
        row['payload_hex']='';row['publisher_gid']='00'*24
        self.assertEqual(c.cloud.raw_cdr,cloud_wire());self.assertEqual(c.cloud.publisher_gid,'12'*24)


if __name__=='__main__':unittest.main()
