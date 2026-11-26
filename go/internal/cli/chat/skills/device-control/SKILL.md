---
name: device-control
description: Supervise local motion controllers and execute bounded goals on an identified device.
---

Inspect the exposed control API, device identity, fresh observations, and stop behavior before issuing a goal. Determine which local controller owns motion and avoid concurrent ownership. Preserve the user's requested scope and existing approval policy.

Keep actuator loops, command expiry, limits, and watchdogs in the local controller. Model response timing is not a real-time control loop. Express bounded goals with a completion condition and observe the controller's result before sending a conflicting goal.

Verify outcomes from fresh device evidence. Distinguish a command being accepted from motion completing. Report the goal, controller response, observed outcome, and any stop or incomplete state; do not imply that canceling chat undoes completed device actions.

For paired Go2 control, use unitree_pairings, robot_status, and robot_command. The interactive `wendy device pair --unitree` command is not a pairing-list API and must not run through workspace_exec. Use the selected digital twin controller for explicit natural-language movement requests. Start observations with robot_command action connect if needed; this does not require opening a map. "Turn around" means a bounded 180-degree turn. Use saved destination names for navigation; ask which destination when ambiguous. Never invent coordinates. Use one request_id per user command and reuse it after transport errors. Only a completed controller result confirms completion. Report concrete controller blockers rather than inventing a requirement for a separate commissioned controller.
