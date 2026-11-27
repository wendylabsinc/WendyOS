"""Bounded cursor for one ordinary hazard or people reader's live NDJSON output.

No filesystem, Docker, network or clock-authority operations occur here. The
launcher injects read(offset, inode), taking one same-file size/identity snapshot:

* read(None, None) returns completeBytes=b'', newOffset=size (exact EOF),
  inode (nonempty opaque string), size, partial (EOF is inside a line), eof=True.
* Subsequent reads return the newline-terminated prefix from offset, at most
  MAX_CHUNK_BYTES. newOffset is offset + len(completeBytes). Unfinished bytes
  stay in the file, not in the cursor. eof means no unread complete lines;
  partial means the remaining suffix is one incomplete line. With eof=False,
  partial=False and the next poll drains the remaining complete lines.
* The callback must report replacement/truncation, never restart at offset zero.
  Its size includes unfinished bytes and must be captured before reading. Only
  bytes through that size belong in the response. It must not read beyond the
  bounds below. Initial attachment does not read or emit historical contents.

poll() returns bridge update bases. The launcher adds the matching original
presentation and optional media, preserving collection_started_monotonic_ns.
The same host monotonic clock must be used by this cursor and the live bridge.
Each row is charged from BEFORE the preceding authoritative EOF observation,
not from the later poll that discovers it. Partial lines and drain batches keep
that earlier time. Source JSON and the embedded reader.clock are unmodified.

A terminal reader Result is returned for the launcher to clear its display;
the CLI's final {summary:...} is retained as summary, never as a current read.
The explicit reader_kind selects its summary count field; hazard is the default.
Any cursor failure is terminal for this instance. Reattach a new cursor and
clear the bridge after replacement; never treat historical bytes as current.
"""
from __future__ import annotations

import base64
import json
import re
import time

MAX_FILE_BYTES = 32 << 20
MAX_CHUNK_BYTES = 1 << 20
MAX_LINE_BYTES = 512 << 10
MAX_ROWS = 512
MAX_DURATION_NS = 120_000_000_000
CLOCK_FIELDS = {'sample_time', 'sample_clock_domain', 'physical_earliest',
                'physical_latest', 'server_boot_id', 'deployment_generation'}

# Invoke with Node.python(READ_FILE_SCRIPT, absolute_path, offset_or_dash,
# inode_or_dash). The optional final uid is for explicit local test ownership;
# runtime callers omit it so the file must be root-owned. No shell is involved.
READ_FILE_SCRIPT = r'''
import base64, json, os, stat, sys
try:
    if len(sys.argv) not in (4, 5): raise ValueError()
    path, offset_arg, expected_inode = sys.argv[1:4]
    uid = int(sys.argv[4]) if len(sys.argv) == 5 else 0
    if uid < 0 or not os.path.isabs(path): raise ValueError()
    attaching = offset_arg == '-' and expected_inode == '-'
    if (offset_arg == '-') != (expected_inode == '-'): raise ValueError()
    offset = None if attaching else int(offset_arg)
    if offset is not None and not 0 <= offset <= 33554432: raise ValueError()
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        s = os.fstat(fd)
        if not stat.S_ISREG(s.st_mode) or s.st_uid != uid or s.st_nlink != 1 or s.st_size > 33554432:
            raise ValueError()
        size, inode = s.st_size, str(s.st_dev) + ':' + str(s.st_ino)
        if attaching:
            data, end, eof = b'', size, True
            partial = size > 0 and os.pread(fd, 1, size - 1) != b'\n'
        elif inode != expected_inode or size < offset:
            data, end, eof, partial = b'', size, True, False
        else:
            # At most one output chunk plus one bounded trailing line. Appends
            # beyond the initial fstat size belong to the next collection.
            count = min(size - offset, 1048576 + 524288)
            raw = os.pread(fd, count, offset)
            if len(raw) != count: raise ValueError()
            last = raw.rfind(b'\n', 0, 1048576)
            data = raw[:last + 1]
            if any(len(line) > 524288 for line in data.split(b'\n')[:-1]): raise ValueError()
            end = offset + len(data)
            tail = raw[len(data):]
            eof = b'\n' not in tail
            partial = eof and end < size
            if eof and size - end >= 524288: raise ValueError()
            if not eof and not data: raise ValueError()
        if os.fstat(fd).st_size < size: raise ValueError()
        print(json.dumps(dict(completeBytes=base64.b64encode(data).decode('ascii'),
            newOffset=end, inode=inode, size=size, partial=partial, eof=eof), separators=(',', ':')))
    finally:
        os.close(fd)
except (OSError, ValueError, OverflowError):
    print('{"error":"reader_file_unavailable"}')
    sys.exit(1)
'''


