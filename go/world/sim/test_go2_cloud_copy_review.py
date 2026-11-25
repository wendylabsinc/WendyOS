"""Independent ownership and validation checks for the bounded cloud copy."""
import copy
import unittest
from unittest.mock import patch

from sim.go2_bridge import core
from sim.go2_bridge.protocol import Refused
from sim.test_go2_bridge import Time, Transport, configuration
from sim.test_go2_cloud_bridge_review import CloudTransport, cloud_configuration


class AnnotatedText(str):
    def __new__(cls, value):
        item = super().__new__(cls, value)
        item.notes = []
        return item


class CloudCopyReviewTests(unittest.TestCase):
    def make(self):
        cfg = cloud_configuration()
        clock = Time()
        transport = CloudTransport(clock, cfg)
        bridge = core.Bridge(cfg, transport, clock.clock(), fault_reporter=lambda _: None)
        return cfg, clock, transport, bridge

    def test_accepted_string_subclass_metadata_keeps_deepcopy_ownership(self):
        _, _, transport, bridge = self.make()
        source = transport.capture()
        keys = ('model', 'source_digest', 'frame', 'serialized_sha256', 'scan_sha256',
                'odom_sha256', 'epoch_sha256', 'mount_sha256', 'cloud_mount_sha256')
        for key in keys:
            source['cloud'][key] = AnnotatedText(source['cloud'][key])
            source['cloud'][key].notes.append(key)
        bridge._cloud(source)
        result = core._copy_validated_capture(source)
        for key in keys:
            with self.subTest(field=key):
                self.assertIsNot(source['cloud'][key], result['cloud'][key])
                self.assertIsNot(source['cloud'][key].notes, result['cloud'][key].notes)
                result['cloud'][key].notes.append('output')
                self.assertEqual(source['cloud'][key].notes, [key])
                source['cloud'][key].notes.append('input')
                self.assertEqual(result['cloud'][key].notes, [key, 'output'])

    def test_invalid_range_values_cannot_enter_optimized_copy(self):
        changes = {
            'boolean': lambda c: c['cloud']['ranges'].__setitem__(0, True),
            'container': lambda c: c['cloud']['ranges'].__setitem__(0, [1]),
            'number_subclass': lambda c: c['cloud']['ranges'].__setitem__(0, type('Number', (float,), {})(1)),
            'nonfinite': lambda c: c['cloud']['ranges'].__setitem__(0, float('inf')),
            'short': lambda c: c['cloud']['ranges'].pop(),
            'extra': lambda c: c['cloud'].update(extra='unexpected'),
            'missing': lambda c: c['cloud'].pop('ranges'),
        }
        for name, change in changes.items():
            with self.subTest(case=name):
                cfg, _, transport, bridge = self.make()
                change(transport.latest)
                with patch.object(core, '_copy_validated_capture', side_effect=AssertionError('unvalidated copy')) as copier:
                    with self.assertRaises(Refused):
                        bridge.dispatch('/v1/session', {'binding': cfg.binding})
                    copier.assert_not_called()
                self.assertEqual(transport.arms, 0)
                self.assertEqual(bridge.served_captures, {})

    def test_zero_copy_transport_cannot_mutate_retained_or_returned_capture(self):
        cfg, _, transport, bridge = self.make()
        transport.capture = lambda: transport.latest
        original = copy.deepcopy(transport.latest)
        state = bridge.dispatch('/v1/session', {'binding': cfg.binding})
        retained = bridge.served_captures[1]
        self.assertIsNot(state['cloud']['ranges'], retained['cloud']['ranges'])
        self.assertIsNot(retained['cloud']['ranges'], transport.latest['cloud']['ranges'])
        state['cloud']['ranges'][4320] = 9.
        state['scan']['ranges'][0] = 9.
        state['pose_covariance'][0] = 9.
        self.assertEqual(retained, original)
        self.assertEqual(transport.latest, original)
        transport.latest['cloud']['ranges'][0] = 2.
        with self.assertRaisesRegex(Refused, 'cloud_capture_changed'):
            bridge.dispatch('/v1/state', {'session': state['session']})
        self.assertEqual(retained, original)
        self.assertEqual(bridge.served_cloud_originals[1], transport.originals[1])

    def test_legacy_capture_still_detaches_nested_values(self):
        cfg, _, _ = configuration()
        clock = Time()
        transport = Transport(clock, cfg)
        source = transport.capture()
        self.assertNotIn('cloud', source)
        expected = copy.deepcopy(source)
        result = core._copy_validated_capture(source)
        result['scan']['ranges'][0] = 9.
        result['pose_covariance'][0] = 9.
        self.assertEqual(source, expected)


if __name__ == '__main__':
    unittest.main()
