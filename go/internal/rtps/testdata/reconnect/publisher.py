import signal,sys
import rclpy
from sensor_msgs.msg import Image
from rclpy.qos import qos_profile_sensor_data
rclpy.init()
node=rclpy.create_node('reconnect_publisher')
topics=[f'/thermal/cell{i}/temperature' for i in range(1,5)]
if sys.argv[1]=='restart': topics += [f'/thermal/cell{i}/preview' for i in range(1,5)]
publishers=[node.create_publisher(Image,t,qos_profile_sensor_data) for t in topics]
stop=False
def shutdown(*_):
 global stop
 stop=True
signal.signal(signal.SIGTERM,shutdown)
try:
 while not stop:
  for topic,p in zip(topics,publishers):
   msg=Image();msg.header.stamp=node.get_clock().now().to_msg();msg.header.frame_id=topic
   msg.width=32;msg.height=24;msg.encoding='rgb8' if topic.endswith('/preview') else '32FC1'
   msg.step=msg.width*(3 if msg.encoding=='rgb8' else 4);msg.data=bytes([70])*(msg.step*msg.height)
   p.publish(msg)
  rclpy.spin_once(node,timeout_sec=.05)
finally:
 node.destroy_node();rclpy.shutdown()
