FROM ros:humble-ros-base@sha256:1813d3c85d7f96ff7d3012d865204583255740182db5d0065f8f8cd029a83138
ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 R2_HOST=0.0.0.0 R2_PORT=8890
RUN apt-get update && apt-get install -y --no-install-recommends \
    ros-humble-rmw-cyclonedds-cpp ros-humble-tf2-ros \
    nftables iptables util-linux python3-numpy python3-pil && rm -rf /var/lib/apt/lists/*
WORKDIR /opt/wendy-r2
COPY ros_ws/src ./ros_ws/src
RUN /bin/bash -c 'source /opt/ros/humble/setup.bash && cd ros_ws && colcon build --merge-install --cmake-args -DBUILD_TESTING=OFF'
COPY r2_sim ./r2_sim
COPY licenses ./licenses
COPY compatibility.json UPSTREAM.md cyclonedds.xml entrypoint.sh ./
EXPOSE 8890
CMD ["/bin/bash", "/opt/wendy-r2/entrypoint.sh"]
