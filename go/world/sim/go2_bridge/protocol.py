"""Bounded protocol and immutable commissioned configuration."""
import hashlib
import json
import math
import os
import re
import stat
from urllib.parse import urlsplit

class Refused(ValueError):
    pass

def strict(raw, limit=65536):
    if not isinstance(raw, bytes) or not raw or len(raw) > limit:
        raise Refused("json_size")
    def pairs(items):
        result = {}
        for key, value in items:
            if key in result:
                raise Refused("duplicate_json_field")
            result[key] = value
        return result
    def bad(_):
        raise Refused("nonfinite_json")
    try:
        return json.loads(raw.decode("utf-8"), object_pairs_hook=pairs, parse_constant=bad)
    except (UnicodeError, ValueError, RecursionError) as exc:
        raise Refused("invalid_json") from exc

def encoded(value):
    return json.dumps(value, separators=(",", ":"), ensure_ascii=False, allow_nan=False).encode()

def fields(value, expected):
    if not isinstance(value, dict) or set(value) != set(expected) or any(v is None for v in value.values()):
        raise Refused("protocol_fields")
    return value

def integer(value, low=0, high=(1 << 63)-1):
    if type(value) is not int or not low <= value <= high:
        raise Refused("integer_bound")
    return value

def number(value, low=-1e6, high=1e6):
    if type(value) not in (int, float) or not math.isfinite(value) or not low <= value <= high:
        raise Refused("number_bound")
    return value

def digest(value):
    if not isinstance(value, str) or not re.fullmatch(r"[0-9a-f]{64}", value):
        raise Refused("digest")
    return value

def token(value):
    if not isinstance(value, str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._:-]{0,127}", value):
        raise Refused("token")
    return value

def private_file(path, limit=65536, private=False):
    flags = os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC
    fd = os.open(path, flags)
    try:
        st = os.fstat(fd)
        if not stat.S_ISREG(st.st_mode) or st.st_size > limit or (private and st.st_mode & 0o077):
            raise Refused("input_file")
        raw = b""
        while len(raw) <= limit:
            part = os.read(fd, min(16384, limit+1-len(raw)))
            if not part:
                break
            raw += part
        if len(raw) > limit:
            raise Refused("input_file_size")
        return raw
    finally:
        os.close(fd)

def endpoint(value):
    p = urlsplit(value)
    if p.scheme != "http" or p.hostname != "127.0.0.1" or p.username or p.password or p.path or p.query or p.fragment:
        raise Refused("loopback_endpoint")
    if p.port is None or not 1 <= p.port <= 65535 or value != f"http://127.0.0.1:{p.port}":
        raise Refused("loopback_endpoint")
    return value

BINDING_FIELDS = ("schema", "package_sha256", "conversion_sha256", "loss_sha256", "project_revision", "project_id", "vm_name", "source_digest", "policy_bundle", "world_epoch", "run_id", "frame", "alignment_sha256", "profile_sha256")
ALIGNMENT_FIELDS = ("schema", "source_frame", "target_frame", "translation_xy_m", "yaw_rad", "reference_point", "synthetic", "position_uncertainty_m", "yaw_uncertainty_rad")
PROFILE_FIELDS = ("schema", "flat_floor", "energy_observed", "half_length_m", "half_width_m", "clearance_m", "max_position_uncertainty_m", "max_yaw_uncertainty_rad", "max_forward_mps", "max_yaw_rate_rps", "observation_max_age_ms", "source_skew_max_ms", "decision_max_ms", "expiry_lead_ms", "horizon_s", "extra_stop_m", "extra_stop_rad", "clock_error_ms", "max_abs_roll_rad", "max_abs_pitch_rad", "vertical_obstacles", "odometry_max_epoch_age_s", "odometry_integration_allowance_m", "motion_envelope")
TILTED_FIELDS = ("model", "base_height_min_m", "base_height_max_m", "obstacle_min_height_m", "slab_margin_m", "body_half_height_m", "max_support_range_m", "range_error_m", "attitude_error_rad", "max_translation_m_per_wall_s", "max_rotation_rad_per_wall_s", "odom_floor_z_m", "height_calibration_error_m")

# Rates are specific to the audited simulator's ros.py, not generic odometry.
ODOMETRY_MODELS = {"sha256:2869d018ae55e4c83318329bb20cc670e16f6f62838989bb5192829002af8d2b": (math.hypot(.001, -.0005), .0002)}

