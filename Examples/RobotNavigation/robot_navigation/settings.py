"""Deployment configuration is local; MCP callers cannot change motion limits."""
import json
import math
import os
from pathlib import Path
import re


_NAME = re.compile(r"/[A-Za-z_][A-Za-z0-9_/]*")


def validate_settings(data):
    from .runtime import RuntimeConfig
    from .guard import GuardConfig
    from .motor import MotorConfig
    expected = {"runtime", "guard", "topics", "motor", "database", "start_nav2", "start_detector"}
    if not isinstance(data, dict) or set(data) != expected:
        raise ValueError(f"configuration must contain exactly {sorted(expected)}")
    for name in ("start_nav2", "start_detector"):
        if type(data[name]) is not bool:
            raise ValueError(f"{name} must be a boolean")
    required_topics = {"scan", "odometry", "imu", "rgb", "depth", "camera_info", "detections"}
    if not isinstance(data["topics"], dict) or set(data["topics"]) != required_topics:
        raise ValueError("all seven sensor topic mappings are required")
    for topic in data["topics"].values():
        if not isinstance(topic, str) or not _NAME.fullmatch(topic):
            raise ValueError("topics must be absolute ROS names")
    raw = data["runtime"]
    if not isinstance(raw, dict) or {"sensors", "allowed_postures"} & raw.keys():
        raise ValueError("required sensors and posture checks cannot be overridden")
    cfg = RuntimeConfig(**raw)
    for key in ("motion_enabled", "watchdog_commissioned"):
        if type(raw.get(key)) is not bool:
            raise ValueError(f"runtime.{key} must be an explicit boolean")
    for key in ("navigation_frame", "odometry_frame", "base_frame"):
        if not isinstance(raw.get(key), str) or not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_/]*", raw[key]):
            raise ValueError(f"runtime.{key} must be an explicit ROS frame")
    for key in ("max_linear_speed", "max_angular_speed"):
        value = raw.get(key)
        if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value) or value <= 0:
            raise ValueError(f"runtime.{key} must be positive and finite")
    guard = GuardConfig(**data["guard"], base_frame=cfg.base_frame,
                        max_linear=cfg.max_linear_speed, max_angular=cfg.max_angular_speed,
                        commissioned=cfg.motion_enabled and cfg.watchdog_commissioned)
    motor = MotorConfig.from_settings(data)
    if motor.output_topic.startswith("/robot_navigation/") or motor.output_topic in data["topics"].values():
        raise ValueError("motor output must be separate from navigation and sensor inputs")
    # Account for detection, downstream command expiry and a bounded RPC, in
    # addition to the measured braking deceleration configured on this device.
    if guard.reaction_seconds < max(guard.sensor_timeout, guard.permit_timeout, guard.command_timeout) + motor.command_timeout + .1:
        raise ValueError("reaction_seconds must cover guard timeout + motor timeout + 0.1s delivery")
    if cfg.motion_enabled and (motor.mode == "disabled" or not data["start_nav2"]):
        raise ValueError("motion requires a motor backend and Nav2 managed by this app")
    if not isinstance(data["database"], str) or not Path(data["database"]).is_absolute():
        raise ValueError("database must be an absolute path on persistent storage")
    return data


def load_settings():
    path = Path(os.environ.get("ROBOT_NAVIGATION_CONFIG", "/app/config.json"))
    return validate_settings(json.loads(path.read_text()))
