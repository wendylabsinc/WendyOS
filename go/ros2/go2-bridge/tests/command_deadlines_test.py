"""Real ROS callbacks -> unchanged bridge -> production Go Agent -> Pion peer.

The parent Go test owns a protocol robot with delayed acknowledgements. This
checks executor/transport timing; it does not establish physical posture safety.
"""
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time
import urllib.request

import rclpy
from geometry_msgs.msg import Twist
from nav_msgs.msg import Odometry
from unitree_api.msg import Request, Response


def main():
    rclpy.init()
    node = rclpy.create_node('go2_deadline_check')
    responses = {}
    odometry = []
    node.create_subscription(Odometry, '/odom', lambda msg: odometry.append(msg.pose.pose.position.x), 10)
    node.create_subscription(Response, '/api/sport/response',
                             lambda msg: responses.__setitem__(msg.header.identity.id,
                                 (time.monotonic(), msg.header.status.code)), 10)
    sport = node.create_publisher(Request, '/api/sport/request', 10)
    velocity = node.create_publisher(Twist, '/cmd_vel', 10)
    bridge = subprocess.Popen([sys.executable, '-m', 'go2_bridge.node'])
    evidence = {'status': 'failed', 'physical_robot': False, 'checks': {}}

    def wait(predicate, timeout, description):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if bridge.poll() is not None:
                raise AssertionError('bridge exited before '+description)
            if predicate():
                return
            rclpy.spin_once(node, timeout_sec=0.01)
        raise AssertionError(description)

    def events():
        with urllib.request.urlopen(os.environ['GO2_FIXTURE_CONTROL']+'/events', timeout=1) as response:
            return json.load(response)

    def policy(hello_ms=1200, stop_ms=0):
        request = urllib.request.Request(os.environ['GO2_FIXTURE_CONTROL']+'/policy',
            data=json.dumps(dict(hello_ms=hello_ms, stop_ms=stop_ms)).encode(), method='POST')
        with urllib.request.urlopen(request, timeout=1):
            pass

    def request(identity, api, parameter=''):
        message = Request()
        message.header.identity.id, message.header.identity.api_id = identity, api
        message.parameter = parameter
        sport.publish(message)

    def move(speed):
        twist = Twist()
        twist.linear.x = speed
        velocity.publish(twist)

    def wire(api, start=0):
        return [event for event in events()[start:] if event['api'] == api]

    try:
        wait(lambda: sport.get_subscription_count() == 1 and velocity.get_subscription_count() == 1,
             15, 'ROS subscriptions did not become ready')
        # Delayed posture completion must not serialize the ROS executor or
        # keep a stop behind the posture acknowledgement in the Agent/peer.
        policy()
        start = len(events())
        request(101, 1016)
        wait(lambda: wire(1016, start), 2, 'Hello did not reach the Pion peer')
        hello_wire = wire(1016, start)[0]
        begun = time.monotonic()
        data_before = len(odometry)
        request(102, 1009)  # unsupported: local callback needs no Agent RPC
        move(0.6)
        wait(lambda: 102 in responses and wire(1008, start) and len(set(odometry[data_before:])) >= 3, 0.5,
             'delayed posture acknowledgement blocked concurrent ROS callbacks')
        assert responses[102][1] == 3203, responses[102]
        assert 101 not in responses, 'posture delay was not actually exercised'
        request(103, 1003)
        wait(lambda: 103 in responses, 0.5, 'stop waited behind posture acknowledgement')
        assert responses[103][1] == 0, responses[103]
        assert 101 not in responses, 'stop completed only after posture acknowledgement'
        evidence['checks']['concurrent_callbacks_seconds'] = responses[102][0]-begun
        evidence['checks']['stop_during_posture_seconds'] = responses[103][0]-begun
        wait(lambda: 101 in responses, 2, 'delayed posture acknowledgement did not finish')
        assert responses[101][1] == 0, responses[101]
        acknowledged = next(e for e in events() if e['id'] == hello_wire['id'])
        evidence['checks']['posture_ack_delay_seconds'] = acknowledged['ack_at']-acknowledged['at']
        assert evidence['checks']['posture_ack_delay_seconds'] >= 1.1, 'posture acknowledgement was not delayed'
        assert len(set(odometry[data_before:])) >= 3, 'concurrent robot data was stale'
        evidence['checks']['fresh_odometry_callbacks'] = len(set(odometry[data_before:]))

        # No velocity refresh: the production 500ms lease must send StopMove,
        # not merely return success to the ROS publisher.
        start = len(events())
        move(0.55)
        wait(lambda: wire(1008, start), 1, 'lease move never reached peer')
        moving = wire(1008, start)[0]
        assert moving['type'] == 'msg' and moving['noreply'], moving
        wait(lambda: wire(1003, start), 1.2, 'move lease did not stop on the actual data channel')
        stopped = wire(1003, start)[0]
        elapsed = stopped['at']-moving['at']
        assert 0.40 <= elapsed <= 1.0, ('move lease deadline', elapsed)
        evidence['checks']['move_lease_seconds'] = elapsed

        # Robot replies after the actual Agent stop deadline. That late reply
        # must not convert the timed-out command into success or stall ROS for
        # the bridge's much larger one-second gRPC timeout.
        policy(stop_ms=900)
        start = len(events())
        begun = time.monotonic()
        request(104, 1003)
        wait(lambda: wire(1003, start), 0.5, 'deadline stop did not reach peer')
        request(105, 1009)
        wait(lambda: 104 in responses and 105 in responses, 0.65,
             'stop acknowledgement deadline blocked the executor')
        elapsed = responses[104][0]-begun
        assert responses[104][1] == 3205, ('stop timeout response', responses[104])
        assert 0.15 <= elapsed <= 0.65, ('stop deadline', elapsed)
        assert responses[105][1] == 3203, responses[105]
        evidence['checks']['stop_timeout_seconds'] = elapsed
        # Prove the connection remains usable and late IDs cannot satisfy a
        # different command. This uses the same bridge, Agent and Pion link.
        policy(hello_ms=1100, stop_ms=0)
        recovery_started = time.monotonic()
        request(106, 1016)
        wait(lambda: 106 in responses, 1.7, 'transport did not recover after stop timeout')
        assert responses[106][1] == 0, responses[106]
        assert responses[106][0]-recovery_started >= 0.9, 'late stop acknowledgement satisfied a different command'
        delayed_stop = wire(1003, start)[0]
        assert delayed_stop['ack_at']-delayed_stop['at'] >= 0.85, 'late stop acknowledgement was not sent'
        evidence['events'] = events()
        evidence['status'] = 'passed'
        print('PASS: real ROS callbacks, delayed Pion acknowledgements, Agent stop and move lease deadlines', flush=True)
    finally:
        if bridge.poll() is None:
            bridge.send_signal(signal.SIGINT)
            try:
                bridge.wait(timeout=5)
            except subprocess.TimeoutExpired:
                bridge.kill()
                bridge.wait(timeout=5)
                evidence['cleanup_error'] = 'bridge did not stop after SIGINT'
                evidence['status'] = 'failed'
        node.destroy_node()
        rclpy.try_shutdown()
        target = os.environ.get('GO2_DEADLINE_EVIDENCE')
        if target:
            Path(target).write_text(json.dumps(evidence, indent=2)+'\n')
        if evidence.get('cleanup_error'):
            raise AssertionError(evidence['cleanup_error'])


if __name__ == '__main__':
    main()
