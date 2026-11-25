"""Private simulation bridge and zero-only transport probe."""
import argparse
import hashlib
import errno
import hmac
import json
import os
import re
import signal
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, HTTPServer
from .protocol import Configuration, Refused, private_file, strict, encoded
from .core import Bridge, Clock


REFUSAL_REASONS = frozenset({
    "request_refused", "command_source_nonadvancing", "command_budget_exhausted",
    "command_task_identity", "combined_axis_command", "capture_not_served",
    "served_capture_stale", "session_mismatch", "clock_fault_latched",
    "clock_continuity", "decision_expired", "explicit_stop",
    "publication_outcome_unknown", "publication_completed_late",
    "runtime_unavailable", "runtime_identity", "runtime_owner_lost",
    "runtime_not_ready", "runtime_project", "runtime_project_epoch",
    "runtime_time_regression", "automatic_control_unsupported",
    "sensor_unavailable", "sensor_stale", "sensor_pair_skew",
    "sensor_identity_changed", "sensor_epoch_changed", "sensor_startup_rejected",
    "unsupported_body_tilt", "odometry_epoch_age_exceeded",
    "planar_uncertainty_exceeded", "tilted_height_outside_commissioning",
    "tilted_pair_uncertainty_exceeded",
    "cloud_profile_mismatch", "cloud_capture_invalid", "cloud_capture_changed",
    "cloud_evidence_unavailable",
})


def refusal_response(exc):
    # Map only reviewed static codes. A regex-shaped exception can still contain
    # private information. This runs after dispatch's existing latch/zero path.
    reason = str(exc) if isinstance(exc, Refused) else "request_refused"
    if reason not in REFUSAL_REASONS:
        reason = "request_refused"
    return {"error": "bridge_refused", "reason": reason}


class StartupDiagnostic:
    PHASES={"configuration","runtime_status","dds_writer","sensor_helper","sensor_wait","transport_ready","http_bind","serving"}
    CODES={"token_file_format","configuration_digest","protocol_fields","simulation_binding","alignment","alignment_translation","operating_profile","clock_or_vertical_profile","integer_bound","number_bound","input_file","input_file_size","invalid_json","loopback_endpoint","runtime_requires_idle_manual_control","runtime_http_status","sensor_helper_unavailable","sensors_not_ready","startup_sensors_or_dds_unavailable","unsupported_body_tilt","sensor_publisher_changed","sensor_capture_nonadvancing","sensor_epoch_changed","unsupported_lidar_mount","odom_frame","odom_covariance","scan_frame_or_bound","scan_coverage"}
    def __init__(self):
        self.phase="configuration"
    def set(self,phase):
        self.phase=phase if phase in self.PHASES else "configuration"
    def failure(self,exc):
        code="startup_unavailable"
        if isinstance(exc,Refused) and str(exc) in self.CODES:code=str(exc)
        elif isinstance(exc,TimeoutError):code="timeout"
        elif isinstance(exc,OSError):
            code={errno.EADDRINUSE:"address_in_use",errno.EACCES:"permission_denied",errno.EPERM:"permission_denied",errno.ENOENT:"dependency_or_input_missing",errno.ENOEXEC:"dependency_not_executable",errno.ECONNREFUSED:"connection_refused",errno.ETIMEDOUT:"timeout"}.get(exc.errno,"io_unavailable")
        elif isinstance(exc,(ImportError,AttributeError,TypeError)):code="dependency_api_unavailable"
        elif type(exc).__name__=="DDSException":code="dds_unavailable"
        return {"event":"bridge_startup_failed","simulation":True,"phase":self.phase,"error_class":code}


def write_new(path, value):
    fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
    try:
        raw=encoded(value)+b"\n"
        while raw:
            n=os.write(fd,raw)
            if n<=0:
                raise OSError("short write")
            raw=raw[n:]
        os.fsync(fd)
    finally:
        os.close(fd)


