"""Focused append-stream tests; no Agent, network, VM or native window."""
from copy import deepcopy
import json
import os
from pathlib import Path
import tempfile
import subprocess
import sys
import unittest

from integration.livetwin.collector import (ReaderCursor, CursorError, MAX_CHUNK_BYTES,
    MAX_LINE_BYTES, MAX_FILE_BYTES, MAX_ROWS, MAX_DURATION_NS, READ_FILE_SCRIPT, decode_read_response)


class Clock:
    def __init__(self): self.now = 10_000_000_000
    def __call__(self): return self.now


def row(tick=1, status='ready', attempt=0):
    value = {'version': 1, 'run_id': 'live-test', 'tick': tick, 'reader_generation': 7,
             'status': status, 'hazards': [], 'sample_time': '2026-01-01T00:00:01Z',
             'origin_attested_inference': status == 'ready', 'raw_inputs_independently_verified': False}
    if attempt: value['startup_attempt'] = attempt
    if status == 'ready':
        value['clock'] = {'sample_time': value['sample_time'], 'sample_clock_domain': 'simulation:live-test',
                          'physical_earliest': '2026-11-12T12:00:00Z', 'physical_latest': '2026-11-12T12:00:00.010Z',
                          'server_boot_id': 'reader-boot', 'deployment_generation': 7}
    return value


def encode(value): return json.dumps(value, separators=(',', ':')).encode() + b'\n'


class AppendFile:
    """Example same-file snapshot callback using an actual private local file."""
    def __init__(self, path):
        self.path, self.calls, self.limit = path, [], MAX_CHUNK_BYTES
        path.write_bytes(b'')
        self.after_read = lambda: None

    def append(self, raw):
        with self.path.open('ab') as file: file.write(raw)

    def __call__(self, offset, inode):
        self.calls.append((offset, inode))
        with self.path.open('rb') as file:
            import os
            stat = os.fstat(file.fileno())
            size, identity = stat.st_size, str(stat.st_dev) + ':' + str(stat.st_ino)
            if offset is None:
                file.seek(max(0, size - 1))
                partial = size > 0 and file.read(1) != b'\n'
                result = dict(completeBytes=b'', newOffset=size, inode=identity, size=size, partial=partial, eof=True)
            elif identity != inode or offset > size:
                result = dict(completeBytes=b'', newOffset=size, inode=identity, size=size, partial=False, eof=True)
            else:
                file.seek(offset)
                raw = file.read(size - offset)
                last = raw.rfind(b'\n', 0, self.limit)
                complete = raw[:last + 1]
                tail = raw[len(complete):]
                eof = b'\n' not in tail
                result = dict(completeBytes=complete, newOffset=offset + len(complete), inode=identity,
                              size=size, partial=eof and bool(tail), eof=eof)
        self.after_read()
        return result


class CollectorTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.file = AppendFile(Path(self.tmp.name) / 'reader.jsonl')
        self.clock = Clock()
        self.cursor = ReaderCursor('live-test', self.file, monotonic_ns=self.clock)

    def test_attach_skips_history_and_keeps_pre_snapshot_time(self):
        self.file.append(encode(row(80)))
        before = self.clock.now
        self.file.after_read = lambda: setattr(self.clock, 'now', self.clock.now + 100_000_000)
        self.cursor.attach()
        self.assertEqual(self.file.calls[0], (None, None))
        self.file.append(encode(row(81)))
        result = self.cursor.poll()
        self.assertEqual(len(result), 1)
        self.assertEqual(result[0]['reader'], row(81))
        self.assertEqual(result[0]['collection_started_monotonic_ns'], before)
        self.assertNotEqual(before, self.clock.now)

    def test_attach_inside_historical_partial_discards_its_completion(self):
        raw = encode(row(80)); self.file.append(raw[:100])
        self.cursor.attach()
        self.assertEqual(self.cursor.poll(), [])
        self.file.append(raw[100:] + encode(row(81)))
        result = self.cursor.poll()
        self.assertEqual([r['reader']['tick'] for r in result], [81])

    def test_new_partial_preserves_timestamp_across_multiple_polls(self):
        self.cursor.attach(); started = self.clock.now
        raw = encode(row()); self.file.append(raw[:30])
        self.clock.now += 400_000_000
        self.assertEqual(self.cursor.poll(), [])
        self.file.append(raw[30:100]); self.clock.now += 400_000_000
        self.assertEqual(self.cursor.poll(), [])
        self.file.append(raw[100:]); self.clock.now += 400_000_000
        result = self.cursor.poll()
        self.assertEqual(result[0]['collection_started_monotonic_ns'], started)
        self.assertGreater(self.clock.now - started, 1_000_000_000)

    def test_next_rows_use_prior_eof_start_not_current_poll(self):
        self.cursor.attach(); self.clock.now += 100
        previous = self.clock.now
        self.assertEqual(self.cursor.poll(), [])
        self.file.append(encode(row()))
        self.clock.now += 200
        result = self.cursor.poll()
        self.assertEqual(result[0]['collection_started_monotonic_ns'], previous)
        self.assertNotEqual(previous, self.clock.now)

    def test_bounded_drain_keeps_same_age_until_authoritative_eof(self):
        self.cursor.attach(); started = self.clock.now
        raw = encode(row()); self.file.limit = len(raw)
        self.file.append(raw + encode(row(2)))
        self.clock.now += 100
        first = self.cursor.poll(); self.clock.now += 100
        second = self.cursor.poll()
        self.assertEqual([r[0]['reader']['tick'] for r in (first, second)], [1, 2])
        self.assertEqual([r[0]['collection_started_monotonic_ns'] for r in (first, second)], [started, started])

    def test_duplicate_tick_cannot_emit_or_refresh(self):
        self.cursor.attach(); self.file.append(encode(row(2)))
        first = self.cursor.poll()[0]
        self.clock.now += 100
        self.file.append(encode(row(2)) + encode(row(1)))
        self.assertEqual(self.cursor.poll(), [])
        self.assertEqual(self.cursor.sequence, first['sequence'])

    def test_waiting_attempts_then_ready_same_tick_keep_original_clock(self):
        self.cursor.attach()
        events = [row(status='waiting_for_source', attempt=1), row(status='waiting_for_source', attempt=1),
                  row(status='waiting_for_source', attempt=2), row()]
        self.file.append(b''.join(encode(value) for value in events))
        result = self.cursor.poll()
        self.assertEqual([r['reader'] for r in result], [events[0], events[2], events[3]])
        self.assertEqual([r['sequence'] for r in result], [1, 2, 3])
        self.assertIsNotNone(result[-1]['reader']['clock'])

    def test_terminal_result_and_summary_are_not_ready_reads(self):
        self.cursor.attach()
        summary = {'ticks': 0, 'hazards_read': 0, 'startup_waits': 1, 'status': 'failed'}
        self.file.append(encode(row(status='failed')) + encode({'summary': summary}))
        result = self.cursor.poll()
        self.assertEqual(result[0]['reader']['status'], 'failed')
        self.assertEqual(self.cursor.summary, summary)
        calls = len(self.file.calls)
        self.assertEqual(self.cursor.poll(), [])
        self.assertEqual(len(self.file.calls), calls)

    def test_people_rows_and_summary_preserve_original_observations(self):
        def people_row(*args, **kwargs):
            value = row(*args, **kwargs)
            value['tracks'] = value.pop('hazards')
            value['camera_inputs_independently_verified'] = value.pop('raw_inputs_independently_verified')
            return value

        cursor = ReaderCursor('live-test', self.file, reader_kind='people', monotonic_ns=self.clock)
        self.file.append(encode(people_row(80)))
        cursor.attach(); started = self.clock.now
        waiting = people_row(status='waiting_for_source', attempt=1)
        waiting['clock'] = deepcopy(row()['clock'])
        waiting['rpc_code'] = 'PermissionDenied'
        present = people_row()
        # The cursor preserves opaque track bytes; the typed bridge validates them.
        present['tracks'] = [{'envelope_json': 'e30=', 'envelope_sha256': 'a' * 64,
                              'record_sha256': 'b' * 64, 'version': {'epoch': 1, 'sequence': 1},
                              'payload': {'track_id': 'track-1'}}]
        empty = people_row(2); empty['absent_track_ids'] = ['track-1']
        failed = people_row(3, status='failed')
        events = [waiting, present, empty, failed]
        summary = {'ticks': 2, 'tracks_read': 1, 'startup_waits': 1, 'status': 'failed'}
        self.file.append(b''.join(encode(value) for value in events + [{'summary': summary}]))
        self.clock.now += 100
        result = cursor.poll()
        self.assertEqual([event['reader'] for event in result], events)
        self.assertEqual([event['collection_started_monotonic_ns'] for event in result], [started] * 4)
        self.assertEqual([event['sequence'] for event in result], [1, 2, 3, 4])
        self.assertEqual(cursor.summary, summary)
        self.assertTrue(cursor.closed)
        calls = len(self.file.calls)
        self.assertEqual(cursor.poll(), [])
        self.assertEqual(len(self.file.calls), calls)

    def test_summary_count_requires_explicit_matching_reader_kind(self):
        for kind, count in (('hazard', 'hazards_read'), ('people', 'tracks_read')):
            for wrong in (False, True, 'both'):
                with self.subTest(kind=kind, wrong=wrong):
                    file = AppendFile(Path(self.tmp.name) / (kind + str(wrong)))
                    cursor = ReaderCursor('live-test', file, reader_kind=kind, monotonic_ns=self.clock)
                    cursor.attach()
                    other = 'tracks_read' if count == 'hazards_read' else 'hazards_read'
                    summary = {'ticks': 0, 'startup_waits': 0, 'status': 'complete'}
                    summary[other if wrong is True else count] = 0
                    if wrong == 'both': summary[other] = 0
                    file.append(encode({'summary': summary}))
                    if wrong:
                        with self.assertRaises(CursorError): cursor.poll()
                        self.assertTrue(cursor.faulted)
                        self.assertIsNone(cursor.summary)
                    else:
                        self.assertEqual(cursor.poll(), [])
                        self.assertEqual(cursor.summary, summary)
                        self.assertTrue(cursor.closed)

    def test_unknown_reader_kind_refuses_before_attachment(self):
        for kind in ('', 'tracks', 'person', None):
            with self.subTest(kind=kind), self.assertRaises(CursorError):
                ReaderCursor('live-test', self.file, reader_kind=kind, monotonic_ns=self.clock)
        self.assertEqual(self.file.calls, [])

    def test_replacement_truncation_and_clock_regression_fault_without_reattach(self):
        for mode in ('replace', 'truncate', 'clock'):
            with self.subTest(mode=mode):
                file = AppendFile(Path(self.tmp.name) / mode)
                clock = Clock(); cursor = ReaderCursor('live-test', file, monotonic_ns=clock)
                file.append(encode(row())); cursor.attach()
                if mode == 'replace':
                    file.path.rename(file.path.with_suffix('.old')); file.path.write_bytes(b'')
                elif mode == 'truncate': file.path.write_bytes(b'')
                else: clock.now -= 1
                with self.assertRaises(CursorError): cursor.poll()
                calls = len(file.calls)
                with self.assertRaises(CursorError): cursor.poll()
                self.assertEqual(len(file.calls), calls)

    def test_malformed_clock_scope_or_json_refuses(self):
        bad = [b'{"version":1,"version":1}\n', b'{"x":NaN}\n', b'[]\n', b'\n']
        for change in ('run_id', 'clock', 'generation'):
            value = row()
            if change == 'run_id': value['run_id'] = 'other'
            elif change == 'clock': value.pop('clock')
            else: value['clock']['deployment_generation'] = 8
            bad.append(encode(value))
        for index, raw in enumerate(bad):
            with self.subTest(index=index):
                file = AppendFile(Path(self.tmp.name) / str(index))
                cursor = ReaderCursor('live-test', file, monotonic_ns=self.clock)
                cursor.attach(); file.append(raw)
                with self.assertRaises(CursorError): cursor.poll()

    def test_file_line_row_and_duration_budgets(self):
        for mode in ('file', 'line', 'rows', 'duration'):
            with self.subTest(mode=mode):
                file = AppendFile(Path(self.tmp.name) / mode)
                clock = Clock(); cursor = ReaderCursor('live-test', file, monotonic_ns=clock)
                cursor.attach()
                if mode == 'file':
                    original = cursor.read
                    def huge(offset, inode):
                        value = original(offset, inode); value['size'] = MAX_FILE_BYTES + 1; return value
                    cursor.read = huge
                elif mode == 'line': file.append(b'x' * MAX_LINE_BYTES)
                elif mode == 'rows': file.append(encode(row()) * (MAX_ROWS + 1))
                else: clock.now += MAX_DURATION_NS
                with self.assertRaises(CursorError): cursor.poll()

    def test_callback_failure_latches_and_receipt_clock_is_not_restamped(self):
        self.cursor.attach(); value = row(); original = deepcopy(value)
        self.file.append(encode(value)); result = self.cursor.poll()[0]
        self.assertEqual(result['reader'], original)
        self.assertNotIn('clock', result)
        def unavailable(*_): raise OSError('private transport details')
        self.cursor.read = unavailable
        with self.assertRaisesRegex(CursorError, '^live reader collection unavailable$'): self.cursor.poll()
        self.assertTrue(self.cursor.faulted)

    def script_read(self, offset, inode, *, path=None, uid=None):
        command = [sys.executable, '-c', READ_FILE_SCRIPT, str(path or self.file.path),
                   str(offset) if offset is not None else '-', inode if inode is not None else '-',
                   str(os.getuid() if uid is None else uid)]
        result = subprocess.run(command, capture_output=True, timeout=5, check=True)
        self.assertEqual(result.stderr, b'')
        return decode_read_response(result.stdout)

    def test_actual_standalone_script_append_partial_and_history(self):
        old = encode(row(80)); self.file.append(old[:30])
        cursor = ReaderCursor('live-test', self.script_read, monotonic_ns=self.clock)
        cursor.attach(); started = self.clock.now
        self.file.append(old[30:] + encode(row(81))[:60]); self.clock.now += 100
        self.assertEqual(cursor.poll(), [])
        self.file.append(encode(row(81))[60:]); self.clock.now += 100
        result = cursor.poll()
        self.assertEqual(result[0]['reader'], row(81))
        self.assertEqual(result[0]['collection_started_monotonic_ns'], started)
        self.assertEqual(cursor.offset, self.file.path.stat().st_size)

    def test_actual_standalone_script_ownership_symlink_and_single_link(self):
        self.script_read(None, None)
        with self.assertRaises(subprocess.CalledProcessError):
            self.script_read(None, None, uid=os.getuid() + 1)
        link = self.file.path.with_suffix('.symlink'); link.symlink_to(self.file.path)
        with self.assertRaises(subprocess.CalledProcessError): self.script_read(None, None, path=link)
        hardlink = self.file.path.with_suffix('.hardlink'); os.link(self.file.path, hardlink)
        with self.assertRaises(subprocess.CalledProcessError): self.script_read(None, None)

    def test_actual_standalone_script_bounded_chunk_and_large_line(self):
        cursor = ReaderCursor('live-test', self.script_read, monotonic_ns=self.clock); cursor.attach()
        value = row(); value['padding'] = 'x' * 300_000
        self.file.append(encode(value) * 4)
        self.assertEqual(len(cursor.poll()), 1)  # Three rows fit, duplicate ticks do not re-emit.
        self.assertLess(cursor.offset, self.file.path.stat().st_size)
        self.assertEqual(cursor.poll(), [])
        self.assertEqual(cursor.offset, self.file.path.stat().st_size)
        self.file.append(b'x' * MAX_LINE_BYTES)
        with self.assertRaises(CursorError): cursor.poll()

    def test_late_partial_completion_stays_stale_in_live_bridge(self):
        from sim.native_replica_bridge import NativeReplicaBridge
        from sim.test_native_replica_bridge import live_fixture
        config, presentation, update = live_fixture()
        reader = deepcopy(update['reader']); reader['clock'] = deepcopy(update['clock'])
        cursor = ReaderCursor(config['run_id'], self.script_read, monotonic_ns=self.clock)
        bridge = NativeReplicaBridge(config, presentation, monotonic_ns=self.clock)
        cursor.attach()
        raw = encode(reader); self.file.append(raw[:100])
        self.clock.now += 600_000_000
        self.assertEqual(cursor.poll(), [])
        self.file.append(raw[100:]); self.clock.now += 600_000_000
        collected = cursor.poll()[0]
        with self.assertRaises(ValueError): bridge.ingest({**collected, 'presentation': presentation})
        self.assertEqual(bridge.export()['snapshot']['fusion']['state'], 'unknown')
        self.assertFalse(bridge.export()['snapshot']['controls_available'])


if __name__ == '__main__':
    unittest.main()
