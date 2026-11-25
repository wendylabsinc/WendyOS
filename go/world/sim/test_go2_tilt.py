"""Tilt evidence tests use synthetic ROS messages, never simulator truth or DDS."""
import copy
import hashlib
import math
import threading
import unittest
from collections import deque
from types import SimpleNamespace as N
from sim.go2_bridge.core import Bridge
from sim.go2_bridge.protocol import Configuration,Refused,encoded,strict
from sim.go2_bridge.transport import Sensors
from sim.test_go2_bridge import configuration,Time,Transport


def tilted_configuration():
    old,ar,pr=configuration();p=strict(pr)
    p.update(schema="wendy.world.go2-profile.v2",max_abs_roll_rad=.2,max_abs_pitch_rad=.2,
        tilted_scan=dict(model="solid-vertical-slab-v1",base_height_min_m=.15,base_height_max_m=.6,
        obstacle_min_height_m=1.,slab_margin_m=.03,body_half_height_m=.35,max_support_range_m=4.,
        range_error_m=.02,attitude_error_rad=.01,max_translation_m_per_wall_s=.5,
        max_rotation_rad_per_wall_s=1.,odom_floor_z_m=0.,height_calibration_error_m=.02))
    raw=encoded(p);binding=copy.deepcopy(old.binding);binding["profile_sha256"]=hashlib.sha256(raw).hexdigest()
    return Configuration(binding,ar,raw),ar,raw


def quaternion(roll,pitch,yaw=0.):
    cr,sr=math.cos(roll/2),math.sin(roll/2)
    cp,sp=math.cos(pitch/2),math.sin(pitch/2)
    cy,sy=math.cos(yaw/2),math.sin(yaw/2)
    return [cr*cp*cy+sr*sp*sy,sr*cp*cy-cr*sp*sy,cr*sp*cy+sr*cp*sy,cr*cp*sy-sr*sp*cy]


def observer(profile):
    s=Sensors.__new__(Sensors);s.profile=copy.deepcopy(profile);s.rejected=None
    s.lock=threading.Lock();s.odom=deque(maxlen=64);s.scan=None;s.mount=None;s.epoch=2
    s.fault="";s.identities={};s.last_stamp={};s.sequence=0
    return s


