"""Verify a deployed app using two live observations and its ROS camera route."""

import argparse
import json
import time
from urllib.request import urlopen


def verify(url, vm, seconds):
    def fetch(path):
        with urlopen(url.rstrip("/") + path, timeout=10) as response:
            return response.read()

    first = json.loads(fetch("/healthz"))
    time.sleep(seconds)
    last = json.loads(fetch("/healthz"))
    assert first["ready"] and last["ready"], "Core sensor data is not fresh"
    assert last["simulator"]["simulation"] is True
    assert last["simulator"]["vm_name"] == vm
    counts = {}
    for key in ("joints", "imu", "odom", "camera", "camera_info"):
        stream = last["topics"][key]
        delta = stream["count"] - first["topics"][key]["count"]
        assert delta > 0 and stream["fresh"], f"No new live {key} messages"
        counts[key] = {"new_messages": delta, "reported_hz": stream["hz"],
                       "source_age_ms": round(stream["source_age_seconds"] * 1000, 2)}
    jpeg = fetch("/camera.jpg")
    assert jpeg.startswith(b"\xff\xd8") and jpeg.endswith(b"\xff\xd9"), "Invalid JPEG"
    assert not last["policy"]["running"] and not last["policy"]["compatible"]
    return {"passed": True, "vm": vm, "window_seconds": seconds,
            "app": last["app"], "version": last["version"],
            "simulator_source_digest": last["simulator"]["source_digest"],
            "streams": counts, "camera": last["topics"]["camera"]["value"],
            "camera_jpeg_bytes": len(jpeg),
            "observed_joint_count": last["policy"]["observed_joint_count"],
            "policy_running": last["policy"]["running"]}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", default="http://127.0.0.1:8891")
    parser.add_argument("--vm", default="g1-sim")
    parser.add_argument("--seconds", type=float, default=5)
    args = parser.parse_args()
    if not 0 < args.seconds <= 60:
        parser.error("--seconds must be between 0 and 60")
    print(json.dumps(verify(args.url, args.vm, args.seconds), indent=2))
