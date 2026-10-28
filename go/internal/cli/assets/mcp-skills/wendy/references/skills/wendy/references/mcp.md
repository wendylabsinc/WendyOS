# Wendy MCP tools

When the Wendy MCP server (`wendy mcp serve`) is connected, prefer its tools to
CLI commands for device state, apps, logs and deploys. Results are structured
and reuse the session's single device connection.

## Deploy and verify

1. Call `wendy_status`. It reports the connection, Cloud sign-in (`auth`), the
   server's CLI version and any update notice, and a suggested next step.
2. Find a target with `device_list` (`scan=true` adds LAN discovery). Pass its
   selector unchanged to `device_connect`: host:port, `vm:<name>` or a
   `cloud://` selector. `device_disconnect` closes the connection. With no
   physical device, follow the [wendy](../SKILL.md) skill's
   *No device available* section. If `wendy_status`'s `suggested_next_step`
   does not mention simulators, the server predates simulator rows: enable the
   `simulator` group and call `simulator_list`.
3. Deploy with `run(project_path=<absolute path>)`. It reuses the connected
   target unless you pass `device`. `start=false` creates without starting.
   By default, `run` deploys every service and waits for readiness when starting.
   Pass `service="api"` to deploy that service and its dependencies. On supported
   WendyOS targets, `wait_ready=true` waits for configured TCP readiness or
   process stability; `readiness_timeout_seconds=30` bounds that wait within
   `timeout_seconds`. Set `wait_ready=false` to skip the wait and report
   `readiness: "not_checked"`; local Docker/Podman require this opt-out.
   Create-only requests skip readiness unless an incompatible explicit wait is requested.
   Success returns a compact receipt. Read `build_log_uri` for the bounded log
   tail (last eight runs in this server session), or set `include_output=true`.
   Check `project_validate(project_path=...)` (`setup` group) first when the
   configuration is new or changed.
4. Verify: `container_list` (`running_state`, `termination_reason`), then
   `telemetry_logs(app_name=..., service_name=..., min_severity=...)` for
   startup errors, then the app's own output. `telemetry_logs` returns a
   bounded sample, not a live stream. `app_inspect` (`observability` group)
   adds service states, recorded exits and recent errors; with `project_path`
   it also checks the declared TCP readiness on a direct device. Request
   `fields=["state","readiness"]` to skip logs and usage. Repeated diagnostic
   rows include `repeat_count` and first/last observed timestamps. A passed TCP
   check does not establish application behavior.

`container_start` may restart a running app. `container_delete` takes
`delete_image` and `delete_volumes`. `container_exec(app_name=..., command=[...])`
runs a bounded command in a running app, without shell expansion, TTY or stdin.

## Sign-in

On `AUTH_REQUIRED`, or a device error that names `wendy auth login`, call
`auth_login` and show the user the returned link; it must be opened on the
computer running the server. Fall back to `wendy auth login` in a terminal only
when `auth_login` is unavailable. When `wendy_status` reports `auth` as
`logged_out` or `expired`, only Cloud devices are out of reach: local simulators
and LAN devices that are not Cloud-enrolled need no sign-in. Continue with
those, and sign in only when the task needs a Cloud device.

## Tool groups

Only the core group is listed at first. `wendy_tools(groups=[...])` replaces the
enabled specialist groups (core always stays), so list every group you still
need. Hosts that cache the tool list can start `wendy mcp serve --tool-groups all`.
The hosted relay (`wendy cloud mcp`) offers a subset of these same tools under
its own permission policy.

| Group | Tools | Use for |
|---|---|---|
| core | `wendy_status`, `wendy_tools`, `auth_login`, `device_list`, `device_connect`, `device_disconnect`, `device_info`, `hardware_capabilities`, `run`, `container_list`, `container_start`, `container_stop`, `container_delete`, `container_stats`, `container_exec`, `telemetry_logs` | Connect, deploy, control apps, read app logs, versions, battery and hardware |
| setup | `project_validate`, `os_install_plan`, `os_list_drives`, `os_install_start`, `os_install_resume`, `os_install_status`, `os_install_verify`, `os_update`, `os_update_status`, `device_update_agent`, `device_set_default`, `cloud_enroll_device`, `provisioning_start`, `provisioning_status`, `wifi_list`, `wifi_status`, `wifi_known_networks`, `wifi_connect`, `wifi_disconnect` | Project checks, installs, agent and OS updates, saved default, Cloud enrollment, WiFi |
| simulator | `simulator_list`, `simulator_create`, `simulator_start`, `simulator_stop`, `simulator_delete` | Local VMs; `device_connect(device="vm:<name>")` also boots one |
| hardware | `camera_list`, `camera_controls`, `camera_set_control`, `camera_snapshot`, `bluetooth_scan`, `bluetooth_connect`, `bluetooth_disconnect` | Cameras and Bluetooth |
| robotics | `ros2_topics`, `ros2_topic_info`, `ros2_topic_sample`, `ros2_topic_hz`, `ros2_lidar_summary` | ROS 2 inspection; see `wendy-robot-deploy` |
| observability | `app_inspect`, `job_inspect`, `job_artifacts_pull`, `compute_status`, `job_submit`, `checkpoint_inspect`, `experiment_plan`, `device_os_logs`, `telemetry_metrics`, `telemetry_traces` | Service state, training jobs (see `wendy-training`), recorded exits, kernel log (dmesg), metrics, traces |
| cloud | `cloud_discover`, `cloud_connect`, `cloud_ping`, `cloud_tunnel`, `cloud_tunnel_list`, `cloud_tunnel_close` | Cloud devices, reachability and port forwards |

## Resources, prompts and errors

- Resources: read `wendy://guide` for tool details this reference omits,
  `wendy://docs/...` for Wendy documentation (listed by `resources/list`), and
  `wendy://diagnostics` when an expected tool is missing.
- Prompts: `deploy_app`, `diagnose_container` and `provision_device`.
- App tools: a running app with an `mcp` entitlement adds tools prefixed with its
  app name while its device is connected.

Errors carry an `error_code` to branch on: `NOT_CONNECTED`, `DEVICE_UNREACHABLE`,
`ENTITLEMENT_DENIED`, `AUTH_REQUIRED`, `INVALID_ARGUMENT`, `NOT_FOUND`,
`MULTIPLE_SESSIONS`, `UNSUPPORTED`, `TIMEOUT`, `CANCELLED`, `INTERNAL`, or
`DENIED` when a `wendy cloud mcp` relay policy refuses the call. List
results nest rows under one key such as `devices`, `containers` or `logs`. Ask
before calls that interrupt or erase: `container_stop`, `container_delete`,
`wifi_connect` (the device leaves its current network), `os_update`,
`device_update_agent` and `simulator_delete`.

A running server keeps the tools of the CLI that started it. When `wendy_status`
reports a CLI update, give the user the update command and ask them to restart
the MCP server; newer skill text cannot add tools to an older process.
