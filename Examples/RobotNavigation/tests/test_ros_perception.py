"""Real ROS/OpenCV adapter tests; run inside the dependency image, network none.

No robot interfaces, motion topics, Nav2 goals, or host network are used. Local
publishers provide synthetic RGB-D, calibrated camera data and capture-time TF.
"""

import math
import base64
import json
import time
import unittest
import uuid

try:
    import cv2
    import numpy as np
    import rclpy
    from geometry_msgs.msg import TransformStamped
    from rclpy.executors import SingleThreadedExecutor
    from rclpy.node import Node
    from rclpy.qos import qos_profile_sensor_data
    from sensor_msgs.msg import CameraInfo, Image
    from tf2_ros import StaticTransformBroadcaster, TransformBroadcaster
    from vision_msgs.msg import Detection2D, Detection2DArray, ObjectHypothesisWithPose
    ROS_AVAILABLE = True
except ImportError:
    ROS_AVAILABLE = False

from robot_navigation.grounding import GroundingConfig, GroundingError, RobotPose, TargetRegistry
from robot_navigation.perception import Perception


@unittest.skipUnless(ROS_AVAILABLE, "requires the ROS/OpenCV dependency image")
class ROSPerceptionTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        rclpy.init()

    @classmethod
    def tearDownClass(cls):
        rclpy.shutdown()

    def setUp(self):
        suffix = uuid.uuid4().hex[:12]
        self.node = Node("perception_test_"+suffix)
        self.source = Node("perception_source_"+suffix)
        self.executor = SingleThreadedExecutor()
        self.executor.add_node(self.node)
        self.executor.add_node(self.source)
        self.registry = TargetRegistry(GroundingConfig(nav_frame="odom"))
        settings = {"runtime": {"navigation_frame": "odom"},
                    "topics": {kind: "/perception_test_"+suffix+"/"+kind
                               for kind in ("rgb","depth","camera_info","detections")}}
        self.perception = Perception(self.node,settings,self.registry)
        self.publishers = {kind: self.source.create_publisher(type_,settings["topics"][kind],qos_profile_sensor_data)
                           for kind,type_ in (("rgb",Image),("depth",Image),("camera_info",CameraInfo),("detections",Detection2DArray))}
        self.static_tf = StaticTransformBroadcaster(self.source)
        self.dynamic_tf = TransformBroadcaster(self.source)
        mount = TransformStamped()
        mount.header.frame_id = "camera_mount"
        mount.child_frame_id = "camera_optical"
        mount.transform.translation.z = 1.3
        q = mount.transform.rotation
        q.x,q.y,q.z,q.w = -0.5,0.5,-0.5,0.5
        self.static_tf.sendTransform(mount)
        self.await_condition(lambda: all(publisher.get_subscription_count() for publisher in self.publishers.values()), timeout=3.0)

    def tearDown(self):
        self.executor.remove_node(self.source)
        self.executor.remove_node(self.node)
        self.source.destroy_node()
        self.node.destroy_node()
        self.executor.shutdown()

    def await_condition(self, condition, timeout=3.0, publish=None):
        deadline = time.monotonic()+timeout
        next_publish = 0.0
        while time.monotonic() < deadline:
            if publish and time.monotonic() >= next_publish:
                publish()
                next_publish = time.monotonic()+0.1
            self.executor.spin_once(timeout_sec=0.02)
            if condition():
                return
        self.fail(f"condition timed out; perception={self.perception.robot_targets()}")

    def publish_frame(self, people=True, mismatched_depth=False, count=1):
        stamp = self.source.get_clock().now().to_msg()
        self.last_stamp = stamp
        dynamic = TransformStamped()
        dynamic.header.stamp = stamp
        dynamic.header.frame_id = "odom"
        dynamic.child_frame_id = "camera_mount"
        dynamic.transform.rotation.w = 1.0
        self.dynamic_tf.sendTransform(dynamic)
        rgb = Image()
        rgb.header.stamp,rgb.header.frame_id = stamp,"camera_optical"
        rgb.width,rgb.height,rgb.step = 200,200,600
        rgb.encoding = "bgr8"
        pixels = np.zeros((200,200,3),dtype=np.uint8)
        pixels[20:180,60:140,2] = 100
        rgb.data = pixels.tobytes()
        depth = Image()
        depth.header.stamp = stamp
        depth.header.frame_id = "wrong_optical" if mismatched_depth else "camera_optical"
        depth.width,depth.height,depth.step = 200,200,400
        depth.encoding = "16UC1"
        depth.is_bigendian = False
        depth.data = np.full((200,200),3000,dtype="<u2").tobytes()
        info = CameraInfo()
        info.header.stamp,info.header.frame_id = stamp,"camera_optical"
        info.width,info.height = 200,200
        info.k = [400.,0.,100.,0.,400.,100.,0.,0.,1.]
        info.p = [400.,0.,100.,0.,0.,400.,100.,0.,0.,0.,1.,0.]
        info.r = [1.,0.,0.,0.,1.,0.,0.,0.,1.]
        detections = Detection2DArray()
        detections.header.stamp,detections.header.frame_id = stamp,"camera_optical"
        if people:
            item = Detection2D()
            item.header = detections.header
            item.id = "person-track-1"
            center = item.bbox.center
            position = center.position if hasattr(center,"position") else center
            position.x,position.y = 100.,100.
            item.bbox.size_x,item.bbox.size_y = 80.,160.
            hypothesis = ObjectHypothesisWithPose()
            hypothesis.hypothesis.class_id = "person"
            hypothesis.hypothesis.score = 0.95
            item.results = [hypothesis]
            import copy
            detections.detections = []
            for index in range(count):
                clone = copy.deepcopy(item)
                clone.id = f"person-track-{index}"
                detections.detections.append(clone)
        for kind,message in (("rgb",rgb),("depth",depth),("camera_info",info),("detections",detections)):
            self.publishers[kind].publish(message)

    def observe_person(self):
        self.await_condition(lambda: bool(self.perception.robot_targets()["targets"]), publish=self.publish_frame)
        return self.perception.robot_targets()["targets"][0]

    def test_synchronized_rgbd_tf_grounding_jpeg_and_physical_standoff(self):
        target = self.observe_person()
        observation,jpeg = self.perception.observation()
        self.assertEqual("observed",observation["status"])
        self.assertEqual((60,20,140,180),tuple(target["bbox"]))
        self.assertAlmostEqual(3.0,target["pose"]["x"])
        self.assertIsNotNone(jpeg)
        self.assertLessEqual(len(jpeg),40*1024)
        decoded = cv2.imdecode(np.frombuffer(jpeg,dtype=np.uint8),cv2.IMREAD_COLOR)
        self.assertEqual((200,200,3),decoded.shape)
        self.assertTrue(np.any((decoded[:,:,1] > 170) & (decoded[:,:,2] < 100)))
        self.assertEqual(target["target_id"],observation["image"]["labels"]["1"])
        self.assertEqual(target["source_stamp"],observation["image"]["source_stamp"])
        now = self.node.get_clock().now().nanoseconds*1e-9
        choice = self.perception.choose_goal(target["target_id"],RobotPose(-1,0,0,"odom",now,0.1),1.5)
        self.assertEqual("requires_plan_validation",choice["status"])
        distance = math.hypot(choice["goal"]["x"]-target["pose"]["x"],choice["goal"]["y"]-target["pose"]["y"])
        self.assertAlmostEqual(1.5+sum(choice["margins_m"].values()),distance)

    def test_loss_invalidates_selected_id_and_reappearance_requires_reselection(self):
        target = self.observe_person()
        self.publish_frame(people=False)
        self.await_condition(lambda: self.registry.status(target["target_id"],self.perception._now())["status"] != "observed")
        with self.assertRaises(GroundingError):
            self.perception.choose_goal(target["target_id"],RobotPose(-1,0,0,"odom",self.perception._now()))
        replacement = self.observe_person()
        self.assertNotEqual(target["target_id"],replacement["target_id"])

    def test_stale_source_expires_targets_and_matching_image(self):
        target = self.observe_person()
        self.await_condition(lambda: not self.perception.robot_targets()["targets"],timeout=2.0)
        self.assertEqual("stale",self.registry.status(target["target_id"],self.perception._now())["status"])
        self.assertIsNone(self.perception.observation()[1])
        with self.assertRaises(GroundingError):
            self.perception.choose_goal(target["target_id"],RobotPose(-1,0,0,"odom",self.perception._now()))

    def test_mismatched_depth_frame_cannot_preserve_selected_target(self):
        target = self.observe_person()
        self.publish_frame(mismatched_depth=True)
        self.await_condition(lambda: self.registry.status(target["target_id"],self.perception._now())["status"] != "observed")
        self.assertFalse(self.perception.robot_targets()["targets"])
        self.assertIsNone(self.perception.observation()[1])

    def test_builtin_hog_binding_runs_without_model_downloads(self):
        hog = cv2.HOGDescriptor()
        hog.setSVMDetector(cv2.HOGDescriptor_getDefaultPeopleDetector())
        boxes,_ = hog.detectMultiScale(np.zeros((240,320,3),dtype=np.uint8),hitThreshold=1.0,
                                      winStride=(8,8),padding=(8,8),scale=1.1,finalThreshold=2.0)
        self.assertEqual(0,len(boxes))

    def test_crowded_observation_keeps_matching_labels_below_wendy_proxy_budget(self):
        self.await_condition(lambda: self.perception.robot_targets()["total_targets"] == 64,
                             publish=lambda: self.publish_frame(count=64))
        metadata,jpeg = self.perception.observation()
        self.assertEqual(16,len(metadata["targets"]))
        self.assertTrue(metadata["targets_truncated"])
        self.assertEqual(64,len(self.registry.observed_targets(self.perception._now())))
        self.assertEqual({target["target_id"] for target in metadata["targets"]},
                         set(metadata["image"]["labels"].values()))
        self.assertIsNotNone(jpeg)
        # Measure the full JSON wire shape, including JSON-in-text escaping and
        # worst allowed JPEG size after base64, as the Wendy proxy does.
        wire = {"content": [{"type":"text","text":json.dumps(metadata,allow_nan=False)},
                            {"type":"image","mimeType":"image/jpeg",
                             "data":base64.b64encode(bytes(40*1024)).decode("ascii")}],
                "isError":False}
        self.assertLess(len(json.dumps(wire).encode("utf-8")),100000)

    def test_delayed_empty_frame_does_not_invalidate_a_newer_observation(self):
        target = self.observe_person()
        delayed = Detection2DArray()
        delayed.header.frame_id = "camera_optical"
        delayed.header.stamp.sec = max(0,int(target["source_stamp"])-1)
        self.perception._receive("detections",delayed)
        self.assertEqual("observed",self.registry.status(target["target_id"],self.perception._now())["status"])


if __name__ == "__main__":
    unittest.main()
