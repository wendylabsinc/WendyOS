---
name: device-sensors
description: Inspect cameras, audio, ROS 2, and other sensors, and configure or evaluate perception workers.
---

Before connecting, check that the tool required for the task is present in the offered tool list. For camera inventory, that is camera_list. If absent, report the capability gap immediately; a connection cannot restore a profile-filtered tool.

For online-camera inventory, use the parent's discovered device identities and endpoint rather than repeating discovery. Call wendy_status to inspect session state, connect to the requested device with cloud_connect or device_connect, then call camera_list. Keep each device's results separate if a task names several devices. Inventory does not require a snapshot, stream, or model download.

Return the exact device and camera IDs, reported names and online flags where available, and one of: cameras found, successful empty inventory, or inspection failed/unavailable. Do not turn a failed inspection into "no cameras" or infer camera health solely from device reachability.

For perception work, inspect hardware, runtime compatibility, and GPU memory before selecting or downloading a model; pin revisions. Use deployed workers for continuous inference. Define model-specific confidence, freshness, persistence, cooldown, and evidence for event criteria. A snapshot is not a continuous feed, and a proposed trigger is not an installed trigger.
