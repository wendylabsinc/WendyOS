"""Actual local persistent-process checks; no Docker, Agent or UI operations."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import textwrap
import time
import unittest
from unittest.mock import patch

from integration.livetwin.collector import ReaderCursor, READ_FILE_SCRIPT
from integration.livetwin.pipe import PipeReader, PipeError, SERVER_CODE, MAX_RESPONSE_BYTES, docker_reader
from integration.livetwin.test_collector import row, encode


class PipeTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(); self.addCleanup(self.tmp.cleanup)
        self.path = Path(self.tmp.name) / 'reader.jsonl'; self.path.write_bytes(b'')

    def append(self, raw):
        with self.path.open('ab') as file: file.write(raw)

    def local(self, **kwargs):
        reader = PipeReader(lambda code: [sys.executable, '-u', '-c', code, str(self.path), str(os.getuid())], **kwargs)
        self.addCleanup(reader.close)
        return reader

    def test_actual_persistent_append_partial_and_original_eof_cursor(self):
        self.append(encode(row(80)))
        reader = self.local(); pid = reader.process.pid
        cursor = ReaderCursor('live-test', reader.read); cursor.attach()
        self.assertEqual(cursor.offset, self.path.stat().st_size)
        raw = encode(row(81)); self.append(raw[:40])
        self.assertEqual(cursor.poll(), [])
        self.append(raw[40:]); result = cursor.poll()
        self.assertEqual(result[0]['reader'], row(81))
        self.assertEqual(cursor.offset, self.path.stat().st_size)
        self.assertEqual(reader.process.pid, pid)
        self.assertIsNone(reader.process.poll())
        reader.close(); self.assertEqual(reader.process.returncode, 0)

    def test_fixed_server_reuses_exact_existing_file_reader(self):
        self.assertIn(textwrap.indent(READ_FILE_SCRIPT, '    '), SERVER_CODE)
        reader = self.local(); first = reader.read(None, None)
        self.append(b'{"tick":1}\n')
        next_row = reader.read(first['newOffset'], first['inode'])
        self.assertEqual(next_row['completeBytes'], b'{"tick":1}\n')
        self.assertEqual(next_row['inode'], first['inode'])
        self.assertEqual(next_row['size'], self.path.stat().st_size)

    def test_eof_failure_is_terminal_without_reconnect(self):
        launches = []
        def factory(code):
            launches.append(code)
            return [sys.executable, '-c', 'pass']
        reader = PipeReader(factory); self.addCleanup(reader.close)
        with self.assertRaises(PipeError): reader.read(None, None)
        self.assertTrue(reader.closed)
        self.assertIsNotNone(reader.process.poll())
        with self.assertRaises(PipeError): reader.read(None, None)
        self.assertEqual(len(launches), 1)

    def test_actual_timeout_kills_and_reaps_only_owned_process(self):
        other = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(10)'])
        self.addCleanup(lambda: (other.terminate(), other.wait()))
        reader = PipeReader(lambda _: [sys.executable, '-u', '-c',
            'import sys,time; sys.stdin.buffer.readline(); time.sleep(10)'], timeout=.05)
        self.addCleanup(reader.close)
        started = time.monotonic()
        with self.assertRaises(PipeError): reader.read(None, None)
        self.assertLess(time.monotonic()-started, 2)
        self.assertIsNotNone(reader.process.poll())
        self.assertIsNone(other.poll())

    def test_oversize_or_extra_line_response_refused(self):
        for expression in ("b'x' * %d" % (MAX_RESPONSE_BYTES + 1), "b'{}\\n{}\\n'"):
            with self.subTest(expression=expression):
                reader = PipeReader(lambda _: [sys.executable, '-u', '-c',
                    'import sys; sys.stdin.buffer.readline(); sys.stdout.buffer.write(' + expression + '); sys.stdout.buffer.flush()'])
                self.addCleanup(reader.close)
                with self.assertRaises(PipeError): reader.read(None, None)
                self.assertTrue(reader.closed)

    def test_file_replacement_is_reported_to_cursor_without_replay(self):
        reader = self.local(); cursor = ReaderCursor('live-test', reader.read); cursor.attach()
        self.path.rename(self.path.with_suffix('.old'))
        self.path.write_bytes(encode(row()))
        from integration.livetwin.collector import CursorError
        with self.assertRaises(CursorError): cursor.poll()
        self.assertTrue(cursor.faulted)
        self.assertEqual(cursor.sequence, 0)

    def test_fixed_path_and_owner_cannot_be_replaced_by_request(self):
        reader = self.local()
        request = {'offset': None, 'inode': None, 'path': '/another/file'}
        os.write(reader.process.stdin.fileno(), json.dumps(request).encode()+b'\n')
        reader.process.wait(timeout=2)
        self.assertEqual(os.read(reader.process.stdout.fileno(), 4096), b'{"error":"reader_pipe_unavailable"}\n')

    def test_duration_and_stderr_nonoverwrite(self):
        stderr = Path(self.tmp.name) / 'reader.stderr'
        reader = self.local(stderrpath=stderr, duration=.01)
        reader.deadline = time.monotonic() - 1
        with self.assertRaises(PipeError): reader.read(None, None)
        self.assertTrue(reader.closed)
        self.assertEqual(stderr.read_bytes(), b'')
        with self.assertRaises(FileExistsError): self.local(stderrpath=stderr)

    def test_docker_factory_uses_full_identity_and_fixed_code_without_launch(self):
        with patch('integration.livetwin.pipe.PipeReader') as construct:
            docker_reader('a' * 64, '/owned/proofs/stdout.json', '/private/pipe.stderr')
            factory = construct.call_args.args[0]
            self.assertEqual(factory(SERVER_CODE), ['docker', 'exec', '-i', 'a' * 64,
                'python3', '-u', '-c', SERVER_CODE, '/owned/proofs/stdout.json'])
        for container, path in (('name', '/owned/stdout'), ('a' * 64, 'relative'), ('a' * 64, '/a/../b')):
            with self.subTest(container=container, path=path), self.assertRaises(ValueError):
                docker_reader(container, path, '/unused')


if __name__ == '__main__':
    unittest.main()
