"""Bounded, ROS-free replay of passive cloud reports from their retained bytes.

Byte consistency is not publisher authentication, epoch attestation, calibrated
pairing or clearance. Missing companions remain missing. This module performs
no file, network, ROS or control actions.
"""
from __future__ import annotations

import json
import math
import struct

from .go2_cloud import decode_cloud, SOURCE_DIGEST, CloudDecodeError, RANGE_QUANTIZATION_M
from .go2_cloud_observer import Capture, CaptureError, MAX_REPORT, MAX_MESSAGES, TOPICS, encoded, integer


class RecordingError(ValueError):
    """Static refusal code, without retained payload or exception text."""


def require(ok, code):
    if not ok:
        raise RecordingError(code)


def _json(raw):
    require(type(raw) is bytes and 0 < len(raw) <= MAX_REPORT, 'recording_size_or_type')
    def pairs(items):
        out = {}
        for key, value in items:
            require(key not in out, 'recording_duplicate_key')
            out[key] = value
        return out
    def number(s):
        v = float(s)
        require(math.isfinite(v), 'recording_nonfinite_json')
        return v
    def whole(s):
        require(len(s) <= 20, 'recording_integer_bound')
        v = int(s)
        require(-(1 << 63) <= v < (1 << 63), 'recording_integer_bound')
        return v
    def constant(_):
        raise RecordingError('recording_nonfinite_json')
    value = json.loads(raw.decode('utf-8'), object_pairs_hook=pairs, parse_float=number,
                       parse_int=whole, parse_constant=constant)
    require(type(value) is dict, 'recording_schema')
    return value


class _CDR:
    def __init__(self, raw, limit):
        require(type(raw) is bytes and 4 <= len(raw) <= limit, 'companion_size')
        require(raw[:4] == b'\x00\x01\x00\x00', 'companion_encoding')
        self.raw, self.pos = raw, 4

    def number(self, kind):
        size = struct.calcsize('<' + kind)
        self.pos += (-(self.pos - 4)) % size
        require(self.pos + size <= len(self.raw), 'companion_truncated')
        v = struct.unpack_from('<' + kind, self.raw, self.pos)[0]
        self.pos += size
        return v

    def string(self):
        n = self.number('I')
        require(0 < n <= 129 and self.pos + n <= len(self.raw), 'companion_string_bound')
        v = self.raw[self.pos:self.pos+n]
        self.pos += n
        require(v[-1:] == b'\0' and all(32 <= c < 127 for c in v[:-1]), 'companion_string')
        return v[:-1].decode('ascii')

    def header(self, positive=True):
        sec, nano = self.number('i'), self.number('I')
        require(sec >= 0 and nano < 1_000_000_000, 'companion_stamp')
        stamp = sec * 1_000_000_000 + nano
        require(not positive or stamp > 0, 'companion_stamp')
        return stamp, self.string()

    def vector(self, n, kind='d', finite=True):
        # Fixed vectors or a sequence whose count was checked before allocation.
        v = [self.number(kind) for _ in range(n)]
        require(not finite or all(math.isfinite(x) for x in v), 'companion_nonfinite')
        return v

    def sequence(self, finite=True):
        n = self.number('I')
        require(n <= 8192 and n * 4 <= len(self.raw) - self.pos, 'companion_sequence_bound')
        return self.vector(n, 'f', finite)

    def finish(self):
        require(self.pos == len(self.raw), 'companion_trailing')


def decode_companion(topic, raw):
    """Decode the bounded source layouts, preserving original binary32 values."""
    require(topic in TOPICS and topic != '/utlidar/cloud', 'companion_topic')
    c = _CDR(raw, TOPICS[topic])
    if topic == '/simulation/epoch':
        out = {'epoch': integer(c.number('Q'), 1)}
    elif topic == '/tf_static':
        count = c.number('I')
        require(count <= 32, 'companion_transform_bound')
        transforms = []
        for _ in range(count):
            stamp, frame = c.header(False)
            child = c.string()
            xyz, q = c.vector(3), c.vector(4)
            transforms.append({'capture_unix_ns': stamp, 'frame_id': frame,
                               'child_frame_id': child, 'translation_xyz': xyz,
                               'quaternion_wxyz': [q[3], *q[:3]]})
        out = {'transforms': transforms}
    else:
        stamp, frame = c.header()
        out = {'capture_unix_ns': stamp, 'frame_id': frame}
        if topic == '/scan':
            for name in ('angle_min', 'angle_max', 'angle_increment', 'time_increment',
                         'scan_time', 'range_min', 'range_max'):
                out[name] = c.number('f')
                require(math.isfinite(out[name]), 'companion_nonfinite')
            out['ranges'] = c.sequence(False)
            out['intensities'] = c.sequence()
            require(len(out['ranges']) == 360, 'companion_scan_count')
            require(0 < out['range_min'] < out['range_max'] <= 12 and
                    out['angle_increment'] > 0 and out['time_increment'] >= 0 and
                    out['scan_time'] >= 0, 'companion_scan_bounds')
            require(all(x == math.inf or math.isfinite(x) and out['range_min'] <= x <= out['range_max']
                        for x in out['ranges']), 'companion_scan_range')
        else:
            out['child_frame_id'] = c.string()
            out['position_xyz'] = c.vector(3)
            q = c.vector(4)
            out['quaternion_wxyz'] = [q[3], *q[:3]]
            out['pose_covariance'] = c.vector(36)
            out['linear_velocity_xyz'] = c.vector(3)
            out['angular_velocity_xyz'] = c.vector(3)
            out['twist_covariance'] = c.vector(36)
    c.finish()
    return out


