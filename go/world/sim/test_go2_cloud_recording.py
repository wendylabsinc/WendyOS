"""Offline adversarial recording checks using independent bounded CDR writers."""
import copy
import json
import math
import struct
import unittest

from sim.go2_cloud import SOURCE_DIGEST
from sim.go2_cloud_observer import Capture, encoded, MAX_REPORT
from sim.go2_cloud_recording import load_recording, decode_companion, RecordingError, _summary
from sim.test_go2_cloud import wire, ray

STAMP = 123_000_000_456


class Writer:
    def __init__(self):
        self.data = bytearray(b'\x00\x01\x00\x00')
    def n(self, kind, x):
        size = struct.calcsize('<'+kind)
        self.data.extend(b'\0' * ((-(len(self.data)-4)) % size))
        self.data.extend(struct.pack('<'+kind, x))
    def string(self, s):
        raw = s.encode('ascii')+b'\0'
        self.n('I', len(raw)); self.data.extend(raw)
    def header(self, frame, stamp=STAMP):
        self.n('i', stamp//10**9); self.n('I', stamp%10**9); self.string(frame)
    def vector(self, values, kind='d'):
        for x in values: self.n(kind,x)
    def sequence(self, values, count=None):
        self.n('I',len(values) if count is None else count); self.vector(values,'f')


def scan_wire(stamp=STAMP, **options):
    w=Writer(); w.header('lidar_link',stamp)
    values=[-math.pi,math.pi,math.pi/180,0,.1,.1,12]
    if 'scalar' in options: values[options['scalar'][0]]=options['scalar'][1]
    w.vector(values,'f')
    ranges=[math.inf]*360; ranges[0]=options.get('range',1)
    w.sequence(ranges, options.get('count'))
    w.sequence(options.get('intensities',[.25]*360), options.get('intensity_count'))
    return bytes(w.data)


def odom_wire(stamp=STAMP+20_000_000, bad=None):
    w=Writer(); w.header('odom',stamp); w.string('base_link')
    w.vector([1,2,3] if bad is None else [bad,2,3]); w.vector([.1,.2,.3,.9])
    w.vector([.01]*36); w.vector([.02,.03,.04]); w.vector([.05,.06,.07]); w.vector([.02]*36)
    return bytes(w.data)


def tf_wire(count=1):
    w=Writer(); w.n('I',count); w.header('base_link',0); w.string('utlidar_lidar')
    w.vector([.28,0,.08]); w.vector([.1,.2,.3,.9])
    return bytes(w.data)


def fixtures():
    w=Writer(); w.n('Q',31)
    return [('/simulation/epoch',bytes(w.data)),('/tf_static',tf_wire()),
            ('/odom',odom_wire()),('/scan',scan_wire()),
            ('/utlidar/cloud',wire([ray(0),ray(4320),ray(8999)]))]


def report_for(parts=None):
    parts=fixtures() if parts is None else parts
    c=Capture(SOURCE_DIGEST,31,5,companion=lambda topic,raw:_summary(topic,decode_companion(topic,raw)))
    for i,(topic,raw) in enumerate(parts):
        c.ingest({'kind':'sample','topic':topic,'publisher_gid':'12'*24,
                  'source_timestamp_ns':STAMP+i,'received_timestamp_ns':0,
                  'helper_elapsed_ns':i+1,'payload_hex':raw.hex()},(i+1)*100)
    c.ingest({'kind':'complete','reason':'duration','messages':len(parts),
              'clouds':sum(x[0]=='/utlidar/cloud' for x in parts),
              'serialized_bytes':sum(len(x[1]) for x in parts)},900)
    return c.report(None,0,1000)


class RecordingTests(unittest.TestCase):
    def test_full_raw_values_original_wxyz_and_missing_slots(self):
        original=report_for()
        report,values,clouds=load_recording(encoded(original))
        self.assertEqual(report,original)
        self.assertEqual(len(values),5); self.assertEqual(len(clouds),1)
        self.assertEqual(values[3]['ranges'][0],1)
        self.assertTrue(all(x==math.inf for x in values[3]['ranges'][1:]))
        self.assertEqual(values[3]['intensities'],[.25]*360)
        self.assertEqual(values[3]['angle_min'],float(struct.unpack('<f',struct.pack('<f',-math.pi))[0]))
        self.assertEqual(values[2]['position_xyz'],[1,2,3])
        self.assertEqual(values[2]['quaternion_wxyz'],[.9,.1,.2,.3])
        self.assertEqual(values[1]['transforms'][0]['quaternion_wxyz'],[.9,.1,.2,.3])
        self.assertEqual(report['clouds'][0]['original_odometry'],values[2])
        self.assertEqual(clouds[0].capture_unix_ns,STAMP)
        self.assertEqual(sum(x is None for x in clouds[0].ranges_m),8997)

    def test_partial_joins_and_future_odom_remain_original(self):
        for topic in ('/scan','/odom','/tf_static','/simulation/epoch'):
            with self.subTest(topic=topic):
                parts=[x for x in fixtures() if x[0]!=topic]
                source=report_for(parts)
                report,_,_=load_recording(encoded(source))
                self.assertFalse(report['all_clouds_joined'])
                self.assertEqual(report['clouds'][0]['missing_joins'],source['clouds'][0]['missing_joins'])
        parts=fixtures();parts[2]=('/odom',odom_wire(STAMP+50_000_001))
        report,values,_=load_recording(encoded(report_for(parts)))
        self.assertIsNone(report['clouds'][0]['original_odometry'])
        self.assertEqual(values[2]['capture_unix_ns'],STAMP+50_000_001)

    def test_forged_report_and_byte_metadata_refuse(self):
        mutations=[
            lambda r:r.update(simulation=False),lambda r:r.update(operational_authority=True),
            lambda r:r.update(commands_published=True),lambda r:r.update(runtime_http_calls=1),
            lambda r:r.update(error='ignored'),lambda r:r.update(helper_exit_code=False),
            lambda r:r.update(capture_complete=False),lambda r:r.update(raw_payloads_retained=False),
            lambda r:r.update(source_digest='sha256:'+'0'*64),lambda r:r.update(extra='field'),
            lambda r:r['messages'][0].update(index=1),
            lambda r:r['messages'][0].update(serialized_sha256='0'*64),
            lambda r:r['messages'][0].update(serialized_bytes=1),
            lambda r:r['messages'][0].update(dds_received_timestamp_status='provided'),
            lambda r:r['clouds'][0].update(scan_message_index=None),
            lambda r:r['clouds'][0]['original_odometry']['position_xyz'].__setitem__(0,99),
            lambda r:r['shared_tf_evidence']['first_matching_frame_pair']['transform']['translation_xyz'].__setitem__(0,99),
            lambda r:r['helper_completion'].update(serialized_bytes=1),
            lambda r:r['limits'].update(odom_pair_skew_ns=99_000_000),
            lambda r:r.update(all_clouds_joined=False),lambda r:r.update(joined_clouds=0),
        ]
        for i,mutate in enumerate(mutations):
            with self.subTest(case=i):
                r=report_for();mutate(r)
                with self.assertRaises(RecordingError):load_recording(encoded(r))

    def test_capture_replay_rejects_gid_epoch_stamp_and_elapsed_changes(self):
        source=report_for()
        # Duplicate an original cloud, with internally updated row metadata:
        # replay must still reject its nonadvancing original capture stamp.
        dup=copy.deepcopy(source['messages'][-1]);dup['index']=5
        source['messages'].append(dup)
        with self.assertRaises(RecordingError):load_recording(encoded(source))
        for field,value in [('publisher_gid','0'*48),('source_timestamp_ns',0),
                            ('helper_elapsed_ns',0),('parent_elapsed_ns',0),
                            ('topic','/ground_truth'),('payload_hex','00'*110001)]:
            with self.subTest(field=field):
                source=report_for();source['messages'][-1][field]=value
                with self.assertRaises(RecordingError):load_recording(encoded(source))
        source=report_for(); source['expected_epoch']=32
        with self.assertRaises(RecordingError):load_recording(encoded(source))

    def test_json_bounds_duplicate_keys_nonfinite_and_types(self):
        for raw in [b'{"x":1,"x":2}',b'{"x":{"y":1,"y":2}}',b'{"x":NaN}',
                    b'{"x":Infinity}',b'{"x":1e400}',b'{"x":999999999999999999999999}',
                    b'['*2000,b'\xff',b'{} '*((MAX_REPORT//3)+1),b'[]',bytearray(b'{}'),
                    encoded(report_for()).decode('utf-8').encode('utf-16')]:
            with self.subTest(length=len(raw)),self.assertRaises(RecordingError):load_recording(raw)

    def test_second_valid_gid_and_header_regression_refuse(self):
        parts=fixtures()
        parts.insert(3,('/odom',odom_wire(STAMP+40_000_000)))
        source=report_for(parts)
        source['messages'][3]['publisher_gid']='34'*24
        with self.assertRaises(RecordingError):load_recording(encoded(source))
        source=report_for(parts)
        # A wholly replaced and rehashed second CDR still cannot replay an
        # older source header. Editing claimed metadata cannot override ingest.
        import hashlib
        raw=odom_wire(STAMP+10_000_000)
        source['messages'][3].update(payload_hex=raw.hex(),serialized_sha256=hashlib.sha256(raw).hexdigest(),serialized_bytes=len(raw))
        with self.assertRaises(RecordingError):load_recording(encoded(source))

    def test_companion_truncation_trailing_encoding_and_count_bounds(self):
        for topic,raw in fixtures()[:-1]:
            for n in range(len(raw)):
                with self.subTest(topic=topic,n=n),self.assertRaises((RecordingError,ValueError)):
                    decode_companion(topic,raw[:n])
            for bad in (raw+b'\0',b'\x00\x00'+raw[2:]):
                with self.assertRaises(RecordingError):decode_companion(topic,bad)
        for topic,raw in [('/scan',scan_wire(count=0xffffffff)),('/scan',scan_wire(intensity_count=8193)),
                          ('/tf_static',tf_wire(33))]:
            with self.assertRaises(RecordingError):decode_companion(topic,raw)
        bad=bytearray(odom_wire());struct.pack_into('<I',bad,12,0xffffffff)
        with self.assertRaises(RecordingError):decode_companion('/odom',bytes(bad))

    def test_nonfinite_cdr_and_invalid_scan_ranges_refuse(self):
        for bad in (math.nan,-math.inf,math.inf):
            with self.assertRaises(RecordingError):decode_companion('/odom',odom_wire(bad=bad))
        for bad in (math.nan,-math.inf,-1,13):
            with self.assertRaises(RecordingError):decode_companion('/scan',scan_wire(range=bad))
        for i in range(7):
            with self.assertRaises(RecordingError):decode_companion('/scan',scan_wire(scalar=(i,math.nan)))
        with self.assertRaises(RecordingError):decode_companion('/scan',scan_wire(intensities=[math.inf]))

    def test_coherent_metadata_cannot_hide_horizontal_disagreement(self):
        for mismatch in (2,math.inf):
            parts=fixtures();parts[3]=('/scan',scan_wire(range=mismatch))
            with self.assertRaisesRegex(RecordingError,'horizontal_mismatch'):
                load_recording(encoded(report_for(parts)))

    def test_return_values_do_not_alias_report_summaries(self):
        report,values,clouds=load_recording(encoded(report_for()))
        values[2]['position_xyz'][0]=99
        self.assertEqual(report['clouds'][0]['original_odometry']['position_xyz'][0],1)
        self.assertEqual(clouds[0].points[1].range_m,1)


if __name__ == '__main__':
    unittest.main()
