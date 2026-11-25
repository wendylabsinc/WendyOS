# Private World app. The existing Wendy Go2 runtime supplies physics and sensors.
FROM ros:humble-ros-base@sha256:7bea3d9aa2483d3ca34c8e30d921b79273b0913bd7dc64bebf51d082b5d107e4 AS dependencies
ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 \
    CYCLONEDDS_HOME=/opt/cyclonedds-sdk \
    CYCLONEDDS_URI=file:///opt/world/cyclonedds.xml \
    ROS_DOMAIN_ID=0 RMW_IMPLEMENTATION=rmw_cyclonedds_cpp ROS_LOCALHOST_ONLY=0
RUN apt-get update && apt-get install -y --no-install-recommends \
      python3-pip python3-dev build-essential cmake ros-humble-rclcpp \
      ros-humble-rmw-cyclonedds-cpp ros-humble-nav-msgs \
      ros-humble-sensor-msgs ros-humble-tf2-msgs \
    && rm -rf /var/lib/apt/lists/*
RUN python3 -c 'import pathlib,sysconfig; p=pathlib.Path("/opt/cyclonedds-sdk"); p.mkdir(); ros=pathlib.Path("/opt/ros/humble"); [(p/n).symlink_to(ros/n) for n in ("include","bin")]; (p/"lib").symlink_to(ros/"lib"/sysconfig.get_config_var("MULTIARCH"))'
RUN python3 -m pip install --no-cache-dir --no-binary=cyclonedds cyclonedds==0.10.2
WORKDIR /opt/world

FROM dependencies AS probe
COPY cyclonedds.xml /opt/world/cyclonedds.xml
COPY sim/go2_bridge /opt/world/sim/go2_bridge
COPY probe-entrypoint.sh /opt/world/probe-entrypoint.sh
COPY probe-report.py /opt/world/probe-report.py
USER 65532:65532
ENTRYPOINT ["/bin/bash", "/opt/world/probe-entrypoint.sh"]
