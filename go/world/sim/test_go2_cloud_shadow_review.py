"""Independent passive wrapper checks; no ROS, subprocesses or live authority."""
import json
import unittest
from unittest.mock import patch

from sim.go2_cloud import SOURCE_DIGEST
from sim.go2_cloud_stream import Assembler
from sim import go2_cloud_shadow as shadow
from sim.test_go2_cloud_stream import (
    MS, STAMP, cloud_wire, epoch_wire, mount_wire, odom_wire,
)
from sim.test_go2_cloud_stream_review import scan


class Clocks:
    def __init__(self):
        self.mono = 1_000_000_000
        self.wall = STAMP

    def advance(self, ns):
        self.mono += ns
        self.wall += ns


class Feed:
    def __init__(self, capture):
        self.capture = capture
        self.sequence = 0

    def send(self, topic, raw):
        self.sequence += 1
        row = dict(kind='sample', topic=topic, publisher_gid='12'*24,
                   source_timestamp_ns=STAMP+self.sequence,
                   received_timestamp_ns=0, helper_elapsed_ns=self.sequence,
                   payload_hex=raw.hex())
        self.capture.ingest_line(json.dumps(row).encode()+b'\n')

    def base(self):
        self.send('/simulation/epoch', epoch_wire())
        self.send('/tf_static', mount_wire())

    def pair(self, stamp=STAMP):
        self.send('/scan', scan(stamp))
        self.send('/odom', odom_wire(stamp))
        self.send('/utlidar/cloud', cloud_wire(stamp))

    def completion(self):
        c = self.capture
        c.ingest_line(json.dumps(dict(kind='complete', reason='max_clouds',
                        messages=len(c.messages), clouds=c.clouds,
                        serialized_bytes=c.raw_bytes)).encode()+b'\n')


def capture(clocks, *, delay=0, effect=None, **options):
    class Decoder(Assembler):
        def ingest(self, row, now_ns):
            events = super().ingest(row, now_ns)
            if row['topic'] == '/utlidar/cloud':
                clocks.advance(delay)
                if effect is not None:
                    effect(clocks)
            return events
    args = dict(duration_ms=1000, max_clouds=1, startup_ms=1000,
                companion_ms=100, monotonic_ns=lambda: clocks.mono,
                wall_ns=lambda: clocks.wall, assembler_factory=Decoder)
    args.update(options)
    return shadow.ShadowCapture(SOURCE_DIGEST, 31, **args)


def report(c, *, code=0, eof=True, reaped=True):
    return c.finish(code, eof, dict(actions=[], reaped=reaped, exit_code=code))


