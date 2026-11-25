"""Simulation bridge state machine; transport is injected for deterministic tests."""
import copy
import hashlib
import math
import secrets
import re
import threading
import time
from .protocol import Refused, fields, integer, number, digest, token, encoded, ODOMETRY_MODELS


def _copy_validated_capture(c):
    """Detach a capture after _cloud has checked its exact scalar/list shape."""
    if "cloud" not in c:
        return copy.deepcopy(c)
    out = copy.deepcopy({key: value for key, value in c.items() if key != "cloud"})
    # Only ranges has a strict plain-number/None contract. Preserve deepcopy
    # semantics for all metadata, including accepted string subclasses.
    out["cloud"] = copy.deepcopy({key: value for key, value in c["cloud"].items() if key != "ranges"})
    out["cloud"]["ranges"] = c["cloud"]["ranges"][:]
    return out


class Clock:
    def __init__(self, wall=time.time_ns, mono=time.monotonic_ns):
        self.wall, self.mono = wall, mono
        self.last = None
        self.offset = None
        self.faulted = False
        self.lock = threading.Lock()

    def read(self):
        with self.lock:
            if self.faulted:
                raise Refused("clock_fault_latched")
            m0, w, m1 = self.mono(), self.wall(), self.mono()
            bad = m0 < 0 or m1 < m0 or w <= 0 or m1-m0 > 5_000_000
            if self.last is not None:
                bad |= m0 < self.last[0] or w < self.last[1]
            offset = w-m0
            if self.offset is not None:
                bad |= abs(offset-self.offset) > 5_000_000
            if bad:
                self.faulted = True
                raise Refused("clock_continuity")
            self.offset = offset if self.offset is None else self.offset
            self.last = (m1, w)
            return w, m1

