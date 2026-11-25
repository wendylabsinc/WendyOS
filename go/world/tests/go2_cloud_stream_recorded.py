#!/usr/bin/env python3
"""Replay original passive arrival sequences through the candidate assembler.

This advances a logical clock using retained parent-arrival times. It is not a
live sensor, timing benchmark, runtime-continuity check or permission to move.
Every emitted sample is compared with its original serialized bytes and decoded
value. No source timestamp, missing companion or epoch association is repaired.
"""
from __future__ import annotations

import argparse
from collections.abc import Mapping
from dataclasses import fields, is_dataclass
import gzip
import hashlib
import io
import json
import math
import os
from pathlib import Path
import stat
import sys

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from sim.go2_cloud import SOURCE_DIGEST, decode_cloud
from sim.go2_cloud_recording import load_recording, decode_companion
from sim.go2_cloud_stream import Assembler

MAX_INPUT = 4 << 20
ROW_KEYS = ('kind', 'topic', 'publisher_gid', 'source_timestamp_ns',
            'received_timestamp_ns', 'helper_elapsed_ns', 'payload_hex')
LIMITS = dict(start_ns=0, startup_timeout_ns=1_000_000_000,
              run_timeout_ns=2_000_000_000, companion_timeout_ns=100_000_000)


def require(ok, reason):
    if not ok:
        raise ValueError(reason)


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def normalized(value):
    """Stable comparison only; infinity keeps an explicit nonnumeric marker."""
    if is_dataclass(value):
        return {f.name: normalized(getattr(value, f.name)) for f in fields(value)}
    if isinstance(value, Mapping):
        return {k: normalized(v) for k, v in value.items()}
    if isinstance(value, (tuple, list)):
        return [normalized(x) for x in value]
    if isinstance(value, float) and not math.isfinite(value):
        require(value == math.inf, 'unsupported_nonfinite_value')
        return {'missing_range': 'positive_infinity'}
    return value


def encoded(value):
    return (json.dumps(value, sort_keys=True, separators=(',', ':'),
                       allow_nan=False) + '\n').encode()


def read_capture(path):
    fd = os.open(path, os.O_RDONLY | os.O_NONBLOCK | os.O_NOFOLLOW)
    try:
        info = os.fstat(fd)
        require(stat.S_ISREG(info.st_mode) and 0 < info.st_size <= MAX_INPUT,
                'bounded_regular_capture_required')
        with os.fdopen(fd, 'rb', closefd=False) as source:
            stored = source.read(MAX_INPUT + 1)
    finally:
        os.close(fd)
    require(len(stored) <= MAX_INPUT, 'capture_grew')
    raw = stored
    if path.suffix == '.gz':
        with gzip.GzipFile(fileobj=io.BytesIO(stored)) as compressed:
            raw = compressed.read(MAX_INPUT + 1)
    require(0 < len(raw) <= MAX_INPUT, 'decoded_capture_bound')
    return stored, raw


def verify_sample(sample, available):
    matches = [r for r in available if r['topic'] == sample.topic
               and r['serialized_sha256'] == sample.serialized_sha256
               and r['parent_elapsed_ns'] == sample.arrived_ns]
    require(len(matches) == 1, 'sample_not_unique_original')
    row = matches[0]
    raw = bytes.fromhex(row['payload_hex'])
    require(sample.raw_cdr == raw and digest(raw) == sample.serialized_sha256,
            'sample_bytes_changed')
    for field in ('publisher_gid', 'source_timestamp_ns', 'received_timestamp_ns',
                  'helper_elapsed_ns'):
        require(getattr(sample, field) == row[field], 'sample_metadata_changed')
    original = (decode_cloud(raw, source_digest=SOURCE_DIGEST)
                if sample.topic == '/utlidar/cloud'
                else decode_companion(sample.topic, raw))
    value = encoded(normalized(sample.value))
    require(value == encoded(normalized(original)), 'sample_value_changed')
    return {'message_index': row['index'], 'topic': sample.topic,
            'serialized_sha256': digest(raw), 'decoded_value_sha256': digest(value),
            'arrived_ns': sample.arrived_ns, 'publisher_gid': sample.publisher_gid,
            'source_timestamp_ns': sample.source_timestamp_ns,
            'received_timestamp_ns': sample.received_timestamp_ns}


