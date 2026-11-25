"""Public DDS publication, ordinary ROS sensors, and bounded local runtime HTTP."""
import copy
import http.client
import math
import socket
import re
import threading
import time
from collections import deque
from urllib.parse import urlsplit
from .protocol import Refused, encoded, strict, endpoint, integer, number

class RuntimeHTTP:
    def __init__(self, url, failure_reporter=None):
        self.url = endpoint(url)
        self.port = urlsplit(url).port
        self.failure_reporter = failure_reporter or (lambda event: print(encoded(event).decode(), flush=True))
        self.failure_reported = False
        self.failure_lock = threading.Lock()

    def _report_failure(self, stage, exc, started, received):
        with self.failure_lock:
            if self.failure_reported:
                return
            self.failure_reported = True
        category = "unavailable"
        if isinstance(exc, (TimeoutError, socket.timeout)):
            category = "timeout"
        elif isinstance(exc, OSError):
            category = "connection"
        elif isinstance(exc, (http.client.HTTPException, Refused)):
            category = "protocol"
        elapsed = time.monotonic_ns() - started
        event = {"event":"runtime_http_failure", "simulation":True, "stage":stage,
                 "category":category, "elapsed_ms":elapsed/1_000_000 if 0 <= elapsed <= (1<<63)-1 else None,
                 "received_bytes":received}
        try:
            self.failure_reporter(event)
        except Exception:
            pass  # A diagnostic failure must not replace the original refusal.

    def call(self, method, path, body=None):
        conn = http.client.HTTPConnection("127.0.0.1", self.port, timeout=.15)
        started, stage, received = time.monotonic_ns(), "request", 0
        try:
            raw = None if body is None else encoded(body)
            conn.request(method, path, body=raw, headers={"Content-Type":"application/json", "Connection":"close"})
            stage = "headers"
            response = conn.getresponse()
            if response.status != 200 or response.getheader("Content-Encoding"):
                raise Refused("runtime_http_status")
            stage = "body"
            data = response.read((1<<20)+1)
            received = len(data)
            stage = "decode"
            return strict(data, 1<<20)
        except Exception as exc:
            if isinstance(exc, http.client.IncompleteRead):
                received = min(len(exc.partial), (1<<20)+1)
            self._report_failure(stage, exc, started, received)
            raise
        finally:
            conn.close()
    def status(self):
        return self.call("GET", "/api/status")

class DDSWriter:
    def __init__(self):
        # No rclpy publisher is used: it would assign a fresh timestamp after a
        # suspended bridge resumes. Public Cyclone DataWriter.write(timestamp=)
        # retains this original stamp across serialization and transport delay.
        from cyclonedds.idl import IdlStruct
        from cyclonedds.idl import annotations as ann
        from cyclonedds.domain import DomainParticipant
        from cyclonedds.topic import Topic
        from cyclonedds.pub import DataWriter
        from cyclonedds.core import Listener
        from cyclonedds.qos import Qos, Policy
        from cyclonedds.idl.types import float64
        # Class creation accepts the exact DDS type names through IdlStruct's
        # class keyword; use declarations for stable ROS2-compatible names.
        from dataclasses import dataclass
        @dataclass
        @ann.final
        @ann.autoid("sequential")
        class Vector3_(IdlStruct, typename="geometry_msgs.msg.dds_.Vector3_"):
            x: float64
            y: float64
            z: float64
        @dataclass
        @ann.final
        @ann.autoid("sequential")
        class Twist_(IdlStruct, typename="geometry_msgs.msg.dds_.Twist_"):
            linear: Vector3_
            angular: Vector3_
        self.Vector, self.Twist = Vector3_, Twist_
        self.participant = DomainParticipant(0)
        self.topic = Topic(self.participant, "rt/cmd_vel", Twist_)
        self.matched = 0
        def matched(_writer, status):
            self.matched = int(status.current_count)
        self.listener = Listener(on_publication_matched=matched)
        self.writer = DataWriter(self.participant, self.topic, listener=self.listener, qos=Qos(Policy.Reliability.BestEffort, Policy.Durability.Volatile, Policy.History.KeepLast(1), Policy.DataRepresentation(use_cdrv0_representation=True, use_xcdrv2_representation=False)))
    def write(self, values, source_ns):
        self.writer.write(self.Twist(self.Vector(values[0],values[1],0.),self.Vector(0.,0.,values[2])), timestamp=source_ns)
    def evidence(self):
        return {"dds_guid":str(self.writer.guid), "matched_readers":self.matched}

