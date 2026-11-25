"""Actual Patrol CLI/DDS boundaries with a kinematic, non-physical sensor peer.

No callback adapters, robot SDK, motor connection or MuJoCo accuracy claim.
The existing private ROS container owns this peer and the application child.
"""
import argparse
import json
import math
import os
from pathlib import Path
import signal
import struct
import subprocess
import sys
import time
import uuid

import rclpy
from nav_msgs.msg import Odometry
from rclpy.qos import QoSProfile, ReliabilityPolicy
from sensor_msgs.msg import PointCloud2, PointField
from std_msgs.msg import String
from std_srvs.srv import Trigger
from unitree_api.msg import Request


class Journey:
    def __init__(self, source, name, output, flags):
        self.name = name
        self.node = rclpy.create_node('patrol_peer_' + uuid.uuid4().hex)
        self.prefix = '/patrol_test_' + uuid.uuid4().hex
        self.state = None
        self.commands = []
        self.x = 0.0
        self.integrate = False
        self.stamp = 'current'
        self.cloud = 'full'
        self.publish = True
        self.fixed = time.time_ns() - 60_000_000_000
        self.next_publish = 0.0
        self.last_publish = time.monotonic()
        self.child = None
        self.log = None
        qos = QoSProfile(depth=1, reliability=ReliabilityPolicy.BEST_EFFORT)
        self.odom = self.node.create_publisher(Odometry, self.prefix + '/odom', qos)
        self.points = self.node.create_publisher(PointCloud2, self.prefix + '/cloud', qos)
        self.node.create_subscription(String, self.prefix + '/status', self.status, 10)
        self.node.create_subscription(Request, self.prefix + '/sport', self.command, 10)
        self.start = self.node.create_client(Trigger, self.prefix + '/start')
        try:
            self.log = (output / (name + '-app.log')).open('w')
            argv = [sys.executable, str(source / 'ros_app.py'), '--autostart',
                    '--waypoints', '[[1,0]]', *flags, '--ros-args']
            for old, new in [('/utlidar/robot_odom', 'odom'), ('/utlidar/cloud_base', 'cloud'),
                             ('/api/sport/request', 'sport'), ('/patrol/status', 'status'),
                             ('/patrol/start', 'start'), ('/patrol/stop', 'stop')]:
                argv += ['-r', old + ':=' + self.prefix + '/' + new]
            self.child = subprocess.Popen(argv, stdout=self.log, stderr=subprocess.STDOUT,
                                          env=dict(os.environ, GO2_SENSOR_CLOCK_OFFSET_SECONDS='0'))
        except BaseException:
            self.node.destroy_node()
            if self.log:
                self.log.close()
            raise

    def status(self, message):
        self.state = json.loads(message.data)

    def command(self, message):
        assert message.header.identity.api_id == 1008, 'unexpected sport API'
        assert message.header.policy.noreply, 'velocity request must be noreply'
        value = json.loads(message.parameter)
        assert set(value) == {'x', 'y', 'z'} and all(math.isfinite(v) for v in value.values())
        assert value['y'] == 0 and abs(value['z']) <= .4
        assert value['x'] in (0.0, .55), 'actual velocity did not preserve Go2 forward minimum'
        self.commands.append((time.monotonic(), value))
        assert len(self.commands) < 10000, 'unbounded command stream'

    def tick(self):
        assert self.child.poll() is None, 'Patrol child exited unexpectedly'
        now = time.monotonic()
        if self.publish and now >= self.next_publish:
            dt = min(now - self.last_publish, .1)
            if self.integrate and self.commands:
                self.x += self.commands[-1][1]['x'] * dt
            self.last_publish, self.next_publish = now, now + .03
            stamp = (time.time_ns() if self.stamp == 'current' else
                     time.time_ns() - 60_000_000_000 if self.stamp == 'old' else
                     self.fixed if self.stamp == 'fixed' else 0)
            odom = Odometry()
            odom.header.frame_id, odom.child_frame_id = 'odom', 'base_link'
            odom.header.stamp.sec, odom.header.stamp.nanosec = divmod(stamp, 1_000_000_000)
            odom.pose.pose.position.x = self.x
            odom.pose.pose.orientation.w = 1.0
            self.odom.publish(odom)
            cloud = PointCloud2()
            cloud.header.frame_id = 'base_link'
            cloud.header.stamp = odom.header.stamp
            cloud.height, cloud.point_step = 1, 12
            cloud.fields = [PointField(name=n, offset=i*4, datatype=PointField.FLOAT32, count=1)
                            for i, n in enumerate(('x', 'y', 'z'))]
            if self.cloud == 'full':
                points = [(2*math.cos(-math.pi+i*math.tau/72),
                           2*math.sin(-math.pi+i*math.tau/72), 0.0) for i in range(72)]
            elif self.cloud == 'front':
                points = [(2.0, 0.0, 0.0)]
            elif self.cloud == 'rear':
                points = [(-2.0, 0.0, 0.0)]
            else:
                points = [(0.2, 0.0, 0.0)]
            cloud.width, cloud.row_step = len(points), len(points)*12
            cloud.is_bigendian, cloud.is_dense = False, True
            cloud.data = b''.join(struct.pack('<fff', *point) for point in points)
            self.points.publish(cloud)
        rclpy.spin_once(self.node, timeout_sec=.01)

    def wait(self, predicate, timeout, marker):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            self.tick()
            if predicate():
                return
        raise AssertionError(marker + ': ' + json.dumps(self.state))

    def observe(self, duration, predicate, marker):
        deadline = time.monotonic() + duration
        while time.monotonic() < deadline:
            self.tick()
            assert predicate(), marker

    def ready(self):
        # Discovery and a status callback establish actual bidirectional DDS
        # before sensor assertions; only then begin the selected stimulus.
        self.publish = False
        self.wait(lambda: self.odom.get_subscription_count() == 1 and
                  self.points.get_subscription_count() == 1 and self.state is not None and
                  self.start.service_is_ready(), 15, 'PATROL_DDS_DISCOVERY')
        self.publish = True

    def moving(self, marker):
        start = len(self.commands)
        self.wait(lambda: len(self.commands) > start and self.commands[-1][1]['x'] == .55 and self.state['active'],
                  4, marker)
        assert self.state['active'], marker + ': no active status'

    def stopped(self, reason, marker):
        start = len(self.commands)
        self.wait(lambda: self.state and not self.state['active'] and
                  (reason in self.state['reason'] or reason in str(self.state['sensor_errors'])) and
                  len(self.commands) > start and self.commands[-1][1]['x'] == 0, 3, marker)
        start = len(self.commands)
        self.observe(.45, lambda: all(c['x'] == 0 for _, c in self.commands[start:]), marker)

    def restart(self):
        self.wait(lambda: self.state['readiness_error'] is None, 3, 'PATROL_REACQUIRE')
        assert not self.state['active'], 'Patrol restarted without explicit service request'
        future = self.start.call_async(Trigger.Request())
        self.wait(future.done, 3, 'PATROL_START_SERVICE')
        assert future.result().success, future.result().message

    def close(self):
        error = None
        try:
            if self.child is not None:
                if self.child.poll() is None:
                    self.child.send_signal(signal.SIGINT)
                try:
                    code = self.child.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    self.child.kill()
                    self.child.wait(timeout=5)
                    raise AssertionError('PATROL_CHILD_DRAIN: SIGINT did not stop application')
                if code != 0:
                    error = AssertionError('PATROL_CHILD_EXIT: ' + str(code))
        finally:
            self.node.destroy_node()
            if self.log:
                self.log.close()
        if error:
            raise error


