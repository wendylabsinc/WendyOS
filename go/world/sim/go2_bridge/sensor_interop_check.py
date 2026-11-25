"""Actual ROS -> native helper -> Python CDR check with synthetic messages.

No runtime HTTP, commands, arming or robot simulation. Run in network-none.
"""
import json
import math
import time

def main():
    import rclpy
    from rclpy.node import Node
    from rclpy.qos import QoSProfile, ReliabilityPolicy, DurabilityPolicy
    from nav_msgs.msg import Odometry
    from sensor_msgs.msg import LaserScan
    from std_msgs.msg import UInt64
    from tf2_msgs.msg import TFMessage
    from geometry_msgs.msg import TransformStamped
    from .transport import Sensors
    from .protocol import Refused
    rclpy.init(args=[])
    node=Node("world_go2_synthetic_sensor_check")
    sensors=None
    try:
        q=QoSProfile(depth=1,reliability=ReliabilityPolicy.BEST_EFFORT,durability=DurabilityPolicy.VOLATILE)
        retained=QoSProfile(depth=1,reliability=ReliabilityPolicy.RELIABLE,durability=DurabilityPolicy.TRANSIENT_LOCAL)
        op=node.create_publisher(Odometry,"/odom",q);sp=node.create_publisher(LaserScan,"/scan",q)
        ep=node.create_publisher(UInt64,"/simulation/epoch",retained);tp=node.create_publisher(TFMessage,"/tf_static",retained)
        sensors=Sensors()
        deadline=time.monotonic()+5
        snapshot=None
        while time.monotonic()<deadline:
            now=time.time_ns();sec,nsec=divmod(now,1_000_000_000)
            o=Odometry();o.header.frame_id="odom";o.child_frame_id="base_link";o.header.stamp.sec=sec;o.header.stamp.nanosec=nsec;o.pose.pose.orientation.w=1.;o.pose.pose.position.z=.3
            scan=LaserScan();scan.header.frame_id="lidar_link";scan.header.stamp=o.header.stamp;scan.angle_min=-math.pi;scan.angle_increment=math.pi/180;scan.range_min=.1;scan.range_max=12.;scan.ranges=[3.]*360
            transform=TransformStamped();transform.header.frame_id="base_link";transform.header.stamp=o.header.stamp;transform.child_frame_id="lidar_link";transform.transform.translation.x=.28;transform.transform.translation.z=.08;transform.transform.rotation.w=1.
            op.publish(o);sp.publish(scan);ep.publish(UInt64(data=2));tp.publish(TFMessage(transforms=[transform]))
            time.sleep(.05)
            try:
                snapshot=sensors.snapshot();break
            except Refused as exc:
                if str(exc)!="sensors_not_ready":raise
        if snapshot is None:raise Refused("sensor_check_timeout")
        success=snapshot["world_epoch"]==2 and len(snapshot["scan"]["ranges"])==360 and snapshot["scan"]["ranges"]==[3.]*360 and snapshot["odom_captured_unix_ns"]==snapshot["scan_captured_unix_ns"] and all(len(snapshot[x])==48 for x in ("odom_publisher","scan_publisher","mount_publisher"))
        print(json.dumps({"success":success,"positive_commands":0,"runtime_calls":0,"sensor_source":"synthetic_ros_test_publishers","captures":{"odom":snapshot["odom_captured_unix_ns"],"scan":snapshot["scan_captured_unix_ns"]},"publisher_gids":{k:snapshot[k] for k in ("odom_publisher","scan_publisher","mount_publisher")},"roll_rad":snapshot["roll_rad"],"pitch_rad":snapshot["pitch_rad"],"ranges":len(snapshot["scan"]["ranges"])}),flush=True)
        return 0 if success else 1
    finally:
        if sensors is not None:sensors.close()
        node.destroy_node();rclpy.shutdown()

if __name__=="__main__":raise SystemExit(main())