class Sensors:
    def __init__(self, profile=None):
        import subprocess
        import os
        self.lock = threading.Lock()
        self.profile = copy.deepcopy(profile) if profile is not None else {}
        self.rejected = None
        self.odom = deque(maxlen=64)
        self.scan = None
        self.mount = None
        self.epoch = None
        self.fault = ""
        self.identities = {}
        self.last_stamp = {}
        self.sequence = 0
        self.process=subprocess.Popen(["/opt/world/world-go2-sensors"],stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,stdin=subprocess.DEVNULL)
        self.thread=threading.Thread(target=self._consume,daemon=True)
        self.thread.start()

    def _consume(self):
        from types import SimpleNamespace
        from rclpy.serialization import deserialize_message
        from nav_msgs.msg import Odometry
        from sensor_msgs.msg import LaserScan
        from std_msgs.msg import UInt64
        from tf2_msgs.msg import TFMessage
        types={"/odom":(Odometry,self._odom),"/scan":(LaserScan,self._scan),"/simulation/epoch":(UInt64,self._epoch),"/tf_static":(TFMessage,self._mount)}
        try:
            while True:
                line=self.process.stdout.readline(100001)
                if not line or len(line)>100000 or not line.endswith(b"\n"):
                    raise Refused("sensor_helper_lost_or_overflow")
                value=strict(line,100000)
                if set(value)!={"topic","publisher_gid","source_timestamp_ns","payload_hex"} or value["topic"] not in types:
                    raise Refused("sensor_helper_schema")
                gid=value["publisher_gid"]
                if not isinstance(gid,str) or len(gid)!=48 or any(x not in "0123456789abcdef" for x in gid):
                    raise Refused("sensor_helper_gid")
                integer(value["source_timestamp_ns"],1)
                raw=value["payload_hex"]
                if not isinstance(raw,str) or len(raw)>98304 or len(raw)%2 or any(x not in "0123456789abcdef" for x in raw):
                    raise Refused("sensor_helper_payload")
                msg_type,callback=types[value["topic"]]
                msg=deserialize_message(bytes.fromhex(raw),msg_type)
                callback(msg,SimpleNamespace(publisher_gid=bytes.fromhex(gid)))
        except Exception:
            with self.lock:
                self.fault=self.fault or "sensor_helper_unavailable"

    def close(self):
        if self.process.poll() is None:
            self.process.terminate()
            try:self.process.wait(timeout=1)
            except Exception:
                self.process.kill()
                self.process.wait(timeout=1)

    def _identity(self, key, info):
        identity = bytes(info.publisher_gid).hex()
        if len(identity) != 48 or identity == "0"*48:
            raise Refused("sensor_publisher_identity")
        if key in self.identities and self.identities[key] != identity:
            raise Refused("sensor_publisher_changed")
        self.identities[key] = identity
        return identity

    def _stamp(self, key, stamp):
        ns = int(stamp.sec)*1_000_000_000+int(stamp.nanosec)
        if ns <= 0 or not 0 <= stamp.nanosec < 1_000_000_000 or ns <= self.last_stamp.get(key,0):
            raise Refused("sensor_capture_nonadvancing")
        self.last_stamp[key] = ns
        return ns

    def rejected_sample(self):
        # Published once as an immutable object. No sensor lock, I/O or logging
        # is needed to retrieve a copy, including after the sensor fault latch.
        return copy.deepcopy(getattr(self,"rejected",None))

    def _rejection(self, topic, msg, info, reason):
        out={"schema":"wendy.world.go2-rejected-sensor.v1","topic":topic,"reason":reason,
             "world_epoch":self.epoch,"publisher_gid":None,"captured_unix_ns":None,
             "quaternion_wxyz":None,"normalized_quaternion_wxyz":None,
             "odom_position_xyz":None,"roll_rad":None,"pitch_rad":None}
        try:
            gid=bytes(info.publisher_gid).hex()
            if len(gid)==48:out["publisher_gid"]=gid
            stamp=msg.header.stamp
            ns=int(stamp.sec)*1_000_000_000+int(stamp.nanosec)
            if 0<ns<(1<<63) and 0<=stamp.nanosec<1_000_000_000:out["captured_unix_ns"]=ns
            if topic=="odom":
                q=msg.pose.pose.orientation;v=[q.w,q.x,q.y,q.z]
                if all(type(x) in (int,float) and math.isfinite(x) and abs(x)<=1 for x in v):
                    out["quaternion_wxyz"]=v
                    norm=math.sqrt(sum(x*x for x in v))
                    if norm>1e-12:
                        w,x,y,z=[x/norm for x in v]
                        out["normalized_quaternion_wxyz"]=[w,x,y,z]
                        out["roll_rad"]=math.atan2(2*(w*x+y*z),1-2*(x*x+y*y))
                        out["pitch_rad"]=math.asin(max(-1.,min(1.,2*(w*y-z*x))))
                p=msg.pose.pose.position;v=[p.x,p.y,p.z]
                if all(type(x) in (int,float) and math.isfinite(x) and abs(x)<=100 for x in v):out["odom_position_xyz"]=v
        except Exception:
            pass  # Missing/malformed fields remain explicitly unknown.
        return out

    def _guard(self, callback, topic="unknown", msg=None, info=None):
        with self.lock:
            if self.fault:
                return
            try:
                callback()
            except Exception as exc:
                self.fault = str(exc) if isinstance(exc,Refused) else "sensor_malformed"
                if getattr(self,"rejected",None) is None:
                    reason=self.fault if re.fullmatch(r"[a-z_]{1,64}",self.fault) else "sensor_malformed"
                    self.rejected=self._rejection(topic,msg,info,reason)

    def _odom(self, msg, info):
        def update():
            gid = self._identity("odom",info)
            stamp = self._stamp("odom",msg.header.stamp)
            if msg.header.frame_id != "odom" or msg.child_frame_id != "base_link":
                raise Refused("odom_frame")
            q=msg.pose.pose.orientation
            vals=[number(v,-1,1) for v in (q.w,q.x,q.y,q.z)]
            if abs(sum(v*v for v in vals)-1) > .01:
                raise Refused("odom_quaternion")
            profile=getattr(self,"profile",{})
            tilted=profile.get("schema")=="wendy.world.go2-profile.v2"
            if tilted:
                norm=math.sqrt(sum(v*v for v in vals))
                vals=[v/norm for v in vals]
            w,x,y,z=vals
            roll=math.atan2(2*(w*x+y*z),1-2*(x*x+y*y))
            pitch=math.asin(max(-1.,min(1.,2*(w*y-z*x))))
            yaw=math.atan2(2*(w*z+x*y),1-2*(y*y+z*z))
            if abs(roll)>profile.get("max_abs_roll_rad",.05) or abs(pitch)>profile.get("max_abs_pitch_rad",.05):
                raise Refused("unsupported_body_tilt")
            pc=[number(float(v)) for v in msg.pose.covariance]
            tc=[number(float(v)) for v in msg.twist.covariance]
            if len(pc)!=36 or len(tc)!=36 or any(pc[i]<0 or tc[i]<0 for i in (0,7,14,21,28,35)):
                raise Refused("odom_covariance")
            pose={"x":number(msg.pose.pose.position.x,-100,100),"y":number(msg.pose.pose.position.y,-100,100),"yaw":yaw}
            position=[pose["x"],pose["y"],number(msg.pose.pose.position.z,-100,100)] if tilted else None
            velocity=[number(msg.twist.twist.linear.x,-2,2),number(msg.twist.twist.linear.y,-2,2),number(msg.twist.twist.angular.z,-4,4)]
            self.odom.append(dict(stamp=stamp,gid=gid,pose=pose,velocity=velocity,pose_covariance=pc,twist_covariance=tc,tilt=abs(roll)+abs(pitch),roll=roll,pitch=pitch,quaternion_wxyz=vals,odom_position_xyz=position))
        self._guard(update,"odom",msg,info)

    def _scan(self, msg, info):
        def update():
            gid=self._identity("scan",info)
            stamp=self._stamp("scan",msg.header.stamp)
            if msg.header.frame_id!="lidar_link" or not 180<=len(msg.ranges)<=8192:
                raise Refused("scan_frame_or_bound")
            amin=number(msg.angle_min,-math.pi-1e-6,math.pi+1e-6)
            inc=number(msg.angle_increment,1e-9,math.pi/90+1e-6)
            if abs(len(msg.ranges)*inc-2*math.pi)>1e-5:
                raise Refused("scan_coverage")
            rmin=number(msg.range_min,.001,1)
            rmax=number(msg.range_max,rmin,12)
            ranges=[]
            for v in msg.ranges:
                v=float(v)
                ranges.append(v if math.isfinite(v) and rmin<=v<=rmax else None)
            self.sequence+=1
            self.scan=dict(stamp=stamp,gid=gid,sequence=self.sequence,ranges=ranges,angle_min=amin,angle_increment=inc,range_min=rmin,range_max=rmax)
        self._guard(update,"scan",msg,info)

    def _epoch(self, msg, info):
        def update():
            self._identity("epoch",info)
            epoch=integer(int(msg.data),1)
            if self.epoch is not None and epoch!=self.epoch:
                raise Refused("sensor_epoch_changed")
            self.epoch=epoch
        self._guard(update,"epoch",msg,info)

    def _mount(self, msg, info):
        def update():
            gid=self._identity("mount",info)
            candidates=[t for t in msg.transforms if t.child_frame_id=="lidar_link"]
            if not candidates:
                return
            if len(candidates)!=1:
                raise Refused("mount_ambiguous")
            t=candidates[0]
            q=t.transform.rotation
            xyz=[number(v,-.6,.6) for v in (t.transform.translation.x,t.transform.translation.y,t.transform.translation.z)]
            if t.header.frame_id!="base_link" or [q.x,q.y,q.z,q.w]!=[0.,0.,0.,1.] or max(abs(x-y) for x,y in zip(xyz,[.28,0.,.08]))>1e-6:
                raise Refused("unsupported_lidar_mount")
            value=dict(gid=gid,pose={"x":xyz[0],"y":xyz[1],"yaw":0.},height=xyz[2],translation_xyz=xyz,quaternion_wxyz=[q.w,q.x,q.y,q.z])
            if self.mount is not None and self.mount!=value:
                raise Refused("mount_changed")
            self.mount=value
        self._guard(update,"mount",msg,info)

    def snapshot(self):
        with self.lock:
            if self.fault:
                raise Refused(self.fault)
            if not self.scan or not self.odom or not self.mount or not self.epoch:
                raise Refused("sensors_not_ready")
            s=copy.deepcopy(self.scan)
            o=copy.deepcopy(min(self.odom,key=lambda x:abs(x["stamp"]-s["stamp"])))
            result=dict(capture_sequence=s["sequence"],odom_captured_unix_ns=o["stamp"],scan_captured_unix_ns=s["stamp"],odom_publisher=o["gid"],scan_publisher=s["gid"],mount_publisher=self.mount["gid"],world_epoch=self.epoch,pose=o["pose"],body_velocity=o["velocity"],pose_covariance=o["pose_covariance"],twist_covariance=o["twist_covariance"],tilt=o["tilt"],roll_rad=o["roll"],pitch_rad=o["pitch"],mount_height=self.mount["height"],scan={k:s[k] for k in ("ranges","angle_min","angle_increment","range_min","range_max")}|{"sensor_mount":self.mount["pose"]})
            if getattr(self,"profile",{}).get("schema")=="wendy.world.go2-profile.v2":
                result["tilted_pose"]={"quaternion_wxyz":o["quaternion_wxyz"][:],"odom_position_xyz":o["odom_position_xyz"][:],"mount_translation_xyz":self.mount["translation_xyz"][:],"mount_quaternion_wxyz":self.mount["quaternion_wxyz"][:]}
            return result

