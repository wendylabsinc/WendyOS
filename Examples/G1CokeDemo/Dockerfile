# CPU virtual G1 runtime. The original Jetson build is build.stagefile.yaml.
FROM ros:humble-ros-base@sha256:1813d3c85d7f96ff7d3012d865204583255740182db5d0065f8f8cd029a83138 AS dependencies

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    OPENBLAS_NUM_THREADS=1 \
    OMP_NUM_THREADS=1 \
    MUJOCO_GL=egl \
    PYOPENGL_PLATFORM=egl \
    EGL_PLATFORM=surfaceless \
    LIBGL_ALWAYS_SOFTWARE=1 \
    LP_NUM_THREADS=2 \
    COKE_HOST=127.0.0.1 \
    COKE_PORT=8892 \
    ROS_DOMAIN_ID=0 \
    ROS_LOCALHOST_ONLY=1 \
    RMW_IMPLEMENTATION=rmw_cyclonedds_cpp

RUN apt-get -o Acquire::IndexTargets::deb-src::Sources::DefaultEnabled=false update \
    && apt-get install -y --no-install-recommends \
      ca-certificates python3-venv \
      libegl1 libegl-mesa0 libgl1 libgl1-mesa-dri libosmesa6 \
      ros-humble-rclpy ros-humble-rmw-cyclonedds-cpp \
      ros-humble-sensor-msgs ros-humble-trajectory-msgs \
      ros-humble-geometry-msgs ros-humble-nav-msgs \
      ros-humble-tf2-ros ros-humble-std-srvs ros-humble-rosgraph-msgs \
    && rm -rf /var/lib/apt/lists/*

# ROS Python packages stay visible inside the Python 3.10 environment.
RUN python3 -m venv --system-site-packages /opt/venv
ENV PATH=/opt/venv/bin:$PATH
RUN python -m pip install --no-cache-dir \
      --index-url https://download.pytorch.org/whl/cpu torch==2.7.1
RUN python -m pip install --no-cache-dir \
      numpy==1.26.4 mujoco==3.12.0 Pillow==11.3.0 imageio-ffmpeg==0.6.0

FROM dependencies AS runtime
WORKDIR /app
COPY bundle ./bundle
COPY expert ./expert
COPY assets.lock.json ./assets.lock.json
COPY runtime ./runtime
COPY physical_io ./physical_io
COPY coke_demo ./coke_demo
COPY ui ./ui
COPY container-entrypoint.sh /usr/local/bin/coke-entrypoint
EXPOSE 8892
ENTRYPOINT ["/bin/bash", "/usr/local/bin/coke-entrypoint"]
CMD ["python", "-m", "coke_demo", "serve"]
