"""Convert one local assembler candidate into existing bridge/grid inputs.

This pure conversion reads no clock and grants no authority. The caller still
owns publisher continuity, actual processing deadlines, runtime checks and the
one-use served-capture sequence. Candidate IDs are never reused as that sequence.
Original CDR is decoded again before conversion; this cost belongs inside the
caller's original deadline. Missing rays remain missing.
"""
from __future__ import annotations

from dataclasses import dataclass, fields, is_dataclass, replace
import hashlib
import math
import re
from types import MappingProxyType

from .go2_cloud import SOURCE_DIGEST, decode_cloud
from .go2_cloud_observer import PAIR_NS, TOPICS
from .go2_cloud_recording import decode_companion
from .go2_cloud_stream import Candidate, Sample, MAX_COMPANION_NS


class CaptureError(ValueError):
    pass


def _require(ok, reason):
    if not ok:
        raise CaptureError(reason)


def _integer(value, low=0, high=(1 << 63)-1):
    _require(type(value) is int and low <= value <= high, 'capture_integer')
    return value


def _freeze(value):
    if type(value) is dict:
        return MappingProxyType({k: _freeze(v) for k, v in value.items()})
    if type(value) is list:
        return tuple(_freeze(v) for v in value)
    return value


def _copy(value):
    if type(value) in (dict, MappingProxyType):
        return {k: _copy(v) for k, v in value.items()}
    if type(value) in (list, tuple):
        return [_copy(v) for v in value]
    return value


def _exact(actual, expected):
    """Require matching scalar types as well as values, including nested data."""
    if type(actual) is not type(expected):
        return False
    if expected is None or type(expected) in (str, int, float, bool):
        return actual == expected
    if type(expected) in (dict, MappingProxyType):
        return (actual.keys() == expected.keys() and
                all(_exact(actual[k], v) for k, v in expected.items()))
    if type(expected) in (tuple, list):
        return len(actual) == len(expected) and all(_exact(a, b) for a, b in zip(actual, expected))
    if is_dataclass(expected):
        return all(_exact(getattr(actual, f.name), getattr(expected, f.name)) for f in fields(expected))
    return actual == expected


def _sample(sample, topic):
    _require(type(sample) is Sample and sample.topic == topic, 'capture_sample_topic')
    _require(type(sample.raw_cdr) is bytes and 0 < len(sample.raw_cdr) <= TOPICS[topic],
             'capture_sample_bound')
    _require(type(sample.publisher_gid) is str and
             re.fullmatch('[0-9a-f]{48}', sample.publisher_gid) is not None and
             sample.publisher_gid != '0'*48, 'capture_publisher')
    _integer(sample.source_timestamp_ns, 1)
    for value in (sample.received_timestamp_ns, sample.helper_elapsed_ns, sample.arrived_ns):
        _integer(value)
    _require(hashlib.sha256(sample.raw_cdr).hexdigest() == sample.serialized_sha256,
             'capture_sample_digest')
    value = (decode_cloud(sample.raw_cdr, source_digest=SOURCE_DIGEST)
             if topic == '/utlidar/cloud' else decode_companion(topic, sample.raw_cdr))
    _require(_exact(sample.value, _freeze(value)), 'capture_decoded_value')
    # A caller can construct MappingProxyType around a dict it still owns.
    # Retain an independently owned decoding, not that caller's backing dict.
    return value, replace(sample, value=_freeze(value))


