FROM ros:humble-ros-base

ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 \
    ROS_DOMAIN_ID=0 RMW_IMPLEMENTATION=rmw_cyclonedds_cpp \
    ROS_LOCALHOST_ONLY=1 ROS_AUTOMATIC_DISCOVERY_RANGE=LOCALHOST

RUN apt-get update && apt-get install -y --no-install-recommends \
      ros-humble-rmw-cyclonedds-cpp ros-humble-geometry-msgs \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY teleop.py index.html ./
EXPOSE 8903
CMD ["python3", "/app/teleop.py"]