def probe(args):
    from .transport import Transport
    report={"schema":"wendy.world.go2-dds-zero-probe.v1","simulation":True,"positive_commands":0,"arm_requests":0,"operational_authority":False,"success":False,"phase":"transport_init","requested_delay_ms":args.delay_ms}
    try:
        t=Transport(args.runtime,sensors=False)
        before=t.status()["ros_commands"]
        report["before"]={"accepted":before["accepted"],"rejected":before["rejected"]}
        report["phase"]="dds_match"
        deadline=time.monotonic()+5
        while time.monotonic()<deadline and not t.writer.evidence()["matched_readers"]:
            time.sleep(.02)
        report["dds"]=t.writer.evidence()
        if not report["dds"]["matched_readers"]:
            raise Refused("no_dds_reader")
        report["phase"]="fresh_zero"
        fresh=time.time_ns()
        t.publish([0.,0.,0.],fresh)
        report["fresh_source_timestamp_ns"]=fresh
        report["phase"]="ingress_discovery"
        report["ingress_publisher_gid"]=t.discover()
        after_fresh=t.status()["ros_commands"]
        report["after_fresh"]={"accepted":after_fresh["accepted"],"rejected":after_fresh["rejected"]}
        frozen=time.time_ns()
        report["delayed_source_timestamp_ns"]=frozen
        report["phase"]="delayed_zero"
        time.sleep(args.delay_ms/1000)
        report["delayed_publish_started_unix_ns"]=time.time_ns()
        t.publish([0.,0.,0.],frozen)
        report["phase"]="source_age_observation"
        deadline=time.monotonic()+1
        after=t.status()["ros_commands"]
        while time.monotonic()<deadline and after["rejected"]<=after_fresh["rejected"]:
            time.sleep(.01)
            after=t.status()["ros_commands"]
        report["after"]={"accepted":after["accepted"],"rejected":after["rejected"],"owner":after.get("owner"),"auto_control":after.get("auto_control"),"error_class":"expired_dds_command" if after.get("last_error")=="expired DDS command" else "other_or_none"}
        report["success"]=(after.get("owner") is None and after.get("auto_control") is False and after.get("accepted")==before.get("accepted") and after.get("rejected",0)>after_fresh.get("rejected",0) and after.get("last_error")=="expired DDS command")
        report["phase"]="complete"
        report["identity_limit"]="Ingress handle discovered as the unique new source in the isolated VM; not a cryptographic GUID binding."
    except Exception as exc:
        # Fixed codes only, never raw exceptions, requests or source snapshots.
        allowed={"runtime_requires_idle_manual_control","no_dds_reader","runtime_http_status","publisher_discovery_ambiguous","publisher_discovery_timeout","runtime_control_changed","invalid_json","json_size","loopback_endpoint"}
        report["error_class"]=str(exc) if isinstance(exc,Refused) and str(exc) in allowed else type(exc).__name__ if isinstance(exc,(TimeoutError,ConnectionError,OSError,AttributeError,TypeError,KeyError)) else "transport_or_runtime_error"
    write_new(args.output,report)
    print(encoded({"success":report["success"],"phase":report["phase"],"error_class":report.get("error_class", "none")}).decode())
    return 0 if report["success"] else 1


def probe_sensors(args):
    from .transport import Sensors
    report={"schema":"wendy.world.go2-sensor-probe.v1","simulation":True,"positive_commands":0,"arm_requests":0,"operational_authority":False,"success":False,"phase":"sensor_helper_start"}
    sensors=None
    try:
        sensors=Sensors()
        report["phase"]="sensor_capture"
        deadline=time.monotonic()+5
        while True:
            try:
                snapshot=sensors.snapshot()
                report["snapshot"]=snapshot
                report["observed_unix_ns"]=time.time_ns()
                report["success"]=True
                report["phase"]="complete"
                break
            except Refused as exc:
                if str(exc)!="sensors_not_ready" or time.monotonic()>=deadline:
                    raise
                time.sleep(.01)
    except Exception as exc:
        allowed={"sensors_not_ready","unsupported_body_tilt","sensor_helper_unavailable","sensor_capture_nonadvancing","sensor_publisher_changed","sensor_epoch_changed","unsupported_lidar_mount","odom_frame","scan_frame_or_bound","odom_covariance","scan_coverage"}
        report["error_class"]=str(exc) if isinstance(exc,Refused) and str(exc) in allowed else "sensor_probe_unavailable"
    finally:
        if sensors is not None:sensors.close()
    write_new(args.output,report)
    print(encoded({"success":report["success"],"phase":report["phase"],"error_class":report.get("error_class","none")}).decode())
    return 0 if report["success"] else 1


