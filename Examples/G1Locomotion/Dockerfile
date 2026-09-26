FROM ros:humble-ros-base@sha256:1813d3c85d7f96ff7d3012d865204583255740182db5d0065f8f8cd029a83138
ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 CYCLONEDDS_HOME=/opt/cyclonedds-sdk \
    CYCLONEDDS_URI=file:///app/cyclonedds.xml
RUN apt-get update && apt-get install -y --no-install-recommends \
      python3-pip python3-dev build-essential cmake curl ca-certificates ros-humble-rmw-cyclonedds-cpp \
    && rm -rf /var/lib/apt/lists/*
RUN python3 -c 'import pathlib, sysconfig; root=pathlib.Path("/opt/cyclonedds-sdk"); root.mkdir(); ros=pathlib.Path("/opt/ros/humble"); [(root/name).symlink_to(ros/name) for name in ("include", "bin")]; (root/"lib").symlink_to(ros/"lib"/sysconfig.get_config_var("MULTIARCH"))'
WORKDIR /app
COPY requirements.txt ./
RUN python3 -m pip install --no-cache-dir --no-binary=cyclonedds -r requirements.txt
# Same unmodified SDK revision as go/simulator/g1/unitree.lock.json.
RUN curl -fsSL https://codeload.github.com/unitreerobotics/unitree_sdk2_python/tar.gz/65691c8a8bc53b98d3976dba4dbf9d5d20b2e7f5 -o /tmp/sdk.tar.gz \
    && echo 'b74e43fb6535929ec2a9efc1685e96111932ac6a9d7105b9cc7564ebaf6b7d58  /tmp/sdk.tar.gz' | sha256sum -c - \
    && python3 -m pip install --no-cache-dir --no-deps /tmp/sdk.tar.gz \
    && rm /tmp/sdk.tar.gz
COPY app.py cyclonedds.xml ./
CMD ["python3", "/app/app.py"]
