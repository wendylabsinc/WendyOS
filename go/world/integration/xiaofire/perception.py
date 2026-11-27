"""Recognize a synthetic flame cue in original SensorLink JPEGs.

This is a fixture-specific color/shape rule, not a trained fire detector or a
physical fire alarm. Only decoded pixels determine the candidate. Synthetic
camera calibration projects its base onto an explicitly configured floor.
"""
from collections import deque
import hashlib
import io
import json
import math
import re

from PIL import Image


MODEL = "synthetic-flame-cue-v1"
MAX_JPEG = 8 << 20


def finite(value):
    return type(value) in (int, float) and math.isfinite(value)


def label(value):
    return isinstance(value, str) and 0 < len(value) <= 256 and not any(c in value for c in "\x00\r\n")


def simulated_pinhole_contract(width, height):
    """Declare the renderer's full-frame JPEG optics; never infer a real lens."""
    return {"projection_model": "pinhole", "distortion_model": "none", "distortion": [],
            "capture_mode": f"MJPEG {width}x{height}", "image_crop": [0, 0, width, height],
            "rectified": False, "calibration_provenance": "simulated_model"}


def validate_calibration(calibration, run_id, dimensions):
    c = calibration
    if not isinstance(c, dict) or c.get("schema") != "wendy.xiao-fire-camera.v1" or c.get("origin") != "simulated" or c.get("run_id") != run_id:
        raise ValueError("explicit simulated calibration for this run is required")
    if (c.get("width"), c.get("height")) != dimensions or any(type(c.get(k)) is not int for k in ("width", "height")):
        raise ValueError("calibration image dimensions differ")
    for key, expected in simulated_pinhole_contract(*dimensions).items():
        if key not in c or type(c[key]) is not type(expected) or c[key] != expected:
            raise ValueError("unsupported or ambiguous simulated camera contract: " + key)
    if not all(label(c.get(k)) for k in ("revision", "frame_id", "frame_revision")):
        raise ValueError("calibration and coordinate frame revisions are required")
    if not all(finite(c.get(k)) for k in ("fx", "fy", "cx", "cy", "floor_y_m")):
        raise ValueError("calibration values must be finite")
    width, height = dimensions
    if not (0 < c["fx"] <= width * 16 and 0 < c["fy"] <= height * 16 and 0 <= c["cx"] <= width and 0 <= c["cy"] <= height):
        raise ValueError("invalid optical intrinsics")
    m = c.get("camera_to_world")
    if not isinstance(m, list) or len(m) != 16 or not all(finite(v) for v in m) or any(abs(m[12 + i] - v) > 1e-6 for i, v in enumerate((0, 0, 0, 1))):
        raise ValueError("camera_to_world must be a finite optical rigid transform")
    r = [m[i:i+3] for i in (0, 4, 8)]
    for i in range(3):
        for j in range(3):
            if abs(sum(r[i][k] * r[j][k] for k in range(3)) - (1 if i == j else 0)) > 1e-4:
                raise ValueError("camera rotation must be orthonormal")
    determinant = (r[0][0] * (r[1][1]*r[2][2] - r[1][2]*r[2][1])
                   - r[0][1] * (r[1][0]*r[2][2] - r[1][2]*r[2][0])
                   + r[0][2] * (r[1][0]*r[2][1] - r[1][1]*r[2][0]))
    if determinant < .999 or m[7] <= c["floor_y_m"]:
        raise ValueError("camera must have a proper rotation above the World Y-up floor")
    return hashlib.sha256(json.dumps(c, sort_keys=True, separators=(",", ":"), allow_nan=False).encode()).hexdigest()


def floor_point(pixel, calibration):
    """Intersect an optical ray with the declared World Y-up floor plane."""
    c = calibration
    optical = [(pixel[0] - c["cx"]) / c["fx"], (pixel[1] - c["cy"]) / c["fy"], 1.]
    m = c["camera_to_world"]
    origin = [m[3], m[7], m[11]]
    ray = [sum(m[i*4+j] * optical[j] for j in range(3)) for i in range(3)]
    if ray[1] >= -1e-6:
        raise ValueError("detected cue base does not project toward the floor")
    distance = (c["floor_y_m"] - origin[1]) / ray[1]
    if distance <= 0 or distance * math.sqrt(sum(v*v for v in ray)) > 50:
        raise ValueError("floor intersection is outside the bounded camera range")
    return [origin[i] + distance * ray[i] for i in range(3)]