def serve(args, startup=None):
    startup=startup or StartupDiagnostic()
    startup.set("configuration")
    from .transport import Transport
    raw_token=private_file(args.token_file,256,private=True).strip()
    if not re.fullmatch(rb"[0-9a-f]{64}",raw_token):
        raise Refused("token_file_format")
    cfg=Configuration(strict(private_file(args.binding)),private_file(args.alignment,16384),private_file(args.profile,16384))
    clock=Clock()
    transport=Transport(args.runtime,progress=startup.set,profile=cfg.profile,binding=cfg.binding,clock=clock)
    bridge=Bridge(cfg,transport,clock=clock)
    closing=threading.Event()
    def watch():
        while not closing.wait(.01):
            bridge.watchdog()
    threading.Thread(target=watch,daemon=True).start()
    class Handler(BaseHTTPRequestHandler):
        protocol_version="HTTP/1.1"
        def log_message(self,*args):
            pass
        def setup(self):
            super().setup()
            self.connection.settimeout(.5)
        def reply(self,code,value):
            data=encoded(value)
            self.send_response(code)
            self.send_header("Content-Type","application/json")
            self.send_header("Content-Length",str(len(data)))
            self.send_header("Connection","close")
            self.end_headers()
            self.wfile.write(data)
            self.close_connection=True
        def do_GET(self):
            self.reply(405,{"error":"method_not_allowed"})
        def do_POST(self):
            try:
                expected=b"Bearer "+raw_token
                supplied=self.headers.get("Authorization","").encode("ascii",errors="replace")
                if len(self.headers.get_all("Authorization",[]))!=1 or not hmac.compare_digest(supplied,expected):
                    self.reply(401,{"error":"unauthorized"});return
                if self.headers.get("Transfer-Encoding") or self.headers.get("Expect") or self.headers.get("Content-Encoding") or len(self.headers.get_all("Content-Length",[]))!=1:
                    self.reply(400,{"error":"request_framing"});return
                length=self.headers.get("Content-Length","")
                if not re.fullmatch(r"[1-9][0-9]{0,4}",length) or int(length)>65536:
                    self.reply(400,{"error":"request_size"});return
                body=self.rfile.read(int(length))
                if len(body)!=int(length):
                    raise Refused("request_incomplete")
                result=bridge.dispatch(self.path,strict(body))
                self.reply(200,result)
            except Refused as exc:
                self.reply(409,refusal_response(exc))
            except Exception:
                # No exception text, token, request or proof enters logs.
                self.reply(503,{"error":"bridge_unavailable"})
    startup.set("http_bind")
    try:
        server=HTTPServer(("127.0.0.1",args.listen_port),Handler)
    except BaseException:
        closing.set()
        transport.sensors.close()
        raise
    startup.set("serving")
    server.timeout=.2
    def stop(*_):
        closing.set()
    signal.signal(signal.SIGTERM,stop)
    signal.signal(signal.SIGINT,stop)
    try:
        while not closing.is_set():
            server.handle_request()
    finally:
        closing.set()
        bridge._zero()
        server.server_close()
        transport.sensors.close()
    return 0


def main():
    parser=argparse.ArgumentParser(description="Private simulation-only Go2 bridge")
    modes=parser.add_subparsers(dest="mode",required=True)
    p=modes.add_parser("probe",help="zero-only immutable DDS timestamp probe; never arms")
    p.add_argument("--runtime",required=True)
    p.add_argument("--output",required=True)
    p.add_argument("--delay-ms",type=int,default=250,choices=range(250,1001))
    p=modes.add_parser("probe-sensors",help="bounded read-only actual ROS sensor capture; no writer or arm")
    p.add_argument("--output",required=True)
    p=modes.add_parser("serve")
    for arg in ("binding","alignment","profile","token-file","runtime"):
        p.add_argument("--"+arg,required=True)
    p.add_argument("--listen-port",type=int,required=True)
    args=parser.parse_args()
    if args.mode=="serve" and not 1024<=args.listen_port<=65535:
        parser.error("listen port must be1024..65535")
    startup=StartupDiagnostic()
    try:
        if args.mode=="probe":return probe(args)
        if args.mode=="probe-sensors":return probe_sensors(args)
        return serve(args,startup)
    except Exception as exc:
        if args.mode=="serve":
            print(encoded(startup.failure(exc)).decode(),flush=True)
        print("Go2 bridge refused; no authority granted",file=sys.stderr)
        return 1

if __name__=="__main__":
    raise SystemExit(main())
