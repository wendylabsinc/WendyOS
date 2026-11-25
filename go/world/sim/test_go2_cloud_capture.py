"""Original-byte conversion tests. No runtime or motion authority is exercised."""
import dataclasses
import hashlib
import json
import math
import unittest
from types import MappingProxyType
from unittest.mock import patch

from sim.go2_cloud_capture import CaptureError, convert
from sim.test_go2_cloud_stream import (STAMP, MS, Feeder, assembler, scan_wire,
                                       odom_wire, cloud_wire, mount_wire)


def candidate(odom_stamp=STAMP, quaternion=(0, 0, 0, 1), split_mounts=False):
    f = Feeder(assembler())
    if split_mounts:
        from sim.test_go2_cloud_stream import epoch_wire
        f.send('/simulation/epoch', epoch_wire())
        f.send('/tf_static', mount_wire(('lidar_link',)))
        f.send('/tf_static', mount_wire(('utlidar_lidar',)))
    else:
        f.base()
    f.send('/scan', scan_wire())
    f.send('/odom', odom_wire(odom_stamp, quaternion=quaternion))
    return f.send('/utlidar/cloud', cloud_wire())[0].candidate


def enriched_scan(value):
    """Explicit synthetic bounds for a checker fixture, never calibration."""
    b = value.bridge_capture()
    p = dict(b['tilted_pose'], odom_captured_unix_ns=b['odom_captured_unix_ns'],
             pair_translation_bound_m=0., pair_attitude_bound_rad=0.,
             odometry_epoch_age_s=1., base_height_min_m=.2, base_height_max_m=.5)
    return {'TiltedPose': p, 'Sequence': b['capture_sequence'],
            'CapturedUnixNS': b['scan_captured_unix_ns'], 'WorldEpoch': b['world_epoch'],
            'Frame': 'odom', 'Ranges': b['scan']['ranges'],
            'AngleMin': b['scan']['angle_min'], 'AngleIncrement': b['scan']['angle_increment'],
            'RangeMin': b['scan']['range_min'], 'RangeMax': b['scan']['range_max'],
            'Robot': {'X': b['pose']['x'], 'Y': b['pose']['y'], 'Yaw': b['pose']['yaw']},
            'SensorMount': {'X': .28, 'Y': 0., 'Yaw': 0.}, 'MountKnown': True,
            'PositionUncertaintyM': .1, 'YawUncertaintyRad': .02, 'UncertaintyKnown': True}


