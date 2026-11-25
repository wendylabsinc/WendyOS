"""Independent replay checks bind emitted candidates to original ROS bytes."""
from dataclasses import replace
import gzip
import importlib.util
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
spec = importlib.util.spec_from_file_location('stream_recorded', ROOT/'tests/go2_cloud_stream_recorded.py')
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)


from verification_archive import archive_file


class RecordedStreamTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.raw = [runner.read_capture(archive_file(f'go2-cloud/capture/{n}/passive-{n}.json.gz'))[1]
                   for n in ('a', 'b')]

    def test_original_arrival_sequences_keep_seven_exact_candidates(self):
        expected = [(25, [7, 8, 14, 21], [5, 10, 13, 20]),
                    (26, [8, 15, 22], [7, 14, 21])]
        for raw, (epoch, clouds, odoms) in zip(self.raw, expected):
            result = runner.replay(raw)
            self.assertTrue(result['success'])
            self.assertEqual(result['expected_epoch'], epoch)
            self.assertEqual([c['samples']['cloud']['message_index'] for c in result['candidates']], clouds)
            self.assertEqual([c['samples']['odom']['message_index'] for c in result['candidates']], odoms)
            self.assertEqual(result['final_status']['state'], 'closed')
            self.assertEqual(result['final_status']['pending'], 0)
            self.assertFalse(result['operational_authority'])
            self.assertTrue(result['logical_time_only'])

    def mutated(self, transform):
        original = runner.Assembler

        class ChangedAssembler(original):
            def ingest(self, *args):
                return tuple(transform(e) if e.kind == 'ready' else e for e in super().ingest(*args))

        return patch.object(runner, 'Assembler', ChangedAssembler)

    def test_changed_original_cloud_stamp_is_detected(self):
        def change(event):
            c = event.candidate
            value = replace(c.cloud.value, capture_unix_ns=c.cloud.value.capture_unix_ns+1)
            return replace(event, candidate=replace(c, cloud=replace(c.cloud, value=value)))
        with self.mutated(change), self.assertRaisesRegex(ValueError, 'sample_value_changed'):
            runner.replay(self.raw[0])

    def test_changed_original_cdr_or_metadata_is_detected(self):
        for update in ({'raw_cdr': b'forged'}, {'source_timestamp_ns': 1}, {'arrived_ns': 0}):
            def change(event):
                c = event.candidate
                return replace(event, candidate=replace(c, odom=replace(c.odom, **update)))
            with self.subTest(update=list(update)), self.mutated(change), self.assertRaises(ValueError):
                runner.replay(self.raw[0])

    def test_changed_epoch_or_deadline_is_detected(self):
        for update in ({'observed_epoch': 999}, {'assembled_at_ns': 0},
                       {'companion_deadline_ns': 0}, {'odom_minus_cloud_ns': 0}):
            with self.subTest(update=update), self.mutated(
                lambda e: replace(e, candidate=replace(e.candidate, **update))
            ), self.assertRaises(ValueError):
                runner.replay(self.raw[0])

    def test_no_candidates_cannot_pass(self):
        original = runner.Assembler
        class SilentAssembler(original):
            def ingest(self, *args):
                return tuple(e for e in super().ingest(*args) if e.kind != 'ready')
        with patch.object(runner, 'Assembler', SilentAssembler):
            result = runner.replay(self.raw[0])
        self.assertFalse(result['success'])
        self.assertEqual(result['candidates'], [])

    def test_deadline_extension_and_missing_mounts_are_detected(self):
        for change in (lambda c: replace(c, companion_deadline_ns=c.companion_deadline_ns+1),
                       lambda c: replace(c, transforms=()),
                       lambda c: replace(c, transforms=(c.odom, c.odom))):
            with self.mutated(lambda e: replace(e, candidate=change(e.candidate))), self.assertRaises(ValueError):
                runner.replay(self.raw[0])

    def test_suppressed_unavailable_event_cannot_pass(self):
        original = runner.Assembler
        class SilentUnavailable(original):
            def poll(self, *args):
                return tuple(e for e in super().poll(*args) if e.kind != 'unavailable')
        with patch.object(runner, 'Assembler', SilentUnavailable):
            result = runner.replay(self.raw[0])
        self.assertFalse(result['success'])
        self.assertFalse(result['all_clouds_have_one_disposition'])

    def test_input_is_bounded_after_decompression_and_refuses_symlinks(self):
        with tempfile.TemporaryDirectory() as name:
            root = Path(name)
            archive = root/'capture.gz'
            archive.write_bytes(gzip.compress(b'x'*(runner.MAX_INPUT+1)))
            with self.assertRaisesRegex(ValueError, 'decoded_capture_bound'):
                runner.read_capture(archive)
            link = root/'link.gz'
            link.symlink_to(archive)
            with self.assertRaises(OSError):
                runner.read_capture(link)
            with self.assertRaises(ValueError):
                runner.read_capture(root)


if __name__ == '__main__':
    unittest.main()
