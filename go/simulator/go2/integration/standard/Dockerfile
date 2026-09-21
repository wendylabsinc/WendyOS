FROM ros:humble-ros-base

ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 \
    ROS_DOMAIN_ID=0 RMW_IMPLEMENTATION=rmw_cyclonedds_cpp \
    ROS_LOCALHOST_ONLY=1 ROS_AUTOMATIC_DISCOVERY_RANGE=LOCALHOST \
    GO2_SIMULATOR_URL=http://127.0.0.1:8890

RUN apt-get update && apt-get install -y --no-install-recommends \
      ros-humble-rmw-cyclonedds-cpp ros-humble-geometry-msgs \
      ros-humble-sensor-msgs ros-humble-nav-msgs ros-humble-tf2-msgs \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY exercise.py ./
CMD ["python3", "/app/exercise.py"]
