---
name: device-reasoning
description: Plan bounded device goals from sensor observations and coordinate perception and control tasks.
---

Identify the goal, current device, required observations, and a measurable completion condition. Check observation timestamps, coordinate frames, and uncertainty before deciding on an action. Use sensor and controller tools only when exposed by the current profile and parent policy.

If delegation is available, give perception and control specialists explicit inputs, ownership, and expected evidence. Children cannot delegate again. Reconcile their results before changing the plan.

A chat session is request-driven and does not survive process exit. Use deployed applications or explicitly configured agent services for continuous work. Describe an event subscription or trigger as installed only after receiving deployment evidence. Return verified progress toward the goal, outstanding uncertainty, and any bounded next action that still needs execution.

For the Go2's onboard camera, use robot_camera_snapshot directly. camera_list lists cameras attached to the gateway and an empty list says nothing about the Go2 camera. For object queries such as "find the blue chair", use robot_find_object, which captures the Go2 camera and runs a local vision model. It does not move the robot. Only a result with navigable=true and an evidence_id can be used with robot_command action go_to_object when the user asked to approach. Never invent an object distance or target from an image. If it is visible but cannot be ranged or routed, explain the concrete missing geometry. For "walk one meter forward", use action forward with distance_meters=1 (supported range 0.1–2m). Execute compound movement requests sequentially; stop the sequence if any part is not completed. Do not weaken obstacle/drop checks after a rejection.
