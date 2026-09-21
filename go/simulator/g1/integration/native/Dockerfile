ARG SIMULATOR_IMAGE=wendy-g1-ros:dev
FROM ${SIMULATOR_IMAGE}
ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 \
    CYCLONEDDS_HOME=/opt/cyclonedds-sdk \
    ROS_DOMAIN_ID=0 RMW_IMPLEMENTATION=rmw_cyclonedds_cpp ROS_LOCALHOST_ONLY=0 \
    CYCLONEDDS_URI=file:///opt/wendy-g1/cyclonedds.xml
COPY unitree.lock.json /app/unitree.lock.json
COPY unitree.lock.json /opt/wendy-g1/unitree.lock.json
COPY tools/fetch_assets.py tools/fetch_unitree.py /opt/wendy-g1/tools/
RUN python3 /opt/wendy-g1/tools/fetch_unitree.py
WORKDIR /app/native
COPY integration/native/verify_sources.py ./
RUN python3 verify_sources.py \
    && python3 -c 'import pathlib, sysconfig; root=pathlib.Path("/opt/cyclonedds-sdk"); root.mkdir(); ros=pathlib.Path("/opt/ros/humble"); [(root/name).symlink_to(ros/name) for name in ("include", "bin")]; (root/"lib").symlink_to(ros/"lib"/sysconfig.get_config_var("MULTIARCH"))' \
    && python3 -m pip install --no-cache-dir --no-binary=cyclonedds cyclonedds==0.10.2 \
    && python3 -m pip install --no-cache-dir --no-deps /opt/wendy-g1/assets/sdk2_python
RUN /bin/bash -c 'source /opt/ros/humble/setup.bash && colcon --log-base /app/stock_ros/log build --base-paths /opt/wendy-g1/ros_ws/src/unitree --build-base /app/stock_ros/build --install-base /app/stock_ros/install --cmake-args -DBUILD_TESTING=OFF'
COPY integration/native/ ./
ENTRYPOINT ["/bin/bash", "/app/native/entrypoint.sh"]
CMD ["python3", "/app/native/exercise.py"]
