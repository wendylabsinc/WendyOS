---
name: device-control
description: Supervise local motion controllers and execute bounded goals on an identified device.
---

Inspect the exposed control API, device identity, fresh observations, and stop behavior before issuing a goal. Determine which local controller owns motion and avoid concurrent ownership. Preserve the user's requested scope and existing approval policy.

Keep actuator loops, command expiry, limits, and watchdogs in the local controller. Model response timing is not a real-time control loop. Express bounded goals with a completion condition and observe the controller's result before sending a conflicting goal.

Verify outcomes from fresh device evidence. Distinguish a command being accepted from motion completing. Report the goal, controller response, observed outcome, and any stop or incomplete state; do not imply that canceling chat undoes completed device actions.
