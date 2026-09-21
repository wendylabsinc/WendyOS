FROM ros:humble-ros-base AS dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    python3-pip python3-pytest python3-opencv python3-yaml \
    ros-humble-nav2-bringup ros-humble-rmw-cyclonedds-cpp \
    ros-humble-vision-msgs ros-humble-cv-bridge ros-humble-tf2-ros \
    && rm -rf /var/lib/apt/lists/*
RUN pip3 install --no-cache-dir 'mcp==1.30.0' 'uvicorn==0.52.4'
WORKDIR /app
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 PYTHONPATH=/app

FROM dependencies AS runtime
COPY robot_navigation /app/robot_navigation
COPY config.json nav2.launch.py navigate.xml reject_waypoints.xml /app/
FROM runtime AS test
COPY tests /app/tests
ENTRYPOINT ["/ros_entrypoint.sh"]
CMD ["python3", "-m", "unittest", "discover", "-s", "tests"]

FROM runtime AS app
ENTRYPOINT ["/ros_entrypoint.sh", "python3", "-m", "robot_navigation.launch"]
