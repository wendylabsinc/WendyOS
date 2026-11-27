"""One persistent, owned subprocess for bounded ordinary-reader file polling.

The server embeds the existing READ_FILE_SCRIPT unchanged. Its path and owner
are fixed at process startup; requests contain only a cursor offset and inode.
There is no reconnect, offset reset, timestamp replacement or background thread.
ReaderCursor still timestamps before each authoritative file snapshot.
"""
from __future__ import annotations

import json
import os
from pathlib import PurePosixPath
import re
import selectors
import subprocess
import textwrap
import time

from integration.livetwin.collector import READ_FILE_SCRIPT, MAX_FILE_BYTES, MAX_CHUNK_BYTES, decode_read_response

MAX_REQUEST_BYTES = 2048
MAX_RESPONSE_BYTES = 4 * ((MAX_CHUNK_BYTES + 2) // 3) + 4096

# The unchanged reader source is a function body in this fixed startup program.
# Requests cannot supply code, a path or a uid; no dynamic evaluator is used.
SERVER_CODE = 'def read_file():\n' + textwrap.indent(READ_FILE_SCRIPT, '    ') + r'''
import json, sys
fixed_args = list(sys.argv)
if len(fixed_args) not in (2, 3): sys.exit(2)
def unique(pairs):
    out = {}
    for key, value in pairs:
        if key in out: raise ValueError()
        out[key] = value
    return out
while True:
    raw = sys.stdin.buffer.readline(2049)
    if not raw: break
    try:
        if len(raw) > 2048 or not raw.endswith(b'\n'): raise ValueError()
        request = json.loads(raw, object_pairs_hook=unique)
        if not isinstance(request, dict) or set(request) != {'offset', 'inode'}: raise ValueError()
        offset, inode = request['offset'], request['inode']
        if offset is None and inode is None:
            cursor = ['-', '-']
        else:
            if type(offset) is not int or not 0 <= offset <= 33554432: raise ValueError()
            if not isinstance(inode, str) or not 0 < len(inode) <= 256: raise ValueError()
            cursor = [str(offset), inode]
        sys.argv = [fixed_args[0], fixed_args[1], *cursor, *fixed_args[2:]]
        read_file()
        sys.stdout.buffer.flush()
    except (ValueError, TypeError, OverflowError, UnicodeError):
        sys.stdout.buffer.write(b'{"error":"reader_pipe_unavailable"}\n')
        sys.stdout.buffer.flush()
        break
'''


class PipeError(OSError):
    """The owned reader pipe has ended or cannot deliver a bounded response."""


class PipeReader:
    """argv_factory receives only SERVER_CODE and returns explicit Popen argv.

    Local tests may pass python3 -u -c code path expected_uid. Production uses
    docker_reader, which fixes a full container ID and root-owned stdout path.
    The caller uses this instance from one thread and closes it before cleanup.
    """
    def __init__(self, argv_factory, *, stderrpath=None, timeout=0.75, duration=120):
        if not callable(argv_factory) or type(timeout) not in (int, float) or not .01 <= timeout <= 5:
            raise ValueError('bounded pipe timeout and argv factory required')
        if type(duration) not in (int, float) or not .01 <= duration <= 120:
            raise ValueError('bounded pipe duration required')
        argv = argv_factory(SERVER_CODE)
        if not isinstance(argv, (list, tuple)) or not 1 <= len(argv) <= 32 or not all(
                isinstance(arg, str) and arg and '\x00' not in arg and len(arg) <= 32768 for arg in argv):
            raise ValueError('explicit subprocess argv required')
        self.timeout, self.deadline = float(timeout), time.monotonic() + duration
        self.closed = self.busy = False
        self.process = None
        self.stderr = open(stderrpath, 'xb') if stderrpath is not None else None
        try:
            self.process = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                stderr=self.stderr if self.stderr is not None else subprocess.DEVNULL, bufsize=0)
            os.set_blocking(self.process.stdin.fileno(), False)
            os.set_blocking(self.process.stdout.fileno(), False)
        except Exception:
            self.close()
            raise

    def read(self, offset, inode):
        if self.closed or self.busy:
            raise PipeError('owned reader pipe is closed or busy')
        if not (offset is None and inode is None) and not (
                type(offset) is int and 0 <= offset <= MAX_FILE_BYTES and
                isinstance(inode, str) and 0 < len(inode) <= 256):
            raise ValueError('exact bounded cursor required')
        request = json.dumps({'offset': offset, 'inode': inode}, separators=(',', ':')).encode() + b'\n'
        if len(request) > MAX_REQUEST_BYTES:
            raise ValueError('cursor request exceeds bound')
        self.busy = True
        try:
            deadline = min(self.deadline, time.monotonic() + self.timeout)
            if time.monotonic() >= deadline:
                raise PipeError('owned reader pipe duration exceeded')
            response = bytearray()
            sent = 0
            with selectors.DefaultSelector() as selector:
                selector.register(self.process.stdin, selectors.EVENT_WRITE)
                selector.register(self.process.stdout, selectors.EVENT_READ)
                while True:
                    remaining = deadline - time.monotonic()
                    if remaining <= 0:
                        raise PipeError('owned reader pipe timed out')
                    events = selector.select(remaining)
                    if not events:
                        raise PipeError('owned reader pipe timed out')
                    for key, _ in events:
                        if key.fileobj is self.process.stdin:
                            try:
                                count = os.write(key.fd, request[sent:])
                            except BlockingIOError:
                                continue
                            if count <= 0:
                                raise PipeError('owned reader request ended')
                            sent += count
                            if sent == len(request):
                                selector.unregister(self.process.stdin)
                        else:
                            try:
                                chunk = os.read(key.fd, min(65536, MAX_RESPONSE_BYTES + 1 - len(response)))
                            except BlockingIOError:
                                continue
                            if not chunk:
                                raise PipeError('owned reader pipe ended')
                            response.extend(chunk)
                            if len(response) > MAX_RESPONSE_BYTES:
                                raise PipeError('owned reader response exceeds bound')
                            if b'\n' in response:
                                if sent != len(request) or response.count(b'\n') != 1 or not response.endswith(b'\n'):
                                    raise PipeError('unexpected reader response framing')
                                return decode_read_response(bytes(response))
        except Exception as exc:
            self.close()
            raise PipeError('owned reader pipe unavailable') from exc
        finally:
            self.busy = False

    def close(self):
        """Close stdin, then terminate/reap only the Popen this instance owns."""
        if self.closed:
            return
        self.closed = True
        process = self.process
        if process is not None:
            if process.stdin is not None:
                process.stdin.close()
            try:
                process.wait(timeout=.25)
            except subprocess.TimeoutExpired:
                process.terminate()
                try:
                    process.wait(timeout=.25)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=.5)
            finally:
                if process.stdout is not None:
                    process.stdout.close()
        if self.stderr is not None:
            self.stderr.close()


def docker_reader(container_id, path, stderrpath, *, timeout=0.75, duration=120):
    """Launch one reader on the exact already-owned container; no inspection."""
    if not isinstance(container_id, str) or not re.fullmatch(r'[0-9a-f]{64}', container_id):
        raise ValueError('full owned container ID required')
    if not isinstance(path, str) or not path.startswith('/') or str(PurePosixPath(path)) != path or '..' in PurePosixPath(path).parts:
        raise ValueError('exact absolute stdout path required')
    return PipeReader(lambda code: ['docker', 'exec', '-i', container_id, 'python3', '-u', '-c', code, path],
                      stderrpath=stderrpath, timeout=timeout, duration=duration)
