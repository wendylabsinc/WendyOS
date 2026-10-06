"""Command ownership, watchdog and one clock for browser and ROS observations."""

import os
import secrets
import threading
import time

from .simulation import Simulation

COMMAND_TIMEOUT = 0.3
MODEL_BUNDLE = "r2-ackermann-kinematic-v1"


class Runtime:
    def __init__(self):
        self.lock = threading.RLock()
        self.sim = Simulation()
        self.epoch = 1
        self.paused = False
        self.owner = None
        self.control_mode = "ros"
        self.armed_ns = time.time_ns()
        self.sequence = -1
        self.deadline = 0.0
        self.error = None
        self.ros = None
        self.thread = None
        self.closed = threading.Event()
        self.scan = self.sim.scan()
        self.camera_frame = None
        self.camera_frames = 0
        self.camera_thread = None
        self.capture_ns = self.scan_ns = time.time_ns()

    def revoke(self, mode="none"):
        self.sim.stop()
        self.owner = None
        self.control_mode = mode
        self.armed_ns = time.time_ns()
        self.sequence = -1
        self.deadline = 0.0

    def arm(self, mode):
        if mode not in {"browser", "ros", "app"}:
            raise ValueError("unknown control mode")
        if self.paused or self.error:
            raise ValueError("resume a healthy simulator before enabling controls")
        if mode == "ros" and self.ros is None:
            raise ValueError("ROS is not enabled in this runtime")
        self.revoke(mode)
        if mode == "browser":
            self.owner = secrets.token_urlsafe(24)
        return {"token": self.owner, "epoch": self.epoch}

    def claim_app(self):
        if self.paused or self.error or self.control_mode not in {"app", "ros"} or self.owner:
            raise ValueError("select Use app in the simulator to release the current controller")
        self.revoke("app")
        self.owner = secrets.token_urlsafe(24)
        return {"token": self.owner, "epoch": self.epoch}

    def app_command(self, body):
        if self.paused or self.error or self.control_mode != "app" or not self.owner or body.get("token") != self.owner:
            raise ValueError("app control revoked; select Use app in the simulator to rearm")
        sequence = body.get("sequence")
        if type(sequence) is not int or sequence <= self.sequence:
            raise ValueError("app command sequence is stale")
        self.sim.drive(body.get("speed"), body.get("steering"))
        if body.get("stop") is True:
            self.sim.stop()
        self.sequence = sequence
        self.deadline = time.monotonic() + COMMAND_TIMEOUT

    def command(self, body):
        if self.paused or self.error or self.control_mode != "browser" or not self.owner or body.get("token") != self.owner:
            raise ValueError("enable browser controls before driving")
        sequence = body.get("sequence")
        if type(sequence) is not int or sequence <= self.sequence:
            raise ValueError("command sequence is stale")
        self.sim.drive(body.get("speed"), body.get("steering"))
        self.sequence = sequence
        self.deadline = time.monotonic() + COMMAND_TIMEOUT

    def ros_command(self, msg, info):
        with self.lock:
            if self.paused or self.error or self.control_mode != "ros":
                return
            now = time.time_ns()
            stamp = info.source_timestamp
            if stamp <= self.armed_ns or stamp > now + 50_000_000 or now - stamp > COMMAND_TIMEOUT * 1e9:
                return
            owner = bytes(info.publisher_gid).hex()
            if self.owner not in (None, owner) or stamp <= self.sequence:
                return
            try:
                self.sim.twist(msg.linear.x, msg.linear.y, msg.angular.z)
            except ValueError:
                if self.owner == owner:
                    self.sim.stop()
                return
            self.owner = owner
            self.sequence = stamp
            self.deadline = time.monotonic() + COMMAND_TIMEOUT

    def reset(self):
        self.revoke()
        self.sim.reset()
        self.paused = False
        self.epoch += 1
        self.scan = self.sim.scan()
        self.camera_frame = None
        self.capture_ns = self.scan_ns = time.time_ns()

    def tick(self, dt, now=None):
        with self.lock:
            if self.paused:
                return
            if (time.monotonic() if now is None else now) > self.deadline:
                self.sim.stop()
            self.sim.step(dt)
            self.capture_ns = time.time_ns()

    def status(self):
        with self.lock:
            alive = self.thread is not None and self.thread.is_alive() and not self.closed.is_set()
            healthy = alive and self.error is None
            return {"vm_name": os.getenv("R2_VM_NAME", ""), "simulation": True,
                    "profile_version": 1, "robot_kind": "rosmaster-r2",
                    "source_digest": os.getenv("R2_SOURCE_DIGEST", "development"),
                    "policy_bundle": MODEL_BUNDLE, "world": "indoor", "clock_mode": "device",
                    "seed": int(os.getenv("R2_SEED", "0")),
                    "visual_detail": os.getenv("R2_VISUAL_DETAIL", "balanced"),
                    "dds_isolation": os.getenv("R2_ISOLATION_STATUS", "standalone"),
                    "healthy": healthy, "ready": healthy and self.camera_frame is not None, "error": self.error or "",
                    "mode": "paused" if self.paused else "driving" if abs(self.sim.speed) > .001 else "idle",
                    "epoch": self.epoch, "control_mode": self.control_mode,
                    "owner": self.control_mode if self.owner else None,
                    "ros_enabled": self.ros is not None, "state": self.sim.state(),
                    "camera": {"frames": self.camera_frames, "width": 320, "height": 240,
                               "capture_ns": self.camera_frame["capture_ns"] if self.camera_frame else 0},
                    "capture_ns": self.capture_ns, "scan_ns": self.scan_ns}

    def run(self):
        previous = time.monotonic()
        next_scan = next_ros = previous
        scan_pending = False
        try:
            while not self.closed.wait(.005):
                now = time.monotonic()
                dt, previous = min(now - previous, .05), now
                if self.ros is not None:
                    self.ros.spin_once()
                self.tick(dt)
                with self.lock:
                    if self.paused:
                        continue
                    scan_due = now >= next_scan
                    if scan_due:
                        self.scan = self.sim.scan()
                        self.scan_ns = self.capture_ns
                        next_scan = max(next_scan + .1, now)
                        scan_pending = True
                    if self.ros is not None and now >= next_ros:
                        self.ros.publish(self.sim.state(), self.capture_ns, self.scan if scan_pending else None)
                        scan_pending = False
                        next_ros = max(next_ros + .02, now)
        except Exception as exc:
            with self.lock:
                self.error = str(exc)
                self.revoke()

    def run_camera(self):
        try:
            from .camera import render, jpeg
            while not self.closed.is_set():
                started = time.monotonic()
                with self.lock:
                    state = self.sim.state()
                    epoch, captured, paused = self.epoch, self.capture_ns, self.paused
                    obstacles = self.sim.obstacles
                if not paused:
                    color, depth, preview = render(state, obstacles)
                    frame = {"color": jpeg(color), "depth": jpeg(preview),
                             "rgb": color.tobytes(), "depth_raw": depth.tobytes(),
                             "capture_ns": captured, "epoch": epoch}
                    with self.lock:
                        if self.epoch == epoch and not self.paused:
                            self.camera_frame = frame
                            self.camera_frames += 1
                self.closed.wait(max(.001, .1 - (time.monotonic()-started)))
        except Exception as exc:
            with self.lock:
                self.error = "camera: " + str(exc)
                self.revoke()

    def start(self):
        if os.getenv("R2_ROS") == "1":
            from .ros import Bridge
            self.ros = Bridge(self)
        self.thread = threading.Thread(target=self.run, daemon=True, name="r2-simulation")
        self.thread.start()
        self.camera_thread = threading.Thread(target=self.run_camera, daemon=True, name="r2-camera")
        self.camera_thread.start()

    def close(self):
        self.closed.set()
        if self.thread:
            self.thread.join(timeout=3)
        if self.camera_thread:
            self.camera_thread.join(timeout=3)
        if self.ros:
            self.ros.close()