@dataclass(frozen=True)
class ConvertedCapture:
    """Immutable original tuple plus detached bridge and BuildCloud values.

    The original includes all DDS/arrival timestamps, bytes, hashes, epoch and
    both mount associations. `bridge_capture` returns fresh mutable containers
    for the existing bridge. Neither method creates capture-time evidence.
    """
    original: Candidate
    sequence: int
    _bridge: object
    _cloud: object

    def bridge_capture(self):
        return _copy(self._bridge)

    def cloud_payload(self):
        """Bounded cloud input with original lineage for a strict live decoder.

        Missing ranges remain null. There is no scan attachment or clock value
        here; the bridge must bind this payload to its unchanged served tuple.
        """
        c = self.original
        value = _copy(self._cloud)
        value.update(sequence=self.sequence, observed_epoch=c.observed_epoch,
                     scan_sha256=c.scan.serialized_sha256, odom_sha256=c.odom.serialized_sha256,
                     epoch_sha256=c.epoch_sample.serialized_sha256,
                     mount_sha256=c.transforms[0].serialized_sha256,
                     cloud_mount_sha256=c.transforms[1].serialized_sha256,
                     epoch_publisher=c.epoch_sample.publisher_gid,
                     mount_publisher=c.transforms[0].publisher_gid,
                     cloud_mount_publisher=c.transforms[1].publisher_gid)
        return value

    def cloud_observation(self, scan):
        """Attach an enriched scan to the existing cloud-check JSON input.

        `scan` uses the existing Go Scan field names. Its caller supplies the
        commissioned uncertainty after current runtime checks. The source pose,
        ranges, epoch and timestamps must still match this immutable capture.
        The world-go2-cloud-check input decoder maps null ranges to +Inf before
        BuildCloud validates geometry/profile bounds. A future live decoder must
        preserve that rule; direct JSON decoding into Go []float64 invents zero
        for null and is not a compatible adapter.
        """
        b = self._bridge
        _require(type(scan) is dict, 'capture_scan_object')
        expected = {'Sequence': self.sequence,
                    'CapturedUnixNS': b['scan_captured_unix_ns'],
                    'WorldEpoch': b['world_epoch'], 'Frame': 'odom',
                    'Ranges': _copy(b['scan']['ranges']),
                    'AngleMin': b['scan']['angle_min'],
                    'AngleIncrement': b['scan']['angle_increment'],
                    'RangeMin': b['scan']['range_min'], 'RangeMax': b['scan']['range_max'],
                    'Robot': {'X': b['pose']['x'], 'Y': b['pose']['y'], 'Yaw': b['pose']['yaw']},
                    'SensorMount': {'X': .28, 'Y': 0., 'Yaw': 0.}, 'MountKnown': True}
        _require(set(scan) == set(expected) | {'TiltedPose', 'PositionUncertaintyM',
                                             'YawUncertaintyRad', 'UncertaintyKnown'},
                 'capture_scan_fields')
        _require(all(_exact(scan.get(k), v) for k, v in expected.items()),
                 'capture_scan_originals')
        pose = scan.get('TiltedPose')
        pose_scalars = {'pair_translation_bound_m', 'pair_attitude_bound_rad',
                        'odometry_epoch_age_s', 'base_height_min_m', 'base_height_max_m'}
        _require(type(pose) is dict and set(pose) == set(b['tilted_pose']) | pose_scalars |
                 {'odom_captured_unix_ns'} and type(pose['odom_captured_unix_ns']) is int and
                 pose['odom_captured_unix_ns'] == b['odom_captured_unix_ns'],
                 'capture_scan_odometry')
        for key, value in b['tilted_pose'].items():
            _require(_exact(pose.get(key), _copy(value)), 'capture_scan_pose')
        _require(scan['UncertaintyKnown'] is True and
                 all(type(scan[k]) in (int, float) and math.isfinite(scan[k]) and 0 <= scan[k] <= .5
                     for k in ('PositionUncertaintyM', 'YawUncertaintyRad')) and
                 all(type(pose[k]) in (int, float) and math.isfinite(pose[k]) and 0 <= pose[k] <= 120
                     for k in pose_scalars), 'capture_scan_uncertainty')
        out = _copy(self._cloud)
        out['scan'] = _copy(scan)
        return out


