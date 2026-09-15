# Build with the repository root as context.
FROM ros:humble-ros-base@sha256:1813d3c85d7f96ff7d3012d865204583255740182db5d0065f8f8cd029a83138 AS base
RUN apt-get update && apt-get install -y --no-install-recommends \
      ros-humble-tf2-ros ros-humble-rmw-cyclonedds-cpp \
    && rm -rf /var/lib/apt/lists/*

FROM base AS build
RUN apt-get update && apt-get install -y --no-install-recommends \
      curl ros-humble-rosidl-generator-dds-idl \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /opt/wendy-ros2
# Use upstream hardware definitions, without the simulator's compatibility patches.
RUN curl -fsSL https://codeload.github.com/unitreerobotics/unitree_ros2/tar.gz/668d1ec5a05d1c38d3306bdca7d59f2ba3581a88 -o /tmp/unitree.tar.gz \
    && echo '3e96da843c611878e28148ce113d978e5464dec922fc06dcd8c340f8232a6883  /tmp/unitree.tar.gz' | sha256sum -c - \
    && mkdir src \
    && tar -xzf /tmp/unitree.tar.gz --strip-components=4 -C src \
      unitree_ros2-668d1ec5a05d1c38d3306bdca7d59f2ba3581a88/cyclonedds_ws/src/unitree/unitree_go \
      unitree_ros2-668d1ec5a05d1c38d3306bdca7d59f2ba3581a88/cyclonedds_ws/src/unitree/unitree_api \
    && tar -xOf /tmp/unitree.tar.gz unitree_ros2-668d1ec5a05d1c38d3306bdca7d59f2ba3581a88/LICENSE > UNITREE-LICENSE \
    && rm /tmp/unitree.tar.gz
RUN /bin/bash -c 'source /opt/ros/humble/setup.bash && colcon build --merge-install --cmake-args -DBUILD_TESTING=OFF'

FROM base
COPY --from=build /opt/wendy-ros2/install /opt/wendy-ros2/install
COPY --from=build /opt/wendy-ros2/UNITREE-LICENSE /opt/wendy-ros2/UNITREE-LICENSE
COPY go/ros2/inspector/audio.py /opt/wendy-ros2/install/lib/wendy_ros2_inspection/audio
# Register the executable with ros2 run without a second Python package installer.
RUN chmod 755 /opt/wendy-ros2/install/lib/wendy_ros2_inspection/audio \
    && touch /opt/wendy-ros2/install/share/ament_index/resource_index/packages/wendy_ros2_inspection
ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1
LABEL org.opencontainers.image.source="https://github.com/wendylabsinc/WendyOS" \
      sh.wendy.ros2.inspector.version="humble-unitree-v1" \
      sh.wendy.unitree.commit="668d1ec5a05d1c38d3306bdca7d59f2ba3581a88"
COPY go/ros2/inspector/entrypoint.sh /wendy-entrypoint.sh
ENTRYPOINT ["/bin/sh", "/wendy-entrypoint.sh"]
CMD ["sleep", "infinity"]