def _summary(topic, value):
    if topic != '/scan':
        return value
    return {'capture_unix_ns': value['capture_unix_ns'], 'frame_id': value['frame_id'],
            'range_count': len(value['ranges']),
            'finite_range_count': sum(math.isfinite(x) for x in value['ranges']),
            'angle_min': value['angle_min'], 'angle_increment': value['angle_increment']}


def load_recording(raw):
    """Return (original report, full values by message index, decoded clouds).

The cloud list follows report.clouds order. A cloud value at its message index
contains its original frame/stamp; its immutable full geometry is in that list.
Reports with incomplete companion joins are valid inputs. No join is invented.
"""
    try:
        report = _json(raw)
        require(report['schema'] == 'wendy.world.go2-passive-cloud-capture.v1' and
                report['simulation'] is True and report['operational_authority'] is False and
                type(report['commands_published']) is int and report['commands_published'] == 0 and
                type(report['runtime_http_calls']) is int and report['runtime_http_calls'] == 0 and
                report['source_digest'] == SOURCE_DIGEST and report['error'] is None and
                type(report['helper_exit_code']) is int and report['helper_exit_code'] == 0 and
                report['capture_complete'] is True and report['raw_payloads_retained'] is True,
                'recording_not_complete_passive_capture')
        messages = report['messages']
        require(type(messages) is list and 0 < len(messages) <= MAX_MESSAGES, 'recording_message_bound')
        total_elapsed = integer(report['parent_elapsed_ns'])
        values, clouds = [], []
        current = None
        def companion(topic, payload):
            nonlocal current
            current = decode_companion(topic, payload)
            return _summary(topic, current)
        replay = Capture(SOURCE_DIGEST, report['expected_epoch'], report['limits']['max_clouds'], companion)
        previous_elapsed = 0
        keys = ('kind', 'topic', 'publisher_gid', 'source_timestamp_ns',
                'received_timestamp_ns', 'helper_elapsed_ns', 'payload_hex')
        for i, row in enumerate(messages):
            require(type(row) is dict and type(row['index']) is int and row['index'] == i, 'recording_index')
            elapsed = integer(row['parent_elapsed_ns'])
            require(previous_elapsed <= elapsed <= total_elapsed, 'recording_parent_time')
            previous_elapsed = elapsed
            current = None
            replay.ingest({key: row[key] for key in keys}, elapsed)
            if row['topic'] == '/utlidar/cloud':
                cloud = decode_cloud(bytes.fromhex(row['payload_hex']), source_digest=SOURCE_DIGEST)
                clouds.append(cloud)
                current = {'capture_unix_ns': cloud.capture_unix_ns, 'frame_id': cloud.frame_id}
            values.append(current)
            # Canonical JSON equality distinguishes bool/int/float and catches
            # unknown fields as well as any tampered byte/hash/count metadata.
            require(encoded(row) == encoded(replay.messages[-1]), 'recording_message_mismatch')
        replay.ingest(report['helper_completion'], total_elapsed)
        actual = replay.report(None, 0, total_elapsed)
        require(encoded(actual) == encoded(report), 'recording_replay_mismatch')
        for meta, cloud in zip(actual['clouds'], clouds):
            if meta['scan_message_index'] is None:
                continue
            scan = values[meta['scan_message_index']]
            for i, measured in enumerate(scan['ranges']):
                recovered = cloud.ranges_m[4320+i]
                require((recovered is None and measured == math.inf) or
                        (recovered is not None and math.isfinite(measured) and
                         abs(recovered-measured) <= RANGE_QUANTIZATION_M+12*2**-24),
                        'recording_horizontal_mismatch')
        return report, values, clouds
    except RecordingError:
        raise
    except (CaptureError, CloudDecodeError, KeyError, TypeError, ValueError,
            OverflowError, RecursionError, UnicodeError, struct.error):
        raise RecordingError('recording_invalid') from None
