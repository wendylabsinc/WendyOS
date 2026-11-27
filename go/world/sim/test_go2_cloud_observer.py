"""Passive stream/lifecycle tests. Fake helper processes have no ROS or network."""
import hashlib
import json
import os
from pathlib import Path
import signal
import stat
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock

from sim import go2_cloud_observer as obs
from sim.go2_cloud import SOURCE_DIGEST
from sim.test_go2_cloud import ray, wire


STAMP = 123_000_000_456
GID = '12' * 24


def companion(_topic, raw):
    # Deliberately separate from production ROS decoding: these tests check
    # aggregation, exact metadata, missing joins, bounds and helper lifecycle.
    return json.loads(raw)


def row(topic, raw, elapsed=1, gid=GID):
    return {'kind': 'sample', 'topic': topic, 'publisher_gid': gid,
            'source_timestamp_ns': 999_000_000_000 + elapsed,
            'received_timestamp_ns': 999_000_000_002 + elapsed,
            'helper_elapsed_ns': elapsed, 'payload_hex': raw.hex()}


def transformed():
    return {'frame_id': 'base_link', 'child_frame_id': 'utlidar_lidar',
            'capture_unix_ns': 0, 'translation_xyz': [.12, 0, .15],
            'quaternion_wxyz': [1, 0, 0, 0]}


def odom(stamp=STAMP + 20_000_000):
    return {'capture_unix_ns': stamp, 'frame_id': 'odom', 'child_frame_id': 'base_link',
            'position_xyz': [1, 2, 3], 'quaternion_wxyz': [1, 0, 0, 0],
            'linear_velocity_xyz': [.01, 0, 0], 'angular_velocity_xyz': [0, 0, .02],
            'pose_covariance': [.01] * 36, 'twist_covariance': [.02] * 36}


def fixtures():
    values = [('/simulation/epoch', {'epoch': 31}),
              ('/tf_static', {'transforms': [transformed()]}),
              ('/odom', odom()),
              ('/scan', {'capture_unix_ns': STAMP, 'frame_id': 'lidar_link', 'range_count': 360})]
    result = [row(topic, obs.encoded(value), index + 1) for index, (topic, value) in enumerate(values)]
    result.append(row('/utlidar/cloud', wire([ray(0), ray(4320), ray(8999)]), len(result) + 1))
    return result