class ShadowIndependentReview(unittest.TestCase):
    def test_postdecode_exclusive_companion_boundary(self):
        for delay, expected in ((20*MS-1, 'retained'), (20*MS, 'refused')):
            with self.subTest(delay=delay):
                clocks = Clocks()
                c = capture(clocks, delay=delay, companion_ms=20)
                f = Feed(c); f.base(); f.pair(); f.completion()
                r = report(c)
                self.assertTrue(r['capture_complete'])
                candidate = r['candidates'][0]
                self.assertEqual(candidate['disposition'], expected)
                self.assertEqual(candidate['original_capture_stamps_ns'],
                                 dict(cloud=STAMP, scan=STAMP, odom=STAMP))
                self.assertEqual(candidate['assembled_monotonic_ns'], 1_000_000_000)
                self.assertEqual(candidate['postdecode_clock']['monotonic_after_ns'],
                                 1_000_000_000+delay)
                if expected == 'refused':
                    self.assertEqual(candidate['refusal_reason'], 'postdecode_companion_deadline')
                    self.assertEqual(r['retained_candidates'], 0)

    def test_postdecode_startup_and_run_cutoffs_never_retain(self):
        for options, delay, reason in (
            (dict(startup_ms=50), 50*MS, 'startup_deadline'),
            (dict(duration_ms=100, startup_ms=100), 100*MS, 'run_deadline'),
        ):
            with self.subTest(reason=reason):
                clocks = Clocks(); c = capture(clocks, delay=delay, **options)
                f = Feed(c); f.base(); f.pair(); f.completion()
                r = report(c)
                self.assertEqual(r['error'], reason)
                self.assertFalse(r['capture_complete'])
                self.assertEqual(r['retained_candidates'], 0)
                self.assertEqual(r['candidates'][0]['refusal_reason'], reason)
                count = len(c.messages)
                clocks.advance(MS); f.pair(STAMP+MS)
                self.assertEqual(len(c.messages), count)

    def test_json_decoding_cost_is_charged_before_retention(self):
        clocks = Clocks(); c = capture(clocks, companion_ms=20)
        f = Feed(c); f.base()
        f.send('/scan', scan()); f.send('/odom', odom_wire())
        real = shadow.strict
        def delayed(raw):
            decoded = real(raw)
            clocks.advance(20*MS)
            return decoded
        with patch.object(shadow, 'strict', delayed):
            f.send('/utlidar/cloud', cloud_wire())
        self.assertEqual(c.retained, 0)
        self.assertEqual(c.candidates[0]['refusal_reason'], 'postdecode_companion_deadline')

    def test_final_reference_work_is_charged_before_retention(self):
        clocks = Clocks(); c = capture(clocks, companion_ms=20)
        f = Feed(c); f.base()
        f.send('/scan', scan()); f.send('/odom', odom_wire())
        original = c._sample_ref
        count = 0
        def delayed(sample):
            nonlocal count
            value = original(sample)
            count += 1
            if count == 6:
                clocks.advance(20*MS)
            return value
        with patch.object(c, '_sample_ref', delayed):
            f.send('/utlidar/cloud', cloud_wire())
        self.assertEqual(count, 6)
        self.assertEqual(c.retained, 0)
        self.assertEqual(c.candidates[0]['refusal_reason'], 'postdecode_companion_deadline')
        self.assertEqual(c.events[-1]['kind'], 'assembler_ready')

    def test_clock_fault_during_decode_latches_without_retained_output(self):
        cases = [
            (lambda c: setattr(c, 'wall', c.wall-1), 'wall_regression'),
            (lambda c: setattr(c, 'mono', c.mono-1), 'monotonic_regression'),
            (lambda c: setattr(c, 'wall', c.wall+50*MS+1), 'wall_monotonic_divergence'),
        ]
        for effect, expected in cases:
            with self.subTest(expected=expected):
                clocks = Clocks(); c = capture(clocks, effect=effect)
                f = Feed(c); f.base(); f.pair()
                self.assertEqual(c.error, expected)
                self.assertEqual(c.retained, 0)
                clocks.advance(MS); f.completion()
                self.assertEqual(c.error, expected)
                self.assertFalse(report(c)['capture_complete'])

    def test_each_original_age_checked_not_only_cloud_or_average(self):
        clocks = Clocks(); clocks.wall += 249*MS
        c = capture(clocks); f = Feed(c); f.base()
        f.send('/odom', odom_wire(STAMP-20*MS))
        f.send('/scan', scan()); f.send('/utlidar/cloud', cloud_wire())
        f.send('/odom', odom_wire(STAMP+30*MS)); f.completion()
        r = report(c); candidate = r['candidates'][0]
        self.assertEqual(candidate['local_wall_ages_ns'],
                         dict(cloud=249*MS, scan=249*MS, odom=269*MS))
        self.assertEqual(candidate['refusal_reason'], 'original_age_exceeds_local_limit')
        self.assertEqual(r['retained_candidates'], 0)
        self.assertTrue(r['capture_complete'])

    def test_future_original_is_preserved_and_never_promoted_on_later_tick(self):
        clocks = Clocks(); c = capture(clocks)
        f = Feed(c); f.base(); f.pair(STAMP+1)
        self.assertEqual(c.candidates[0]['refusal_reason'], 'original_stamp_future_local_wall')
        clocks.advance(MS); c.tick(); f.completion()
        r = report(c)
        self.assertEqual(r['retained_candidates'], 0)
        self.assertEqual(r['candidates'][0]['original_capture_stamps_ns']['cloud'], STAMP+1)
        self.assertFalse(r['operational_authority'])
        self.assertEqual(r['commands_published'], 0)
        self.assertEqual(r['runtime_http_calls'], 0)

    def test_completion_eof_exit_and_reaping_are_distinct_requirements(self):
        for kwargs, expected in ((dict(eof=False), 'helper_missing_eof'),
                                 (dict(code=2), 'helper_nonzero_exit'),
                                 (dict(reaped=False), 'helper_cleanup_incomplete')):
            with self.subTest(expected=expected):
                clocks = Clocks(); c = capture(clocks); f = Feed(c)
                f.base(); f.pair(); f.completion()
                r = report(c, **kwargs)
                self.assertEqual(r['error'], expected)
                self.assertFalse(r['capture_complete'])
                # Historical point-in-time retention is not a successful run.
                self.assertEqual(r['retained_candidates'], 1)

    def test_completion_mismatch_and_trailing_sample_do_not_hide_as_success(self):
        for mismatch in (True, False):
            c = capture(Clocks()); f = Feed(c); f.base(); f.pair()
            if mismatch:
                c.ingest_line(b'{"kind":"complete","reason":"max_clouds",'
                              b'"messages":0,"clouds":1,"serialized_bytes":0}\n')
                expected = 'completion_counts'
            else:
                f.completion(); f.send('/odom', odom_wire(STAMP+MS))
                expected = 'sample_after_completion'
            self.assertEqual(report(c)['error'], expected)

    def test_report_copy_cannot_modify_originals_or_retention(self):
        c = capture(Clocks()); f = Feed(c); f.base(); f.pair(); f.completion()
        r = report(c)
        r['messages'][0]['row']['payload_hex'] = ''
        r['candidates'][0]['original_capture_stamps_ns']['cloud'] = 0
        r['candidates'][0]['disposition'] = 'refused'
        r['helper_completion']['row']['messages'] = 0
        r['assembler_status']['publisher_gids'].clear()
        again = report(c)
        self.assertNotEqual(again['messages'][0]['row']['payload_hex'], '')
        self.assertEqual(again['candidates'][0]['original_capture_stamps_ns']['cloud'], STAMP)
        self.assertEqual(again['candidates'][0]['disposition'], 'retained')
        self.assertEqual(again['helper_completion']['row']['messages'], 5)
        self.assertTrue(again['assembler_status']['publisher_gids'])


if __name__ == '__main__':
    unittest.main()
