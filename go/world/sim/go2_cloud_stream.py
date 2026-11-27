"""Pure bounded assembly of original cloud/scan/odometry observations.

All time is supplied by the caller. Deadlines bound joining, not sensor age:
monotonic arrival time cannot establish original wall-clock freshness. The caller
must recheck real clocks after decoding and before any future use. Publisher GIDs,
the commissioned source pin and separately observed epoch are not authentication.
No ready output is cached, no clock is read, and no I/O or control occurs here.
"""
from __future__ import annotations

from dataclasses import dataclass
import hashlib
import math
import re
import struct
from types import MappingProxyType
from typing import Any

from .go2_cloud import decode_cloud, SOURCE_DIGEST, RANGE_QUANTIZATION_M
from .go2_cloud_recording import decode_companion
from .go2_cloud_observer import TOPICS, PAIR_NS

MAX_PENDING = 2
MAX_SCANS = 8
MAX_ODOMS = 16
MAX_RUN_NS = 60_000_000_000
MAX_STARTUP_NS = 5_000_000_000
MAX_COMPANION_NS = 100_000_000
MAX_INT = (1 << 63)-1
# Exact ROS binary32 scalar values emitted by the pinned slow_sensors source.
_SCAN_SCALARS = dict(zip(
    ('angle_min', 'angle_max', 'angle_increment', 'time_increment',
     'scan_time', 'range_min', 'range_max'),
    (struct.unpack('<f', struct.pack('<f', x))[0]
     for x in (-math.pi, math.pi-2*math.pi/360, 2*math.pi/360, 0, .1, .1, 12))))
_SAMPLE_FIELDS = frozenset(('kind', 'topic', 'publisher_gid', 'source_timestamp_ns',
                            'received_timestamp_ns', 'helper_elapsed_ns', 'payload_hex'))


def _integer(value, low=0, high=MAX_INT):
    if type(value) is not int or not low <= value <= high:
        raise ValueError('integer_bound')
    return value


def _freeze(value):
    if isinstance(value, dict):
        return MappingProxyType({k: _freeze(v) for k, v in value.items()})
    if isinstance(value, list):
        return tuple(_freeze(v) for v in value)
    return value


@dataclass(frozen=True)
class Sample:
    topic: str
    publisher_gid: str
    source_timestamp_ns: int
    received_timestamp_ns: int
    helper_elapsed_ns: int
    arrived_ns: int
    serialized_sha256: str
    raw_cdr: bytes
    value: Any


@dataclass(frozen=True)
class Candidate:
    capture_id: int
    source_digest: str
    observed_epoch: int
    epoch_sample: Sample
    cloud: Sample
    scan: Sample
    odom: Sample
    transforms: tuple[Sample, ...]
    assembled_at_ns: int
    companion_deadline_ns: int
    odom_minus_cloud_ns: int


@dataclass(frozen=True)
class Event:
    kind: str
    reason: str
    capture_id: int | None = None
    candidate: Candidate | None = None


@dataclass(frozen=True)
class _Pending:
    capture_id: int
    cloud: Sample
    epoch: Sample
    deadline: int


