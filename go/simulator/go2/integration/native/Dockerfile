ARG SIMULATOR_IMAGE=wendy-go2-ros:dev
FROM ${SIMULATOR_IMAGE}

ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 \
    CYCLONEDDS_HOME=/opt/cyclonedds-sdk \
    ROS_DOMAIN_ID=0 RMW_IMPLEMENTATION=rmw_cyclonedds_cpp ROS_LOCALHOST_ONLY=0 \
    CYCLONEDDS_URI=file:///opt/wendy-go2/cyclonedds.xml

WORKDIR /app/native
COPY unitree.lock.json /app/unitree.lock.json
COPY integration/native/verify_sources.py /app/native/verify_sources.py
RUN python3 verify_sources.py \
    && python3 -c 'import pathlib, sysconfig; root=pathlib.Path("/opt/cyclonedds-sdk"); root.mkdir(); ros=pathlib.Path("/opt/ros/humble"); [(root/name).symlink_to(ros/name) for name in ("include", "bin")]; (root/"lib").symlink_to(ros/"lib"/sysconfig.get_config_var("MULTIARCH"))' \
    && python3 -m pip install --no-cache-dir --no-binary=cyclonedds cyclonedds==0.10.2 \
    && python3 -m pip install --no-cache-dir --no-deps /opt/wendy-go2/assets/sdk2_python
RUN /bin/bash -c 'source /opt/ros/humble/setup.bash && colcon --log-base /app/stock_ros/log build --base-paths /opt/wendy-go2/ros_ws/src/unitree --build-base /app/stock_ros/build --install-base /app/stock_ros/install --cmake-args -DBUILD_TESTING=OFF'
COPY integration/native/ /app/native/
# Keep the exact SDK source (including non-package files) available at runtime.
ENV PYTHONPATH=/opt/wendy-go2/assets/sdk2_python
ENTRYPOINT ["/bin/bash", "/app/native/entrypoint.sh"]
CMD ["python3", "/app/native/exercise.py"]
