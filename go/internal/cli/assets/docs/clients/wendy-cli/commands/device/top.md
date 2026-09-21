# `wendy device top`

Live CPU, memory, disk, GPU, and temperature telemetry for the device and its containers — an `htop`-style monitor for a WendyOS device.

## Usage

```sh
wendy device top [flags]
```

## Description

`wendy device top` opens a full-screen, auto-refreshing dashboard showing whole-machine CPU and memory utilization, the hottest current device temperature, per-GPU utilization/memory (and temperature/power where reported), and a per-app/per-container table of CPU% and memory. CPU percentages are computed from deltas between refreshes, so the first frame may read low until a second sample is taken.

The temperature header normally shows the maximum reading and its source. A yellow `●` appears when any classified sensor is within 5°C of its operational warning threshold; the circle turns red at or above that threshold. On a Unitree Go2 discovered through LowState telemetry, the agent adds the IMU and 12 physical motor temperatures to the Linux thermal zones and expires them after 15 seconds without a fresh sample. The current operational thresholds are 70°C for Go2 motors and 85°C for the Go2 IMU and host thermal zones. These values were observed and chosen for Woof operations; they are not Unitree or NVIDIA vendor ratings.

When the device stops answering polls — because it lost power, dropped off the network, or ran its battery flat — the dashboard raises a banner in place of the usual status flash:

```
 ⚠ DEVICE OFFLINE — no response for 41s (last battery reading 3%, discharging)
 Readings below are the last values received, not live.
```

The meters keep showing the last successful sample, since there is nothing newer to draw, and the second line marks them as stale. The battery percentage is named only when that last sample showed the pack discharging — a charging or full pack is not evidence of why the device went away. Before the first sample arrives the dashboard shows a `Connecting…` placeholder instead; once the device is offline the banner replaces that placeholder rather than appearing alongside it.

Only transport-level failures raise the banner. An error the agent itself returns means the device answered and is therefore still reachable, so it appears as the normal status flash at the bottom of the screen — and it clears the banner if one was up. The banner also clears on the next successful poll.

Apps are grouped the same way as [`wendy device dashboard`](dashboard.md): multi-service apps show a group header with one subrow per service. Running apps (`●`), stopped apps (`○`), and crash-looping apps (`↻`) have distinct row styling and are counted separately. Resource columns show unavailable values for stopped and crash-looping rows instead of presenting them as active zero-usage workloads. A side panel shows the listening ports of the currently selected running app.

Press `Enter` to open the selected app's recent and live logs. Use the arrow keys or Page Up/Page Down to scroll, `End` to resume following, and `Esc` to return to the monitor. Press `s` to start the selected app.

The disk meter shows the filesystem holding container storage, which is `/data` on WendyOS. It refreshes every ten seconds. Press `p` to clear unused container layer and snapshot caches. Current images and active apps remain intact; containerd reclaims unreachable data in the background.

Press `x` to stop the selected app. For a multi-service app, this stops the whole app even when the cursor is on one of its service rows. Stop uses Wendy's normal graceful shutdown behavior and may escalate to a force kill when the app does not exit within its grace period.

> **Note:** This command requires a recent device agent. Against an agent that's too old to report resource stats, the command reports that the agent doesn't support resource stats and suggests updating it with [`wendy device update`](update.md).

### Keyboard shortcuts

| Key | Action |
|---|---|
| `↑` / `k`, `↓` / `j` | Move the selection up / down |
| `c` | Sort apps by CPU usage (descending) |
| `m` | Sort apps by memory usage (descending) |
| `Enter` | Open recent and live logs for the selected app |
| `Esc` | Return from logs to the monitor |
| `s` | Start the selected app and all of its services |
| `x` | Stop the selected app and all of its services |
| `p` | Clear unused container caches |
| `q` / `Ctrl+C` | Quit |

## Flags

| Flag | Default | Description |
|---|---|---|
| `--interval` | `2s` | Refresh interval for the live view. A device that loses power mid-request cannot freeze the dashboard — a stalled poll times out and raises the offline banner. |

The [global `--json` flag](../../global-flags.md) is also honored — see below.

## JSON snapshot mode

`wendy device top` is a live TUI, so it cannot stream into a pipe. When `--json` is passed (or stdout is not a TTY), the command switches to a **one-shot snapshot**: it samples the device, prints a single JSON object, and exits instead of rendering the dashboard.

```sh
wendy device top --json
```

Plain snapshots include a `STATE` column. JSON snapshots have this shape:

```json
{
  "host": {
    "cpuPercent": 12.5,
    "cpuCount": 8,
    "memUsedBytes": 2147483648,
    "memTotalBytes": 8589934592,
    "thermalZones": [
      { "name": "go2/imu", "tempC": 79.0 },
      { "name": "go2/motor/fr-thigh", "tempC": 66.0 },
      { "name": "tj-thermal", "tempC": 55.0 }
    ],
    "maximumTemperature": { "name": "go2/imu", "tempC": 79.0 },
    "gpus": [
      {
        "index": 0,
        "name": "Orin",
        "utilPercent": 30.0,
        "memUsedBytes": 1073741824,
        "memTotalBytes": 8589934592,
        "tempC": 45.0,
        "powerW": 7.5
      }
    ]
  },
  "containers": [
    { "name": "my-app", "state": "running", "cpuPercent": 4.2, "memBytes": 134217728 }
  ]
}
```

- `host.containerStorage` reports `mountpoint`, `usedBytes`, and `totalBytes` for the container filesystem, and is omitted when unavailable. Plain snapshots also show this disk usage.
- `host.gpus` is omitted on devices that report no GPU.
- `host.thermalZones` is omitted when the agent has no readable temperature source.
- `host.maximumTemperature` is the hottest valid thermal-zone or GPU reading and is omitted when no temperature is available.
- Go2 IMU and motor temperatures require a device agent with the LowState thermal extension; older agents continue to report host thermal zones only.
- Each GPU's `tempC` and `powerW` are omitted when the agent doesn't report them.
- `containers[].state` is `running`, `stopped`, or `crash-loop`.
- `containers[].cpuPercent` is each container's share of the whole machine (0–100 across all cores).

## Related

- [`wendy device dashboard`](dashboard.md) — full-screen app/service status dashboard
- [`wendy device info`](info.md) — one-shot device hardware and GPU metadata
- [`wendy device apps`](apps/) — list and manage deployed apps
