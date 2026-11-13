# With MCP tools

The Wendy MCP server covers the same operations on its connected device. Enable
the groups named below with `wendy_tools(groups=[...])` first; each call replaces
the enabled set, so list every group you still need.

- Inspect (core): `device_info` (versions, storage, battery) and
  `hardware_capabilities(category=...)`.
- Default device (`setup`): `device_set_default(address=...)`.
- WiFi (`setup`): `wifi_status`, `wifi_list`, `wifi_known_networks`,
  `wifi_connect(ssid=..., password=...)` and `wifi_disconnect`. Connecting drops
  the current network, so the session may lose the device. Forget and rank have
  no MCP tool; use the CLI. For first-boot setup of an unenrolled device that
  `device_connect` refuses, pass its selector as `device` to the Wi-Fi tool
  itself. That device's identity cannot be verified, so `wifi_connect` with a
  password returns `CONFIRMATION_REQUIRED`: tell the user the password is sent
  unencrypted, and retry with `allow_unverified_device=true` only if they agree.
- Updates (`setup`): `device_update_agent` (`nightly`, `binary_path`) updates only
  the agent, verifies it and reconnects. `os_update` starts an OS update;
  `os_update_status` reports whether it committed or rolled back, and why.
- Cameras and Bluetooth (`hardware`): `camera_list`, `camera_controls`,
  `camera_set_control`, `camera_snapshot(device_id=...)` (activates the camera,
  so ask first), `bluetooth_scan`, `bluetooth_connect` and `bluetooth_disconnect`.
- Cloud (`cloud`): `cloud_discover`, `cloud_connect`, `cloud_ping`, and
  `cloud_tunnel(local_port=..., remote_port=...)` with `cloud_tunnel_list` and
  `cloud_tunnel_close(id=...)` for port forwards owned by this server.
- Enrollment (`setup`): `cloud_enroll_device` uses the current Cloud login;
  `provisioning_start` needs a supplied enrollment token; `provisioning_status`
  checks the result.
