#!/usr/bin/env python3
"""Contribute G1/Go2 PointCloud2 observations to a Linux-owned room.

Run in the robot's ROS 2 environment, on the twin gateway or through an
explicitly opened authenticated app tunnel. No motion topics are published.
"""
import argparse
import json
import math
import struct
import time
import urllib.request
import uuid


def request(endpoint, path, value=None, method=None):
    body = None if value is None else json.dumps(value, allow_nan=False).encode()
    req = urllib.request.Request(endpoint.rstrip('/') + path, data=body,
                                 method=method, headers={'Content-Type': 'application/json'})
    # Ignore HTTP_PROXY: robot geometry must stay on the configured tunnel.
    with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(req, timeout=5) as response:
        return json.load(response)


def transform_point(point, translation, rotation):
    x, y, z = point
    qx, qy, qz, qw = rotation
    tx, ty, tz = 2 * (qy*z-qz*y), 2 * (qz*x-qx*z), 2 * (qx*y-qy*x)
    return (x+qw*tx+qy*tz-qz*ty+translation[0],
            y+qw*ty+qz*tx-qx*tz+translation[1],
            z+qw*tz+qx*ty-qy*tx+translation[2])


def cloud_points(message):
    fields = {f.name: f for f in message.fields}
    if not all(name in fields and fields[name].datatype == 7 and fields[name].count == 1
               for name in ('x', 'y', 'z')):
        raise ValueError('PointCloud2 requires scalar FLOAT32 x/y/z fields')
    if message.width * message.height > 2_000_000 or message.point_step < 12:
        raise ValueError('unsupported cloud dimensions')
    offsets = [fields[name].offset for name in ('x', 'y', 'z')]
    if any(offset < 0 or offset + 4 > message.point_step for offset in offsets):
        raise ValueError('invalid point fields')
    if message.row_step < message.width * message.point_step or len(message.data) < message.row_step * message.height:
        raise ValueError('truncated cloud')
    step = max(1, math.ceil(message.width * message.height / 50000))
    fmt = '>f' if message.is_bigendian else '<f'
    for index in range(0, message.width * message.height, step):
        row, col = divmod(index, message.width)
        base = row * message.row_step + col * message.point_step
        point = tuple(struct.unpack_from(fmt, message.data, base + offset)[0] for offset in offsets)
        if all(math.isfinite(v) and abs(v) < 10000 for v in point):
            yield point


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--endpoint', default='http://127.0.0.1:8791')
    parser.add_argument('--source', required=True, help='Stable node name, such as g1:warehouse')
    parser.add_argument('--topic', required=True, help='PointCloud2 topic from G1 or Go2')
    parser.add_argument('--frame', required=True, help='Robot SLAM map frame, resolved at capture time')
    parser.add_argument('--room', help='Existing room UUID; otherwise start a new room for this SLAM epoch')
    parser.add_argument('--room-from-ros', type=float, nargs=4, metavar=('X', 'Y', 'Z', 'YAW'),
                        help='Explicit room registration: Y-up translation in metres and yaw in radians')
    args = parser.parse_args()
    if args.room and args.room_from_ros is None:
        parser.error('--room requires explicit --room-from-ros registration; odometry origins are not interchangeable')
    if args.room_from_ros and not all(math.isfinite(v) for v in args.room_from_ros):
        parser.error('registration must be finite')
    if args.room:
        room = request(args.endpoint, '/v1/rooms/' + str(uuid.UUID(args.room)))
    else:
        identity, revision = str(uuid.uuid4()), str(uuid.uuid4())
        room = request(args.endpoint, '/v1/rooms/' + identity, {
            'site': {'version': 1, 'id': identity, 'modelRevision': revision,
                     'name': args.source + ' space', 'metresPerModelUnit': 1, 'cameras': []},
            'revision': 0, 'removed': False}, 'PUT')
    print('Contributing to room', room['site']['id'], flush=True)
    import rclpy
    from rclpy.node import Node
    from rclpy.qos import qos_profile_sensor_data
    from rclpy.time import Time
    from sensor_msgs.msg import PointCloud2
    from tf2_ros import Buffer, TransformListener

    class Contributor(Node):
        def __init__(self):
            super().__init__('wendy_twin_contributor')
            self.buffer = Buffer()
            self.listener = TransformListener(self.buffer, self)
            self.last = 0
            self.last_stamp = -1
            self.subscription = self.create_subscription(PointCloud2, args.topic, self.observe, qos_profile_sensor_data)

        def observe(self, message):
            now = time.monotonic()
            if now - self.last < 1:
                return
            self.last = now
            try:
                stamp = Time.from_msg(message.header.stamp)
                age = (self.get_clock().now().nanoseconds - stamp.nanoseconds) / 1e9
                if not 0 <= age <= 2 or stamp.nanoseconds <= self.last_stamp:
                    raise ValueError('stale, duplicate, or reset cloud clock; start a new room after SLAM resets')
                tf = self.buffer.lookup_transform(args.frame, message.header.frame_id, stamp).transform
                t, q = tf.translation, tf.rotation
                if not all(math.isfinite(v) for v in (t.x,t.y,t.z,q.x,q.y,q.z,q.w)) or abs(q.x*q.x+q.y*q.y+q.z*q.z+q.w*q.w-1) > .001:
                    raise ValueError('invalid capture-time transform')
                ox, oy, oz, yaw = args.room_from_ros or (0, 0, 0, 0)
                c, s = math.cos(yaw), math.sin(yaw)
                points = []
                for point in cloud_points(message):
                    x, y, z = transform_point(point, (t.x, t.y, t.z), (q.x, q.y, q.z, q.w))
                    # ROS X-forward/Y-left/Z-up -> twin X/Y-up/-Z-forward plane.
                    rx, rz = x, -y
                    points.append({'x': c*rx+s*rz+ox, 'y': z+oy, 'z': -s*rx+c*rz+oz})
                if points:
                    request(args.endpoint, '/v1/rooms/' + room['site']['id'] + '/contributions', {
                        'source': args.source, 'sequence': time.time_ns(),
                        'modelRevision': room['site']['modelRevision'], 'points': points}, 'POST')
                    self.last_stamp = stamp.nanoseconds
            except Exception as error:
                self.get_logger().warning(str(error))

    rclpy.init()
    node = Contributor()
    try:
        rclpy.spin(node)
    finally:
        node.destroy_node()
        rclpy.shutdown()


if __name__ == '__main__':
    main()
