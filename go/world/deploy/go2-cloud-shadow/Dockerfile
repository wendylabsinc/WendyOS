FROM __WORLD_BASE_IMAGE__
COPY --chown=65532:65532 world-go2-cloud-shadow /opt/world/world-go2-cloud-shadow
COPY --chown=65532:65532 sim/ /opt/world/sim/
COPY --chown=65532:65532 capture.sh /opt/world/cloud-shadow-capture
COPY --chown=65532:65532 entrypoint.sh /opt/world/cloud-shadow-entrypoint
COPY --chown=65532:65532 export_report.py /opt/world/cloud-shadow-export.py
COPY --chown=65532:65532 launch.py /opt/world/cloud-shadow-launch.py
WORKDIR /opt/world
ENV ROS_LOG_DIR=/tmp/ros-logs
ENV PYTHONDONTWRITEBYTECODE=1
USER 65532:65532
ENTRYPOINT ["/bin/bash", "/opt/world/cloud-shadow-entrypoint"]
