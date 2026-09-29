---
name: wendy-robot-deploy
description: Build and deploy robot applications with Wendy, ROS 2, Unitree Go2/G1, or a managed robot simulator. Use for sensor discovery, DDS configuration, simulation-to-hardware transfer, and verifying deployed behavior; use wendy-device-install when the device has no Agent yet.
---

# Robot deployment

Start with the actual device and tool inventory. Call `wendy_status`, select an
explicit local, cloud or `vm:<name>` target, then inspect `device_info` and
`hardware_capabilities`. Compare the running CLI/agent versions and advertised
features before relying on a tool described by a newer skill. If `ros2_*` or
`container_exec` is missing, inspect `wendy device ros2 --help` or update and
restart the MCP server when authorized. Do not invent calls.

## Establish the robot's interfaces

Use bounded read-only observations: `ros2_topics`, `ros2_topic_info`,
`ros2_topic_sample`, `ros2_topic_hz`, and `ros2_lidar_summary`. Inspect host scope
when reaching a robot's existing DDS graph; app scope defaults to app isolation.
Identify message types, domain, RMW, QoS, frames, timestamp clock and sensor rates.
Topic discovery alone does not prove messages decode or samples are fresh.

For an app that joins the robot's system graph, retain `frameworks.ros2` and set
`discoveryScope: "host"`, an explicit `domainId` matching the robot, and the
host-network entitlement. Do not remove the framework block to bypass isolation.
Bundle custom message packages such as pinned Unitree interfaces in the image.
Inspect an existing template before writing the Dockerfile and message adapters.

```json
{
  "frameworks": {
    "ros2": { "distro": "humble", "rmw": "rmw_cyclonedds_cpp", "domainId": 0, "discoveryScope": "host" }
  },
  "entitlements": [{ "type": "network", "mode": "host" }]
}
```

Use the robot's measured settings rather than assuming those example values.
Managed simulator deployment may restrict discovery to the guest bus. Check the
effective environment before treating simulator and physical networks as equal.

## Build, deploy and check

Use `wendy-template-app` to scaffold, with `--here` when already in the requested
directory. Build against the target architecture and available CUDA/JetPack where
needed. Test controller bounds and stopping on stale, invalid or missing inputs.

Use MCP `run(project_path=..., device=...)` or connect first and omit the target
to reuse that session. `device_name` is the legacy cloud-only selector. Check the
returned target. CLI fallback: `wendy run --device <selector> --detach --yes`.
`--deploy` creates without starting. Detached deployment does not wait for app
readiness; inspect containers, startup logs and a finite sample of the app's
actual output. A command published to ROS is not evidence the robot moved.

Keep motion disabled at startup and after observation faults until an explicit
start request. Preserve zero-command stop behavior, finite distance/time bounds
and the robot's supported velocity range. Go2 examples use 0.55 m/s forward
because requests below 0.5 m/s are ignored on the tested hardware; do not reduce
this blindly and then infer that control is working.

Run motion in the simulator first when available. Before a physical motion test,
establish the intended robot, bounded action, stop mechanism and operator
authorization. Existing authorization for that exact test is sufficient. A
deployment request alone does not authorize an autonomous roaming session.

Never add `--autostart`, `--ignore-capture-age`, or `--allow-scan-gaps` merely to
make a demo move. Explain and diagnose the failed precondition: clock alignment,
paired sensor timestamps, topic/QoS selection or scan coverage. Compatibility
options are explicit per-test choices. The default Patrol/Roam containers wait
for their Start service with strict capture-age and coverage checks.

Finish with observed results: target identity, deployed app, startup state,
sampled sensor/output evidence, motion test (if authorized), and stop result.
Label simulator-only and untested hardware behavior explicitly.
