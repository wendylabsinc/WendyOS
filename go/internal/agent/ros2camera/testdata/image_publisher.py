"""A real ROS 2 publisher for the privileged CycloneDDS interoperability test."""
import rclpy
from sensor_msgs.msg import Image

rclpy.init()
node = rclpy.create_node("wendy_camera_regression")
publisher = node.create_publisher(Image, "/camera/color/image_raw", 10)
image = Image()
image.header.frame_id = "camera"
image.width = 640
image.height = 360
image.encoding = "rgb8"
image.step = image.width * 3
image.data = bytes([24, 128, 240]) * (image.width * image.height)
last_count = -1


def publish():
    global last_count
    count = publisher.get_subscription_count()
    if count != last_count:
        print(f"matched readers: {count}", flush=True)
        last_count = count
    image.header.stamp = node.get_clock().now().to_msg()
    publisher.publish(image)


node.create_timer(1 / 15, publish)
rclpy.spin(node)
