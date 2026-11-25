"""Passive, bounded cloud/companion capture. No commands, arm, HTTP or authority.

One fixed native subscriber supplies reserialized original ROS messages plus
actual publisher GIDs and DDS timestamps. These are not original transport
packets and do not authenticate the commissioned runtime source digest.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import re
import selectors
import subprocess
import time

from .go2_cloud import decode_cloud, SOURCE_DIGEST, RINGS, RAY_COUNT, CloudDecodeError

HELPER = '/opt/world/world-go2-cloud-observer'
MAX_ROW = 256 << 10
MAX_RAW = 1 << 20
MAX_REPORT = 4 << 20
MAX_MESSAGES = 2048
TOPICS = {'/utlidar/cloud': 110000, '/scan': 49152, '/odom': 8192,
          '/simulation/epoch': 64, '/tf_static': 16384}
PAIR_NS = 50_000_000


class CaptureError(ValueError):
    pass


def require(value, code):
    if not value:
        raise CaptureError(code)


def encoded(value):
    return (json.dumps(value, sort_keys=True, separators=(',', ':'), allow_nan=False) + '\n').encode()


def strict(raw):
    require(type(raw) is bytes and len(raw) <= MAX_ROW, 'helper_row_bound')
    def pairs(items):
        out = {}
        for key, value in items:
            require(key not in out, 'helper_duplicate_field')
            out[key] = value
        return out
    def nonfinite(_):
        raise CaptureError('helper_nonfinite_json')
    try:
        return json.loads(raw, object_pairs_hook=pairs, parse_constant=nonfinite)
    except (json.JSONDecodeError, UnicodeError, RecursionError):
        raise CaptureError('helper_json') from None


def integer(value, minimum=0, maximum=(1 << 63)-1):
    require(type(value) is int and minimum <= value <= maximum, 'helper_integer')
    return value


def text(value):
    require(isinstance(value, str) and len(value) <= 128 and all(32 <= ord(c) < 127 for c in value), 'companion_string')
    return value


def numbers(values):
    out = [float(x) for x in values]
    require(all(math.isfinite(x) for x in out), 'companion_nonfinite')
    return out


def stamp(header):
    sec, nano = header.stamp.sec, header.stamp.nanosec
    integer(sec); integer(nano, 0, 999999999)
    return sec * 1_000_000_000 + nano


def decode_companion(topic, raw):
    # Deserialization creates no ROS node, subscription, publisher or service.
    # Imports are delayed so pure stream/report tests need no ROS installation.
    from rclpy.serialization import deserialize_message
    from nav_msgs.msg import Odometry
    from sensor_msgs.msg import LaserScan
    from std_msgs.msg import UInt64
    from tf2_msgs.msg import TFMessage
    types = {'/odom': Odometry, '/scan': LaserScan, '/simulation/epoch': UInt64, '/tf_static': TFMessage}
    msg = deserialize_message(raw, types[topic])
    if topic == '/simulation/epoch':
        return {'epoch': integer(msg.data, 1)}
    if topic == '/tf_static':
        require(len(msg.transforms) <= 32, 'transform_count')
        transforms = []
        for t in msg.transforms:
            v, q = t.transform.translation, t.transform.rotation
            transforms.append({'frame_id': text(t.header.frame_id), 'child_frame_id': text(t.child_frame_id), 'capture_unix_ns': stamp(t.header), 'translation_xyz': numbers([v.x, v.y, v.z]), 'quaternion_wxyz': numbers([q.w, q.x, q.y, q.z])})
        return {'transforms': transforms}
    capture = stamp(msg.header)
    require(capture > 0, 'companion_timestamp')
    if topic == '/scan':
        require(len(msg.ranges) <= 8192, 'scan_count')
        angles = numbers([msg.angle_min, msg.angle_increment])
        return {'capture_unix_ns': capture, 'frame_id': text(msg.header.frame_id), 'range_count': len(msg.ranges), 'finite_range_count': sum(math.isfinite(x) for x in msg.ranges), 'angle_min': angles[0], 'angle_increment': angles[1]}
    p, q, v, w = msg.pose.pose.position, msg.pose.pose.orientation, msg.twist.twist.linear, msg.twist.twist.angular
    return {'capture_unix_ns': capture, 'frame_id': text(msg.header.frame_id), 'child_frame_id': text(msg.child_frame_id), 'position_xyz': numbers([p.x, p.y, p.z]), 'quaternion_wxyz': numbers([q.w, q.x, q.y, q.z]), 'linear_velocity_xyz': numbers([v.x, v.y, v.z]), 'angular_velocity_xyz': numbers([w.x, w.y, w.z]), 'pose_covariance': numbers(msg.pose.covariance), 'twist_covariance': numbers(msg.twist.covariance)}


class Capture:
    def __init__(self, source_digest, expected_epoch, max_clouds, companion=decode_companion):
        require(source_digest == SOURCE_DIGEST, 'source_digest')
        integer(expected_epoch, 1); integer(max_clouds, 1, 5)
        self.source_digest, self.expected_epoch, self.max_clouds = source_digest, expected_epoch, max_clouds
        self.companion = companion
        self.messages, self.decoded, self.clouds = [], [], []
        self.gids, self.last_stamp = {}, {}
        self.epoch = None
        self.raw_bytes = 0
        self.helper_elapsed = 0
        self.complete = None

    def ingest(self, row, received_elapsed_ns):
        integer(received_elapsed_ns)
        require(isinstance(row, dict), 'helper_schema')
        if row.get('kind') == 'complete':
            require(set(row) == {'kind', 'reason', 'messages', 'clouds', 'serialized_bytes'} and self.complete is None, 'helper_completion_schema')
            require(row['reason'] in {'max_clouds', 'duration', 'capture_limit', 'interrupted'}, 'helper_completion_reason')
            require(integer(row['messages']) == len(self.messages) and integer(row['clouds']) == len(self.clouds) and integer(row['serialized_bytes']) == self.raw_bytes, 'helper_completion_counts')
            self.complete = dict(row)
            return
        require(self.complete is None and set(row) == {'kind', 'topic', 'publisher_gid', 'source_timestamp_ns', 'received_timestamp_ns', 'helper_elapsed_ns', 'payload_hex'} and row['kind'] == 'sample' and row['topic'] in TOPICS, 'helper_schema')
        topic, gid, payload = row['topic'], row['publisher_gid'], row['payload_hex']
        require(isinstance(gid, str) and re.fullmatch('[0-9a-f]{48}', gid) and gid != '0'*48, 'helper_gid')
        integer(row['source_timestamp_ns'], 1); integer(row['received_timestamp_ns'])
        elapsed = integer(row['helper_elapsed_ns'])
        require(elapsed >= self.helper_elapsed, 'helper_monotonic_regression')
        require(isinstance(payload, str) and 0 < len(payload) <= 2*TOPICS[topic] and len(payload)%2 == 0 and re.fullmatch('[0-9a-f]+', payload), 'helper_payload')
        raw = bytes.fromhex(payload)
        require(len(self.messages) < MAX_MESSAGES and self.raw_bytes + len(raw) <= MAX_RAW, 'capture_byte_or_message_limit')
        require(topic != '/utlidar/cloud' or len(self.clouds) < self.max_clouds, 'cloud_count_limit')
        # Retain the offending bounded sample before interpreting it. Subsequent
        # protocol/identity errors are never converted into a successful join.
        saved = dict(row)
        saved.update(index=len(self.messages), parent_elapsed_ns=received_elapsed_ns, serialized_sha256=hashlib.sha256(raw).hexdigest(), serialized_bytes=len(raw))
        saved['dds_received_timestamp_status'] = 'unavailable_zero' if row['received_timestamp_ns'] == 0 else 'provided'
        self.messages.append(saved); self.decoded.append(None)
        self.raw_bytes += len(raw); self.helper_elapsed = elapsed
        require(topic not in self.gids or self.gids[topic] == gid, 'publisher_changed')
        self.gids[topic] = gid
        if topic == '/utlidar/cloud':
            cloud = decode_cloud(raw, source_digest=self.source_digest)
            value = {'capture_unix_ns': cloud.capture_unix_ns, 'frame_id': cloud.frame_id}
            rings = [0] * RINGS
            for point in cloud.points:
                rings[point.ray_index // 360] += 1
            self.clouds.append({'message_index': saved['index'], 'capture_unix_ns': cloud.capture_unix_ns, 'frame_id': cloud.frame_id, 'finite_points': len(cloud.points), 'missing_rays': RAY_COUNT-len(cloud.points), 'points_by_ring': rings, 'range_quantization_m': cloud.range_quantization_m, 'direction_quantization_rad': cloud.direction_quantization_rad, 'observed_epoch_at_arrival': self.epoch})
        else:
            value = self.companion(topic, raw)
        require(isinstance(value, dict), 'companion_schema')
        if topic == '/simulation/epoch':
            observed = integer(value['epoch'], 1)
            require(observed == self.expected_epoch and (self.epoch is None or observed == self.epoch), 'epoch_mismatch_or_changed')
            self.epoch = observed
        if 'capture_unix_ns' in value:
            capture = integer(value['capture_unix_ns'], 1)
            require(capture > self.last_stamp.get(topic, 0), 'capture_nonadvancing')
            self.last_stamp[topic] = capture
        self.decoded[-1] = value

    def report(self, error, exit_code, elapsed_ns):
        scans = [(i, x) for i, x in enumerate(self.decoded) if x is not None and self.messages[i]['topic'] == '/scan']
        odoms = [(i, x) for i, x in enumerate(self.decoded) if x is not None and self.messages[i]['topic'] == '/odom']
        transforms = [(i, x) for i, x in enumerate(self.decoded) if x is not None and self.messages[i]['topic'] == '/tf_static']
        # The original serialized TF evidence remains once in messages. A
        # bounded first matching transform aids inspection; it is neither a
        # calibrated mount nor proof that other observed values agree.
        mount_count = 0
        first_mount = None
        for i, value in transforms:
            for t in value['transforms']:
                if t['frame_id'] == 'base_link' and t['child_frame_id'] == 'utlidar_lidar':
                    mount_count += 1
                    if first_mount is None:
                        first_mount = {'message_index': i, 'transform': t}
        tf_evidence = {'message_indices': [i for i, _ in transforms],
                       'matching_frame_pair_count': mount_count,
                       'first_matching_frame_pair': first_mount,
                       'role': 'frame_presence_only_not_calibration_or_value_consistency'}
        clouds = []
        for original in self.clouds:
            c = dict(original)
            same_scan = [(i, x) for i, x in scans if x['capture_unix_ns'] == c['capture_unix_ns'] and x.get('frame_id') == 'lidar_link']
            near_odom = [(i, x) for i, x in odoms if abs(x['capture_unix_ns']-c['capture_unix_ns']) <= PAIR_NS and x.get('frame_id') == 'odom' and x.get('child_frame_id') == 'base_link']
            near_odom.sort(key=lambda item: (abs(item[1]['capture_unix_ns']-c['capture_unix_ns']), item[0]))
            c['scan_message_index'] = same_scan[0][0] if same_scan else None
            c['odom_message_index'] = near_odom[0][0] if near_odom else None
            c['odom_minus_cloud_ns'] = near_odom[0][1]['capture_unix_ns']-c['capture_unix_ns'] if near_odom else None
            c['original_odometry'] = near_odom[0][1] if near_odom else None
            c['tf_evidence_reference'] = 'shared_tf_evidence'
            c['missing_joins'] = (["same_capture_scan"] if not same_scan else []) + (["odom_within_50ms"] if not near_odom else []) + (["cloud_mount_transform"] if first_mount is None else []) + (["epoch_before_cloud_arrival"] if c['observed_epoch_at_arrival'] != self.expected_epoch else [])
            c['epoch_association'] = 'surrounding_topic_observation_only_not_encoded_in_cloud'
            c['join_role'] = 'timestamp_and_frame_presence_only_not_validated_geometry'
            clouds.append(c)
        complete = error is None and exit_code == 0 and self.complete is not None and self.complete['reason'] in {'duration', 'max_clouds'} and bool(clouds)
        joined = sum(not c['missing_joins'] for c in clouds)
        report = {'schema': 'wendy.world.go2-passive-cloud-capture.v1', 'simulation': True, 'operational_authority': False, 'commands_published': 0, 'runtime_http_calls': 0, 'source_digest': self.source_digest, 'source_identity_role': 'explicit_commissioned_pin_not_verified_by_DDS_capture', 'payload_representation': 'reserialized_original_ROS_message_not_original_transport_packet', 'expected_epoch': self.expected_epoch, 'observed_epoch': self.epoch, 'publisher_gids': dict(self.gids), 'capture_complete': complete, 'all_clouds_joined': bool(clouds) and joined == len(clouds), 'joined_clouds': joined, 'cloud_count': len(clouds), 'messages': self.messages, 'clouds': clouds, 'helper_completion': self.complete, 'helper_exit_code': exit_code, 'error': error, 'parent_elapsed_ns': elapsed_ns, 'limits': {'max_clouds': self.max_clouds, 'max_serialized_bytes': MAX_RAW, 'max_messages': MAX_MESSAGES, 'max_row_bytes': MAX_ROW, 'max_report_bytes': MAX_REPORT, 'odom_pair_skew_ns': PAIR_NS}, 'limits_of_evidence': ['Missing rays are unknown; no occupancy, clearance or controller input is produced.', 'DDS GIDs and payload timestamps do not authenticate the runtime or prove that a cloud belongs to an epoch.', 'Odometry is retained at its original capture; nearest-time pairing is not interpolation or a calibrated synchronization proof.', 'Static transform evidence is observed independently; absence is not a zero transform.', 'Passive capture does not establish continuous sensing, hardware fidelity or permission to move.']}
        report['shared_tf_evidence'] = tf_evidence
        report['raw_payloads_retained'] = True
        report['limits']['cleanup_grace_ns'] = 1_000_000_000
        report['limits_of_evidence'].extend([
            'The scan join compares header capture time and frame, not cloud ranges; TF joins check frame presence, not mount calibration or consistent values.',
            'Native typed DDS deserialization allocates before callback size checks. Those checks bound subsequent serialization; middleware needs external process/container resource limits.',
            'The parent capture deadline includes helper startup and decoding. Terminate/kill/reap has an additional bounded one-second cleanup allowance.',
            'A zero DDS received timestamp means unavailable. The callback publisher GID is retained as opaque middleware metadata; it is not asserted equal to a graph endpoint GID.',
        ])
        if len(encoded(report)) > MAX_REPORT:
            # Retain an explicit failed report even if future summary changes
            # exceed this final cap. Never call truncated evidence complete.
            report['capture_complete'] = False
            report['all_clouds_joined'] = False
            report['prior_error'] = report['error']
            report['error'] = 'report_byte_limit'
            report['raw_payloads_retained'] = False
            report['messages'] = [{key: value for key, value in row.items() if key != 'payload_hex'} for row in self.messages]
        require(len(encoded(report)) <= MAX_REPORT, 'report_metadata_byte_limit')
        return report


def stop_helper(child):
    if child.poll() is None:
        child.terminate()
        try:
            child.wait(timeout=.5)
        except subprocess.TimeoutExpired:
            child.kill(); child.wait(timeout=.5)
    return child.returncode


def observe(source_digest, expected_epoch, duration_ms, max_clouds):
    integer(duration_ms, 100, 15000)
    capture = Capture(source_digest, expected_epoch, max_clouds)
    started = time.monotonic_ns()
    deadline = started + duration_ms*1_000_000
    child = None; error = None; code = None
    try:
        child = subprocess.Popen([HELPER, '--duration-ms', str(duration_ms), '--max-clouds', str(max_clouds)], stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, close_fds=True)
        selector = selectors.DefaultSelector(); selector.register(child.stdout, selectors.EVENT_READ)
        pending = b''
        try:
            while True:
                remaining = (deadline-time.monotonic_ns())/1_000_000_000
                require(remaining > 0, 'parent_capture_deadline')
                events = selector.select(remaining)
                require(events, 'parent_capture_deadline')
                chunk = os.read(child.stdout.fileno(), 65536)
                if not chunk:
                    require(not pending, 'helper_partial_row')
                    break
                pending += chunk
                while b'\n' in pending:
                    line, pending = pending.split(b'\n', 1)
                    capture.ingest(strict(line+b'\n'), time.monotonic_ns()-started)
                require(len(pending) <= MAX_ROW, 'helper_row_bound')
        finally:
            selector.close()
        code = child.wait(timeout=max(.001, min(.5, (deadline-time.monotonic_ns())/1_000_000_000)))
        require(capture.complete is not None, 'helper_missing_completion')
        require(code == 0, 'helper_nonzero_exit')
    except (CaptureError, CloudDecodeError) as exc:
        error = str(exc)
    except subprocess.TimeoutExpired:
        error = 'helper_exit_timeout'
    except (OSError, ValueError, KeyError, TypeError, ImportError, RuntimeError):
        error = 'capture_unavailable_or_invalid'
    except KeyboardInterrupt:
        error = 'capture_interrupted'
    finally:
        if child is not None:
            try: code = stop_helper(child)
            except (OSError, subprocess.TimeoutExpired): error = error or 'helper_cleanup_incomplete'
            if child.stdout is not None: child.stdout.close()
    return capture.report(error, code, time.monotonic_ns()-started)


def output_path(path):
    path = Path(path).absolute()
    require('..' not in path.parts, 'output_path')
    for parent in path.parents:
        require(not parent.is_symlink() and parent.is_dir(), 'output_parent')
    require(not path.exists() and not path.is_symlink(), 'output_exists')
    return path


def write_report(path, report):
    path = output_path(path)
    raw = encoded(report); require(len(raw) <= MAX_REPORT, 'report_byte_limit')
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'wb') as out:
        out.write(raw); out.flush(); os.fsync(out.fileno())


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source-digest', required=True)
    parser.add_argument('--expected-epoch', required=True, type=int)
    parser.add_argument('--duration-ms', type=int, default=10000)
    parser.add_argument('--max-clouds', type=int, default=5)
    parser.add_argument('--out', required=True, help='new private regular JSON report; parent directory must exist')
    args = parser.parse_args(argv)
    output_path(args.out)
    report = observe(args.source_digest, args.expected_epoch, args.duration_ms, args.max_clouds)
    write_report(args.out, report)
    print(encoded({'schema': report['schema'], 'capture_complete': report['capture_complete'], 'all_clouds_joined': report['all_clouds_joined'], 'cloud_count': report['cloud_count'], 'error': report['error'], 'commands_published': 0, 'runtime_http_calls': 0}).decode(), end='')
    return 0 if report['capture_complete'] and report['all_clouds_joined'] else 1


if __name__ == '__main__':
    try:
        raise SystemExit(main())
    except (CaptureError, OSError, ValueError):
        print('passive_cloud_capture_refused', file=__import__('sys').stderr)
        raise SystemExit(1)