def convert(candidate, *, capture_sequence):
    """Check original byte consistency and convert one already joined candidate.

    Cross-candidate source pins, nearest-odometry history and lifecycle belong
    to the Assembler and its live owner. This function cannot establish current
    freshness, calibrated time, epoch-in-cloud proof or runtime authentication.
    """
    try:
        _require(type(candidate) is Candidate, 'capture_candidate_type')
        c = candidate
        _integer(capture_sequence, 1, (1 << 53)-1)
        _integer(c.capture_id, 1)
        _integer(c.observed_epoch, 1, (1 << 53)-1)
        _require(c.source_digest == SOURCE_DIGEST, 'capture_source')
        _require(type(c.transforms) is tuple and len(c.transforms) == 2, 'capture_mount_count')
        epoch, epoch_sample = _sample(c.epoch_sample, '/simulation/epoch')
        cloud, cloud_sample = _sample(c.cloud, '/utlidar/cloud')
        scan, scan_sample = _sample(c.scan, '/scan')
        odom, odom_sample = _sample(c.odom, '/odom')
        decoded_mounts = [_sample(s, '/tf_static') for s in c.transforms]
        mounts = [v for v, _ in decoded_mounts]
        _integer(c.assembled_at_ns)
        _integer(c.companion_deadline_ns, 1)
        _require(c.epoch_sample.arrived_ns <= c.cloud.arrived_ns <= c.assembled_at_ns <
                 c.companion_deadline_ns <= c.cloud.arrived_ns + MAX_COMPANION_NS,
                 'capture_join_deadline')
        _require(all(s.arrived_ns <= c.assembled_at_ns for s in (c.scan, c.odom, *c.transforms)),
                 'capture_join_arrival')
        _require(epoch['epoch'] == c.observed_epoch, 'capture_epoch')
        _require(scan['frame_id'] == 'lidar_link' and scan['capture_unix_ns'] == cloud.capture_unix_ns,
                 'capture_scan_pair')
        _require(odom['frame_id'] == 'odom' and odom['child_frame_id'] == 'base_link',
                 'capture_odometry_frame')
        skew = odom['capture_unix_ns'] - cloud.capture_unix_ns
        _require(type(c.odom_minus_cloud_ns) is int and c.odom_minus_cloud_ns == skew and
                 abs(skew) <= PAIR_NS, 'capture_odometry_pair')
        for child, value in zip(('lidar_link', 'utlidar_lidar'), mounts):
            selected = [t for t in value['transforms'] if t['child_frame_id'] == child]
            _require(len(selected) == 1 and selected[0]['frame_id'] == 'base_link' and
                     selected[0]['translation_xyz'] == [.28, 0., .08] and
                     selected[0]['quaternion_wxyz'] == [1., 0., 0., 0.], 'capture_mount')
        _require(c.transforms[0].publisher_gid == c.transforms[1].publisher_gid, 'capture_mount_publisher')
        q = odom['quaternion_wxyz']
        norm2 = sum(v*v for v in q)
        _require(all(-1. <= v <= 1. for v in q) and abs(norm2-1.) <= 1e-6, 'capture_quaternion')
        # Match the existing v2 sensor adapter. The original quaternion stays
        # unchanged in c.odom; this normalized orientation is derived geometry.
        q = [v/math.sqrt(norm2) for v in q]
        w, x, y, z = q
        roll = math.atan2(2*(w*x+y*z), 1-2*(x*x+y*y))
        pitch = math.asin(max(-1., min(1., 2*(w*y-z*x))))
        yaw = math.atan2(2*(w*z+x*y), 1-2*(y*y+z*z))
        _require(all(abs(v) <= 100 for v in odom['position_xyz']) and
                 all(abs(v) <= 1e6 for cov in (odom['pose_covariance'], odom['twist_covariance']) for v in cov) and
                 all(cov[i] >= 0 for cov in (odom['pose_covariance'], odom['twist_covariance'])
                     for i in (0, 7, 14, 21, 28, 35)), 'capture_odometry_bounds')
        velocity = [odom['linear_velocity_xyz'][0], odom['linear_velocity_xyz'][1],
                    odom['angular_velocity_xyz'][2]]
        _require(abs(velocity[0]) <= 2 and abs(velocity[1]) <= 2 and abs(velocity[2]) <= 4,
                 'capture_velocity_bounds')
        ranges = [None if value == math.inf else value for value in scan['ranges']]
        for i, measured in enumerate(ranges):
            recovered = cloud.ranges_m[4320+i]
            _require((measured is None and recovered is None) or
                     (measured is not None and recovered is not None and
                      abs(measured-recovered) <= cloud.range_quantization_m+12*2**-24),
                     'capture_horizontal_pair')
        bridge = dict(capture_sequence=capture_sequence,
                      odom_captured_unix_ns=odom['capture_unix_ns'],
                      scan_captured_unix_ns=scan['capture_unix_ns'],
                      cloud_captured_unix_ns=cloud.capture_unix_ns,
                      odom_publisher=c.odom.publisher_gid, scan_publisher=c.scan.publisher_gid,
                      cloud_publisher=c.cloud.publisher_gid, mount_publisher=c.transforms[0].publisher_gid,
                      cloud_mount_publisher=c.transforms[1].publisher_gid, world_epoch=c.observed_epoch,
                      pose={'x': odom['position_xyz'][0], 'y': odom['position_xyz'][1], 'yaw': yaw},
                      body_velocity=velocity, pose_covariance=odom['pose_covariance'],
                      twist_covariance=odom['twist_covariance'], tilt=abs(roll)+abs(pitch),
                      roll_rad=roll, pitch_rad=pitch, mount_height=.08,
                      tilted_pose={'quaternion_wxyz': q, 'odom_position_xyz': odom['position_xyz'],
                                   'mount_translation_xyz': [.28, 0., .08],
                                   'mount_quaternion_wxyz': [1., 0., 0., 0.]},
                      scan={key: scan[key] for key in ('angle_min', 'angle_increment', 'range_min', 'range_max')})
        bridge['scan'].update(ranges=ranges, sensor_mount={'x': .28, 'y': 0., 'yaw': 0.})
        observation = dict(model='coherent-multiring-slab-v1', source_digest=SOURCE_DIGEST,
                           frame=cloud.frame_id, publisher_gid=c.cloud.publisher_gid,
                           serialized_sha256=c.cloud.serialized_sha256,
                           captured_unix_ns=cloud.capture_unix_ns, ranges=list(cloud.ranges_m))
        original = replace(c, epoch_sample=epoch_sample, cloud=cloud_sample, scan=scan_sample,
                           odom=odom_sample, transforms=tuple(s for _, s in decoded_mounts))
        return ConvertedCapture(original, capture_sequence, _freeze(bridge), _freeze(observation))
    except CaptureError:
        raise
    except (ValueError, KeyError, TypeError, OverflowError, RecursionError):
        raise CaptureError('capture_invalid') from None