def completion(rows):
    return {'kind': 'complete', 'reason': 'max_clouds', 'messages': len(rows),
            'clouds': sum(r['topic'] == '/utlidar/cloud' for r in rows),
            'serialized_bytes': sum(len(r['payload_hex']) // 2 for r in rows)}


def capture(rows=None, finish=True):
    c = obs.Capture(SOURCE_DIGEST, 31, 5, companion=companion)
    rows = fixtures() if rows is None else rows
    for i, value in enumerate(rows):
        c.ingest(value, (i + 1) * 100)
    if finish:
        c.ingest(completion(rows), (len(rows) + 1) * 100)
    return c


class StreamTests(unittest.TestCase):
    def test_complete_metadata_retained_without_identity_or_geometry_claim(self):
        c = capture()
        report = c.report(None, 0, 1000)
        self.assertTrue(report['capture_complete'])
        self.assertTrue(report['all_clouds_joined'])
        cloud = report['clouds'][0]
        self.assertEqual((cloud['finite_points'], cloud['missing_rays']), (3, 8997))
        self.assertEqual(cloud['odom_minus_cloud_ns'], 20_000_000)
        self.assertEqual(cloud['original_odometry'], odom())
        self.assertEqual(cloud['observed_epoch_at_arrival'], 31)
        self.assertIn('not_encoded_in_cloud', cloud['epoch_association'])
        self.assertIn('not_validated_geometry', cloud['join_role'])
        self.assertIn('not_verified', report['source_identity_role'])
        self.assertIn('not_original_transport_packet', report['payload_representation'])
        raw = bytes.fromhex(report['messages'][4]['payload_hex'])
        self.assertEqual(report['messages'][4]['serialized_sha256'], hashlib.sha256(raw).hexdigest())
        self.assertEqual(report['messages'][4]['publisher_gid'], GID)
        self.assertNotEqual(report['messages'][4]['source_timestamp_ns'], cloud['capture_unix_ns'])
        self.assertFalse(report['operational_authority'])
        self.assertEqual((report['runtime_http_calls'], report['commands_published']), (0, 0))

    def test_missing_joins_and_epoch_observed_later_are_not_invented(self):
        rows = [fixtures()[-1], row('/simulation/epoch', obs.encoded({'epoch': 31}), 6)]
        report = capture(rows).report(None, 0, 1000)
        self.assertTrue(report['capture_complete'])
        self.assertFalse(report['all_clouds_joined'])
        cloud = report['clouds'][0]
        self.assertIsNone(cloud['observed_epoch_at_arrival'])
        self.assertIsNone(cloud['original_odometry'])
        self.assertEqual(set(cloud['missing_joins']), {'same_capture_scan', 'odom_within_50ms', 'cloud_mount_transform', 'epoch_before_cloud_arrival'})
        self.assertEqual(report['observed_epoch'], 31)

    def test_zero_dds_receive_time_is_explicitly_unavailable(self):
        rows = fixtures()
        rows[-1]['received_timestamp_ns'] = 0
        report = capture(rows).report(None, 0, 1)
        self.assertEqual(report['messages'][-1]['received_timestamp_ns'], 0)
        self.assertEqual(report['messages'][-1]['dds_received_timestamp_status'], 'unavailable_zero')
        self.assertEqual(report['messages'][0]['dds_received_timestamp_status'], 'provided')
        self.assertTrue(report['capture_complete'])

    def test_fifth_cloud_late_companion_matches_without_stamp_or_count_change(self):
        # Native ROS interop tests exercise the real callback scheduling.
        # This exact stream verifies parent joins and missing-scan retention.
        for final_scan in (False, True):
            with self.subTest(final_scan=final_scan):
                rows = fixtures()[:2]
                for i in range(5):
                    stamp = STAMP + i * 100_000_000
                    rows.append(row('/odom', obs.encoded(odom(stamp)), len(rows) + 1))
                    if i != 4:
                        rows.append(row('/scan', obs.encoded({'capture_unix_ns': stamp, 'frame_id': 'lidar_link'}), len(rows) + 1))
                    rows.append(row('/utlidar/cloud', wire([ray(0)], nanosec=456 + i * 100_000_000), len(rows) + 1))
                fifth_index = len(rows) - 1
                if final_scan:
                    rows.append(row('/scan', obs.encoded({'capture_unix_ns': STAMP + 400_000_000, 'frame_id': 'lidar_link'}), len(rows) + 1))
                report = capture(rows).report(None, 0, 1)
                self.assertTrue(report['capture_complete'])
                self.assertEqual(report['cloud_count'], 5)
                self.assertEqual(report['joined_clouds'], 5 if final_scan else 4)
                self.assertEqual(report['all_clouds_joined'], final_scan)
                final = report['clouds'][-1]
                self.assertEqual(final['message_index'], fifth_index)
                self.assertEqual(final['capture_unix_ns'], STAMP + 400_000_000)
                self.assertEqual(final['missing_joins'], [] if final_scan else ['same_capture_scan'])
                if final_scan:
                    self.assertGreater(final['scan_message_index'], final['message_index'])
                self.assertEqual(report['messages'][fifth_index]['payload_hex'], rows[fifth_index]['payload_hex'])

    def test_nearest_original_odom_inclusive_bound_no_interpolation(self):
        base = fixtures()
        base[2] = row('/odom', obs.encoded(odom(STAMP - 50_000_000)), 3)
        self.assertEqual(capture(base).report(None, 0, 1)['clouds'][0]['odom_minus_cloud_ns'], -50_000_000)
        base[2] = row('/odom', obs.encoded(odom(STAMP - 50_000_001)), 3)
        result = capture(base).report(None, 0, 1)['clouds'][0]
        self.assertIsNone(result['odom_message_index'])
        base[2] = row('/odom', obs.encoded(odom(STAMP - 30_000_000)), 3)
        base.insert(3, row('/odom', obs.encoded(odom(STAMP + 10_000_000)), 3))
        result = capture(base).report(None, 0, 1)['clouds'][0]
        self.assertEqual(result['original_odometry'], odom(STAMP + 10_000_000))
        self.assertEqual(result['odom_message_index'], 3)

    def test_identity_epoch_and_capture_errors_retain_offending_bounded_sample(self):
        variants = [
            (row('/simulation/epoch', obs.encoded({'epoch': 32}), 6), 'epoch_mismatch_or_changed'),
            (row('/scan', obs.encoded({'capture_unix_ns': STAMP, 'frame_id': 'lidar_link'}), 6), 'capture_nonadvancing'),
            (row('/scan', obs.encoded({'capture_unix_ns': STAMP + 1, 'frame_id': 'lidar_link'}), 6, '34' * 24), 'publisher_changed'),
        ]
        for bad, error in variants:
            with self.subTest(error=error):
                c = capture(finish=False)
                with self.assertRaisesRegex(obs.CaptureError, error):
                    c.ingest(bad, 1000)
                report = c.report(error, 1, 2000)
                self.assertFalse(report['capture_complete'])
                self.assertEqual(report['messages'][-1]['payload_hex'], bad['payload_hex'])
                self.assertEqual(report['error'], error)

    def test_strict_envelopes_and_completion(self):
        for raw in (b'{"kind":1,"kind":2}', b'{"x":NaN}', b'[' * 2000, b'x' * (obs.MAX_ROW + 1)):
            with self.subTest(raw_length=len(raw)), self.assertRaises(obs.CaptureError):
                obs.strict(raw)
        for key, value in [('source_timestamp_ns', 0), ('source_timestamp_ns', True),
                           ('publisher_gid', '0' * 48), ('payload_hex', 'abCD'),
                           ('payload_hex', 'a'), ('topic', '/cmd_vel')]:
            bad = fixtures()[0].copy(); bad[key] = value
            with self.subTest(key=key, value=value), self.assertRaises(obs.CaptureError):
                capture([bad])
        c = capture(finish=False)
        bad = completion(fixtures()); bad['serialized_bytes'] += 1
        with self.assertRaisesRegex(obs.CaptureError, 'helper_completion_counts'):
            c.ingest(bad, 1000)
        c.ingest(completion(fixtures()), 1001)
        with self.assertRaises(obs.CaptureError):
            c.ingest(fixtures()[0], 1002)

    def test_total_bytes_message_cloud_and_topic_bounds(self):
        c = obs.Capture(SOURCE_DIGEST, 31, 5, companion=lambda *_: {'transforms': []})
        data = b'\0' * obs.TOPICS['/tf_static']
        for i in range(obs.MAX_RAW // len(data)):
            c.ingest(row('/tf_static', data, i), i)
        with self.assertRaisesRegex(obs.CaptureError, 'capture_byte_or_message_limit'):
            c.ingest(row('/tf_static', b'x', 100), 100)
        c = obs.Capture(SOURCE_DIGEST, 31, 5, companion=lambda *_: {'transforms': []})
        for i in range(obs.MAX_MESSAGES):
            c.ingest(row('/tf_static', b'\0', i), i)
        with self.assertRaisesRegex(obs.CaptureError, 'capture_byte_or_message_limit'):
            c.ingest(row('/tf_static', b'\0', obs.MAX_MESSAGES), obs.MAX_MESSAGES)
        c = obs.Capture(SOURCE_DIGEST, 31, 1, companion=companion)
        c.ingest(fixtures()[-1], 1)
        with self.assertRaisesRegex(obs.CaptureError, 'cloud_count_limit'):
            c.ingest(row('/utlidar/cloud', wire([], sec=124), 6), 2)
        for topic, cap in obs.TOPICS.items():
            with self.subTest(topic=topic), self.assertRaisesRegex(obs.CaptureError, 'helper_payload'):
                capture([row(topic, b'x' * (cap + 1))])

    def test_tf_flood_is_shared_once_and_report_preserves_full_raw(self):
        c = obs.Capture(SOURCE_DIGEST, 31, 5, companion=lambda *_: {'transforms': [transformed() for _ in range(32)]})
        for i in range(60):
            c.ingest(row('/tf_static', b'x' * 16384, i), i)
        for i in range(5):
            c.ingest(row('/utlidar/cloud', wire([ray(0)], sec=123 + i), 100 + i), 100 + i)
        report = c.report(None, 0, 200)
        self.assertEqual(report['shared_tf_evidence']['matching_frame_pair_count'], 60 * 32)
        self.assertEqual(len(report['shared_tf_evidence']['message_indices']), 60)
        self.assertNotIn('tf_evidence', report['clouds'][0])
        self.assertTrue(report['raw_payloads_retained'])
        self.assertEqual(sum(len(x['payload_hex']) // 2 for x in report['messages']), c.raw_bytes)
        self.assertLess(len(obs.encoded(report)), obs.MAX_REPORT)

    def test_report_overflow_retains_explicit_failed_metadata(self):
        c = capture()
        report = c.report(None, 0, 1)
        metadata_size = len(obs.encoded({**report, 'messages': [{k: v for k, v in r.items() if k != 'payload_hex'} for r in report['messages']]}))
        with mock.patch.object(obs, 'MAX_REPORT', metadata_size + 150):
            fallback = c.report(None, 0, 1)
        self.assertFalse(fallback['capture_complete'])
        self.assertFalse(fallback['all_clouds_joined'])
        self.assertFalse(fallback['raw_payloads_retained'])
        self.assertEqual(fallback['error'], 'report_byte_limit')
        self.assertNotIn('payload_hex', fallback['messages'][0])
        self.assertIn('payload_hex', c.messages[0])


class FilesTests(unittest.TestCase):
    def test_exclusive_private_report_no_symlink_or_fifo(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            report = capture().report(None, 0, 100)
            path = root / 'new.json'
            obs.write_report(path, report)
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
            self.assertEqual(json.loads(path.read_text()), report)
            with self.assertRaises(obs.CaptureError): obs.write_report(path, report)
            link = root / 'link'; link.symlink_to(path)
            with self.assertRaises(obs.CaptureError): obs.write_report(link, report)
            dangling = root / 'dangling'; dangling.symlink_to(root / 'missing')
            with self.assertRaises(obs.CaptureError): obs.write_report(dangling, report)
            alias = root / 'alias'; alias.symlink_to(root, target_is_directory=True)
            with self.assertRaises(obs.CaptureError): obs.write_report(alias / 'other.json', report)
            fifo = root / 'fifo'; os.mkfifo(fifo)
            with self.assertRaises(obs.CaptureError): obs.write_report(fifo, report)
            with self.assertRaises(obs.CaptureError): obs.write_report(root / 'missing' / 'out.json', report)

    def test_bad_output_rejected_before_starting_helper(self):
        with tempfile.TemporaryDirectory() as directory, mock.patch.object(obs, 'observe') as run:
            with self.assertRaises(obs.CaptureError):
                obs.main(['--source-digest', SOURCE_DIGEST, '--expected-epoch', '31', '--out', str(Path(directory) / 'missing' / 'out.json')])
            run.assert_not_called()


class ProcessTests(unittest.TestCase):
    def run_helper(self, script, duration=1000):
        real_popen = subprocess.Popen
        children, commands = [], []
        def fake(command, **options):
            commands.append(command)
            child = real_popen([sys.executable, '-c', script], **options)
            children.append(child)
            return child
        with mock.patch.object(obs.subprocess, 'Popen', side_effect=fake):
            report = obs.observe(SOURCE_DIGEST, 31, duration, 5)
        self.assertEqual(len(children), 1)
        self.assertEqual(commands, [[obs.HELPER, '--duration-ms', str(duration), '--max-clouds', '5']])
        self.assertIsNotNone(children[0].poll())
        return report

    def test_process_complete_cloud_only_retains_missing_joins(self):
        rows = [fixtures()[-1]]
        payload = b''.join(obs.encoded(x) for x in rows + [completion(rows)])
        report = self.run_helper('import sys; sys.stdout.buffer.write(' + repr(payload) + '); sys.stdout.flush()')
        self.assertTrue(report['capture_complete'])
        self.assertFalse(report['all_clouds_joined'])
        self.assertEqual(report['cloud_count'], 1)
        self.assertEqual(report['helper_exit_code'], 0)

    def test_eof_partial_or_nonzero_does_not_claim_completion(self):
        rows = [fixtures()[-1]]
        complete = b''.join(obs.encoded(x) for x in rows + [completion(rows)])
        cases = [('pass', 'helper_missing_completion'),
                 ('import sys; sys.stdout.write("{")', 'helper_partial_row'),
                 ('import sys; sys.stderr.write("private detail must not be retained"); sys.exit(7)', 'helper_missing_completion'),
                 ('import sys; sys.stdout.write("x" * 300000); sys.stdout.flush()', 'helper_row_bound'),
                 ('import sys; sys.stdout.buffer.write(' + repr(complete) + '); sys.stdout.flush(); sys.exit(7)', 'helper_nonzero_exit')]
        for script, error in cases:
            with self.subTest(error=error):
                report = self.run_helper(script)
                self.assertEqual(report['error'], error)
                self.assertFalse(report['capture_complete'])
                self.assertNotIn('private detail', obs.encoded(report).decode())

    def test_independent_deadline_terminates_kills_and_reaps_no_retry(self):
        started = time.monotonic()
        report = self.run_helper('import signal, time; signal.signal(signal.SIGTERM, signal.SIG_IGN); time.sleep(20)', 200)
        self.assertEqual(report['error'], 'parent_capture_deadline')
        self.assertEqual(report['helper_exit_code'], -signal.SIGKILL)
        self.assertFalse(report['capture_complete'])
        self.assertLess(time.monotonic() - started, 2)

    def test_no_helper_for_invalid_commission_or_duration(self):
        with mock.patch.object(obs.subprocess, 'Popen') as run:
            for source, epoch, duration, clouds in [(SOURCE_DIGEST, 0, 100, 1), ('sha256:' + '0'*64, 1, 100, 1), (SOURCE_DIGEST, 1, 15001, 1), (SOURCE_DIGEST, 1, 100, 6)]:
                with self.assertRaises(obs.CaptureError):
                    obs.observe(source, epoch, duration, clouds)
            run.assert_not_called()


if __name__ == '__main__':
    unittest.main()
