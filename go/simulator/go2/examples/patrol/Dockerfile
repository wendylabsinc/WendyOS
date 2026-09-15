FROM ros:humble-ros-base

ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 \
    ROS_DOMAIN_ID=0 RMW_IMPLEMENTATION=rmw_cyclonedds_cpp \
    ROS_LOCALHOST_ONLY=0 ROS_AUTOMATIC_DISCOVERY_RANGE=SUBNET

RUN apt-get update && apt-get install -y --no-install-recommends \
      python3-numpy ros-humble-rmw-cyclonedds-cpp ros-humble-geometry-msgs \
      ros-humble-sensor-msgs ros-humble-nav-msgs \
      ros-humble-std-msgs ros-humble-std-srvs \
    && rm -rf /var/lib/apt/lists/*
COPY unitree_api /opt/go2_msgs/src/unitree_api
RUN /bin/bash -c 'source /opt/ros/humble/setup.bash && cd /opt/go2_msgs && colcon build --merge-install --cmake-args -DBUILD_TESTING=OFF'
ENV PYTHONPATH=/opt/go2_msgs/install/local/lib/python3.10/dist-packages \
    LD_LIBRARY_PATH=/opt/go2_msgs/install/lib \
    AMENT_PREFIX_PATH=/opt/go2_msgs/install
WORKDIR /app
COPY go2_io.py ./
COPY patrol.py ros_app.py ./
CMD ["python3", "/app/ros_app.py", "--autostart"]