class CaptureTests(unittest.TestCase):
    def test_live_payload_retains_lineage_and_detaches_ranges(self):
        value=convert(candidate(split_mounts=True),capture_sequence=8)
        payload=value.cloud_payload();original=value.original
        self.assertEqual(payload['sequence'],8);self.assertEqual(payload['observed_epoch'],31)
        self.assertEqual(payload['mount_sha256'],original.transforms[0].serialized_sha256)
        self.assertEqual(payload['cloud_mount_sha256'],original.transforms[1].serialized_sha256)
        self.assertEqual(payload['epoch_publisher'],original.epoch_sample.publisher_gid)
        self.assertIsNone(payload['ranges'][1]);payload['ranges'][4320]=99
        self.assertEqual(value.cloud_payload()['ranges'][4320],1.)

    def test_original_refs_and_clock_evidence_survive_conversion(self):
        original = candidate(STAMP+20*MS, split_mounts=True)
        value = convert(original, capture_sequence=73)
        self.assertEqual(value.original, original)
        self.assertIsNot(value.original, original)
        self.assertEqual(value.sequence, 73)
        self.assertEqual(value.original.capture_id, 1)
        self.assertEqual(value.original.odom_minus_cloud_ns, 20*MS)
        b = value.bridge_capture()
        self.assertEqual(b['capture_sequence'], 73)
        self.assertEqual(b['cloud_captured_unix_ns'], STAMP)
        self.assertEqual(b['scan_captured_unix_ns'], STAMP)
        self.assertEqual(b['odom_captured_unix_ns'], STAMP+20*MS)
        self.assertEqual(b['body_velocity'], [.02, .03, .07])
        self.assertEqual(b['pose'], {'x': 1., 'y': 2., 'yaw': 0.})
        for sample in (original.epoch_sample, original.cloud, original.scan, original.odom, *original.transforms):
            self.assertEqual(hashlib.sha256(sample.raw_cdr).hexdigest(), sample.serialized_sha256)
        self.assertNotEqual(original.transforms[0].raw_cdr, original.transforms[1].raw_cdr)

    def test_existing_cloud_observation_shape_and_missing_returns(self):
        value = convert(candidate(), capture_sequence=73)
        observation = value.cloud_observation(enriched_scan(value))
        self.assertEqual(set(observation), {'model', 'source_digest', 'frame', 'publisher_gid',
                                         'serialized_sha256', 'captured_unix_ns', 'ranges', 'scan'})
        self.assertEqual(observation['scan']['Sequence'], 73)
        self.assertEqual(len(observation['ranges']), 9000)
        self.assertEqual(sum(v is None for v in observation['ranges']), 8999)
        self.assertEqual(observation['ranges'][4320], 1.)
        self.assertIsNone(observation['scan']['Ranges'][1])
        self.assertEqual(json.loads(json.dumps(observation, allow_nan=False)), observation)
        self.assertNotIn('operational_authority', observation)

    def test_detached_outputs_cannot_mutate_capture_or_later_output(self):
        value = convert(candidate(), capture_sequence=2)
        b = value.bridge_capture()
        b['scan']['ranges'][0] = 9
        b['tilted_pose']['quaternion_wxyz'][0] = 0
        self.assertEqual(value.bridge_capture()['scan']['ranges'][0], 1.)
        self.assertEqual(value.original.odom.value['quaternion_wxyz'], (1., 0., 0., 0.))
        scan = enriched_scan(value)
        out = value.cloud_observation(scan)
        scan['Ranges'][0] = 2
        out['ranges'][4320] = 3
        self.assertEqual(value.cloud_observation(enriched_scan(value))['ranges'][4320], 1.)
        with self.assertRaises(dataclasses.FrozenInstanceError):
            value.sequence = 3
        with self.assertRaises(TypeError):
            value._bridge['pose']['x'] = 4

    def test_original_orientation_retained_while_derived_matches_v2_normalization(self):
        q = (.01, .02, .03, math.sqrt(1-.01**2-.02**2-.03**2))
        q = tuple(v*(1+1e-8) for v in q)
        original = candidate(quaternion=q)
        value = convert(original, capture_sequence=1)
        derived = value.bridge_capture()['tilted_pose']['quaternion_wxyz']
        original_q = original.odom.value['quaternion_wxyz']
        norm = math.sqrt(sum(v*v for v in original_q))
        self.assertEqual(derived, [v/norm for v in original_q])
        self.assertNotEqual(tuple(derived), original_q)
        self.assertEqual(original.odom.value['quaternion_wxyz'], (q[3], *q[:3]))

    def test_no_clock_is_read_or_capture_timestamp_renewed(self):
        original = candidate()
        with patch('time.time_ns', side_effect=AssertionError('clock read')), \
             patch('time.monotonic_ns', side_effect=AssertionError('clock read')):
            value = convert(original, capture_sequence=44)
            self.assertEqual(value.cloud_observation(enriched_scan(value))['captured_unix_ns'], STAMP)

    def test_explicit_served_sequence_is_required_and_bounded(self):
        c = candidate()
        with self.assertRaises(TypeError):
            convert(c)
        for value in (0, -1, True, 1., 1 << 53):
            with self.subTest(value=value), self.assertRaises(CaptureError):
                convert(c, capture_sequence=value)
        self.assertEqual(convert(c, capture_sequence=(1 << 53)-1).sequence, (1 << 53)-1)

    def test_changed_raw_digest_or_decoded_value_refused(self):
        c = candidate()
        for key in ('cloud', 'scan', 'odom', 'epoch_sample'):
            sample = getattr(c, key)
            for change in (dict(raw_cdr=sample.raw_cdr+b'\x00'), dict(serialized_sha256='a'*64),
                           dict(value=MappingProxyType({})), dict(publisher_gid='0'*48)):
                with self.subTest(key=key, change=change), self.assertRaises(CaptureError):
                    convert(dataclasses.replace(c, **{key: dataclasses.replace(sample, **change)}), capture_sequence=1)

    def test_changed_candidate_identity_pair_and_deadlines_refused(self):
        c = candidate()
        changes = (dict(source_digest='sha256:'+'0'*64), dict(observed_epoch=32), dict(capture_id=True),
                   dict(odom_minus_cloud_ns=1), dict(companion_deadline_ns=c.assembled_at_ns),
                   dict(companion_deadline_ns=c.cloud.arrived_ns+100*MS+1),
                   dict(assembled_at_ns=c.cloud.arrived_ns-1), dict(transforms=c.transforms[:1]),
                   dict(epoch_sample=dataclasses.replace(c.epoch_sample, arrived_ns=c.cloud.arrived_ns+1)))
        for change in changes:
            with self.subTest(change=change), self.assertRaises(CaptureError):
                convert(dataclasses.replace(c, **change), capture_sequence=1)

    def test_enriched_scan_cannot_replace_originals_or_hide_missing_uncertainty(self):
        value = convert(candidate(), capture_sequence=2)
        changes = (lambda s: s.update(Sequence=1), lambda s: s.update(CapturedUnixNS=STAMP+1),
                   lambda s: s.update(WorldEpoch=32), lambda s: s.update(Frame='world'),
                   lambda s: s['Ranges'].__setitem__(1, 12.), lambda s: s['Robot'].update(X=0.),
                   lambda s: s['TiltedPose']['quaternion_wxyz'].__setitem__(0, 0.),
                   lambda s: s['TiltedPose'].update(odom_captured_unix_ns=STAMP+1),
                   lambda s: s.update(UncertaintyKnown=False), lambda s: s.pop('PositionUncertaintyM'),
                   lambda s: s.update(YawUncertaintyRad=math.nan), lambda s: s.update(extra=1),
                   lambda s: s['TiltedPose'].pop('pair_attitude_bound_rad'))
        for change in changes:
            scan = enriched_scan(value)
            change(scan)
            with self.subTest(scan=scan), self.assertRaises(CaptureError):
                value.cloud_observation(scan)


if __name__ == '__main__':
    unittest.main()