class Bridge:
    """One explicit session and task per process. Faults never renew ownership."""
    def __init__(self, config, transport, clock=None, fault_reporter=None):
        self.cfg, self.transport = config, transport
        self.clock = clock or Clock()
        self.boot_id = secrets.token_hex(16)
        self.session = None
        self.sequence = 0
        self.begin_request = None
        self.fault = "sensor_startup_rejected" if getattr(transport,"startup_fault","")=="sensor_startup_rejected" else ""
        self.fault_reported = False
        self.fault_reporter = fault_reporter or (lambda value: print(encoded(value).decode(),flush=True))
        self.started = False
        self.used_capture = 0
        self.served_captures = {}
        self.served_cloud_originals = {}
        self.previous = [0.0, 0.0, 0.0]
        self.last_source = 0
        self.deadline = None
        self.pins = None
        self.project_epoch = None
        self.runtime_time = None
        self.lock = threading.RLock()

    def _zero(self):
        # Fixed once before serialization, even for zero. This is best effort;
        # the receiver's own receipt watchdog remains the independent fence.
        stamp = time.time_ns()
        try:
            stamp, _ = self.clock.read()
        except Refused:
            pass
        try:
            self.transport.publish([0.0, 0.0, 0.0], stamp)
        except Exception:
            return False
        self.last_source = max(self.last_source, stamp)
        return True

    def _latch(self, reason):
        self.fault = self.fault or reason
        if not self.fault_reported:
            self.fault_reported = True
            code = self.fault if re.fullmatch(r"[a-z_]{1,64}",self.fault) else "bridge_unavailable"
            try:self.fault_reporter({"event":"bridge_fault","simulation":True,"code":code})
            except Exception:pass  # Diagnostics must never prevent a stop.

    def fail(self, reason):
        self._latch(reason)
        self.deadline = None
        self.previous = [0.0, 0.0, 0.0]
        self._zero()
        raise Refused(self.fault)

    def _clock(self):
        try:
            return self.clock.read()
        except Refused:
            self.fail("clock_fault")

    def _runtime(self, require_owner):
        try:
            s = self.transport.status()
            b = self.cfg.binding
            required = {"simulation":True,"robot":"go2","robot_kind":"go2","profile_version":1,"vm_name":b["vm_name"],"source_digest":b["source_digest"],"dds_isolation":"udp-rtps-loopback","policy_bundle":b["policy_bundle"],"clock_mode":"device","epoch":b["world_epoch"],"healthy":True}
            if any(s.get(k) != v or type(s.get(k)) is not type(v) for k,v in required.items()):
                raise Refused("runtime_identity")
            if s.get("error") is not None or "error" not in s or s.get("mode") not in ("standing", "moving") or s.get("ready") is not True:
                raise Refused("runtime_not_ready")
            sim_time = number(s["time"], 0)
            if sim_time > self.cfg.profile["odometry_max_epoch_age_s"]:
                raise Refused("odometry_epoch_age_exceeded")
            if self.runtime_time is not None and sim_time < self.runtime_time:
                raise Refused("runtime_time_regression")
            self.runtime_time = sim_time
            p = s["project"]
            if p.get("project_id") != b["project_id"] or p.get("revision") != b["project_revision"] or "error" not in p or p["error"] is not None:
                raise Refused("runtime_project")
            pe = integer(p["epoch"], 1, b["world_epoch"])
            if self.project_epoch is not None and pe != self.project_epoch:
                raise Refused("runtime_project_epoch")
            self.project_epoch = pe
            c = s["ros_commands"]
            if c.get("auto_control") is not False:
                raise Refused("automatic_control_unsupported")
            if require_owner and c.get("owner") != self.transport.publisher_id():
                raise Refused("runtime_owner_lost")
            if not require_owner and c.get("owner") is not None:
                raise Refused("runtime_already_owned")
            return s
        except Exception as exc:
            self.fail(str(exc) if isinstance(exc, Refused) else "runtime_unavailable")

    def _capture(self):
        try:
            c = self.transport.capture()
            wall, _ = self._clock()
            p = self.cfg.profile
            integer(c["capture_sequence"], 1)
            cloud = self._cloud(c)
            stamp_keys = ("odom_captured_unix_ns", "scan_captured_unix_ns") + (("cloud_captured_unix_ns",) if cloud is not None else ())
            stamps = [integer(c[k], 1) for k in stamp_keys]
            if any(wall-t < 0 or wall-t >= p["observation_max_age_ms"]*1_000_000 for t in stamps):
                raise Refused("sensor_stale")
            if abs(stamps[0]-stamps[1]) > p["source_skew_max_ms"]*1_000_000:
                raise Refused("sensor_pair_skew")
            identities = (c["odom_publisher"], c["scan_publisher"], c["mount_publisher"])
            if cloud is not None:
                identities += (cloud["publisher_gid"],cloud["epoch_publisher"],cloud["cloud_mount_publisher"])
            if not all(isinstance(x, str) and len(x)==48 and all(ch in "0123456789abcdef" for ch in x) for x in identities):
                raise Refused("sensor_identity")
            if self.pins is not None and identities != self.pins:
                raise Refused("sensor_identity_changed")
            self.pins = identities
            if c["world_epoch"] != self.cfg.binding["world_epoch"]:
                raise Refused("sensor_epoch_changed")
            # Cloud validation and copying must not renew original freshness.
            if cloud is not None:self._original_capture_fresh(c)
            return c
        except Exception as exc:
            self.fail(str(exc) if isinstance(exc, Refused) else "sensor_unavailable")

    def _cloud(self,c):
        enabled="cloud_experiment" in self.cfg.profile
        if not enabled:
            if "cloud" in c:raise Refused("cloud_profile_mismatch")
            return None
        value=c.get("cloud")
        names=("model","source_digest","frame","publisher_gid","serialized_sha256","captured_unix_ns","ranges","sequence","observed_epoch","scan_sha256","odom_sha256","epoch_sha256","mount_sha256","cloud_mount_sha256","epoch_publisher","mount_publisher","cloud_mount_publisher")
        try:
            fields(value,names)
            if value["model"]!=self.cfg.profile["cloud_experiment"]["model"] or value["source_digest"]!=self.cfg.binding["source_digest"] or value["frame"]!="utlidar_lidar":raise Refused("cloud_capture_invalid")
            if integer(value["sequence"],1)!=c["capture_sequence"] or integer(value["observed_epoch"],1)!=c["world_epoch"]:raise Refused("cloud_capture_invalid")
            stamp=integer(value["captured_unix_ns"],1)
            if stamp!=c["scan_captured_unix_ns"] or stamp!=c["cloud_captured_unix_ns"]:raise Refused("cloud_capture_invalid")
            for key in ("serialized_sha256","scan_sha256","odom_sha256","epoch_sha256","mount_sha256","cloud_mount_sha256"):digest(value[key])
            for key in ("publisher_gid","epoch_publisher","mount_publisher","cloud_mount_publisher"):
                if type(value[key]) is not str or not re.fullmatch("[0-9a-f]{48}",value[key]) or value[key]=="0"*48:raise Refused("cloud_capture_invalid")
            if value["publisher_gid"]!=c["cloud_publisher"] or value["mount_publisher"]!=c["mount_publisher"] or value["cloud_mount_publisher"]!=c["cloud_mount_publisher"] or value["mount_publisher"]!=value["cloud_mount_publisher"]:raise Refused("cloud_capture_invalid")
            if type(value["ranges"]) is not list or len(value["ranges"])!=9000:raise Refused("cloud_capture_invalid")
            for x in value["ranges"]:
                if x is not None:number(x,0,12.000002)
            return value
        except (ValueError,KeyError,TypeError,OverflowError):
            raise Refused("cloud_capture_invalid") from None

    def _original_capture_fresh(self, c):
        wall, _ = self._clock()
        limit = self.cfg.profile["observation_max_age_ms"]*1_000_000
        keys=("odom_captured_unix_ns","scan_captured_unix_ns") + (("cloud_captured_unix_ns",) if "cloud_experiment" in self.cfg.profile else ())
        if any(wall-c[k] < 0 or wall-c[k] >= limit for k in keys):
            raise Refused("served_capture_stale")

    def _state(self):
        self._clock()
        c = self._capture()
        sequence = c["capture_sequence"]
        if "cloud_experiment" in self.cfg.profile and sequence in self.served_captures and c!=self.served_captures[sequence]:
            self.fail("cloud_capture_changed")
        if sequence not in self.served_captures:
            if "cloud_experiment" in self.cfg.profile:
                from ..go2_cloud_stream import Candidate, Sample
                from ..go2_cloud_observer import TOPICS
                original=self.transport.original_capture(sequence)
                if type(original) is not Candidate or type(original.transforms) is not tuple or len(original.transforms)!=2 or original.source_digest!=self.cfg.binding["source_digest"] or original.observed_epoch!=c["world_epoch"]:
                    self.fail("cloud_evidence_unavailable")
                cloud=c["cloud"]
                refs=((original.cloud,"/utlidar/cloud","serialized_sha256",cloud["publisher_gid"]),
                      (original.scan,"/scan","scan_sha256",c["scan_publisher"]),
                      (original.odom,"/odom","odom_sha256",c["odom_publisher"]),
                      (original.epoch_sample,"/simulation/epoch","epoch_sha256",cloud["epoch_publisher"]),
                      (original.transforms[0],"/tf_static","mount_sha256",cloud["mount_publisher"]),
                      (original.transforms[1],"/tf_static","cloud_mount_sha256",cloud["cloud_mount_publisher"]))
                try:
                    for sample,topic,key,publisher in refs:
                        if (type(sample) is not Sample or sample.topic!=topic or
                                sample.publisher_gid!=publisher or sample.serialized_sha256!=cloud[key] or
                                type(sample.raw_cdr) is not bytes or not 0<len(sample.raw_cdr)<=TOPICS[topic] or
                                hashlib.sha256(sample.raw_cdr).hexdigest()!=sample.serialized_sha256):
                            raise Refused("cloud_evidence_unavailable")
                        integer(sample.source_timestamp_ns,1)
                        for value in (sample.received_timestamp_ns,sample.helper_elapsed_ns,sample.arrived_ns):integer(value)
                    integer(original.capture_id,1);integer(original.assembled_at_ns);integer(original.companion_deadline_ns,1)
                    if (original.cloud.value.capture_unix_ns!=cloud["captured_unix_ns"] or
                            original.scan.value["capture_unix_ns"]!=c["scan_captured_unix_ns"] or
                            original.odom.value["capture_unix_ns"]!=c["odom_captured_unix_ns"] or
                            original.epoch_sample.value["epoch"]!=c["world_epoch"]):
                        raise Refused("cloud_evidence_unavailable")
                except (ValueError,KeyError,TypeError,AttributeError,OverflowError):
                    self.fail("cloud_evidence_unavailable")
                self.served_cloud_originals[sequence]=original
            self.served_captures[sequence] = _copy_validated_capture(c)
            while len(self.served_captures)>8:
                oldest=min(self.served_captures)
                del self.served_captures[oldest]
                self.served_cloud_originals.pop(oldest,None)
        c = _copy_validated_capture(self.served_captures[sequence])
        # The status read follows the retained capture. Its full epoch age
        # conservatively includes drift before this bridge ever started.
        age = self._runtime(True)["time"]
        self._original_capture_fresh(c)
        a = self.cfg.alignment
        scan = copy.deepcopy(c["scan"])
        tilt = c.get("tilt", 0.0)
        velocity_bias, yaw_bias = ODOMETRY_MODELS[self.cfg.binding["source_digest"]]
        position_error = a["position_uncertainty_m"] + self.cfg.profile["odometry_integration_allowance_m"] + velocity_bias*age
        yaw_error = a["yaw_uncertainty_rad"] + yaw_bias*age
        if self.cfg.profile["schema"]=="wendy.world.go2-profile.v2":
            t=self.cfg.profile["tilted_scan"]
            pose=copy.deepcopy(c.get("tilted_pose"))
            fields(pose,("quaternion_wxyz","odom_position_xyz","mount_translation_xyz","mount_quaternion_wxyz"))
            for key,size in (("quaternion_wxyz",4),("odom_position_xyz",3),("mount_translation_xyz",3),("mount_quaternion_wxyz",4)):
                if type(pose[key]) is not list or len(pose[key])!=size:raise Refused("tilted_pose_shape")
                for v in pose[key]:number(v,-100,100)
            q=pose["quaternion_wxyz"]
            if abs(sum(v*v for v in q)-1)>1e-9 or pose["mount_translation_xyz"]!=[.28,0.,.08] or pose["mount_quaternion_wxyz"]!=[1.,0.,0.,0.]:raise Refused("tilted_pose_transform")
            w,x,y,z=q
            observed_yaw=math.atan2(2*(w*z+x*y),1-2*(y*y+z*z))
            observed_roll=math.atan2(2*(w*x+y*z),1-2*(x*x+y*y))
            observed_pitch=math.asin(max(-1.,min(1.,2*(w*y-z*x))))
            if pose["odom_position_xyz"][:2]!=[c["pose"]["x"],c["pose"]["y"]] or abs(math.atan2(math.sin(observed_yaw-c["pose"]["yaw"]),math.cos(observed_yaw-c["pose"]["yaw"])))>1e-9:raise Refused("tilted_pose_consistency")
            if abs(observed_roll)>self.cfg.profile["max_abs_roll_rad"] or abs(observed_pitch)>self.cfg.profile["max_abs_pitch_rad"]:raise Refused("unsupported_body_tilt")
            skew=abs(c["scan_captured_unix_ns"]-c["odom_captured_unix_ns"])/1_000_000_000
            translation=t["max_translation_m_per_wall_s"]*skew
            attitude=t["max_rotation_rad_per_wall_s"]*skew
            height_error=t["height_calibration_error_m"]+self.cfg.profile["odometry_integration_allowance_m"]+velocity_bias*age+translation
            height=pose["odom_position_xyz"][2]-t["odom_floor_z_m"]
            low,high=height-height_error,height+height_error
            if low<t["base_height_min_m"] or high>t["base_height_max_m"]:raise Refused("tilted_height_outside_commissioning")
            if position_error+translation>self.cfg.profile["max_position_uncertainty_m"] or yaw_error+t["attitude_error_rad"]+attitude>.5:raise Refused("tilted_pair_uncertainty_exceeded")
            pose.update(odom_captured_unix_ns=c["odom_captured_unix_ns"],pair_translation_bound_m=translation,pair_attitude_bound_rad=attitude,odometry_epoch_age_s=age,base_height_min_m=low,base_height_max_m=high)
            scan["tilted_pose"]=pose
        else:
            position_error += scan["range_max"]*(1-math.cos(tilt)) + abs(c.get("mount_height",0.0))*math.sin(tilt)
            yaw_error += tilt
        if position_error > self.cfg.profile["max_position_uncertainty_m"] or yaw_error > self.cfg.profile["max_yaw_uncertainty_rad"]:
            self.fail("planar_uncertainty_exceeded")
        scan.update(sequence=c["capture_sequence"], captured_unix_ns=c["scan_captured_unix_ns"], world_epoch=c["world_epoch"], frame="odom", robot=c["pose"], mount_known=True, position_uncertainty_m=position_error, yaw_uncertainty_rad=yaw_error, uncertainty_known=True)
        out=dict(bridge_boot_id=self.boot_id,session=self.session,sequence=self.sequence,executor_epoch=0 if self.begin_request is None else self.begin_request["executor_epoch"],binding=self.cfg.binding,odometry_epoch_age_s=age,capture_sequence=c["capture_sequence"],sample_unix_ms=min(c["odom_captured_unix_ns"], c["scan_captured_unix_ns"])//1_000_000,odom_captured_unix_ns=c["odom_captured_unix_ns"],scan_captured_unix_ns=c["scan_captured_unix_ns"],odom_publisher=c["odom_publisher"],scan_publisher=c["scan_publisher"],command_publisher=self.transport.publisher_id(),owner=self.transport.publisher_id(),auto_control=False,synthetic=True,ready=not bool(self.fault),fault=self.fault,pose=c["pose"],roll_rad=c.get("roll_rad",0.0),pitch_rad=c.get("pitch_rad",0.0),body_velocity=c["body_velocity"],previous_command=self.previous[:],scan=scan,pose_covariance=c["pose_covariance"],twist_covariance=c["twist_covariance"])
        if "cloud_experiment" in self.cfg.profile:
            out["cloud"]=c["cloud"]
            self._original_capture_fresh(c)
        return out

    def dispatch(self, route, body):
        if route=="/v1/cloud-evidence":
            # A read of an immutable served original, including after a fault.
            # It does not refresh sensor time or participate in dispatch.
            fields(body,("capture_sequence",));sequence=integer(body["capture_sequence"],1)
            with self.lock:
                original=self.served_cloud_originals.get(sequence)
            if original is None:raise Refused("cloud_evidence_unavailable")
            samples=[]
            for role,sample in (("epoch",original.epoch_sample),("cloud",original.cloud),("scan",original.scan),("odom",original.odom),("mount",original.transforms[0]),("cloud_mount",original.transforms[1])):
                samples.append(dict(role=role,topic=sample.topic,publisher_gid=sample.publisher_gid,source_timestamp_ns=sample.source_timestamp_ns,received_timestamp_ns=sample.received_timestamp_ns,helper_elapsed_ns=sample.helper_elapsed_ns,arrived_monotonic_ns=sample.arrived_ns,serialized_sha256=sample.serialized_sha256,payload_hex=sample.raw_cdr.hex()))
            out=dict(schema="wendy.world.go2-served-cloud-evidence.v1",simulation=True,operational_authority=False,capture_sequence=sequence,candidate_id=original.capture_id,source_digest=original.source_digest,observed_epoch=original.observed_epoch,assembled_monotonic_ns=original.assembled_at_ns,companion_deadline_ns=original.companion_deadline_ns,samples=samples)
            if len(encoded(out))>512<<10:raise Refused("cloud_evidence_unavailable")
            return out
        if route=="/v1/diagnostics":
            # The HTTP handler already checks the local bearer token. This
            # immutable first-rejection copy needs no runtime/sensor I/O or
            # bridge lock and can never issue an accepted observation.
            fields(body,())
            sensors=getattr(self.transport,"sensors",None)
            return {"schema":"wendy.world.go2-bridge-diagnostics.v1","simulation":True,"operational_authority":False,"rejected_sensor":None if sensors is None else sensors.rejected_sample()}
        with self.lock:
            try:
                if route == "/v1/stop":
                    fields(body, ("session", "sequence"))
                    self._session(body["session"])
                    integer(body["sequence"], self.sequence+1)
                    self.sequence = body["sequence"]
                    self._latch("explicit_stop")
                    self.deadline = None
                    self.previous = [0.0, 0.0, 0.0]
                    self._zero()
                    return {"accepted":True}
                if self.fault:
                    raise Refused(self.fault)
                if route == "/v1/session":
                    fields(body, ("binding",))
                    if self.started or body["binding"] != self.cfg.binding:
                        raise Refused("session_already_started_or_binding")
                    self.started = True
                    self.session = secrets.token_hex(16)
                    self._clock()
                    self._runtime(False)
                    self._capture()
                    # Discover using zero only. Transport waits at most one second
                    # for this exact middleware GID, then performs one arm POST.
                    self._zero()
                    self.transport.arm_once()
                    self._runtime(True)
                    self._zero()
                    return self._state()
                if route == "/v1/begin":
                    fields(body, ("session", "executor_epoch", "task_id", "spec_digest", "binding_sha256"))
                    self._session(body["session"])
                    if self.begin_request is not None or body["binding_sha256"] != self.cfg.binding_sha256:
                        raise Refused("begin_binding_or_repeat")
                    integer(body["executor_epoch"], 1); token(body["task_id"]); digest(body["spec_digest"])
                    self._state()
                    self.begin_request = copy.deepcopy(body)
                    return {"accepted":True}
                if route == "/v1/state":
                    fields(body, ("session",)); self._session(body["session"])
                    return self._state()
                if route == "/v1/command":
                    return self._command(body)
                raise Refused("unknown_route")
            except Refused:
                if self.started and route != "/v1/stop" and not self.fault:
                    self._latch("request_refused")
                    self.deadline = None
                    self._zero()
                raise

    def _session(self, session):
        if not self.started or not self.session or session != self.session:
            raise Refused("session_mismatch")

    def _command(self, body):
        fields(body, ("session","sequence","executor_epoch","task_id","spec_digest","capture_sequence","body_vx","body_vy","yaw_rate","not_after_unix_ms","decision_remaining_ms"))
        self._session(body["session"])
        if self.begin_request is None or any(body[k] != self.begin_request[k] for k in ("executor_epoch","task_id","spec_digest")):
            raise Refused("command_task_identity")
        integer(body["sequence"], self.sequence+1)
        capture = integer(body["capture_sequence"], self.used_capture+1)
        p = self.cfg.profile
        values = [number(body["body_vx"],0,p["max_forward_mps"]),number(body["body_vy"],0,0),number(body["yaw_rate"],-p["max_yaw_rate_rps"],p["max_yaw_rate_rps"])]
        if values[0] != 0 and values[2] != 0:
            raise Refused("combined_axis_command")
        remaining = integer(body["decision_remaining_ms"],1,p["decision_max_ms"])
        expires = integer(body["not_after_unix_ms"],1)*1_000_000
        wall, mono = self._clock()
        # Relative remaining can only tighten the absolute deadline. It never
        # replaces the sender's deadline after HTTP transport delay.
        fixed_wall = min(expires, wall+remaining*1_000_000)
        fixed_mono = mono+max(0, fixed_wall-wall)
        source = min(wall, fixed_wall-p["expiry_lead_ms"]*1_000_000)
        if source <= self.last_source:
            raise Refused("command_source_nonadvancing")
        if wall-source >= 200_000_000 or fixed_wall <= wall:
            raise Refused("command_budget_exhausted")
        # A newer healthy frame does not invalidate an already served frame.
        # Its original pose/rays/timestamps remain immutable and one-use.
        original = self.served_captures.get(capture)
        if original is None:
            raise Refused("capture_not_served")
        self._state()  # Current runtime, identities, epoch and tilt still checked.
        self._original_capture_fresh(original)
        wall, mono = self._clock()
        if wall >= fixed_wall or mono >= fixed_mono or wall-source >= 200_000_000:
            raise Refused("command_budget_exhausted")
        self.used_capture = capture
        self.sequence = body["sequence"]
        self.last_source = source
        self.deadline = (fixed_wall, fixed_mono)
        # Source is immutable across this call, including a suspension inside
        # serialization or DDS write. No positive command is ever retried.
        try:
            self.transport.publish(values, source)
        except Exception:
            self.fail("publication_outcome_unknown")
        self.previous = values
        wall, mono = self._clock()
        if wall >= fixed_wall or mono >= fixed_mono:
            self.fail("publication_completed_late")
        return {"accepted":True,"source_timestamp_ns":source,"not_after_unix_ms":body["not_after_unix_ms"],"sequence":self.sequence}

    def watchdog(self):
        # This thread is supplementary. It cannot preempt the DDS library or an
        # HTTP handler. The independent stock receipt watchdog and fixed source
        # timestamp are what prevent delayed writes renewing command validity.
        deadline = self.deadline
        if deadline is None:
            return
        try:
            wall, mono = self.clock.read()
            expired = wall >= deadline[0] or mono >= deadline[1]
        except Refused:
            expired = True
        if expired:
            with self.lock:
                if self.deadline == deadline:
                    self._latch("decision_expired")
                    self.deadline = None
                    self.previous = [0.0,0.0,0.0]
                    self._zero()
