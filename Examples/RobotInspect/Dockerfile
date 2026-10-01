# The probe implements RTPS itself, so it needs no ROS installation and no libc.
# A scratch image with one static binary keeps the deploy small enough to push to a
# robot over a slow link, and leaves nothing on the device that could touch an actuator.
FROM scratch
COPY robot-inspect /robot-inspect
ENTRYPOINT ["/robot-inspect"]
CMD ["-settle", "12s", "-duration", "5s"]
