# `wendy device power-mode`

Shows and sets the NVIDIA Jetson power mode (`nvpmodel`) of the target device.

## Usage

```sh
wendy device power-mode                      # interactive in a terminal, `list` otherwise
wendy device power-mode list [--json]
wendy device power-mode get [--json]
wendy device power-mode set <name|id> [--yes]
```

## Interactive screen

Run in a terminal, `wendy device power-mode` opens a list of the device's modes with the current one and the boot default marked. Use the arrow keys to move and Enter to set the highlighted mode, or Esc (or `q`) to leave without changing anything. A mode that needs a reboot asks first, exactly as `set` does (below), and the screen then waits for the device to come back and shows the mode it reports.

Without an interactive terminal, or with `--json`, the bare command prints the same output as `list`, so scripts that call it keep working.

## List the current and available modes

`wendy device power-mode list` prints the mode the device is in and every mode its `nvpmodel` configuration offers:

```text
Power mode: MODE_30W (2)

Available modes:
    0  MAXN
    1  MODE_15W
  * 2  MODE_30W  (current, boot default)
    3  MODE_50W
```

Modes are the device's own names and IDs, exactly as `/etc/nvpmodel.conf` declares them. They differ between Jetson models, and Wendy does not translate or alias them. `boot default` is the mode `nvpmodel` applies at boot (`PM_CONFIG DEFAULT`) and is omitted when the configuration does not declare one.

With `--json`:

```json
{
  "current": {"id": 2, "name": "MODE_30W"},
  "available": [{"id": 0, "name": "MAXN"}, {"id": 2, "name": "MODE_30W"}],
  "bootDefault": {"id": 2, "name": "MODE_30W"}
}
```

## Get the current mode

`wendy device power-mode get` prints only the current mode, which suits scripts:

```text
MODE_30W (2)
```

With `--json` it prints `{"current": {"id": 2, "name": "MODE_30W"}}`. The current mode also appears in [`wendy device info`](./info.md).

## Set a mode

```sh
wendy device power-mode set MAXN
wendy device power-mode set 0
```

The argument is a mode name or numeric ID from the list above. An unknown mode is rejected with the available modes.

Some modes switch immediately. Others only take effect after a reboot. For those, the command says so and asks before doing anything:

```text
Power mode MAXN (0) only takes effect after a reboot.
Set MAXN (0) and reboot the device now? (y/n)
```

- **Yes** sets the mode and reboots the device in one step.
- **No** leaves the device untouched. A mode that needs a reboot is never half-applied: NVIDIA's `nvpmodel` records it only when the reboot is accepted.

Without an interactive terminal (a script, CI, or `--json`), the command never asks. If the mode needs a reboot it exits with an error and changes nothing; pass `--yes` (`-y`) to confirm in advance and set the mode and reboot without asking. `--yes` has no effect on a mode that switches immediately.

With `--json`:

```json
{"mode": {"id": 0, "name": "MAXN"}, "rebootRequired": true, "rebootScheduled": true}
```

A mode change can alter CPU and GPU clocks under running workloads, and a reboot stops every app on the device. Check what else is using the device first.

## Unsupported devices and older agents

Devices that are not an NVIDIA Jetson report that power modes are not supported. A device whose agent predates this command reports that the agent needs updating (`wendy device update`).

## Related

- [`wendy device info`](./info.md) — includes the current power mode