def odom(roll=.1,pitch=.08,stamp=1_800_000_000_000_000_001):
    w,x,y,z=quaternion(roll,pitch)
    return N(header=N(frame_id="odom",stamp=N(sec=stamp//10**9,nanosec=stamp%10**9)),child_frame_id="base_link",
        pose=N(pose=N(position=N(x=.2,y=-.1,z=.3),orientation=N(w=w,x=x,y=y,z=z)),covariance=[0.]*36),
        twist=N(twist=N(linear=N(x=0.,y=0.,z=0.),angular=N(x=0.,y=0.,z=0.)),covariance=[0.]*36))


class TiltConfigurationTests(unittest.TestCase):
    def test_explicit_v2_and_v1_unchanged(self):
        cfg,_,_=tilted_configuration();old,_,_=configuration()
        self.assertNotIn("tilted_scan",old.profile)
        self.assertNotEqual(cfg.binding["profile_sha256"],old.binding["profile_sha256"])
        self.assertEqual(cfg.profile["tilted_scan"]["model"],"solid-vertical-slab-v1")

    def test_transport_injects_profile_before_sensor_reader_start(self):
        from unittest.mock import Mock,patch
        from sim.go2_bridge.transport import Transport as ROS
        cfg,_,_=tilted_configuration();http=Mock()
        http.status.return_value={"ros_commands":{"auto_control":False,"owner":None,"sources":[]}}
        with patch("sim.go2_bridge.transport.RuntimeHTTP",return_value=http),patch("sim.go2_bridge.transport.DDSWriter"),patch("sim.go2_bridge.transport.Sensors") as sensors:
            ROS("http://127.0.0.1:8890",profile=cfg.profile)
            sensors.assert_called_once_with(profile=cfg.profile)

    def test_nested_missing_null_unknown_and_bounds_refuse(self):
        cfg,ar,raw=tilted_configuration()
        bad=[]
        for key in cfg.profile["tilted_scan"]:
            p=strict(raw);del p["tilted_scan"][key];bad.append(p)
            p=strict(raw);p["tilted_scan"][key]=None;bad.append(p)
        for key,value in (("base_height_min_m",.1),("base_height_max_m",.7),("obstacle_min_height_m",.5),
            ("slab_margin_m",.01),("body_half_height_m",.2),("max_support_range_m",9.),("range_error_m",0.),
            ("attitude_error_rad",0.),("max_translation_m_per_wall_s",0.),("max_rotation_rad_per_wall_s",5.),
            ("odom_floor_z_m",11.),("height_calibration_error_m",0.),("model","guessed"),("extra",True)):
            p=strict(raw);p["tilted_scan"][key]=value;bad.append(p)
        p=strict(raw);p["schema"]="wendy.world.go2-profile.v1";bad.append(p)
        p=strict(raw);p["max_abs_roll_rad"] = .251;bad.append(p)
        for p in bad:
            with self.subTest(p=p):
                candidate=encoded(p);binding=copy.deepcopy(cfg.binding);binding["profile_sha256"]=hashlib.sha256(candidate).hexdigest()
                with self.assertRaises(Refused):Configuration(binding,ar,candidate)


class TiltSensorTests(unittest.TestCase):
    def setUp(self):
        self.cfg,_,_=tilted_configuration();self.s=observer(self.cfg.profile)
        self.info=N(publisher_gid=bytes.fromhex("1"*48))

    def test_v2_retains_normalized_full_pose_v1_still_refuses(self):
        msg=odom();msg.pose.pose.orientation.w*=1.001
        msg.pose.pose.orientation.x*=1.001;msg.pose.pose.orientation.y*=1.001;msg.pose.pose.orientation.z*=1.001
        self.s._odom(msg,self.info)
        self.assertFalse(self.s.fault)
        sample=self.s.odom[-1]
        self.assertAlmostEqual(sum(x*x for x in sample["quaternion_wxyz"]),1.)
        self.assertAlmostEqual(sample["roll"],.1);self.assertAlmostEqual(sample["pitch"],.08)
        self.assertEqual(sample["odom_position_xyz"],[.2,-.1,.3])
        self.assertEqual(sample["stamp"],1_800_000_000_000_000_001)
        legacy=observer(configuration()[0].profile);legacy._odom(odom(),self.info)
        self.assertEqual(legacy.fault,"unsupported_body_tilt");self.assertFalse(legacy.odom)

    def test_exact_first_rejected_sample_is_not_accepted_or_replaced(self):
        msg=odom(.21,0.);self.s._odom(msg,self.info)
        self.assertEqual(self.s.fault,"unsupported_body_tilt");self.assertFalse(self.s.odom)
        shot=self.s.rejected_sample()
        self.assertEqual(shot["captured_unix_ns"],1_800_000_000_000_000_001)
        self.assertEqual(shot["world_epoch"],2);self.assertEqual(shot["publisher_gid"],"1"*48)
        self.assertAlmostEqual(shot["roll_rad"],.21)
        self.assertEqual(shot["quaternion_wxyz"],quaternion(.21,0.))
        self.s._odom(odom(0.,0.,1_800_000_000_000_000_002),self.info)
        self.assertFalse(self.s.odom)
        shot["quaternion_wxyz"][0]=0
        self.assertNotEqual(self.s.rejected_sample()["quaternion_wxyz"][0],0)

    def test_diagnostic_retrieval_needs_no_locks_or_session(self):
        self.s._odom(odom(.21,0.),self.info)
        t=Time();tr=Transport(t,self.cfg);tr.sensors=self.s
        b=Bridge(self.cfg,tr,t.clock(),fault_reporter=lambda _:None)
        replies=[];done=threading.Event()
        def retrieve():
            replies.append(b.dispatch("/v1/diagnostics",{}));done.set()
        worker=threading.Thread(target=retrieve,daemon=True)
        with self.s.lock,b.lock:
            worker.start()
            self.assertTrue(done.wait(1),"diagnostic retrieval waited for control/sensor locks")
        worker.join(1);result=replies[0]
        self.assertFalse(result["operational_authority"])
        self.assertEqual(result["rejected_sensor"]["reason"],"unsupported_body_tilt")
        self.assertFalse(tr.sent);self.assertEqual(tr.arms,0)
        with self.assertRaises(Refused):b.dispatch("/v1/diagnostics",{"extra":"no"})

    def test_startup_rejection_is_diagnostics_only_and_never_arms(self):
        from unittest.mock import Mock,patch
        from sim.go2_bridge.transport import Transport as ROS
        self.s._odom(odom(.21,0.),self.info)
        http=Mock();http.status.return_value={"ros_commands":{"auto_control":False,"owner":None,"sources":[]}}
        writer=Mock();writer.evidence.return_value={"matched_readers":1}
        with patch("sim.go2_bridge.transport.RuntimeHTTP",return_value=http),patch("sim.go2_bridge.transport.DDSWriter",return_value=writer),patch("sim.go2_bridge.transport.Sensors",return_value=self.s):
            tr=ROS("http://127.0.0.1:8890",profile=self.cfg.profile)
        b=Bridge(self.cfg,tr,Time().clock(),fault_reporter=lambda _:self.fail("must not log"))
        self.assertEqual(b.fault,"sensor_startup_rejected")
        self.assertEqual(b.dispatch("/v1/diagnostics",{})["rejected_sensor"]["reason"],"unsupported_body_tilt")
        with self.assertRaises(Refused):b.dispatch("/v1/session",{"binding":self.cfg.binding})
        self.assertFalse(b.started);self.assertFalse(tr.arm_attempted);writer.write.assert_not_called()
        self.s.fault="" # Even an erroneously cleared sensor cannot reset bridge.
        with self.assertRaises(Refused):b.dispatch("/v1/session",{"binding":self.cfg.binding})

    def test_rejected_sensor_does_not_hide_missing_dds(self):
        from unittest.mock import Mock,patch
        from sim.go2_bridge.transport import Transport as ROS
        sensor=Mock();sensor.snapshot.side_effect=Refused("unsupported_body_tilt");sensor.rejected_sample.return_value={"reason":"unsupported_body_tilt"}
        http=Mock();http.status.return_value={"ros_commands":{"auto_control":False,"owner":None,"sources":[]}}
        writer=Mock();writer.evidence.return_value={"matched_readers":0}
        with patch("sim.go2_bridge.transport.RuntimeHTTP",return_value=http),patch("sim.go2_bridge.transport.DDSWriter",return_value=writer),patch("sim.go2_bridge.transport.Sensors",return_value=sensor):
            with self.assertRaises(Refused):ROS("http://127.0.0.1:8890",profile=self.cfg.profile)
        sensor.close.assert_called_once();writer.write.assert_not_called()

    def test_missing_or_nonfinite_rejected_values_stay_unknown(self):
        msg=odom();msg.pose.pose.orientation.w=float("nan")
        self.s._odom(msg,self.info)
        shot=self.s.rejected_sample()
        self.assertIsNone(shot["quaternion_wxyz"]);self.assertIsNone(shot["roll_rad"])
        self.assertLess(len(encoded(shot)),2048)

    def test_snapshot_retains_full_mount_and_unknown_ranges(self):
        self.s._odom(odom(),self.info)
        header=N(frame_id="lidar_link",stamp=N(sec=1800000000,nanosec=10_000_001))
        self.s._scan(N(header=header,ranges=[float("inf")]+[3.]*359,angle_min=-math.pi,angle_increment=math.pi/180,range_min=.1,range_max=12.),N(publisher_gid=bytes.fromhex("2"*48)))
        self.s._mount(N(transforms=[N(child_frame_id="lidar_link",header=N(frame_id="base_link"),transform=N(translation=N(x=.28,y=0.,z=.08),rotation=N(w=1.,x=0.,y=0.,z=0.)))]),N(publisher_gid=bytes.fromhex("3"*48)))
        result=self.s.snapshot()
        self.assertIsNone(result["scan"]["ranges"][0])
        self.assertEqual(result["scan_captured_unix_ns"]-result["odom_captured_unix_ns"],10_000_000)
        self.assertEqual(result["tilted_pose"]["mount_translation_xyz"],[.28,0.,.08])
        self.assertEqual(result["tilted_pose"]["mount_quaternion_wxyz"],[1.,0.,0.,0.])
        self.assertEqual(result["tilted_pose"]["odom_position_xyz"],[.2,-.1,.3])
        result["tilted_pose"]["odom_position_xyz"][2]=99
        self.assertEqual(self.s.snapshot()["tilted_pose"]["odom_position_xyz"][2],.3)


def bridge_fixture():
    cfg,_,_=tilted_configuration();t=Time();tr=Transport(t,cfg)
    original=tr.capture
    def capture():
        c=original();c.update(roll_rad=.1,pitch_rad=.08,tilt=.18)
        c["tilted_pose"]={"quaternion_wxyz":quaternion(.1,.08),"odom_position_xyz":[0.,0.,.3],
            "mount_translation_xyz":[.28,0.,.08],"mount_quaternion_wxyz":[1.,0.,0.,0.]}
        return c
    tr.capture=capture;b=Bridge(cfg,tr,t.clock(),fault_reporter=lambda _:None)
    state=b.dispatch("/v1/session",{"binding":cfg.binding})
    return cfg,t,tr,b,state


class TiltBridgeTests(unittest.TestCase):
    def test_good_evidence_can_issue_one_fixed_deadline_axis_command(self):
        cfg,t,tr,b,state=bridge_fixture();t.advance(10)
        b.dispatch("/v1/begin",dict(session=state["session"],executor_epoch=7,task_id="task",spec_digest="a"*64,binding_sha256=cfg.binding_sha256))
        t.advance(10)
        cmd=dict(session=state["session"],sequence=1,executor_epoch=7,task_id="task",spec_digest="a"*64,capture_sequence=1,body_vx=.1,body_vy=0.,yaw_rate=0.,not_after_unix_ms=t.w//10**6+500,decision_remaining_ms=500)
        now=t.w;ack=b.dispatch("/v1/command",cmd)
        self.assertEqual(ack["source_timestamp_ns"],now)
        self.assertEqual(ack["not_after_unix_ms"],cmd["not_after_unix_ms"])
        with self.assertRaises(Refused):b.dispatch("/v1/command",cmd)
        self.assertEqual(sum(any(v) for v,_,_ in tr.sent),1)

    def test_missing_mismatched_pose_and_temporal_bounds_fail_closed(self):
        for mode in ("missing","quaternion","xy","mount","skew"):
            with self.subTest(mode=mode):
                cfg,t,tr,b,state=bridge_fixture();tr.capture_seq=2;capture=tr.capture
                def bad():
                    c=capture()
                    if mode=="missing":del c["tilted_pose"]
                    elif mode=="quaternion":c["tilted_pose"]["quaternion_wxyz"][0]=0.
                    elif mode=="xy":c["tilted_pose"]["odom_position_xyz"][0]=1.
                    elif mode=="mount":c["tilted_pose"]["mount_translation_xyz"][2]=.09
                    else:c["odom_captured_unix_ns"]-=51_000_000
                    return c
                tr.capture=bad
                with self.assertRaises(Refused):b.dispatch("/v1/state",{"session":state["session"]})
                self.assertTrue(b.fault);self.assertFalse(any(any(v) for v,_,_ in tr.sent))

    def test_original_evidence_exact_pair_bounds_and_epoch_height(self):
        cfg,t,tr,b,state=bridge_fixture();tr.capture_seq=2
        capture=tr.capture
        def skewed():
            c=capture();c["odom_captured_unix_ns"]-=20_000_000;return c
        tr.capture=skewed;tr.runtime_age=40.
        result=b.dispatch("/v1/state",{"session":state["session"]})
        p=result["scan"]["tilted_pose"]
        self.assertEqual(p["odom_captured_unix_ns"],t.w-20_000_000)
        self.assertEqual(result["scan_captured_unix_ns"],t.w)
        self.assertEqual(p["pair_translation_bound_m"],.01);self.assertEqual(p["pair_attitude_bound_rad"],.02)
        self.assertEqual(p["odometry_epoch_age_s"],40.)
        err=.02+.05+math.hypot(.001,-.0005)*40.+.01
        self.assertAlmostEqual(p["base_height_min_m"],.3-err);self.assertAlmostEqual(p["base_height_max_m"],.3+err)
        self.assertEqual(result["scan"]["ranges"],state["scan"]["ranges"])
        tr.runtime_age=90.
        with self.assertRaisesRegex(Refused,"tilted_height_outside_commissioning"):
            b.dispatch("/v1/state",{"session":state["session"]})

    def test_observed_height_departure_refuses_without_positive_publication(self):
        cfg,t,tr,b,state=bridge_fixture();tr.capture_seq=2;capture=tr.capture
        def high():
            c=capture();c["tilted_pose"]["odom_position_xyz"][2]=.7;return c
        tr.capture=high
        with self.assertRaisesRegex(Refused,"tilted_height_outside_commissioning"):
            b.dispatch("/v1/state",{"session":state["session"]})
        self.assertTrue(b.fault);self.assertFalse(any(any(v) for v,_,_ in tr.sent))

    def test_original_evidence_not_replaced_by_later_capture(self):
        _,_,tr,b,state=bridge_fixture()
        before=copy.deepcopy(b.served_captures[1]);tr.capture_seq=2
        b.dispatch("/v1/state",{"session":state["session"]})
        self.assertEqual(before,b.served_captures[1])
        state["scan"]["tilted_pose"]["quaternion_wxyz"][0]=0.
        self.assertNotEqual(b.served_captures[1]["tilted_pose"]["quaternion_wxyz"][0],0.)


if __name__=="__main__":unittest.main()