class Assembler:
    """Single-owner assembler. Public results share only immutable values.

    Per-topic original capture stamps strictly advance; delivery between topics
    may reorder. Odometry selection waits for its stamp watermark to reach the
    cloud stamp. No later monotone sample can then be closer. Equal-distance
    choices remain unavailable. Eviction never resets identity/stamp watermarks.
    Bounds are two clouds, eight scans, sixteen odoms, one epoch and two mount
    records. Overload retires observations explicitly; malformed data is fatal.
    Fatal/closed instances cannot be reused or armed again.
    """
    def __init__(self, source_digest, expected_epoch, *, start_ns,
                 startup_timeout_ns, run_timeout_ns, companion_timeout_ns):
        if source_digest != SOURCE_DIGEST:
            raise ValueError('source_digest')
        self.source_digest = source_digest
        self.expected_epoch = _integer(expected_epoch, 1)
        self._now = _integer(start_ns)
        run = _integer(run_timeout_ns, 1, MAX_RUN_NS)
        startup = _integer(startup_timeout_ns, 1, min(run, MAX_STARTUP_NS))
        self._companion = _integer(companion_timeout_ns, 1, min(run, MAX_COMPANION_NS))
        if start_ns > MAX_INT-run:
            raise ValueError('deadline_overflow')
        self._run_deadline = start_ns+run
        self._startup_deadline = start_ns+startup
        self._started = False
        self._terminal = None
        self._fault = None
        self._pending = []
        self._scans = []
        self._odoms = []
        self._mounts = {}
        self._epoch = None
        self._gids = {}
        self._stamps = {}
        self._dds_stamps = {}
        self._evicted = {'/scan': 0, '/odom': 0}
        self._helper_elapsed = 0
        self._counts = dict(rows=0, clouds=0, ready=0, unavailable=0, evicted_scans=0,
                            evicted_odoms=0, serialized_bytes=0)

    def status(self):
        return {'state': self._terminal or ('running' if self._started else 'starting'),
                'fault': self._fault, 'run_deadline_ns': self._run_deadline,
                'startup_deadline_ns': self._startup_deadline,
                'pending': len(self._pending), 'scans': len(self._scans),
                'odoms': len(self._odoms), 'mounts': len(self._mounts),
                'publisher_gids': dict(self._gids), 'last_capture_stamps': dict(self._stamps),
                'counters': dict(self._counts)}

    def _count(self, name, n=1):
        self._counts[name] = min(MAX_INT, self._counts[name]+n)

    def _clear(self):
        self._pending.clear(); self._scans.clear(); self._odoms.clear()
        self._mounts.clear(); self._epoch = None

    def _fatal(self, reason):
        self._terminal = 'fault'; self._fault = reason
        self._clear()
        return (Event('fatal', reason),)

    def _unavailable(self, pending, reason):
        self._count('unavailable')
        return Event('unavailable', reason, pending.capture_id)

    def _end(self, reason):
        events = [self._unavailable(p, reason) for p in self._pending]
        self._terminal = 'closed'; self._clear()
        return tuple(events+[Event('closed', reason)])

    def _advance(self, now_ns):
        if self._terminal:
            return ()
        try:
            _integer(now_ns)
        except ValueError:
            return self._fatal('caller_clock_invalid')
        if now_ns < self._now:
            return self._fatal('caller_clock_regression')
        self._now = now_ns
        if now_ns >= self._run_deadline:
            return self._end('run_deadline')
        if not self._started and now_ns >= self._startup_deadline:
            return self._fatal('startup_deadline')
        expired = [p for p in self._pending if now_ns >= p.deadline]
        self._pending = [p for p in self._pending if now_ns < p.deadline]
        return tuple(self._unavailable(p, 'companion_deadline') for p in expired)

    def _sample(self, row, now_ns):
        if type(row) is not dict or len(row) != len(_SAMPLE_FIELDS) or set(row) != _SAMPLE_FIELDS or row['kind'] != 'sample':
            raise ValueError('sample_schema')
        topic, gid, payload = row['topic'], row['publisher_gid'], row['payload_hex']
        if type(topic) is not str or len(topic) > 32 or topic not in TOPICS:
            raise ValueError('topic')
        if type(gid) is not str or len(gid) != 48 or re.fullmatch('[0-9a-f]{48}', gid) is None or gid == '0'*48:
            raise ValueError('publisher_gid')
        if topic in self._gids and self._gids[topic] != gid:
            raise ValueError('publisher_changed')
        source = _integer(row['source_timestamp_ns'], 1)
        received = _integer(row['received_timestamp_ns'])
        elapsed = _integer(row['helper_elapsed_ns'])
        if elapsed < self._helper_elapsed:
            raise ValueError('helper_clock_regression')
        if source <= self._dds_stamps.get(topic, 0):
            raise ValueError('dds_stamp_nonadvancing')
        if type(payload) is not str or not 0 < len(payload) <= 2*TOPICS[topic] or len(payload)%2 or re.fullmatch('[0-9a-f]+', payload) is None:
            raise ValueError('payload_bound_or_encoding')
        raw = bytes.fromhex(payload)
        value = decode_cloud(raw, source_digest=self.source_digest) if topic == '/utlidar/cloud' else decode_companion(topic, raw)
        stamp = value.capture_unix_ns if topic == '/utlidar/cloud' else value.get('capture_unix_ns')
        if stamp is not None:
            if stamp <= self._stamps.get(topic, 0):
                raise ValueError('capture_stamp_nonadvancing')
        if topic == '/scan':
            if value['frame_id'] != 'lidar_link':
                raise ValueError('scan_frame')
            if any(value[k] != expected for k, expected in _SCAN_SCALARS.items()):
                raise ValueError('scan_layout')
        if topic == '/odom':
            if value['frame_id'] != 'odom' or value['child_frame_id'] != 'base_link':
                raise ValueError('odom_frame')
            q = value['quaternion_wxyz']
            if abs(sum(x*x for x in q)-1) > 1e-6:
                raise ValueError('odom_quaternion')
        if topic == '/simulation/epoch' and value['epoch'] != self.expected_epoch:
            raise ValueError('epoch_changed')
        sample = Sample(topic, gid, source, received, elapsed, now_ns,
                        hashlib.sha256(raw).hexdigest(), raw, _freeze(value))
        if topic == '/tf_static':
            children = set()
            for transform in value['transforms']:
                child = transform['child_frame_id']
                if child in children:
                    raise ValueError('duplicate_transform_child')
                children.add(child)
                if child not in ('lidar_link', 'utlidar_lidar'):
                    continue
                if (transform['frame_id'] != 'base_link' or
                    transform['translation_xyz'] != [.28, 0, .08] or
                    transform['quaternion_wxyz'] != [1, 0, 0, 0]):
                    raise ValueError('mount_conflict')
        self._gids[topic] = gid; self._dds_stamps[topic] = source
        self._helper_elapsed = elapsed
        if stamp is not None: self._stamps[topic] = stamp
        self._count('rows'); self._count('serialized_bytes', len(raw))
        return sample

    def _join(self):
        events = []; keep = []
        for pending in self._pending:
            cloud = pending.cloud.value; stamp = cloud.capture_unix_ns
            scans = [s for s in self._scans if s.value['capture_unix_ns'] == stamp]
            if not scans and self._stamps.get('/scan', 0) > stamp:
                events.append(self._unavailable(pending, 'scan_watermark_passed')); continue
            if self._evicted['/odom'] and self._evicted['/odom'] >= stamp-PAIR_NS:
                events.append(self._unavailable(pending, 'odom_history_evicted')); continue
            if not scans or len(self._mounts) != 2 or self._stamps.get('/odom', 0) < stamp:
                keep.append(pending); continue
            near = [o for o in self._odoms if abs(o.value['capture_unix_ns']-stamp) <= PAIR_NS]
            near.sort(key=lambda o: abs(o.value['capture_unix_ns']-stamp))
            if not near:
                events.append(self._unavailable(pending, 'odom_outside_pair_bound')); continue
            if len(near)>1 and abs(near[0].value['capture_unix_ns']-stamp) == abs(near[1].value['capture_unix_ns']-stamp):
                events.append(self._unavailable(pending, 'ambiguous_nearest_odom')); continue
            scan = scans[0]
            for i, measured in enumerate(scan.value['ranges']):
                recovered = cloud.ranges_m[4320+i]
                if not ((recovered is None and measured == math.inf) or
                        (recovered is not None and math.isfinite(measured) and
                         abs(recovered-measured) <= RANGE_QUANTIZATION_M+12*2**-24)):
                    return self._fatal('horizontal_range_mismatch')
            transforms = tuple(self._mounts[k] for k in ('lidar_link','utlidar_lidar'))
            candidate = Candidate(pending.capture_id, self.source_digest, self.expected_epoch,
                                  pending.epoch, pending.cloud, scan, near[0], transforms,
                                  self._now, pending.deadline, near[0].value['capture_unix_ns']-stamp)
            events.append(Event('ready', 'coherent_originals', pending.capture_id, candidate))
        self._pending = keep
        emitted = sum(e.kind == 'ready' for e in events)
        if emitted:
            self._started = True; self._count('ready', emitted)
        return tuple(events)

    def ingest(self, row, now_ns):
        """Accept one native sample row; malformed/changed input latches failure.

        Native completion validation belongs to the caller, which then calls
        close. Completion rows cannot force missing companions into a candidate.
        """
        events = self._advance(now_ns)
        if self._terminal: return events
        try:
            sample = self._sample(row, now_ns)
            topic = sample.topic
            if topic == '/simulation/epoch': self._epoch = sample
            elif topic == '/tf_static':
                for t in sample.value['transforms']:
                    if t['child_frame_id'] in ('lidar_link','utlidar_lidar'):
                        self._mounts[t['child_frame_id']] = sample
            elif topic in ('/scan','/odom'):
                slots, maximum, counter = (self._scans, MAX_SCANS, 'evicted_scans') if topic == '/scan' else (self._odoms, MAX_ODOMS, 'evicted_odoms')
                slots.append(sample)
                if len(slots)>maximum:
                    self._evicted[topic] = slots.pop(0).value['capture_unix_ns']; self._count(counter)
            else:
                self._count('clouds')
                pending = _Pending(self._counts['clouds'], sample, self._epoch,
                                   min(now_ns+self._companion, self._run_deadline))
                if self._epoch is None:
                    return events+(self._unavailable(pending, 'epoch_not_observed_before_cloud'),)
                if len(self._pending) == MAX_PENDING:
                    events += (self._unavailable(self._pending.pop(0), 'pending_capacity'),)
                self._pending.append(pending)
            joined = self._join()
            return joined if self._terminal else events+joined
        except (ValueError, KeyError, TypeError, OverflowError, RecursionError, struct.error) as error:
            # Error text is a bounded static code from these strict decoders.
            code = str(error)
            if len(code)>80 or not re.fullmatch('[a-z0-9_]+', code): code='invalid_sample'
            return self._fatal(code)

    def poll(self, now_ns):
        events = self._advance(now_ns)
        if self._terminal: return events
        joined = self._join()
        return joined if self._terminal else events+joined

    def close(self, now_ns):
        events = self._advance(now_ns)
        if self._terminal: return events
        return events+self._end('caller_closed')