def exercise(source, case, output):
    flags = {'strict': [], 'gaps': ['--allow-scan-gaps'], 'arrival': ['--ignore-capture-age'],
             'route': ['--allow-scan-gaps', '--ignore-capture-age'],
             'invalid': ['--allow-scan-gaps', '--ignore-capture-age']}[case]
    peer = Journey(source, case, output, flags)
    try:
        peer.ready()
        if case == 'strict':
            peer.stamp = 'old'
            peer.stopped('capture_too_old', 'PATROL_STRICT_AGE')
            assert not any(c['x'] for _, c in peer.commands), 'strict age admitted velocity'
            peer.stamp, peer.cloud = 'current', 'front'
            peer.stopped('unknown_scan', 'PATROL_STRICT_GAPS')
            assert not any(c['x'] for _, c in peer.commands), 'strict gaps admitted velocity'
            peer.cloud = 'full'
            peer.moving('PATROL_FULL_CLOUD_FORWARD')
            peer.publish = False
            peer.stopped('stale_', 'PATROL_OBSERVATION_TIMEOUT')
        elif case == 'gaps':
            peer.cloud = 'front'
            peer.moving('PATROL_GAPS_FLAG_FORWARD')
            peer.cloud = 'rear'
            peer.stopped('unknown_scan', 'PATROL_GAPS_REQUIRE_FRONT')
            peer.cloud = 'front'
            peer.restart()
            peer.moving('PATROL_EXPLICIT_RECOVERY')
            peer.cloud = 'obstacle'
            peer.stopped('obstacle', 'PATROL_MEASURED_OBSTACLE')
        elif case == 'arrival':
            peer.stamp = 'old'
            peer.moving('PATROL_ARRIVAL_FLAG_FORWARD')
            peer.fixed = time.time_ns() - 60_000_000_000
            peer.stamp = 'fixed'
            peer.stopped('capture_not_advancing', 'PATROL_ARRIVAL_REJECTS_REPEAT')
        elif case == 'invalid':
            peer.stamp, peer.cloud = 'zero', 'front'
            peer.stopped('nonpositive_capture_stamp', 'PATROL_ARRIVAL_REJECTS_ZERO')
            assert not any(c['x'] for _, c in peer.commands), 'PATROL_ARRIVAL_REJECTS_ZERO'
            peer.stamp = 'old'
            peer.moving('PATROL_VALID_AFTER_ZERO')
        else:
            peer.stamp, peer.cloud, peer.integrate = 'old', 'front', True
            peer.moving('PATROL_BOTH_FLAGS_FORWARD')
            peer.wait(lambda: peer.state['state'] == 'complete' and peer.state['reason'] == 'route_complete',
                      5, 'PATROL_KINEMATIC_ROUTE_COMPLETE')
            assert .7 <= peer.x <= 1.1, ('kinematic displacement', peer.x)
            start = len(peer.commands)
            peer.observe(.45, lambda: all(c['x'] == 0 for _, c in peer.commands[start:]), 'PATROL_ROUTE_STOP')
        return {'case': case, 'commands': len(peer.commands), 'final_status': peer.state,
                'kinematic_x': peer.x, 'physical_robot': False}
    except AssertionError as error:
        (output / 'failure-state.json').write_text(json.dumps({
            'assertion': str(error), 'state': peer.state, 'commands': peer.commands,
            'subscriptions': [peer.odom.get_subscription_count(), peer.points.get_subscription_count()]}, indent=2) + '\n')
        raise
    finally:
        peer.close()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--source', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--case', choices=['strict', 'gaps', 'arrival', 'route', 'invalid'])
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=True)
    evidence = {'status': 'failed', 'cases': [], 'physical_robot': False}
    rclpy.init()
    try:
        for case in [args.case] if args.case else ['strict', 'gaps', 'arrival', 'route', 'invalid']:
            evidence['cases'].append(exercise(args.source, case, args.output))
        evidence['status'] = 'passed'
        print('PATROL_ROS_TRANSPORT_VERIFIED', flush=True)
    except AssertionError as error:
        evidence['assertion'] = str(error)
        raise
    finally:
        rclpy.try_shutdown()
        (args.output / 'result.json').write_text(json.dumps(evidence, indent=2) + '\n')


if __name__ == '__main__':
    main()
