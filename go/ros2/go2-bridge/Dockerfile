FROM ros:humble-ros-base

# ROS_DOMAIN_ID, RMW_IMPLEMENTATION, CYCLONEDDS_URI and the discovery range
# come from frameworks.ros2 in wendy.json.
ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1

RUN apt-get update && apt-get install -y --no-install-recommends \
      ros-humble-rmw-cyclonedds-cpp ros-humble-tf2-ros ros-humble-foxglove-msgs ros-humble-rosidl-generator-dds-idl \
      python3-pip python3-numpy python3-av curl \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /opt/go2-bridge
COPY requirements.txt ./
RUN python3 -m pip install --no-cache-dir -r requirements.txt

# Unitree message types, identical to the Go2 simulator's.
COPY tools/prepare_unitree_msgs.py ./tools/
RUN curl -fsSL -o /tmp/unitree_ros2.tar.gz \
      https://codeload.github.com/unitreerobotics/unitree_ros2/tar.gz/668d1ec5a05d1c38d3306bdca7d59f2ba3581a88 \
    && python3 tools/prepare_unitree_msgs.py /tmp/unitree_ros2.tar.gz ros_ws/src \
    && rm /tmp/unitree_ros2.tar.gz \
    && /bin/bash -c 'source /opt/ros/humble/setup.bash && cd ros_ws && colcon build --cmake-args -DBUILD_TESTING=OFF' \
    && rm -rf ros_ws/build ros_ws/log

COPY LICENSE-unitree_webrtc_connect ./
COPY go2_bridge ./go2_bridge
CMD ["/bin/bash", "-c", "source /opt/go2-bridge/ros_ws/install/setup.bash && exec python3 -m go2_bridge.node"]
