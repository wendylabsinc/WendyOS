"""Bounded zero-only local DDS -> ROS interoperability check, no runtime HTTP.

Humble's executor drops take metadata. This diagnostic uses its exposed
Subscription.handle.take_message method; no RMW memory layout is accessed.
"""
import json
import time

def main():
    import rclpy
    from rclpy.node import Node
    from rclpy.qos import QoSProfile, ReliabilityPolicy, DurabilityPolicy
    from geometry_msgs.msg import Twist
    from .transport import DDSWriter
    rclpy.init(args=[])
    node=Node("world_go2_zero_interop_check")
    try:
        qos=QoSProfile(depth=1,reliability=ReliabilityPolicy.BEST_EFFORT,durability=DurabilityPolicy.VOLATILE)
        sub=node.create_subscription(Twist,"/cmd_vel",lambda _:None,qos)
        writer=DDSWriter()
        deadline=time.monotonic()+5
        while time.monotonic()<deadline and not writer.evidence()["matched_readers"]:
            time.sleep(.02)
        matched=writer.evidence()["matched_readers"]
        stamp=time.time_ns()-500_000_000
        writer.write([0.,0.,0.],stamp)
        deadline=time.monotonic()+2
        received=[]
        while time.monotonic()<deadline and not received:
            with sub.handle:
                taken=sub.handle.take_message(Twist,False)
            if taken is not None:
                msg,info=taken
                received.append((int(info["source_timestamp"]),[msg.linear.x,msg.linear.y,msg.linear.z,msg.angular.x,msg.angular.y,msg.angular.z]))
            else:time.sleep(.02)
        success=matched>0 and received==[(stamp,[0.]*6)]
        print(json.dumps({"success":success,"matched_readers":matched,"sent_source_timestamp_ns":stamp,"received":received,"positive_commands":0,"runtime_calls":0}))
        return 0 if success else 1
    finally:
        node.destroy_node();rclpy.shutdown()

if __name__=="__main__":raise SystemExit(main())