def pixel_candidate(image):
    """Connected warm silhouette, yellow core and upper taper from pixels only."""
    rgb = image.convert("RGB")
    rgb.thumbnail((384, 384), Image.Resampling.BILINEAR)
    width, height = rgb.size
    raw = rgb.tobytes()
    pixels = list(zip(raw[0::3], raw[1::3], raw[2::3]))
    warm = bytearray(len(pixels))
    yellow = bytearray(len(pixels))
    red = bytearray(len(pixels))
    for i, (r, g, b) in enumerate(pixels):
        warm[i] = r >= 135 and g >= 30 and r >= .95*g and r >= 1.8*b and g >= 1.5*b
        yellow[i] = warm[i] and r >= 170 and g >= 135 and g >= .62*r and b < .6*g
        red[i] = warm[i] and r >= 1.25*g
    candidates = []
    for start in range(len(warm)):
        if not warm[start]:
            continue
        warm[start] = 0
        todo, component = deque([start]), []
        while todo:
            i = todo.popleft()
            component.append(i)
            x, y = i % width, i // width
            for j in ((i-1 if x else -1), (i+1 if x+1 < width else -1),
                      (i-width if y else -1), (i+width if y+1 < height else -1)):
                if j >= 0 and warm[j]:
                    warm[j] = 0
                    todo.append(j)
        if not 30 <= len(component) <= width*height*.35:
            continue
        xs, ys = [i % width for i in component], [i // width for i in component]
        left, right, top, bottom = min(xs), max(xs), min(ys), max(ys)
        bw, bh = right-left+1, bottom-top+1
        core_fraction = sum(yellow[i] for i in component) / len(component)
        red_fraction = sum(red[i] for i in component) / len(component)
        if bw < 4 or bh < 8 or not 1.15 <= bh/bw <= 7 or not .05 <= core_fraction <= .75 or red_fraction < .15:
            continue
        spans = {}
        for x, y in zip(xs, ys):
            lo, hi = spans.get(y, (x, x))
            spans[y] = min(lo, x), max(hi, x)
        upper = [hi-lo+1 for y, (lo, hi) in spans.items() if y < top+bh/3]
        lower = [hi-lo+1 for y, (lo, hi) in spans.items() if top+bh/3 <= y < top+5*bh/6]
        taper = sum(upper)/len(upper) / (sum(lower)/len(lower))
        if taper > .8:
            continue
        base_x = [x for x, y in zip(xs, ys) if y >= bottom-max(1, bh//30)]
        scale_x, scale_y = image.width / width, image.height / height
        score = min(1., .45 + .35*(1-taper) + .2*min(core_fraction/.2, 1))
        candidates.append({"bbox_pixels": [left*scale_x, top*scale_y, (right+1)*scale_x, (bottom+1)*scale_y],
                           "base_pixel": [(sum(base_x)/len(base_x)+.5)*scale_x, (bottom+1)*scale_y],
                           "cue_score": score, "yellow_fraction": core_fraction,
                           "red_fraction": red_fraction, "upper_width_ratio": taper,
                           "segmentation_pixels": len(component)})
    return max(candidates, key=lambda c: c["cue_score"], default=None)


class Detector:
    """Require fresh, distinct, advancing pixel observations before confirmation."""
    def __init__(self, run_id, max_age_s=2.0, required_frames=3):
        if not label(run_id) or not finite(max_age_s) or not 0 < max_age_s <= 10 or type(required_frames) is not int or not 1 <= required_frames <= 10:
            raise ValueError("invalid detector run or bounds")
        self.run_id, self.max_age_s, self.required_frames = run_id, max_age_s, required_frames
        self.session, self.sequence, self.timestamp, self.received = None, None, None, None
        self.channel = None
        self.retired, self.confirmations = set(), []
        self.last_position, self.calibration_sha = None, None

    def detect(self, jpeg_bytes, frame_row, calibration, received_monotonic, now_monotonic):
        r = frame_row
        if not isinstance(r, dict) or not label(r.get("session_id")) or not isinstance(r.get("sha256"), str) or not re.fullmatch("[0-9a-f]{64}", r["sha256"]):
            raise ValueError("original firmware frame metadata is required")
        for key, maximum in (("channel_id", 2**32-1), ("frame_seq", 2**32-1), ("timestamp_us", 2**64-1), ("width", 8192), ("height", 8192)):
            if type(r.get(key)) is not int or not 0 <= r[key] <= maximum:
                raise ValueError("invalid firmware frame metadata: " + key)
        if r["width"]*r["height"] == 0 or r["width"]*r["height"] > 16 << 20:
            raise ValueError("invalid frame dimensions")
        if self.channel is not None and r["channel_id"] != self.channel:
            raise ValueError("one detector cannot combine different camera channels")
        if not isinstance(jpeg_bytes, bytes) or not 4 <= len(jpeg_bytes) <= MAX_JPEG or hashlib.sha256(jpeg_bytes).hexdigest() != r["sha256"]:
            raise ValueError("original JPEG hash or size differs")
        if not all(finite(v) for v in (received_monotonic, now_monotonic)) or not 0 <= received_monotonic <= now_monotonic:
            raise ValueError("local receipt times must use one advancing monotonic clock")
        calibration_sha = validate_calibration(calibration, self.run_id, (r["width"], r["height"]))
        evidence = {k: r[k] for k in ("session_id", "channel_id", "frame_seq", "timestamp_us", "sha256", "width", "height")}
        evidence.update(origin="simulated", simulation_run=self.run_id,
                        timestamp_clock="firmware-boot-microseconds", local_age_s=now_monotonic-received_monotonic,
                        received_monotonic_s=received_monotonic, expires_monotonic_s=received_monotonic+self.max_age_s)
        result = {"schema": "wendy.xiao-fire-detection.v1", "origin": "inferred", "simulation_run": self.run_id,
                  "model": MODEL, "score_kind": "fixture_rule_not_probability", "detected": False, "candidate": False,
                  "reason": "no_flame_cue", "confirmation_frames": 0, "source": evidence,
                  "calibration_sha256": calibration_sha, "calibration_revision": calibration["revision"],
                  "evaluated_monotonic_s": now_monotonic,
                  "frame": {"id": calibration["frame_id"], "revision": calibration["frame_revision"]},
                  "spatial_assumption": "synthetic flame base lies on configured World Y-up floor plane"}
        if now_monotonic-received_monotonic >= self.max_age_s:
            self.confirmations = []
            return dict(result, reason="expired_frame")
        if r["session_id"] in self.retired:
            return dict(result, reason="retired_session")
        if self.session == r["session_id"]:
            delta = (r["frame_seq"] - self.sequence) % (2**32)
            if delta == 0 or delta >= 2**31 or r["timestamp_us"] <= self.timestamp or received_monotonic < self.received:
                return dict(result, reason="repeated_or_out_of_order_frame")
            if delta != 1 or received_monotonic-self.received > self.max_age_s:
                self.confirmations = []
        else:
            if self.session is not None:
                if len(self.retired) >= 32:
                    raise ValueError("session history full; create a new run")
                self.retired.add(self.session)
            self.confirmations = []
        if not jpeg_bytes.startswith(b"\xff\xd8") or not jpeg_bytes.endswith(b"\xff\xd9"):
            raise ValueError("complete original JPEG required")
        with Image.open(io.BytesIO(jpeg_bytes)) as image:
            if image.format != "JPEG" or image.size != (r["width"], r["height"]):
                raise ValueError("decoded JPEG dimensions or format differ")
            image.load()
            candidate = pixel_candidate(image)
        self.session, self.sequence, self.timestamp, self.received = r["session_id"], r["frame_seq"], r["timestamp_us"], received_monotonic
        self.channel = r["channel_id"]
        if candidate is None:
            self.confirmations = []
            return result
        try:
            position = floor_point(candidate["base_pixel"], calibration)
        except ValueError:
            self.confirmations = []
            return dict(result, reason="floor_projection_unavailable")
        if self.calibration_sha != calibration_sha or self.last_position is None or math.dist(position, self.last_position) > .75:
            self.confirmations = []
        self.calibration_sha, self.last_position = calibration_sha, position
        self.confirmations = [i for i in self.confirmations if i["expires_monotonic_s"] > now_monotonic]
        self.confirmations = (self.confirmations + [evidence])[-self.required_frames:]
        detected = len(self.confirmations) >= self.required_frames
        return dict(result, **candidate, position_m=position, candidate=True, detected=detected,
                    reason="confirmed_synthetic_fire_cue" if detected else "awaiting_distinct_frames",
                    confirmation_frames=len(self.confirmations), confirming_inputs=list(self.confirmations),
                    expires_monotonic_s=min(i["expires_monotonic_s"] for i in self.confirmations))