class Transport:
    def __init__(self, url, sensors=True, progress=None, profile=None, binding=None, clock=None):
        self.startup_fault=""
        phase=progress or (lambda _:None)
        phase("runtime_status")
        self.http=RuntimeHTTP(url)
        initial=self.http.status()["ros_commands"]
        if initial.get("auto_control") is not False or initial.get("owner") is not None:
            raise Refused("runtime_requires_idle_manual_control")
        self.initial_sources={s["publisher_gid"] for s in initial["sources"]}
        phase("dds_writer")
        self.writer=DDSWriter()
        phase("sensor_helper")
        self.sensors=None
        if sensors:
            if profile is not None and "cloud_experiment" in profile:
                if binding is None:raise Refused("cloud_experiment_binding")
                from ..go2_cloud_live import LiveCloudSensors
                self.sensors=LiveCloudSensors(binding["source_digest"],binding["world_epoch"],profile,clock=clock)
            else:
                self.sensors=Sensors(profile=profile)
        try:
            if sensors:
                phase("sensor_wait")
                deadline=time.monotonic()+3
                while True:
                    try:
                        self.sensors.snapshot()
                        if self.writer.evidence()["matched_readers"]:
                            break
                    except Refused as exc:
                        if str(exc)!="sensors_not_ready":
                            # Preserve a permanently rejected sample for the
                            # read-only diagnostic endpoint. A missing helper
                            # or DDS reader still fails startup normally.
                            if self.sensors.rejected_sample() is not None and self.writer.evidence()["matched_readers"]:
                                self.startup_fault="sensor_startup_rejected"
                                break
                            raise
                    if time.monotonic()>=deadline:
                        raise Refused("startup_sensors_or_dds_unavailable")
                    time.sleep(.01)
        except BaseException:
            if self.sensors is not None:self.sensors.close()
            raise
        phase("transport_ready")
        self.gid=None
        self.arm_attempted=False
    def publish(self, values, stamp):
        self.writer.write(values,stamp)
    def status(self):
        return self.http.status()
    def publisher_id(self):
        if self.gid is None:
            raise Refused("publisher_not_discovered")
        return self.gid
    def discover(self):
        deadline=time.monotonic()+1
        while time.monotonic()<deadline:
            c=self.status()["ros_commands"]
            if c.get("auto_control") is not False or c.get("owner") is not None:
                raise Refused("runtime_control_changed")
            candidates=[s for s in c["sources"] if s["publisher_gid"] not in self.initial_sources]
            if candidates:
                if len(candidates)!=1 or candidates[0]["kind"]!="twist" or candidates[0].get("requires_restart") is not False:
                    raise Refused("publisher_discovery_ambiguous")
                self.gid=candidates[0]["publisher_gid"]
                return self.gid
            time.sleep(.01)
        raise Refused("publisher_discovery_timeout")
    def arm_once(self):
        if self.arm_attempted:
            raise Refused("rearm_refused")
        self.arm_attempted=True
        gid=self.discover()
        result=self.http.call("POST","/api/arm_ros",{"publisher_gid":gid})
        if result.get("publisher_gid")!=gid:
            raise Refused("arm_acknowledgement")
    def capture(self):
        return self.sensors.snapshot()
    def original_capture(self, sequence):
        getter=getattr(self.sensors,"original_capture",None)
        if getter is None:raise Refused("cloud_evidence_unavailable")
        return getter(sequence)
