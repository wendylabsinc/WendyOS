"""Run the native Patrol app against an explicitly identified isolated Go2 simulator.

The test uses a 0.5 m square away from the default box. Run inside the Patrol
image, sharing the simulator container's network namespace:
  python3 /test/patrol_smoke.py --simulator-url http://127.0.0.1:8890
Mount this file at /test/patrol_smoke.py. The application is imported from /app.
"""
import argparse
import json
import math
import sys
import time
import urllib.request
import urllib.error


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--simulator-url', required=True)
    args = parser.parse_args()

    def simulator():
        with urllib.request.urlopen(args.simulator_url.rstrip('/') + '/api/status', timeout=3) as response:
            status = json.load(response)
        if status.get('simulation') is not True or status.get('robot') != 'go2':
            raise RuntimeError('This motion test requires a Go2 simulator')
        return status

    deadline = time.monotonic() + 20
    while True:
        try:
            initial = simulator()  # Identify before creating any command publisher.
            if initial['ready']:
                break
        except urllib.error.URLError:
            pass
        if time.monotonic() >= deadline:
            raise RuntimeError('Simulator did not become ready')
        time.sleep(0.1)
    sys.path.insert(0, '/app')
    import rclpy
    from ros_app import make_node

    rclpy.init()
    node = make_node(((0.5,0), (0.5,-0.5), (0,-0.5), (0,0)), autostart=True)
    started = time.monotonic()
    origin = initial['position'][:2]
    visited = set()
    try:
        while time.monotonic() - started < 60:
            rclpy.spin_once(node, timeout_sec=0.02)
            visited.add(node.controller.index)
            if not node.autostart_pending and not node.controller.active:
                if node.controller.state != 'complete':
                    raise AssertionError(json.dumps(node.controller.status(time.monotonic())))
                break
        else:
            raise AssertionError('Patrol did not complete within 60 seconds')
        elapsed = time.monotonic() - started
        odom_error_at_completion = node.controller.distance
        if not {0, 1, 2, 3, 4} <= visited:
            raise AssertionError('Patrol did not visit every waypoint')
        if node.drive.get_subscription_count() < 1:
            raise AssertionError('The native command publisher has no receiver')
        deadline = time.monotonic() + 2
        while time.monotonic() < deadline:
            rclpy.spin_once(node, timeout_sec=0.02)
            assert not node.controller.active and node.controller.command == (0.0, 0.0)
        final = simulator()
        assert final['ros_commands']['accepted'] > 0
        assert all(source['kind'] == 'sport' for source in final['ros_commands']['sources'])
        assert final['command'] == [0.0, 0.0, 0.0]
        assert max(map(abs, final['applied_command'])) < 0.01
        print(json.dumps({'passed': True, 'seconds': elapsed,
                          'return_error_m': math.dist(origin, final['position'][:2]),
                          'arrival_tolerance_m': node.controller.ARRIVAL_DISTANCE,
                          'odom_error_at_completion_m': odom_error_at_completion,
                          'odom_error_after_hold_m': math.dist(node.controller.pose[:2], node.controller.targets[-1]),
                          'native_commands_accepted': final['ros_commands']['accepted'],
                          'post_completion_zero_seconds': 2}), flush=True)
    finally:
        node.stop_controller('test_complete')
        node.destroy_node()
        rclpy.shutdown()


if __name__ == '__main__':
    main()
