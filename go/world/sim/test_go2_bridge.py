"""Unit boundaries use fake transports/clocks; these do not prove DDS/VM behavior."""
import copy
import hashlib
import math
import os
import tempfile
import unittest
from pathlib import Path
from sim.go2_bridge.core import Bridge, Clock
from sim.go2_bridge.protocol import Configuration, Refused, encoded, strict, private_file, endpoint, ODOMETRY_MODELS

class Time:
    def __init__(self):
        self.w=1_800_000_000_000_000_000
        self.m=1_000_000_000
    def advance(self,ms):
        self.w+=int(ms*1_000_000); self.m+=int(ms*1_000_000)
    def clock(self):
        return Clock(lambda:self.w,lambda:self.m)

def configuration():
    a=dict(schema="wendy.world.go2-alignment.v1",source_frame="odom",target_frame={"id":"office-floor","revision":"v1"},translation_xy_m=[0,0],yaw_rad=0,reference_point="base_footprint",synthetic=True,position_uncertainty_m=.02,yaw_uncertainty_rad=.01)
    p=dict(schema="wendy.world.go2-profile.v1",flat_floor=True,energy_observed=False,half_length_m=.425,half_width_m=.30,clearance_m=.05,max_position_uncertainty_m=.25,max_yaw_uncertainty_rad=.2,max_forward_mps=.35,max_yaw_rate_rps=.5,observation_max_age_ms=250,source_skew_max_ms=50,decision_max_ms=500,expiry_lead_ms=450,horizon_s=1,extra_stop_m=.1,extra_stop_rad=.1,clock_error_ms=50,max_abs_roll_rad=.05,max_abs_pitch_rad=.05,vertical_obstacles=True,odometry_max_epoch_age_s=120,odometry_integration_allowance_m=.05,motion_envelope="isotropic-v1")
    ar,pr=encoded(a),encoded(p)
    b=dict(schema="wendy.world.go2-execution.v1",package_sha256="a"*64,conversion_sha256="b"*64,loss_sha256="c"*64,project_revision="d"*64,project_id="e"*32,vm_name="asdasdas",source_digest=next(iter(ODOMETRY_MODELS)),policy_bundle="go2-moe-cts-164k-0.6715-v1",world_epoch=2,run_id="test-run",frame=a["target_frame"],alignment_sha256=hashlib.sha256(ar).hexdigest(),profile_sha256=hashlib.sha256(pr).hexdigest())
    return Configuration(b,ar,pr),ar,pr

class Transport:
    def __init__(self,t,cfg):
        self.t=t;self.cfg=cfg;self.sent=[];self.owner=None;self.arms=0
        self.delay=0;self.write_delay=0;self.auto=False;self.project="d"*64;self.capture_seq=1;self.gid="1"*48;self.sensor_stamp=None;self.runtime_age=1.
    def publisher_id(self):return self.gid
    def publish(self,v,stamp):
        if any(v):self.t.advance(self.write_delay)
        self.sent.append((v[:],stamp,self.t.w))
    def arm_once(self):self.arms+=1;self.owner=self.gid
    def status(self):
        self.t.advance(self.delay)
        b=self.cfg.binding
        return dict(simulation=True,robot="go2",robot_kind="go2",profile_version=1,vm_name=b["vm_name"],source_digest=b["source_digest"],dds_isolation="udp-rtps-loopback",policy_bundle=b["policy_bundle"],clock_mode="device",epoch=2,healthy=True,ready=True,error=None,mode="standing",time=self.runtime_age,project={"project_id":b["project_id"],"revision":self.project,"error":None,"epoch":2},ros_commands={"auto_control":self.auto,"owner":self.owner})
    def capture(self):
        stamp=self.t.w if self.sensor_stamp is None else self.sensor_stamp
        return dict(capture_sequence=self.capture_seq,odom_captured_unix_ns=stamp,scan_captured_unix_ns=stamp,odom_publisher="2"*48,scan_publisher="3"*48,mount_publisher="4"*48,world_epoch=2,pose={"x":0.,"y":0.,"yaw":0.},body_velocity=[0.,0.,0.],pose_covariance=[0.]*36,twist_covariance=[0.]*36,scan={"ranges":[3.]*360,"angle_min":-math.pi,"angle_increment":math.pi/180,"range_min":.1,"range_max":12.,"sensor_mount":{"x":.28,"y":0.,"yaw":0.}})

