"""Exercise real DDS delivery and the live app in an identified hallway simulator."""
import argparse
import json
from pathlib import Path
import time
from urllib.request import urlopen


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    args.output.write_text(json.dumps({"passed": False, "stage": "running", "started_at": time.time()}) + "\n")
    def status():
        with urlopen("http://127.0.0.1:8895/api/status", timeout=3) as response:
            value = json.load(response)
        assert value.get("simulation") is True and value.get("robot") == "go2" and value.get("hallway")
        return value
    deadline = time.monotonic()+30
    while True:
        try:
            initial = status()
            if initial["ready"]:
                break
        except OSError:
            pass
        if time.monotonic() > deadline:
            raise RuntimeError("Hallway simulator did not become ready")
        time.sleep(.1)
    import rclpy
    from ros_app import make_node
    rclpy.init()
    node = make_node(autostart=True, max_seconds=45, ignore_capture_age=True)
    started = time.monotonic()
    try:
        while time.monotonic()-started < 55:
            rclpy.spin_once(node, timeout_sec=.02)
            if not node.autostart_pending and not node.app.controller.active:
                assert node.app.controller.reason == "blocked_or_dead_end", node.app.status(time.monotonic())
                break
        else:
            raise AssertionError("Hallway route did not finish")
        hold = time.monotonic()+1
        while time.monotonic() < hold:
            rclpy.spin_once(node, timeout_sec=.02)
        final = status()
        assert final["hallway"]["wall_contacts"] == 0
        assert final["ros_commands"]["accepted"] > 0
        assert final["command"] == [0, 0, 0]
        assert node.app.controller.turns == 1
        assert final["position"][1] > 2
        result = {"passed": True, "scope": "MuJoCo hallway with real ROS DDS and native Move requests",
                  "elapsed_s": time.monotonic()-started, "hallway": final["hallway"],
                  "position": final["position"], "controller": node.app.status(time.monotonic()),
                  "native_commands_accepted": final["ros_commands"]["accepted"]}
        args.output.write_text(json.dumps(result, indent=2, allow_nan=False)+"\n")
        print(json.dumps(result, allow_nan=False), flush=True)
    finally:
        node.app.controller.stop("test_complete")
        node.publish(0, 0)
        node.destroy_node()
        rclpy.shutdown()


if __name__ == "__main__":
    main()
