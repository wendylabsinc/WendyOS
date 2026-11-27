"""Independent bridge compatibility and ownership boundary regressions."""
import copy
import dataclasses
import math
from types import MappingProxyType, SimpleNamespace as N
import unittest
from unittest.mock import patch

from sim.go2_cloud_capture import CaptureError, convert
from sim.test_go2_cloud_capture import candidate, enriched_scan
from sim.test_go2_tilt import observer, tilted_configuration, quaternion
from sim.test_go2_cloud_recording import Writer
from sim.test_go2_cloud_stream import Feeder, assembler, scan_wire, cloud_wire, STAMP


def xyz(values):
    return N(x=values[0],y=values[1],z=values[2])


def header(frame,stamp):
    return N(frame_id=frame,stamp=N(sec=stamp//10**9,nanosec=stamp%10**9))


def legacy_snapshot(original):
    """Exercise the existing Sensors handlers without starting ROS or a helper."""
    sensors=observer(tilted_configuration()[0].profile)
    def info(sample):return N(publisher_gid=bytes.fromhex(sample.publisher_gid))
    epoch=original.epoch_sample
    sensors.epoch=None;sensors._epoch(N(data=epoch.value['epoch']),info(epoch))
    mount=original.transforms[0]
    transforms=[]
    for value in mount.value['transforms']:
        q=value['quaternion_wxyz']
        transforms.append(N(header=header(value['frame_id'],value['capture_unix_ns']),
                            child_frame_id=value['child_frame_id'],
                            transform=N(translation=xyz(value['translation_xyz']),
                                        rotation=N(w=q[0],x=q[1],y=q[2],z=q[3]))))
    sensors._mount(N(transforms=transforms),info(mount))
    odom=original.odom;value=odom.value;q=value['quaternion_wxyz']
    sensors._odom(N(header=header(value['frame_id'],value['capture_unix_ns']),
                    child_frame_id=value['child_frame_id'],
                    pose=N(pose=N(position=xyz(value['position_xyz']),
                                  orientation=N(w=q[0],x=q[1],y=q[2],z=q[3])),
                           covariance=list(value['pose_covariance'])),
                    twist=N(twist=N(linear=xyz(value['linear_velocity_xyz']),
                                    angular=xyz(value['angular_velocity_xyz'])),
                            covariance=list(value['twist_covariance']))),info(odom))
    scan=original.scan;value=scan.value
    sensors._scan(N(header=header(value['frame_id'],value['capture_unix_ns']),
                    ranges=list(value['ranges']),angle_min=value['angle_min'],
                    angle_increment=value['angle_increment'],range_min=value['range_min'],
                    range_max=value['range_max']),info(scan))
    if sensors.fault:raise AssertionError(sensors.fault)
    return sensors.snapshot()


class CaptureReviewTests(unittest.TestCase):
    def test_pose_axes_mount_and_velocity_match_existing_sensor_handlers(self):
        for roll,pitch,yaw in [(.11,-.07,1.2),(-.12,.09,-2.4),(0.,0.,math.pi)]:
            q=quaternion(roll,pitch,yaw)
            for sign in (-1.,1.):
                scale=1. if max(abs(v) for v in q)==1. else 1+1e-8
                supplied=tuple(sign*scale*v for v in (*q[1:],q[0]))
                original=candidate(quaternion=supplied)
                actual=convert(original,capture_sequence=1).bridge_capture()
                expected=legacy_snapshot(original)
                with self.subTest(roll=roll,pitch=pitch,yaw=yaw,sign=sign):
                    self.assertEqual({key:actual[key] for key in expected},expected)
                    self.assertAlmostEqual(actual['roll_rad'],roll)
                    self.assertAlmostEqual(actual['pitch_rad'],pitch)
                    self.assertAlmostEqual(math.sin(actual['pose']['yaw']),math.sin(yaw))
                    self.assertEqual(actual['tilted_pose']['odom_position_xyz'],[1.,2.,.33])
                    self.assertEqual(actual['scan']['sensor_mount'],{'x':.28,'y':0.,'yaw':0.})

    def test_component_outside_existing_quaternion_bounds_is_not_normalized_away(self):
        for sign in (-1.,1.):
            original=candidate(quaternion=(0.,0.,0.,sign*(1+1e-8)))
            with self.assertRaisesRegex(AssertionError,'number_bound'):
                legacy_snapshot(original)
            with self.assertRaises(CaptureError):convert(original,capture_sequence=1)

    def test_original_covariance_keeps_existing_numeric_bounds(self):
        for extreme in (-1_000_001.,1_000_001.):
            w=Writer();w.header('odom',STAMP);w.string('base_link')
            w.vector([1.,2.,.33]);w.vector([0.,0.,0.,1.])
            covariance=[.01]*36;covariance[1]=extreme;w.vector(covariance)
            w.vector([.02,.03,.04]);w.vector([.05,.06,.07]);w.vector([.02]*36)
            feeder=Feeder(assembler());feeder.base();feeder.send('/scan',scan_wire())
            feeder.send('/odom',bytes(w.data));original=feeder.send('/utlidar/cloud',cloud_wire())[0].candidate
            with self.assertRaisesRegex(AssertionError,'number_bound'):legacy_snapshot(original)
            with self.assertRaises(CaptureError):convert(original,capture_sequence=1)

    def test_nested_booleans_cannot_replace_numeric_originals(self):
        converted=convert(candidate(),capture_sequence=1)
        changes=[lambda s:s['Ranges'].__setitem__(0,True),
                 lambda s:s['Robot'].update(X=True),
                 lambda s:s['SensorMount'].update(Y=False),
                 lambda s:s['TiltedPose']['quaternion_wxyz'].__setitem__(0,True),
                 lambda s:s['TiltedPose']['odom_position_xyz'].__setitem__(0,True),
                 lambda s:s['TiltedPose']['mount_translation_xyz'].__setitem__(1,False)]
        for change in changes:
            supplied=enriched_scan(converted);change(supplied)
            with self.subTest(change=change),self.assertRaises(CaptureError):
                converted.cloud_observation(supplied)

    def test_equivalent_mutable_sample_is_rejected_or_owned(self):
        original=candidate();backing=dict(original.odom.value)
        changed=dataclasses.replace(original,odom=dataclasses.replace(original.odom,value=backing))
        try:converted=convert(changed,capture_sequence=1)
        except CaptureError:return
        backing['position_xyz']=(9.,9.,9.)
        self.assertEqual(converted.original.odom.value['position_xyz'],(1.,2.,.33))

    def test_mapping_proxy_external_backing_cannot_change_retained_original(self):
        original=candidate();backing=dict(original.odom.value)
        changed=dataclasses.replace(original,odom=dataclasses.replace(original.odom,value=MappingProxyType(backing)))
        try:converted=convert(changed,capture_sequence=1)
        except CaptureError:return
        backing['position_xyz']=(9.,9.,9.)
        backing['pose_covariance']=(0.,)*36
        self.assertEqual(converted.original.odom.value['position_xyz'],(1.,2.,.33))
        self.assertEqual(converted.original.odom.value['pose_covariance'],(.01,)*36)
        self.assertEqual(converted.bridge_capture()['pose']['x'],1.)

    def test_nested_mount_mapping_proxy_cannot_change_retained_original(self):
        original=candidate();first=original.transforms[0]
        backing=dict(first.value['transforms'][0])
        nested=(MappingProxyType(backing),*first.value['transforms'][1:])
        changed=dataclasses.replace(first,value=MappingProxyType({'transforms':nested}))
        c=dataclasses.replace(original,transforms=(changed,changed))
        try:converted=convert(c,capture_sequence=1)
        except CaptureError:return
        backing['translation_xyz']=(8.,8.,8.)
        self.assertEqual(converted.original.transforms[0].value['transforms'][0]['translation_xyz'],(.28,0.,.08))

    def test_each_output_owns_nested_copies(self):
        converted=convert(candidate(),capture_sequence=2)
        baseline=converted.bridge_capture();a=converted.bridge_capture();b=converted.bridge_capture()
        a['pose_covariance'][0]=99.;a['twist_covariance'][0]=99.
        a['tilted_pose']['mount_translation_xyz'][0]=99.;a['scan']['sensor_mount']['x']=99.
        a['body_velocity'][0]=99.;a['pose']['x']=99.
        self.assertEqual(b,baseline);self.assertEqual(converted.bridge_capture(),baseline)
        supplied=enriched_scan(converted);out=converted.cloud_observation(supplied)
        original_output=copy.deepcopy(out)
        supplied['TiltedPose']['mount_translation_xyz'][0]=99.;supplied['Robot']['X']=99.
        self.assertEqual(out,original_output)
        out['scan']['TiltedPose']['odom_position_xyz'][0]=99.
        out['scan']['Ranges'][0]=99.;out['ranges'][0]=99.
        self.assertEqual(converted.cloud_observation(enriched_scan(converted)),original_output)

    def test_original_dds_stamps_and_metadata_are_not_rebased(self):
        original=candidate();converted=convert(original,capture_sequence=991)
        for before,after in zip((original.epoch_sample,original.cloud,original.scan,original.odom,*original.transforms),
                                (converted.original.epoch_sample,converted.original.cloud,converted.original.scan,
                                 converted.original.odom,*converted.original.transforms)):
            for name in ('publisher_gid','source_timestamp_ns','received_timestamp_ns','helper_elapsed_ns',
                         'arrived_ns','serialized_sha256','raw_cdr'):
                self.assertEqual(getattr(before,name),getattr(after,name))
        self.assertEqual(converted.original.capture_id,original.capture_id)
        self.assertEqual(converted.original.assembled_at_ns,original.assembled_at_ns)
        self.assertEqual(converted.original.companion_deadline_ns,original.companion_deadline_ns)
        self.assertEqual(converted.sequence,991)

    def test_no_time_network_or_process_is_used_for_conversion(self):
        original=candidate()
        with patch('time.time_ns',side_effect=AssertionError('time')), \
             patch('time.monotonic_ns',side_effect=AssertionError('time')), \
             patch('subprocess.Popen',side_effect=AssertionError('process')), \
             patch('socket.socket',side_effect=AssertionError('network')):
            converted=convert(original,capture_sequence=1)
            value=converted.cloud_observation(enriched_scan(converted))
        self.assertEqual(value['captured_unix_ns'],original.cloud.value.capture_unix_ns)
        self.assertNotIn('ready',value);self.assertNotIn('fresh',value)


if __name__=='__main__':unittest.main()
