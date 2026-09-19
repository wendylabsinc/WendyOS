# Build from the CLI's small, embedded source bundle. Every model, policy and
# upstream interface file is downloaded from an immutable URL and hash checked.
FROM ros:humble-ros-base@sha256:1813d3c85d7f96ff7d3012d865204583255740182db5d0065f8f8cd029a83138 AS runtime-base

ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 \
    OPENBLAS_NUM_THREADS=1 OMP_NUM_THREADS=1 MUJOCO_GL=osmesa \
    G1_HOST=0.0.0.0 G1_PORT=8890

RUN apt-get update && apt-get install -y --no-install-recommends \
      python3-pip libosmesa6 libgl1 libegl1 nftables util-linux \
      ros-humble-rmw-cyclonedds-cpp ros-humble-tf2-ros \
      ros-humble-rosidl-generator-dds-idl \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /opt/wendy-g1
COPY requirements.txt ./
RUN python3 -m pip install --no-cache-dir -r requirements.txt

FROM runtime-base AS assets
COPY assets.lock.json unitree.lock.json ./
COPY tools/fetch_assets.py tools/fetch_unitree.py ./tools/
RUN python3 tools/fetch_assets.py && python3 tools/fetch_unitree.py \
    && python3 tools/fetch_assets.py --check \
    && python3 tools/fetch_unitree.py --check
COPY tools/prepare_unitree.py ./tools/
RUN python3 tools/prepare_unitree.py
COPY ros_ws/src/g1_command_ingress ./ros_ws/src/g1_command_ingress
RUN /bin/bash -c 'source /opt/ros/humble/setup.bash && cd ros_ws && colcon build --merge-install --base-paths src/g1_command_ingress generated_unitree --cmake-args -DBUILD_TESTING=OFF'

FROM runtime-base
RUN apt-get update && apt-get install -y --no-install-recommends iptables \
    && rm -rf /var/lib/apt/lists/*
COPY --from=assets /opt/wendy-g1/assets/robot ./assets/robot
COPY --from=assets /opt/wendy-g1/assets/policy ./assets/policy
COPY --from=assets /opt/wendy-g1/assets/licenses ./assets/licenses
COPY --from=assets /opt/wendy-g1/ros_ws/install ./ros_ws/install
COPY --from=assets /opt/wendy-g1/ros_ws/generated_unitree/wendy-compatibility.json ./wendy-compatibility.json
COPY licenses ./licenses
COPY assets.lock.json unitree.lock.json UPSTREAM.md compatibility.json ./
COPY g1_sim/*.py g1_sim/index.html g1_sim/viewer.js ./g1_sim/
COPY g1_sim/vendor/three.module.js g1_sim/vendor/three.core.js g1_sim/vendor/OrbitControls.js ./g1_sim/vendor/
COPY entrypoint.sh cyclonedds.xml ./
ENV G1_VISUAL_DETAIL=balanced LP_NUM_THREADS=2 G1_NATIVE=1
EXPOSE 8890
CMD ["/bin/bash", "/opt/wendy-g1/entrypoint.sh"]