def decode_read_response(value):
    """Decode the standalone callback JSON; ReaderCursor validates its fields."""
    if isinstance(value, bytes):
        _require(len(value) <= (MAX_CHUNK_BYTES * 4 // 3) + 4096, 'callback response exceeds bound')
        value = json.loads(value, object_pairs_hook=_object)
    _require(isinstance(value, dict) and isinstance(value.get('completeBytes'), str), 'invalid callback response')
    value = dict(value)
    _require(len(value['completeBytes']) <= 4 * ((MAX_CHUNK_BYTES + 2) // 3), 'encoded chunk exceeds bound')
    value['completeBytes'] = base64.b64decode(value['completeBytes'], validate=True)
    return value


class CursorError(ValueError):
    """The current append-only reader stream cannot be consumed as live data."""


def _require(value, message):
    if not value:
        raise CursorError(message)


def _integer(value, low=0, high=(1 << 63) - 1):
    _require(type(value) is int and low <= value <= high, 'invalid bounded integer')
    return value


def _object(pairs):
    result = {}
    for key, value in pairs:
        _require(key not in result, 'duplicate JSON field')
        result[key] = value
    return result


def _decode(raw):
    _require(0 < len(raw) <= MAX_LINE_BYTES, 'reader line exceeds bound')
    result = json.loads(raw, object_pairs_hook=_object,
                        parse_constant=lambda _: (_ for _ in ()).throw(CursorError('nonfinite JSON')))
    _require(isinstance(result, dict), 'reader row must be an object')
    return result


class ReaderCursor:
    def __init__(self, run_id, read, *, reader_kind='hazard', monotonic_ns=time.monotonic_ns):
        _require(isinstance(run_id, str) and re.fullmatch(r'[A-Za-z0-9_-]{1,80}', run_id), 'one run required')
        _require(callable(read) and callable(monotonic_ns), 'reader and monotonic clock required')
        _require(reader_kind in ('hazard', 'people'), 'known reader kind required')
        self.summary_count_key = 'tracks_read' if reader_kind == 'people' else 'hazards_read'
        self.run_id, self.read, self.monotonic_ns = run_id, read, monotonic_ns
        self.offset = self.inode = self.started_ns = self.known_since_ns = None
        self.last_now = None
        self.size = self.rows = self.sequence = self.last_tick = self.last_attempt = 0
        self.last_status = self.summary = self.generation = None
        self.discard_partial = self.closed = self.faulted = False

    def _now(self):
        now = _integer(self.monotonic_ns())
        _require(self.last_now is None or now >= self.last_now, 'host monotonic clock regressed')
        _require(self.started_ns is None or now - self.started_ns < MAX_DURATION_NS, 'collector duration exceeded')
        self.last_now = now
        return now

    def _response(self, result, attaching=False):
        _require(isinstance(result, dict) and set(result) ==
                 {'completeBytes', 'newOffset', 'inode', 'size', 'partial', 'eof'}, 'invalid reader response')
        data = result['completeBytes']
        _require(type(data) is bytes and len(data) <= MAX_CHUNK_BYTES, 'reader chunk exceeds bound')
        size = _integer(result['size'], high=MAX_FILE_BYTES)
        offset = _integer(result['newOffset'], high=size)
        inode = result['inode']
        _require(isinstance(inode, str) and 0 < len(inode) <= 256, 'file identity required')
        _require(type(result['partial']) is bool and type(result['eof']) is bool, 'reader flags required')
        if attaching:
            _require(data == b'' and offset == size and result['eof'], 'attach must skip exact EOF')
            _require(size > 0 or not result['partial'], 'empty file has no partial line')
        else:
            _require(inode == self.inode and size >= self.size and offset >= self.offset, 'reader file replaced or truncated')
            _require(offset == self.offset + len(data) and (not data or data.endswith(b'\n')), 'inexact complete-row cursor')
            if result['eof']:
                _require(result['partial'] == (offset < size) and size - offset < MAX_LINE_BYTES,
                         'invalid partial row or line exceeds bound')
            else:
                _require(not result['partial'] and data and offset < size, 'bounded drain must make progress')
        return data

    def attach(self):
        """Skip all bytes already present, including a partial historical row."""
        _require(not self.faulted and self.offset is None, 'cursor already attached or failed')
        try:
            started = self._now()
            self.started_ns = started
            result = self.read(None, None)
            self._now()
            self._response(result, attaching=True)
            self.offset, self.inode, self.size = result['newOffset'], result['inode'], result['size']
            self.known_since_ns = started
            self.discard_partial = result['partial']
        except Exception as exc:
            self.faulted = True
            raise CursorError('reader attachment unavailable') from exc

    def _row(self, row, started):
        if set(row) == {'summary'}:
            summary = row['summary']
            _require(isinstance(summary, dict) and set(summary) ==
                     {'ticks', self.summary_count_key, 'startup_waits', 'status'}, 'invalid reader summary')
            for key in ('ticks', self.summary_count_key, 'startup_waits'):
                _integer(summary[key], high=MAX_ROWS * 16)
            _require(summary['status'] in ('complete', 'failed'), 'invalid terminal status')
            self.summary, self.closed = summary, True
            return None
        _require(not self.closed, 'reader row after terminal summary')
        _require(row.get('version') == 1 and row.get('run_id') == self.run_id, 'reader run differs')
        tick = _integer(row.get('tick'), 1, MAX_ROWS)
        attempt = _integer(row.get('startup_attempt', 0), 0, 32)
        generation = _integer(row.get('reader_generation'), 1, (1 << 64) - 1)
        _require(self.generation is None or generation == self.generation, 'reader generation changed')
        status = row.get('status')
        _require(status in ('ready', 'waiting_for_source', 'failed'), 'invalid reader status')
        clock = row.get('clock')
        _require(status != 'ready' or isinstance(clock, dict), 'ready reader requires its final clock')
        if clock is not None:
            _require(isinstance(clock, dict) and set(clock) == CLOCK_FIELDS and
                     clock['deployment_generation'] == generation, 'reader clock differs')
        self.generation = generation
        if tick < self.last_tick:
            return None
        if tick == self.last_tick:
            transition = self.last_status == 'waiting_for_source' and (
                status in ('ready', 'failed') or status == 'waiting_for_source' and attempt > self.last_attempt)
            if not transition:
                return None
        _require(status != 'waiting_for_source' or self.last_status != 'ready', 'startup waiting after ready')
        self.last_tick, self.last_attempt, self.last_status = tick, attempt, status
        self.sequence += 1
        if status == 'failed':
            self.closed = True
        return {'version': 1, 'run_id': self.run_id, 'sequence': self.sequence,
                'collection_started_monotonic_ns': started, 'reader': row}

    def poll(self):
        """Return new reader events without renewing duplicate or partial rows."""
        _require(not self.faulted and self.offset is not None, 'attach a healthy cursor first')
        if self.closed:
            return []
        try:
            poll_started = self._now()
            result = self.read(self.offset, self.inode)
            self._now()
            data = self._response(result)
            lines = data.split(b'\n')[:-1] if data else []
            if self.discard_partial and lines:
                lines.pop(0)
                self.discard_partial = False
            _require(self.rows + len(lines) <= MAX_ROWS, 'reader row budget exceeded')
            output = []
            for line in lines:
                self.rows += 1
                update = self._row(_decode(line), self.known_since_ns)
                if update is not None:
                    output.append(update)
            self.offset, self.size = result['newOffset'], result['size']
            if result['eof'] and not result['partial']:
                self.known_since_ns = poll_started
            return output
        except Exception as exc:
            self.faulted = True
            raise CursorError('live reader collection unavailable') from exc
