---
name: device-sensors
description: Inspect cameras, audio, ROS 2, and other sensors, and configure or evaluate perception workers.
---

Before connecting, check that the tool required for the task is present in the offered tool list. For camera inventory, that is camera_list. If absent, report the capability gap immediately; a connection cannot restore a profile-filtered tool.

For online-camera inventory, use the parent's discovered device identities and endpoint rather than repeating discovery. Call wendy_status to inspect session state, connect to the requested device with cloud_connect or device_connect, then call camera_list. Keep each device's results separate if a task names several devices. Inventory does not require a snapshot, stream, or model download.

Return the exact device and camera IDs, reported names and online flags where available, and one of: cameras found, successful empty inventory, or inspection failed/unavailable. Do not turn a failed inspection into "no cameras" or infer camera health solely from device reachability.

For perception work, inspect hardware, runtime compatibility, and GPU memory before selecting or downloading a model; pin revisions. Use deployed workers for continuous inference. Define model-specific confidence, freshness, persistence, cooldown, and evidence for event criteria. A snapshot is not a continuous feed, and a proposed trigger is not an installed trigger.

For Wendy Data source selection and capture planning, read `wendy-data/SKILL.md` through profile_skill. Camera or ROS inventory alone does not prove the Data recorder can capture that source. This profile has no workspace_exec, so if dedicated Data tools are absent, report that gap and return discovered source identities and requirements to the parent for developer or debugger execution. In a standalone session, explain that a profile with workspace_exec is needed for the documented CLI workflow. Do not substitute container_exec for missing host CLI access.

For the Go2's onboard camera, use robot_camera_snapshot directly. camera_list lists cameras attached to the gateway and an empty list says nothing about the Go2 camera. For object queries such as "find the blue chair", use robot_find_object, which captures the Go2 camera and runs a local vision model. It does not move the robot. Only a result with navigable=true and an evidence_id can be used with robot_command action go_to_object when the user asked to approach. Never invent an object distance or target from an image. If it is visible but cannot be ranged or routed, explain the concrete missing geometry. For "walk one meter forward", use action forward with distance_meters=1 (supported range 0.1–2m). Execute compound movement requests sequentially; stop the sequence if any part is not completed. Do not weaken obstacle/drop checks after a rejection.
