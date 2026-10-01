---
name: wendy-robot-deploy
description: Build and deploy robot applications with Wendy, ROS 2, Unitree Go2/G1, or a managed robot simulator. Use for sensor discovery, DDS configuration, simulation-to-hardware transfer, and verifying deployed behavior; use wendy-device-install when the device has no Agent yet.
---

# Robot deployment

## Simulation and sim-to-real must never use scripting

Simulation and sim-to-real must NEVER use any form of scripting, including for
sensor interpretation or motor control. Do not write, execute, deploy or delegate
shell, Python, JavaScript, notebook or inline scripts for these workflows. A
scripted control loop is prohibited even if it reads live sensors. Existing
templates, demos and tests do not create an exception. This rule takes precedence
over the general build and deployment guidance in this skill.

Interpret the robot's current sensor observations and measured motor movement
directly, then use the sensor and motor tools exposed by the connected target:

1. Read fresh sensor data and motor feedback. Check timestamps, frames and
   validity before deciding what the robot should do.
2. Choose a bounded motor action based on those observations and the user's goal.
3. Issue that action through the supported motor interface. In simulation, let
   the physics engine produce the movement and virtual sensor readings.
4. Read the resulting sensor and motor feedback, compare the actual movement
   with the intended effect, and use that evidence to decide the next action.
   A command acknowledgement, elapsed time or viewer animation alone does not
   establish that the intended motor movement occurred.

Never use timed command sequences, canned trajectories, prerecorded sensor data,
replayed motion or hard-coded choreography to perform the task. Do not teleport
the robot, directly rewrite its pose or joint state, fabricate observations or
use privileged simulator state in place of sensor perception. Changing the
programming language or hiding a sequence inside an app does not make it valid.
If feedback is stale, invalid or missing, stop motion through the supported stop
interface and report the gap. Missing tools are a blocker, not permission to
create a script or bypass the physics engine.

For sim-to-real, retain the same observation, interpretation, motor action and
feedback process. Inspect the physical robot's actual sensor and motor interfaces
and limits before transfer. Reinterpret fresh hardware observations for every
action; never replay a successful simulator sequence on hardware. Report success
only from observed sensor and motor results on the selected target, and label
simulator-only evidence explicitly.

## Inspect the target

Start with the actual device and tool inventory. With the CLI MCP server, call
`wendy_status`, select an explicit local, cloud or `vm:<name>` target, then inspect
`device_info` and `hardware_capabilities`. With the ChatGPT gateway, use
`list_robots` and `inspect_robot` with the exact authorized `robot_id`. These
servers have separate tool inventories and target selection.

Compare the running CLI/agent versions and advertised features before relying
on a tool described by a newer skill. On the CLI server, the `ros2_*` tools are
in the `robotics` group: call `wendy_tools(groups=["robotics"])` to list them.
For missing inspection tools, check `wendy device ros2 --help` when a local
terminal is available, or update and restart the MCP server when authorized.
Do not invent calls or use `container_exec` or terminal scripts to work around
missing sensor or motor tools.

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