def replay(raw):
    report, decoded, _ = load_recording(raw)
    require(report['parent_elapsed_ns'] < LIMITS['run_timeout_ns'],
            'recording_exceeds_fixed_run')
    assembler = Assembler(SOURCE_DIGEST, report['expected_epoch'], **LIMITS)
    events, candidates, available, emitted_ids = [], [], [], set()

    def receive(batch, now):
        for event in batch:
            events.append({'kind': event.kind, 'reason': event.reason,
                           'capture_id': event.capture_id, 'observed_at_ns': now})
            if event.kind != 'ready':
                require(event.candidate is None, 'nonready_has_candidate')
                continue
            c = event.candidate
            require(c is not None and c.capture_id not in emitted_ids,
                    'duplicate_or_absent_candidate')
            emitted_ids.add(c.capture_id)
            require(event.capture_id == c.capture_id and c.source_digest == SOURCE_DIGEST
                    and c.observed_epoch == report['expected_epoch'], 'candidate_identity')
            require(c.assembled_at_ns == now < c.companion_deadline_ns
                    <= LIMITS['run_timeout_ns'], 'candidate_deadline')
            require(c.companion_deadline_ns == min(c.cloud.arrived_ns + LIMITS['companion_timeout_ns'],
                                                  LIMITS['run_timeout_ns']),
                    'candidate_deadline_extended_or_changed')
            bound = {name: verify_sample(getattr(c, name), available)
                     for name in ('epoch_sample', 'cloud', 'scan', 'odom')}
            require(len(c.transforms) == 2, 'candidate_mounts_missing')
            transforms = [verify_sample(s, available) for s in c.transforms]
            for child, sample in zip(('lidar_link', 'utlidar_lidar'), c.transforms):
                require(sample.topic == '/tf_static', 'candidate_mount_topic')
                mounts = [t for t in sample.value['transforms'] if t['child_frame_id'] == child]
                require(len(mounts) == 1 and mounts[0]['frame_id'] == 'base_link'
                        and tuple(mounts[0]['translation_xyz']) == (.28, 0, .08)
                        and tuple(mounts[0]['quaternion_wxyz']) == (1, 0, 0, 0),
                        'candidate_mount_value')
            require(bound['epoch_sample']['message_index'] < bound['cloud']['message_index'],
                    'epoch_backfilled')
            require(c.epoch_sample.value['epoch'] == c.observed_epoch, 'epoch_changed')
            stamp = c.cloud.value.capture_unix_ns
            require(c.scan.value['capture_unix_ns'] == stamp, 'scan_stamp_changed')
            skew = c.odom.value['capture_unix_ns'] - stamp
            require(c.odom_minus_cloud_ns == skew and abs(skew) <= 50_000_000,
                    'odom_skew_changed')
            # Independently examine only messages already delivered at emission.
            odoms = [(abs(decoded[r['index']]['capture_unix_ns'] - stamp), r['index'])
                     for r in available if r['topic'] == '/odom']
            nearest = min(d for d, _ in odoms)
            nearest_indices = [i for d, i in odoms if d == nearest]
            require(nearest_indices == [bound['odom']['message_index']],
                    'nearest_odom_ambiguous_or_changed')
            require(max(decoded[r['index']]['capture_unix_ns'] for r in available
                        if r['topic'] == '/odom') >= stamp, 'odom_watermark_missing')
            candidates.append({'capture_id': c.capture_id, 'capture_unix_ns': stamp,
                'observed_epoch': c.observed_epoch, 'assembled_at_ns': now,
                'companion_deadline_ns': c.companion_deadline_ns,
                'odom_minus_cloud_ns': skew, 'samples': bound, 'transforms': transforms,
                'finite_points': len(c.cloud.value.points),
                'missing_rays': sum(v is None for v in c.cloud.value.ranges_m)})

    for row in report['messages']:
        now = row['parent_elapsed_ns']
        available.append(row)
        receive(assembler.ingest({k: row[k] for k in ROW_KEYS}, now), now)
    end = report['parent_elapsed_ns']
    receive(assembler.poll(end), end)
    receive(assembler.close(end), end)
    require(assembler.poll(end) == () and assembler.close(end) == (), 'closed_reemission')
    dispositions = [e['capture_id'] for e in events if e['kind'] in ('ready', 'unavailable')]
    complete_dispositions = sorted(dispositions) == list(range(1, report['cloud_count']+1))
    return {'schema': 'wendy.world.go2-cloud-stream-recorded.v1',
            'simulation': True, 'operational_authority': False,
            'logical_time_only': True, 'new_sensor_capture': False,
            'commands_published': 0, 'runtime_calls': 0,
            'source_digest': SOURCE_DIGEST, 'recording_sha256': digest(raw),
            'recording_bytes': len(raw), 'expected_epoch': report['expected_epoch'],
            'limits': LIMITS, 'events': events, 'candidates': candidates,
            'all_clouds_have_one_disposition': complete_dispositions,
            'final_status': assembler.status(),
            'batch_joined_clouds': report['joined_clouds'],
            'batch_missing': [{'message_index': c['message_index'], 'reasons': c['missing_joins']}
                              for c in report['clouds'] if c['missing_joins']],
            'success': bool(candidates) and complete_dispositions
                       and not any(e['kind'] == 'fatal' for e in events)}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--capture', action='append', required=True, type=Path)
    args = parser.parse_args(argv)
    require(len(args.capture) <= 8, 'capture_count_bound')
    results = []
    for path in args.capture:
        stored, raw = read_capture(path)
        result = replay(raw)
        result['stored_sha256'] = digest(stored)
        result['stored_bytes'] = len(stored)
        results.append(result)
    sys.stdout.buffer.write(encoded({'results': results,
                                     'success': all(r['success'] for r in results)}))
    return 0 if all(r['success'] for r in results) else 1


if __name__ == '__main__':
    raise SystemExit(main())
