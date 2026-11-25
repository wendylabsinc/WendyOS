# The launcher may select the existing private Humble dependency image by digest.
ARG NAV_BASE=ros:humble-ros-base
FROM ${NAV_BASE}
USER root
RUN if [ ! -d /opt/ros/humble/share/nav2_bringup ]; then apt-get update && apt-get install -y --no-install-recommends ros-humble-navigation2 ros-humble-nav2-bringup python3-yaml && rm -rf /var/lib/apt/lists/*; fi
WORKDIR /world
COPY sim/__init__.py sim/patrol_nav.py sim/patrol_exclusions.py sim/patrol_localization.py sim/
COPY examples/collaborative-office/scene.json examples/collaborative-office/scene.json
COPY deploy/patrol-nav /world/deploy/patrol-nav
RUN /bin/bash -c 'source /opt/ros/humble/setup.bash && \
    cmake -S /world/deploy/patrol-nav/exact_goal_planner -B /tmp/exact-goal-build \
      -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/opt/ros/humble -DBUILD_TESTING=ON && \
    cmake --build /tmp/exact-goal-build --parallel 2 && \
    ctest --test-dir /tmp/exact-goal-build --output-on-failure && \
    cmake --install /tmp/exact-goal-build && rm -rf /tmp/exact-goal-build'
ENV ROS_DOMAIN_ID=72 ROS_LOCALHOST_ONLY=1
ENTRYPOINT ["/ros_entrypoint.sh", "python3", "-m", "sim.patrol_nav"]
