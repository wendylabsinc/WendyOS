FROM ros:humble-ros-base

ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 \
    ROS_DOMAIN_ID=0 RMW_IMPLEMENTATION=rmw_cyclonedds_cpp \
    ROS_LOCALHOST_ONLY=1 ROS_AUTOMATIC_DISCOVERY_RANGE=LOCALHOST

RUN apt-get update && apt-get install -y --no-install-recommends \
      ros-humble-rmw-cyclonedds-cpp ros-humble-nav-msgs ros-humble-sensor-msgs \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY dashboard.py index.html ./
EXPOSE 8904
CMD ["python3", "/app/dashboard.py"]
