FROM ros:humble-ros-base

ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 \
    ROS_DOMAIN_ID=0 RMW_IMPLEMENTATION=rmw_cyclonedds_cpp \
    ROS_LOCALHOST_ONLY=0 ROS_AUTOMATIC_DISCOVERY_RANGE=SUBNET

RUN apt-get update && apt-get install -y --no-install-recommends \
      python3-numpy ros-humble-rmw-cyclonedds-cpp ros-humble-geometry-msgs \
      ros-humble-sensor-msgs ros-humble-nav-msgs \
      ros-humble-std-msgs ros-humble-std-srvs \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY go2_io.py ./
COPY dashboard.py index.html ./
EXPOSE 8904
CMD ["python3", "/app/dashboard.py", "--ignore-capture-age"]
