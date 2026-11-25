"""Independent adversarial joins; candidates carry no live authority or freshness."""
import itertools
import math
import unittest

from sim.test_go2_cloud_recording import Writer
from sim.test_go2_cloud_stream import (
    Feeder, MS, STAMP, assembler, cloud_wire, epoch_wire, mount_wire, odom_wire,
)


def scan(stamp=STAMP, distance=1):
    w = Writer()
    w.header('lidar_link', stamp)
    w.vector([-math.pi, math.pi-2*math.pi/360, 2*math.pi/360,
              0, .1, .1, 12], 'f')
    ranges = [math.inf]*360
    ranges[0] = distance
    w.sequence(ranges)
    w.sequence([0]*360)
    return bytes(w.data)


class IndependentStreamReview(unittest.TestCase):
    def test_two_cloud_arrival_orders_preserve_nearest_originals(self):
        """All 90 interleavings preserve each topic's order, including late TF."""
        parts = [('/utlidar/cloud', cloud_wire(STAMP)),
                 ('/utlidar/cloud', cloud_wire(STAMP+30*MS)),
                 ('/scan', scan(STAMP)), ('/scan', scan(STAMP+30*MS)),
                 ('/odom', odom_wire(STAMP-20*MS)),
                 ('/odom', odom_wire(STAMP+35*MS))]
        checked = 0
        for order in itertools.permutations(range(6)):
            if any(order.index(i) > order.index(i+1) for i in (0, 2, 4)):
                continue
            for late_tf in (False, True):
                a = assembler()
                f = Feeder(a)
                f.send('/simulation/epoch', epoch_wire())
                if not late_tf:
                    f.send('/tf_static', mount_wire())
                ready = []
                odom_watermark = 0
                for i in order:
                    topic, raw = parts[i]
                    if topic == '/odom':
                        odom_watermark = STAMP+(-20 if i == 4 else 35)*MS
                    for event in f.send(topic, raw):
                        self.assertEqual(event.kind, 'ready', (order, event))
                        self.assertGreaterEqual(odom_watermark,
                                                event.candidate.cloud.value.capture_unix_ns)
                        ready.append(event.candidate)
                if late_tf:
                    ready.extend(e.candidate for e in f.send('/tf_static', mount_wire()))
                self.assertEqual(len(ready), 2, (order, late_tf))
                by_id = {c.capture_id: c for c in ready}
                self.assertEqual(by_id[1].odom_minus_cloud_ns, -20*MS)
                self.assertEqual(by_id[2].odom_minus_cloud_ns, 5*MS)
                self.assertEqual(by_id[1].odom.raw_cdr, parts[4][1])
                self.assertEqual(by_id[2].odom.raw_cdr, parts[5][1])
                self.assertEqual(a.poll(1000), ())
                self.assertEqual(a.status()['counters']['ready'], 2)
            checked += 1
        self.assertEqual(checked, 90)

    def test_later_invalid_pending_suppresses_batch_without_phantom_ready(self):
        a = assembler()
        f = Feeder(a)
        f.send('/simulation/epoch', epoch_wire())
        for stamp, distance in ((STAMP, 1), (STAMP+MS, 2)):
            f.send('/scan', scan(stamp, distance))
            f.send('/odom', odom_wire(stamp))
            f.send('/utlidar/cloud', cloud_wire(stamp))
        events = f.send('/tf_static', mount_wire())
        self.assertEqual([(e.kind, e.reason) for e in events],
                         [('fatal', 'horizontal_range_mismatch')])
        self.assertEqual(a.status()['counters']['ready'], 0)
        self.assertEqual(a.status()['pending'], 0)
        self.assertEqual(a.poll(100), ())

    def test_evicted_identity_and_epoch_cannot_be_reintroduced(self):
        for fault in ('publisher', 'original_stamp', 'epoch'):
            with self.subTest(fault=fault):
                a = assembler()
                f = Feeder(a)
                f.base()
                for i in range(20):
                    f.send('/odom', odom_wire(STAMP+i))
                self.assertEqual(a.status()['counters']['evicted_odoms'], 4)
                if fault == 'publisher':
                    events = f.send('/odom', odom_wire(STAMP+20), publisher_gid='34'*24)
                    expected = 'publisher_changed'
                elif fault == 'original_stamp':
                    events = f.send('/odom', odom_wire(STAMP))
                    expected = 'capture_stamp_nonadvancing'
                else:
                    events = f.send('/simulation/epoch', epoch_wire(32))
                    expected = 'epoch_changed'
                self.assertEqual(events[0].reason, expected)
                self.assertEqual(a.status()['state'], 'fault')
                self.assertEqual(f.send('/simulation/epoch', epoch_wire()), ())

    def test_pending_eviction_does_not_reuse_capture_id_or_extend_deadline(self):
        a = assembler()
        f = Feeder(a)
        f.base()
        for i in range(3):
            events = f.send('/utlidar/cloud', cloud_wire(STAMP+i*MS), (10+i)*MS)
        self.assertEqual([(e.capture_id, e.reason) for e in events], [(1, 'pending_capacity')])
        f.send('/scan', scan(STAMP+MS), 50*MS)
        ready = f.send('/odom', odom_wire(STAMP+2*MS), 51*MS)[0].candidate
        self.assertEqual(ready.capture_id, 2)
        self.assertEqual(ready.companion_deadline_ns, 111*MS)
        events = f.send('/scan', scan(STAMP+2*MS), 112*MS)
        self.assertEqual([(e.capture_id, e.reason) for e in events], [(3, 'companion_deadline')])
        self.assertEqual(a.status()['counters']['ready'], 1)

    def test_original_utc_is_not_replaced_by_caller_join_clock(self):
        # The assembler has no UTC clock. It must preserve, not certify or stamp,
        # both old and far-future originals. A future bridge must check their age.
        for stamp in (1, 2_000_000_000_000_000_000):
            a = assembler()
            f = Feeder(a)
            f.base()
            f.send('/scan', scan(stamp), 10)
            f.send('/odom', odom_wire(stamp), 11)
            c = f.send('/utlidar/cloud', cloud_wire(stamp), 12)[0].candidate
            self.assertEqual(c.cloud.value.capture_unix_ns, stamp)
            self.assertEqual(c.scan.value['capture_unix_ns'], stamp)
            self.assertEqual(c.odom.value['capture_unix_ns'], stamp)
            self.assertEqual(c.assembled_at_ns, 12)

    def test_retained_candidate_survives_later_mount_fault_without_mutation(self):
        a = assembler()
        f = Feeder(a)
        f.base()
        f.send('/scan', scan())
        f.send('/odom', odom_wire())
        c = f.send('/utlidar/cloud', cloud_wire())[0].candidate
        original = c.transforms[0].raw_cdr
        self.assertEqual(f.send('/tf_static', mount_wire(x=.281))[0].reason, 'mount_conflict')
        self.assertEqual(c.transforms[0].raw_cdr, original)
        with self.assertRaises(TypeError):
            c.transforms[0].value['transforms'][0]['translation_xyz'][0] = 99
        self.assertEqual(a.status()['mounts'], 0)


if __name__ == '__main__':
    unittest.main()