class Configuration:
    def __init__(self, binding, alignment_raw, profile_raw):
        fields(binding, BINDING_FIELDS)
        self.binding = strict(encoded(binding))
        b = self.binding
        if b["schema"] != "wendy.world.go2-execution.v1" or b["policy_bundle"] != "go2-moe-cts-164k-0.6715-v1":
            raise Refused("simulation_binding")
        for key in ("package_sha256", "conversion_sha256", "loss_sha256", "project_revision", "alignment_sha256", "profile_sha256"):
            digest(b[key])
        if not re.fullmatch(r"[0-9a-f]{32}", b["project_id"]):
            raise Refused("project_id")
        if not isinstance(b["source_digest"], str) or not b["source_digest"].startswith("sha256:"):
            raise Refused("source_digest")
        digest(b["source_digest"][7:])
        if b["source_digest"] not in ODOMETRY_MODELS:
            raise Refused("unsupported_odometry_model")
        for key in ("vm_name", "run_id"):
            token(b[key])
        integer(b["world_epoch"], 1, (1<<53)-1)
        fields(b["frame"], ("id", "revision"))
        for v in b["frame"].values():
            token(v)
        if hashlib.sha256(alignment_raw).hexdigest() != b["alignment_sha256"] or hashlib.sha256(profile_raw).hexdigest() != b["profile_sha256"]:
            raise Refused("configuration_digest")
        self.alignment = a = fields(strict(alignment_raw, 16384), ALIGNMENT_FIELDS)
        p = strict(profile_raw, 16384)
        tilted = isinstance(p,dict) and p.get("schema") == "wendy.world.go2-profile.v2"
        lateral_explicit = isinstance(p,dict) and "max_measured_lateral_mps" in p
        cloud_explicit = isinstance(p,dict) and "cloud_experiment" in p
        self.profile = p = fields(p, PROFILE_FIELDS + (("tilted_scan",) if tilted else ()) + (("max_measured_lateral_mps",) if lateral_explicit else ()) + (("cloud_experiment",) if cloud_explicit else ()))
        if a["schema"] != "wendy.world.go2-alignment.v1" or a["source_frame"] != "odom" or a["target_frame"] != b["frame"] or a["translation_xy_m"] != [0, 0] or a["yaw_rad"] != 0 or a["reference_point"] != "base_footprint" or a["synthetic"] is not True:
            raise Refused("alignment")
        if p["schema"] not in ("wendy.world.go2-profile.v1", "wendy.world.go2-profile.v2") or p["flat_floor"] is not True or p["energy_observed"] is not False:
            raise Refused("operating_profile")
        if type(p["motion_envelope"]) is not str or p["motion_envelope"] not in ("isotropic-v1", "directional-v1", "isotropic-gait-v1"):
            raise Refused("motion_envelope")
        if p["motion_envelope"] == "isotropic-gait-v1":
            if not tilted or not lateral_explicit or number(p["max_measured_lateral_mps"],0,.2) <= 0:
                raise Refused("gait_lateral_profile")
        elif lateral_explicit and number(p["max_measured_lateral_mps"]) != 0:
            raise Refused("legacy_lateral_override")
        if cloud_explicit:
            experiment=fields(p["cloud_experiment"],("model","scope","collector_duration_ms"))
            if not tilted or p["motion_envelope"]!="isotropic-gait-v1" or experiment["model"]!="coherent-multiring-slab-v1" or experiment["scope"]!="contained-simulation":
                raise Refused("cloud_experiment_profile")
            integer(experiment["collector_duration_ms"],100,60000)
        if type(p["clock_error_ms"]) is not int or p["clock_error_ms"] != 50 or p["vertical_obstacles"] is not True:
            raise Refused("clock_or_vertical_profile")
        if tilted:
            for key in ("max_abs_roll_rad","max_abs_pitch_rad"):number(p[key],1e-9,.25)
            t=fields(p["tilted_scan"],TILTED_FIELDS)
            if t["model"]!="solid-vertical-slab-v1":raise Refused("tilted_scan_model")
            bounds={"base_height_min_m":(.15,.6),"base_height_max_m":(.15,.6),"obstacle_min_height_m":(.5,3),"slab_margin_m":(.02,.1),"body_half_height_m":(.35,.6),"max_support_range_m":(.5,8),"range_error_m":(.02,.1),"attitude_error_rad":(.001,.05),"max_translation_m_per_wall_s":(.1,2),"max_rotation_rad_per_wall_s":(.1,4),"odom_floor_z_m":(-10,10),"height_calibration_error_m":(.001,.1)}
            for key,bound in bounds.items():number(t[key],*bound)
            if t["base_height_min_m"]>=t["base_height_max_m"] or t["obstacle_min_height_m"]<=t["base_height_max_m"]+.08+t["slab_margin_m"]:
                raise Refused("tilted_scan_height_profile")
        elif number(p["max_abs_roll_rad"]) != .05 or number(p["max_abs_pitch_rad"]) != .05:
            raise Refused("clock_or_vertical_profile")
        number(p["odometry_max_epoch_age_s"], 1e-9, 120)
        if number(p["odometry_integration_allowance_m"]) != .05:
            raise Refused("odometry_allowance")
        bounds = {"half_length_m":(.425,.6), "half_width_m":(.30,.4), "clearance_m":(.02,.3), "max_position_uncertainty_m":(1e-9,.5), "max_yaw_uncertainty_rad":(1e-9,.5), "max_forward_mps":(1e-9,.35), "max_yaw_rate_rps":(1e-9,.5), "horizon_s":(1,2), "extra_stop_m":(.05,.5), "extra_stop_rad":(.05,.5)}
        for key, bound in bounds.items():
            number(p[key], *bound)
        for key, bound in {"observation_max_age_ms":(1,250),"source_skew_max_ms":(0,50),"decision_max_ms":(1,500),"expiry_lead_ms":(450,1000)}.items():
            integer(p[key], *bound)
        number(a["yaw_rad"], 0, 0)
        if type(a["translation_xy_m"]) is not list or len(a["translation_xy_m"]) != 2:
            raise Refused("alignment_translation")
        for v in a["translation_xy_m"]:
            number(v,0,0)
        number(a["position_uncertainty_m"], 0, p["max_position_uncertainty_m"])
        number(a["yaw_uncertainty_rad"], 0, p["max_yaw_uncertainty_rad"])
        # Go's struct encoding uses declared order, including its FrameRef.
        canonical = {k:b[k] for k in BINDING_FIELDS}
        canonical["frame"] = {k:b["frame"][k] for k in ("id", "revision")}
        self.binding_sha256 = hashlib.sha256(encoded(canonical)).hexdigest()
