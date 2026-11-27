"""Independent rejection tests for the raw laser's declared static mount.

These test the consumed producer contract, not tf2 cache eviction, calibration
accuracy or physical stopping. A static mount needs no per-scan timestamp.
"""
from copy import deepcopy
import math
import unittest

from sim.patrol_nav import validated_sensor_transforms


def baseline_mounts():
    return [dict(parent='base_link', child='laser',
                 translation=[.28, 0., .08], quaternion=[0., 0., 0., 1.])]


class StaticMountContractTests(unittest.TestCase):
    def test_baseline_mount_without_stamp_is_returned_with_original_identity(self):
        mounts = baseline_mounts()
        before = deepcopy(mounts)
        entry, translation, quaternion = mounts[0], mounts[0]['translation'], mounts[0]['quaternion']
        accepted = validated_sensor_transforms(mounts)
        self.assertIs(accepted, mounts)
        self.assertIs(accepted[0], entry)
        self.assertIs(accepted[0]['translation'], translation)
        self.assertIs(accepted[0]['quaternion'], quaternion)
        self.assertEqual(mounts, before)
        self.assertNotIn('stamp', accepted[0])

    def test_valid_unit_rotation_and_integer_coordinates_are_not_rewritten(self):
        mounts = baseline_mounts()
        mounts[0]['translation'] = (0, 0, 1)
        mounts[0]['quaternion'] = (0., 0., math.sqrt(.5), -math.sqrt(.5))
        before = deepcopy(mounts)
        self.assertIs(validated_sensor_transforms(mounts, scan_frame='laser'), mounts)
        self.assertEqual(mounts, before)

    def test_allowed_norm_tolerance_does_not_normalize_or_copy(self):
        mounts = baseline_mounts()
        mounts[0]['quaternion'][3] = math.sqrt(1. + .5e-6)
        original = mounts[0]['quaternion']
        accepted = validated_sensor_transforms(mounts)
        self.assertIs(accepted[0]['quaternion'], original)
        self.assertEqual(accepted[0]['quaternion'][3], math.sqrt(1. + .5e-6))

    def test_static_mount_extra_metadata_is_preserved_without_stamp_freshness_rule(self):
        mounts = baseline_mounts()
        mounts[0].update(stamp=-100., calibration_revision='fixture-mount-v1')
        self.assertIs(validated_sensor_transforms(mounts), mounts)
        self.assertEqual(mounts[0]['stamp'], -100.)
        self.assertEqual(mounts[0]['calibration_revision'], 'fixture-mount-v1')

    def test_missing_empty_nonlist_duplicate_and_nondictionary_mounts_rejected(self):
        for mounts in (None, [], {}, 'laser', (baseline_mounts()[0],),
                       baseline_mounts() * 2, [None], [0], [[]]):
            with self.subTest(mounts=mounts), self.assertRaises(ValueError):
                validated_sensor_transforms(mounts)

    def test_all_consumed_mount_fields_are_required(self):
        for field in ('parent', 'child', 'translation', 'quaternion'):
            mounts = baseline_mounts()
            del mounts[0][field]
            with self.subTest(field=field), self.assertRaises(ValueError):
                validated_sensor_transforms(mounts)

    def test_wrong_parent_child_and_scan_frame_rejected(self):
        for field, value in (('parent', 'map'), ('parent', 'odom'),
                             ('parent', ''), ('child', 'camera'), ('child', 'base_link'),
                             ('child', None)):
            mounts = baseline_mounts()
            mounts[0][field] = value
            with self.subTest(field=field, value=value), self.assertRaises(ValueError):
                validated_sensor_transforms(mounts)
        for frame in ('map', 'base_link', 'laser_level', '', None, True):
            with self.subTest(scan_frame=frame), self.assertRaises(ValueError):
                validated_sensor_transforms(baseline_mounts(), scan_frame=frame)

    def test_vector_lengths_and_container_types_are_rejected(self):
        for field, values in (
                ('translation', [None, [], [0., 0.], [0., 0., 0., 0.], '000', {0: 0., 1: 0., 2: 0.}]),
                ('quaternion', [None, [], [0., 0., 1.], [0., 0., 0., 1., 0.], '0001'])):
            for value in values:
                mounts = baseline_mounts()
                mounts[0][field] = value
                with self.subTest(field=field, value=value), self.assertRaises(ValueError):
                    validated_sensor_transforms(mounts)

    def test_nonfinite_boolean_string_and_null_vector_components_rejected(self):
        for field, length in (('translation', 3), ('quaternion', 4)):
            for index in range(length):
                for value in (math.nan, math.inf, -math.inf, True, False, '0', None):
                    mounts = baseline_mounts()
                    mounts[0][field][index] = value
                    with self.subTest(field=field, index=index, value=value), self.assertRaises(ValueError):
                        validated_sensor_transforms(mounts)

    def test_zero_and_nonunit_quaternions_rejected_before_any_normalization(self):
        for quaternion in ([0., 0., 0., 0.], [0., 0., 0., 2.],
                           [0., 0., 0., .99], [0., 0., 0., math.sqrt(1. + 2e-6)]):
            mounts = baseline_mounts()
            mounts[0]['quaternion'] = quaternion
            before = deepcopy(mounts)
            with self.subTest(quaternion=quaternion), self.assertRaises(ValueError):
                validated_sensor_transforms(mounts)
            self.assertEqual(mounts, before)


if __name__ == '__main__':
    unittest.main()
