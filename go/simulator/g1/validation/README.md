# G1 validation

The G1 profile passed the following checks on 2026-09-14. These are finite
functional and timing checks, with their scope recorded in each report.

| Report | Result |
| --- | --- |
| [Linux suite](linux-suite.json) | 76 tests passed, including genuine OSMesa RGB/segmentation after policy-controlled physics, physical gait, command ownership/watchdogs, sensors, and HTTP lifecycle. |
| [Native SDK](native-sdk.json) | Independent pinned G1 SDK decoded HG CRC/motor order, drove 0.652 m of forward motion and 0.099 rad of wrist motion, and verified Loco errors, damping, zero torque, and both watchdogs. |
| [Local ROS application](standard.json) | 60-second separate application process, real Twist motion and sensor readers, pause/reset publisher fencing, and finite rate gate. Runtime limited to 4 CPUs/4 GiB. |
| [VM ROS application](vm-standard.json) | Same checks through the agent into `g1-sim`, with managed identity verified. Measured 0.632 m forward, 0.598 m backward, and a 0.775 rad walking turn. |
| [VM lifecycle](vm-lifecycle.json) | Profile creation, agent update, managed deployment/restart, typed HG inspection, source identity, and ready/disarmed final state. |

The VM ran WendyOS 0.19.1 on ARM64 with four CPUs and 4096 MiB. Its pinned
runtime source was
`sha256:3b9728e303f90e73f1f6b482b4c5a73b2a926931a8d7d10ac81a239deee1b40e`.
During the 12-second uninterrupted reader window, the VM measured 500.3 Hz
physics, 50.0 Hz policy, 491.9 Hz received HG state, 198.3 Hz IMU, 49.8 Hz
odometry/joints, 10.0 Hz scan/cloud, and 14.7 Hz image/info. Whole-run rates also
include deliberate pauses/resets and are reported separately.

Native SDK acceptance uses the initial image of the same policy/native bridge.
Its report includes exact source hashes. The final image additionally contains
an HTTP compression-header fix and clearer control guidance; all 18 final
Python source hashes are verified by the Linux suite. No native adapter changes
were made after SDK validation. The VM application imports standard/Unitree
ROS messages and sends real DDS commands; it does not import simulator modules.
It was a separate application process attached into the managed container,
not a separately deployed container.

Not established by these reports: a 600-second sustained performance gate,
physical G1/factory-controller parity, Nav2 acceptance, cross-VM packet escape
probes, or a manual browser walkthrough. Camera pixels and browser scene HTTP
contracts were tested; the user's active browser was left alone. The learned
policy's weak initiation for small commands and in-place yaw remains a declared
limitation, with no artificial root movement or command amplification.