class BridgeTests(unittest.TestCase):
    def setUp(self):
        self.cfg,_,_=configuration();self.t=Time();self.tr=Transport(self.t,self.cfg);self.b=Bridge(self.cfg,self.tr,self.t.clock(),fault_reporter=lambda _:None)
        self.state=self.b.dispatch("/v1/session",{"binding":self.cfg.binding})
        self.t.advance(10)
        self.begin=dict(session=self.state["session"],executor_epoch=7,task_id="test-task",spec_digest="a"*64,binding_sha256=self.cfg.binding_sha256)
        self.b.dispatch("/v1/begin",self.begin)
        self.t.advance(10)
    def command(self):
        return dict(session=self.state["session"],sequence=1,executor_epoch=7,task_id="test-task",spec_digest="a"*64,capture_sequence=1,body_vx=.2,body_vy=0.,yaw_rate=0.,not_after_unix_ms=self.t.w//1_000_000+500,decision_remaining_ms=500)
    def test_fixed_absolute_cap_source_nonadvance_is_distinct_and_stops(self):
        for speed in (0.,.2):
            with self.subTest(speed=speed):
                self.setUp()
                first=self.command();first.update(body_vx=speed,not_after_unix_ms=self.t.w//1_000_000+480,decision_remaining_ms=480)
                ack=self.b.dispatch('/v1/command',first)
                self.t.advance(100);self.tr.capture_seq=2
                self.b.dispatch('/v1/state',{'session':self.state['session']})
                second=dict(first,sequence=2,capture_sequence=2,decision_remaining_ms=380)
                second_ack=self.b.dispatch('/v1/command',second)
                self.assertGreater(second_ack['source_timestamp_ns'],ack['source_timestamp_ns'])
                self.t.advance(100);self.tr.capture_seq=3
                self.b.dispatch('/v1/state',{'session':self.state['session']})
                before=len(self.tr.sent)
                third=dict(first,sequence=3,capture_sequence=3,decision_remaining_ms=280)
                self.assertGreater(third['not_after_unix_ms'],self.t.w//1_000_000)
                with self.assertRaisesRegex(Refused,'^command_source_nonadvancing$'):
                    self.b.dispatch('/v1/command',third)
                self.assertEqual(self.b.fault,'request_refused')
                self.assertEqual(self.b.sequence,2)
                self.assertEqual(len(self.tr.sent),before+1)
                self.assertEqual(self.tr.sent[-1][0],[0.,0.,0.])
    def test_advancing_but_expired_source_keeps_budget_reason(self):
        for speed in (0.,.2):
            with self.subTest(speed=speed):
                self.setUp();self.t.advance(400)
                req=self.command();req.update(body_vx=speed,decision_remaining_ms=200,not_after_unix_ms=self.t.w//1_000_000+200)
                before=len(self.tr.sent)
                with self.assertRaisesRegex(Refused,'^command_budget_exhausted$'):
                    self.b.dispatch('/v1/command',req)
                self.assertEqual(self.b.fault,'request_refused')
                self.assertEqual(self.b.sequence,0)
                self.assertEqual(len(self.tr.sent),before+1)
                self.assertEqual(self.tr.sent[-1][0],[0.,0.,0.])
    def test_full_epoch_bias_not_bridge_age(self):
        self.tr.runtime_age=90.
        state=self.b.dispatch("/v1/state",{"session":self.state["session"]})
        self.assertEqual(state["odometry_epoch_age_s"],90.)
        self.assertAlmostEqual(state["scan"]["position_uncertainty_m"],.02+.05+math.hypot(.001,-.0005)*90)
        self.assertAlmostEqual(state["scan"]["yaw_uncertainty_rad"],.01+.0002*90)
        self.assertEqual(state["scan"]["ranges"],self.state["scan"]["ranges"])
        self.assertEqual(state["sample_unix_ms"],self.state["sample_unix_ms"])
    def test_age_is_sampled_after_capture_without_retimestamp(self):
        original=self.tr.capture
        def capture():
            result=original();self.tr.runtime_age=100.;return result
        self.tr.capture=capture;self.tr.capture_seq=2
        stamp=self.t.w
        state=self.b.dispatch("/v1/state",{"session":self.state["session"]})
        self.assertEqual(state["odometry_epoch_age_s"],100.)
        self.assertEqual(state["odom_captured_unix_ns"],stamp)
    def test_late_runtime_age_refuses_and_does_not_rearm(self):
        self.tr.runtime_age=120.001
        with self.assertRaisesRegex(Refused,"odometry_epoch_age_exceeded"):
            self.b.dispatch("/v1/command",self.command())
        self.assertFalse(any(any(v) for v,_,_ in self.tr.sent))
        self.tr.runtime_age=1
        with self.assertRaises(Refused):self.b.dispatch("/v1/state",{"session":self.state["session"]})
        self.assertEqual(self.tr.arms,1)
    def test_old_epoch_refuses_before_arm(self):
        tr=Transport(self.t,self.cfg);tr.runtime_age=317
        bridge=Bridge(self.cfg,tr,self.t.clock(),fault_reporter=lambda _:None)
        with self.assertRaisesRegex(Refused,"odometry_epoch_age_exceeded"):
            bridge.dispatch("/v1/session",{"binding":self.cfg.binding})
        self.assertEqual(tr.arms,0)
    def test_epoch_age_crossed_during_capture_refuses(self):
        original=self.tr.capture
        def capture():
            result=original();self.tr.runtime_age=121;return result
        self.tr.capture=capture
        with self.assertRaisesRegex(Refused,"odometry_epoch_age_exceeded"):
            self.b.dispatch("/v1/state",{"session":self.state["session"]})
    def test_total_uncertainty_still_refuses(self):
        self.tr.runtime_age=120
        self.cfg.profile["max_position_uncertainty_m"]=.20
        with self.assertRaisesRegex(Refused,"planar_uncertainty_exceeded"):
            self.b.dispatch("/v1/state",{"session":self.state["session"]})
    def test_post_capture_status_io_cannot_refresh_old_sensor(self):
        self.t.advance(130)
        self.tr.delay=130
        with self.assertRaisesRegex(Refused,"served_capture_stale"):
            self.b.dispatch("/v1/command",self.command())
        self.assertFalse(any(any(v) for v,_,_ in self.tr.sent))
    def test_each_controller_cycle_has_three_post_capture_status_reads(self):
        calls=[];capture=self.tr.capture;status=self.tr.status
        def recorded_capture():
            calls.append("capture");return capture()
        def recorded_status():
            calls.append("status");return status()
        self.tr.capture=recorded_capture;self.tr.status=recorded_status
        for _ in range(2):self.b.dispatch("/v1/state",{"session":self.state["session"]})
        self.assertTrue(self.b.dispatch("/v1/command",self.command())["accepted"])
        self.assertEqual(calls,["capture","status"]*3)
    def test_runtime_changes_during_capture_refuse_before_positive_publish(self):
        for change,reason in (("owner","runtime_owner_lost"),("project","runtime_project"),("epoch","runtime_identity"),("auto","automatic_control_unsupported"),("failure","runtime_unavailable")):
            with self.subTest(change=change):
                self.setUp();capture=self.tr.capture;status=self.tr.status
                def changed_capture():
                    c=capture()
                    if change=="owner":self.tr.owner=None
                    elif change=="project":self.tr.project="f"*64
                    elif change=="auto":self.tr.auto=True
                    elif change=="epoch":
                        def wrong_epoch():
                            s=status();s["epoch"]=3;return s
                        self.tr.status=wrong_epoch
                    else:
                        def unavailable():raise TimeoutError("must not become authority")
                        self.tr.status=unavailable
                    return c
                self.tr.capture=changed_capture
                with self.assertRaisesRegex(Refused,reason):self.b.dispatch("/v1/command",self.command())
                self.assertEqual(self.b.fault,reason)
                self.assertFalse(any(any(v) for v,_,_ in self.tr.sent))
                self.assertEqual(self.tr.arms,1)
    def test_unreviewed_source_cannot_inherit_model(self):
        cfg,ar,pr=configuration();binding=copy.deepcopy(cfg.binding)
        binding["source_digest"]="sha256:"+"f"*64
        with self.assertRaisesRegex(Refused,"unsupported_odometry_model"):
            Configuration(binding,ar,pr)
    def test_explicit_odometry_profile_bounds(self):
        for key,value in (("odometry_max_epoch_age_s",0),("odometry_max_epoch_age_s",121),("odometry_max_epoch_age_s",True),("odometry_integration_allowance_m",0),("odometry_integration_allowance_m",.049)):
            with self.subTest(key=key,value=value):
                cfg,ar,pr=configuration();p=strict(pr);p[key]=value;pr=encoded(p)
                binding=copy.deepcopy(cfg.binding);binding["profile_sha256"]=hashlib.sha256(pr).hexdigest()
                with self.assertRaises(Refused):Configuration(binding,ar,pr)
    def test_motion_envelope_is_explicit_and_digest_bound(self):
        cfg,ar,pr=configuration()
        p=strict(pr);p["motion_envelope"]="directional-v1";changed=encoded(p)
        with self.assertRaisesRegex(Refused,"configuration_digest"):
            Configuration(cfg.binding,ar,changed)
        binding=copy.deepcopy(cfg.binding)
        binding["profile_sha256"]=hashlib.sha256(changed).hexdigest()
        directional=Configuration(binding,ar,changed)
        self.assertEqual(directional.profile["motion_envelope"],"directional-v1")
        self.assertNotEqual(directional.binding_sha256,cfg.binding_sha256)
        self.assertEqual(cfg.profile["motion_envelope"],"isotropic-v1")
    def test_motion_envelope_no_missing_or_unknown_default(self):
        for value in (None,"","directional","isotropic","DIRECTIONAL-V1",True,{},[]):
            with self.subTest(value=value):
                cfg,ar,pr=configuration();p=strict(pr);p["motion_envelope"]=value
                changed=encoded(p);binding=copy.deepcopy(cfg.binding)
                binding["profile_sha256"]=hashlib.sha256(changed).hexdigest()
                with self.assertRaises(Refused):Configuration(binding,ar,changed)
        cfg,ar,pr=configuration();p=strict(pr);del p["motion_envelope"]
        changed=encoded(p);binding=copy.deepcopy(cfg.binding)
        binding["profile_sha256"]=hashlib.sha256(changed).hexdigest()
        with self.assertRaises(Refused):Configuration(binding,ar,changed)
    def test_fixed_timestamp_and_no_repeat(self):
        cmd=self.command();now=self.t.w
        ack=self.b.dispatch("/v1/command",cmd)
        self.assertEqual(ack["source_timestamp_ns"],now)
        self.assertEqual(ack["not_after_unix_ms"],cmd["not_after_unix_ms"])
        with self.assertRaises(Refused):self.b.dispatch("/v1/command",cmd)
        self.assertEqual(sum(any(v) for v,_,_ in self.tr.sent),1)
        self.assertTrue(self.b.fault)
    def test_combined_axis_command_refuses_before_publish(self):
        cmd=self.command();cmd["yaw_rate"]=-.1
        with self.assertRaisesRegex(Refused,"combined_axis_command"):
            self.b.dispatch("/v1/command",cmd)
        self.assertFalse(any(any(v) for v,_,_ in self.tr.sent))
        self.assertTrue(self.b.fault)
    def test_zero_and_pure_yaw_remain_legal(self):
        for yaw in (0.,-.1,.1):
            with self.subTest(yaw=yaw):
                self.setUp();cmd=self.command();cmd["body_vx"]=0.;cmd["yaw_rate"]=yaw
                self.assertTrue(self.b.dispatch("/v1/command",cmd)["accepted"])
    def test_delayed_serialization_cannot_renew_stamp(self):
        cmd=self.command();before=self.t.w;self.tr.write_delay=600
        with self.assertRaises(Refused):self.b.dispatch("/v1/command",cmd)
        positive=[x for x in self.tr.sent if any(x[0])]
        self.assertEqual(len(positive),1)
        self.assertEqual(positive[0][1],before)
        self.assertEqual(positive[0][2]-positive[0][1],600_000_000)
        self.assertGreaterEqual(positive[0][2]-positive[0][1],200_000_000) # stock source-age refusal
        self.assertEqual(self.b.fault,"publication_completed_late")
    def test_runtime_io_consumes_budget_before_publish(self):
        cmd=self.command();self.tr.delay=250
        with self.assertRaises(Refused):self.b.dispatch("/v1/command",cmd)
        self.assertFalse(any(any(v) for v,_,_ in self.tr.sent))
    def test_backdated_stamp_can_only_shorten(self):
        cmd=self.command();cmd["not_after_unix_ms"]-=100
        # Ensure source precedes recent zero and wait until original grant is old enough.
        self.t.advance(120);cmd["not_after_unix_ms"]=self.t.w//1_000_000+400
        ack=self.b.dispatch("/v1/command",cmd)
        self.assertEqual(ack["source_timestamp_ns"],cmd["not_after_unix_ms"]*1_000_000-450_000_000)
    def test_absolute_expiry_not_renewed_by_relative_remaining(self):
        cmd=self.command();cmd["not_after_unix_ms"]=self.t.w//1_000_000-1
        with self.assertRaises(Refused):self.b.dispatch("/v1/command",cmd)
        self.assertFalse(any(any(v) for v,_,_ in self.tr.sent))
    def test_expired_decision_latches_no_rearm(self):
        self.b.dispatch("/v1/command",self.command());self.t.advance(501);self.b.watchdog()
        self.assertEqual(self.b.fault,"decision_expired")
        with self.assertRaises(Refused):self.b.dispatch("/v1/session",{"binding":self.cfg.binding})
        self.assertEqual(self.tr.arms,1)
    def test_restart_owner_loss_latches(self):
        self.tr.owner=None
        with self.assertRaises(Refused):self.b.dispatch("/v1/state",{"session":self.state["session"]})
        self.assertEqual(self.b.fault,"runtime_owner_lost")
        self.tr.owner=self.tr.gid
        with self.assertRaises(Refused):self.b.dispatch("/v1/command",self.command())
        self.assertEqual(self.tr.arms,1)
    def test_project_change_latches(self):
        self.tr.project="a"*64
        with self.assertRaises(Refused):self.b.dispatch("/v1/command",self.command())
        self.assertFalse(any(any(v) for v,_,_ in self.tr.sent))
    def test_auto_control_change_latches(self):
        self.tr.auto=True
        with self.assertRaises(Refused):self.b.dispatch("/v1/command",self.command())
        self.assertFalse(any(any(v) for v,_,_ in self.tr.sent))
    def test_sensor_expiry_latches(self):
        self.tr.sensor_stamp=self.t.w-250_000_000
        with self.assertRaises(Refused):self.b.dispatch("/v1/command",self.command())
        self.assertEqual(self.b.fault,"sensor_stale")
    def test_fresh_next_frame_preserves_original_served_capture(self):
        cmd=self.command();self.tr.capture_seq=2
        original=copy.deepcopy(self.b.served_captures[1])
        ack=self.b.dispatch("/v1/command",cmd)
        self.assertTrue(ack["accepted"])
        self.assertEqual(self.b.served_captures[1],original)
    def test_unserved_capture_refuses(self):
        cmd=self.command();cmd["capture_sequence"]=2
        with self.assertRaises(Refused):self.b.dispatch("/v1/command",cmd)
    def test_old_served_capture_does_not_refresh(self):
        self.t.advance(250);self.tr.capture_seq=2
        cmd=self.command()
        with self.assertRaises(Refused):self.b.dispatch("/v1/command",cmd)
        self.assertFalse(any(any(v) for v,_,_ in self.tr.sent))
    def test_served_capture_count_is_bounded(self):
        for seq in range(2,15):
            self.tr.capture_seq=seq;self.b.dispatch("/v1/state",{"session":self.state["session"]})
        self.assertEqual(len(self.b.served_captures),8)
        self.assertNotIn(1,self.b.served_captures)
    def test_changed_sensor_identity_refuses_even_cached_capture(self):
        original=self.tr.capture
        def changed():
            c=original();c["scan_publisher"]="5"*48;return c
        self.tr.capture=changed
        with self.assertRaises(Refused):self.b.dispatch("/v1/command",self.command())
        self.assertEqual(self.b.fault,"sensor_identity_changed")
    def test_task_epoch_and_velocity_refuse(self):
        for key,value in (("executor_epoch",8),("body_vy",.1),("body_vx",.36),("yaw_rate",float("nan")),("decision_remaining_ms",0)):
            with self.subTest(key=key):
                self.setUp();cmd=self.command();cmd[key]=value
                with self.assertRaises(Refused):self.b.dispatch("/v1/command",cmd)
                self.assertFalse(any(any(v) for v,_,_ in self.tr.sent))
    def test_stop_after_fault_needs_no_sensor_or_owner(self):
        self.b.fault="sensor_stale";self.tr.owner=None;self.tr.sensor_stamp=1
        ack=self.b.dispatch("/v1/stop",dict(session=self.state["session"],sequence=1))
        self.assertEqual(ack,{"accepted":True});self.assertFalse(any(self.tr.sent[-1][0]))
    def test_clock_rollback_above_original_anchor_latches(self):
        self.t.advance(100);self.b.dispatch("/v1/state",{"session":self.state["session"]})
        self.t.advance(-20)
        with self.assertRaises(Refused):self.b.dispatch("/v1/command",self.command())
        self.assertEqual(self.b.fault,"clock_fault")
    def test_wall_jump_latches(self):
        self.t.w+=10_000_000
        with self.assertRaises(Refused):self.b.dispatch("/v1/command",self.command())
        self.assertEqual(self.b.fault,"clock_fault")
    def test_unknown_extra_fields_refuse(self):
        cmd=self.command();cmd["supervised"]=True
        with self.assertRaises(Refused):self.b.dispatch("/v1/command",cmd)

class ConfigurationTests(unittest.TestCase):
    def test_exact_digest_and_unsupported_profile(self):
        cfg,ar,pr=configuration()
        with self.assertRaises(Refused):Configuration(cfg.binding,ar+b" ",pr)
        p=strict(pr);p["energy_observed"]=True;pr=encoded(p);b=copy.deepcopy(cfg.binding);b["profile_sha256"]=hashlib.sha256(pr).hexdigest()
        with self.assertRaises(Refused):Configuration(b,ar,pr)
    def test_strict_json_and_loopback(self):
        for raw in (b'{"a":1,"a":2}',b'{"v":NaN}',b'\xff'):
            with self.assertRaises(Refused):strict(raw)
        for value in ("http://localhost:80","http://127.0.0.1:80/path","https://127.0.0.1:80","http://127.0.0.1:80?x=1"):
            with self.assertRaises(Refused):endpoint(value)
    def test_private_regular_files(self):
        with tempfile.TemporaryDirectory() as d:
            p=Path(d)/"file";p.write_bytes(b"private");p.chmod(0o600)
            self.assertEqual(private_file(p,private=True),b"private")
            link=Path(d)/"link";link.symlink_to(p)
            with self.assertRaises(OSError):private_file(link)
            fifo=Path(d)/"fifo";os.mkfifo(fifo)
            with self.assertRaises(Refused):private_file(fifo)
            p.chmod(0o644)
            with self.assertRaises(Refused):private_file(p,private=True)

if __name__=="__main__":unittest.main()

class SensorTests(unittest.TestCase):
    def setUp(self):
        from sim.go2_bridge.transport import Sensors
        from collections import deque
        import threading
        self.s=Sensors.__new__(Sensors)
        self.s.lock=threading.Lock();self.s.odom=deque(maxlen=64);self.s.scan=None;self.s.mount=None;self.s.epoch=None;self.s.fault="";self.s.identities={};self.s.last_stamp={};self.s.sequence=0
    def message(self):
        from types import SimpleNamespace as N
        q=N(w=1.,x=0.,y=0.,z=0.);p=N(x=0.,y=0.,z=.3);v=N(x=0.,y=0.,z=0.)
        return N(header=N(frame_id="odom",stamp=N(sec=1800000000,nanosec=1)),child_frame_id="base_link",pose=N(pose=N(position=p,orientation=q),covariance=[0.]*36),twist=N(twist=N(linear=v,angular=v),covariance=[0.]*36))
    def test_actual_odom_quaternion_tilt_refusal_latches(self):
        from types import SimpleNamespace as N
        m=self.message();m.pose.pose.orientation.w=math.cos(.1/2);m.pose.pose.orientation.x=math.sin(.1/2)
        self.s._odom(m,N(publisher_gid=bytes.fromhex("1"*48)))
        self.assertEqual(self.s.fault,"unsupported_body_tilt")
        self.assertFalse(self.s.odom)
    def test_odom_identity_change_and_old_capture_latch(self):
        from types import SimpleNamespace as N
        m=self.message();self.s._odom(m,N(publisher_gid=bytes.fromhex("1"*48)))
        self.assertEqual(len(self.s.odom),1)
        m.header.stamp.nanosec=2
        self.s._odom(m,N(publisher_gid=bytes.fromhex("2"*48)))
        self.assertEqual(self.s.fault,"sensor_publisher_changed")
    def test_nonadvancing_capture_refuses(self):
        from types import SimpleNamespace as N
        m=self.message();i=N(publisher_gid=bytes.fromhex("1"*48))
        self.s._odom(m,i);self.s._odom(m,i)
        self.assertEqual(self.s.fault,"sensor_capture_nonadvancing")
    def test_unknown_rays_stay_unknown(self):
        from types import SimpleNamespace as N
        m=N(header=N(frame_id="lidar_link",stamp=N(sec=1800000000,nanosec=1)),ranges=[3.]*360,angle_min=-math.pi,angle_increment=math.pi/180,range_min=.1,range_max=12.)
        m.ranges[0]=float("inf");m.ranges[1]=float("nan");m.ranges[2]=0.
        self.s._scan(m,N(publisher_gid=bytes.fromhex("1"*48)))
        self.assertFalse(self.s.fault)
        self.assertEqual(self.s.scan["ranges"][:4],[None,None,None,3.])


class LifecycleAndDiagnosticTests(unittest.TestCase):
    def test_helper_cleanup_targets_direct_child(self):
        from sim.go2_bridge.transport import Sensors
        from unittest.mock import Mock
        import subprocess
        s=Sensors.__new__(Sensors);s.process=Mock()
        s.process.poll.return_value=None
        s.process.wait.side_effect=[subprocess.TimeoutExpired("sensor",1),0]
        s.close()
        s.process.terminate.assert_called_once_with()
        s.process.kill.assert_called_once_with()
        self.assertEqual(s.process.wait.call_count,2)
    def test_fault_report_is_once_and_contains_no_raw_exception(self):
        cfg,_,_=configuration();t=Time();events=[]
        b=Bridge(cfg,Transport(t,cfg),t.clock(),fault_reporter=events.append)
        with self.assertRaises(Refused):b.fail("sensitive raw detail: never export")
        with self.assertRaises(Refused):b.fail("another")
        self.assertEqual(events,[{"event":"bridge_fault","simulation":True,"code":"bridge_unavailable"}])
    def test_diagnostic_failure_cannot_prevent_zero(self):
        cfg,_,_=configuration();t=Time();tr=Transport(t,cfg)
        def broken(_):raise OSError("diagnostic unavailable")
        b=Bridge(cfg,tr,t.clock(),fault_reporter=broken)
        with self.assertRaises(Refused):b.fail("runtime_owner_lost")
        self.assertEqual(tr.sent[-1][0],[0.,0.,0.])

class RuntimeHTTPDiagnosticTests(unittest.TestCase):
    def test_fixed_stages_once_no_retry_or_raw_details(self):
        import http.client
        from unittest.mock import Mock,patch
        from sim.go2_bridge.transport import RuntimeHTTP
        cases=(("request",OSError("private URL/token"),"connection",0),
               ("headers",TimeoutError("private headers"),"timeout",0),
               ("body",http.client.IncompleteRead(b"private",12),"protocol",7),
               ("decode",None,"protocol",7))
        for stage,exc,category,count in cases:
            with self.subTest(stage=stage):
                events=[];client=RuntimeHTTP("http://127.0.0.1:8890",events.append)
                conn=Mock();response=conn.getresponse.return_value
                response.status=200;response.getheader.return_value=None;response.read.return_value=b"private"
                if stage=="request":conn.request.side_effect=exc
                elif stage=="headers":conn.getresponse.side_effect=exc
                elif stage=="body":response.read.side_effect=exc
                with patch("sim.go2_bridge.transport.http.client.HTTPConnection",return_value=conn) as factory:
                    for _ in range(2):
                        with self.assertRaises(Exception):client.status()
                    self.assertEqual(factory.call_count,2) # One attempt per explicit call; no retries.
                    self.assertEqual(factory.call_args.kwargs["timeout"],.15)
                self.assertEqual(conn.close.call_count,2)
                self.assertEqual(len(events),1)
                event=events[0]
                self.assertEqual(set(event),{"event","simulation","stage","category","elapsed_ms","received_bytes"})
                self.assertEqual((event["stage"],event["category"],event["received_bytes"]),(stage,category,count))
                self.assertGreaterEqual(event["elapsed_ms"],0)
                self.assertNotIn("private",str(event))
    def test_diagnostic_failure_preserves_original_exception(self):
        from unittest.mock import Mock,patch
        from sim.go2_bridge.transport import RuntimeHTTP
        def unavailable(_):raise OSError("report unavailable")
        client=RuntimeHTTP("http://127.0.0.1:8890",unavailable)
        original=TimeoutError("original");conn=Mock();conn.request.side_effect=original
        with patch("sim.go2_bridge.transport.http.client.HTTPConnection",return_value=conn):
            with self.assertRaises(TimeoutError) as caught:client.status()
        self.assertIs(caught.exception,original)
        conn.close.assert_called_once()
    def test_real_local_header_timeout(self):
        import threading
        import socket
        from http.server import BaseHTTPRequestHandler,ThreadingHTTPServer
        from sim.go2_bridge.transport import RuntimeHTTP
        release=threading.Event();entered=threading.Event();events=[]
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                entered.set();release.wait(2)
            def log_message(self,*args):pass
        server=ThreadingHTTPServer(("127.0.0.1",0),Handler)
        worker=threading.Thread(target=server.serve_forever,kwargs={"poll_interval":.01},daemon=True);worker.start()
        try:
            client=RuntimeHTTP("http://127.0.0.1:%d"%server.server_port,events.append)
            with self.assertRaises((TimeoutError,socket.timeout)):client.status()
            self.assertTrue(entered.is_set())
            self.assertEqual(len(events),1)
            self.assertEqual((events[0]["stage"],events[0]["category"],events[0]["received_bytes"]),("headers","timeout",0))
            self.assertGreaterEqual(events[0]["elapsed_ms"],100)
        finally:
            release.set();server.shutdown();server.server_close();worker.join(2)

class RefusalResponseTests(unittest.TestCase):
    """Exercise the actual HTTP method with fake request streams/transport."""
    def handler(self, bridge):
        from types import SimpleNamespace
        from unittest.mock import patch,Mock
        from sim.go2_bridge.__main__ import serve
        cfg,ar,pr=configuration()
        files={'token':b'a'*64,'binding':encoded(cfg.binding),'alignment':ar,'profile':pr}
        args=SimpleNamespace(token_file='token',binding='binding',alignment='alignment',profile='profile',runtime='http://127.0.0.1:8890',listen_port=18886)
        captured=[]
        def capture(_address, handler):
            captured.append(handler)
            raise OSError('test-only bind refusal')
        with patch('sim.go2_bridge.__main__.private_file',side_effect=lambda p,*a,**k:files[p]),patch('sim.go2_bridge.transport.Transport',return_value=Mock()),patch('sim.go2_bridge.__main__.Bridge',return_value=bridge),patch('sim.go2_bridge.__main__.HTTPServer',side_effect=capture):
            with self.assertRaises(OSError):serve(args)
        return captured[0]
    def request(self, bridge, body=b'{}', authorized=True, check_reply=None):
        import io
        from email.message import Message
        cls=self.handler(bridge);handler=cls.__new__(cls)
        handler.path='/v1/command';handler.headers=Message()
        handler.headers['Authorization']='Bearer '+('a' if authorized else 'b')*64
        handler.headers['Content-Length']=str(len(body));handler.rfile=io.BytesIO(body)
        replies=[]
        def reply(code,value):
            if check_reply:check_reply(code,value)
            replies.append((code,value))
        handler.reply=reply;handler.do_POST();return replies
    def test_explicit_allowlist_not_arbitrary_regex_or_exception(self):
        from sim.go2_bridge.__main__ import refusal_response,REFUSAL_REASONS
        for reason in REFUSAL_REASONS:
            self.assertEqual(refusal_response(Refused(reason)),{'error':'bridge_refused','reason':reason})
        for exc in (Refused('private_bearer_secret'),Refused('command_budget_exhausted\nprivate'),RuntimeError('command_budget_exhausted')):
            self.assertEqual(refusal_response(exc),{'error':'bridge_refused','reason':'request_refused'})
    def test_actual_dispatch_zero_and_latch_precede_response(self):
        test=BridgeTests();test.setUp();test.t.advance(400)
        req=test.command();req.update(decision_remaining_ms=200,not_after_unix_ms=test.t.w//1_000_000+200)
        before=len(test.tr.sent)
        def check(code,value):
            self.assertEqual(test.b.fault,'request_refused')
            self.assertEqual(len(test.tr.sent),before+1)
            self.assertEqual(test.tr.sent[-1][0],[0.,0.,0.])
            self.assertEqual((code,value),(409,{'error':'bridge_refused','reason':'command_budget_exhausted'}))
        self.request(test.b,encoded(req),check_reply=check)
    def test_unknown_refusal_and_generic_error_never_echo_body(self):
        from unittest.mock import Mock
        for exc,expected in ((Refused('secret_value'),(409,{'error':'bridge_refused','reason':'request_refused'})),(RuntimeError('private URL and token'),(503,{'error':'bridge_unavailable'}))):
            bridge=Mock();bridge.dispatch.side_effect=exc
            self.assertEqual(self.request(bridge),[expected])
    def test_unauthorized_request_remains_401_without_dispatch(self):
        from unittest.mock import Mock
        bridge=Mock()
        self.assertEqual(self.request(bridge,authorized=False),[(401,{'error':'unauthorized'})])
        bridge.dispatch.assert_not_called()
    def test_invalid_json_never_dispatches_or_leaks_keys(self):
        from unittest.mock import Mock
        bridge=Mock()
        self.assertEqual(self.request(bridge,b'{"private":1,"private":2}'),[(409,{'error':'bridge_refused','reason':'request_refused'})])
        bridge.dispatch.assert_not_called()


class StartupDiagnosticTests(unittest.TestCase):
    def test_fixed_codes_never_emit_raw_details(self):
        import errno
        from sim.go2_bridge.__main__ import StartupDiagnostic
        d=StartupDiagnostic();d.set("http_bind")
        tests=[(OSError(errno.EADDRINUSE,"secret path and token"),"address_in_use"),(Refused("private arbitrary diagnostic"),"startup_unavailable"),(TimeoutError("private URL"),"timeout"),(AttributeError("private value"),"dependency_api_unavailable")]
        for exc,code in tests:
            with self.subTest(code=code):
                result=d.failure(exc)
                self.assertEqual(result,{"event":"bridge_startup_failed","simulation":True,"phase":"http_bind","error_class":code})
                self.assertNotIn("private",str(result));self.assertNotIn("secret",str(result))
    def test_runtime_timeout_has_exact_phase_and_no_writer(self):
        from unittest.mock import patch,Mock
        from sim.go2_bridge.__main__ import StartupDiagnostic
        from sim.go2_bridge.transport import Transport
        d=StartupDiagnostic();http=Mock();http.status.side_effect=TimeoutError("private URL")
        with patch('sim.go2_bridge.transport.RuntimeHTTP',return_value=http),patch('sim.go2_bridge.transport.DDSWriter') as writer:
            with self.assertRaises(TimeoutError) as caught:Transport('http://127.0.0.1:8890',progress=d.set)
            writer.assert_not_called()
        self.assertEqual(d.failure(caught.exception)['phase'],'runtime_status')
    def test_sensor_startup_failure_closes_observer(self):
        from unittest.mock import patch,Mock
        from sim.go2_bridge.__main__ import StartupDiagnostic
        from sim.go2_bridge.transport import Transport
        d=StartupDiagnostic();http=Mock();http.status.return_value={'ros_commands':{'auto_control':False,'owner':None,'sources':[]}}
        sensor=Mock();sensor.snapshot.side_effect=Refused('sensor_helper_unavailable');sensor.rejected_sample.return_value=None
        with patch('sim.go2_bridge.transport.RuntimeHTTP',return_value=http),patch('sim.go2_bridge.transport.DDSWriter'),patch('sim.go2_bridge.transport.Sensors',return_value=sensor):
            with self.assertRaises(Refused) as caught:Transport('http://127.0.0.1:8890',progress=d.set)
        self.assertEqual(d.failure(caught.exception)['phase'],'sensor_wait')
        self.assertEqual(d.failure(caught.exception)['error_class'],'sensor_helper_unavailable')
        sensor.close.assert_called_once_with()
    def test_http_bind_failure_closes_observer_without_arming(self):
        import errno
        from types import SimpleNamespace
        from unittest.mock import patch,Mock
        from sim.go2_bridge.__main__ import StartupDiagnostic,serve
        cfg,ar,pr=configuration();files={'token':b'a'*64,'binding':encoded(cfg.binding),'alignment':ar,'profile':pr}
        args=SimpleNamespace(token_file='token',binding='binding',alignment='alignment',profile='profile',runtime='http://127.0.0.1:8890',listen_port=18886)
        d=StartupDiagnostic();tr=Mock()
        with patch('sim.go2_bridge.__main__.private_file',side_effect=lambda path,*a,**k:files[path]),patch('sim.go2_bridge.transport.Transport',return_value=tr),patch('sim.go2_bridge.__main__.HTTPServer',side_effect=OSError(errno.EADDRINUSE,'secret detail')):
            with self.assertRaises(OSError) as caught:serve(args,d)
        self.assertEqual(d.failure(caught.exception)['phase'],'http_bind')
        self.assertEqual(d.failure(caught.exception)['error_class'],'address_in_use')
        tr.sensors.close.assert_called_once_with();tr.arm_once.assert_not_called();tr.publish.assert_not_called()
