FROM ros:humble-ros-base AS runtime
RUN apt-get update && apt-get install -y --no-install-recommends \
    python3-pil ros-humble-rmw-cyclonedds-cpp \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 \
    RMW_IMPLEMENTATION=rmw_cyclonedds_cpp ROS_DOMAIN_ID=0 ROS_LOCALHOST_ONLY=1
COPY wendy_app /app/wendy_app
EXPOSE 8891
ENTRYPOINT ["/ros_entrypoint.sh", "python3", "-m", "wendy_app"]
