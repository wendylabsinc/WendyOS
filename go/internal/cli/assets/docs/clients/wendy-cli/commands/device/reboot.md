# `wendy device reboot`

Reboots the target device and waits until it answers again from a new boot.

## Usage

```sh
wendy device reboot [--yes] [--no-wait] [--recovery] [--json]
```

Running apps stop with the device. Apps that are set to restart come back after boot.

## What it does

The command asks before it does anything:

```text
A reboot stops the apps running on my-device.local until it is back.
Reboot my-device.local now? (y/n)
```

After a yes, the agent answers the request and starts an orderly reboot two seconds later. The command then waits, for up to five minutes, until the device answers from a boot that began after the request:

```text
✓ my-device.local is back online (up 21s).
```

Reaching the device is not enough for that message. The agent keeps answering for a few seconds while the device shuts down, so the command compares the device's uptime with the time that has passed since the request.

## Rebooting into recovery mode

On an NVIDIA Jetson, `--recovery` reboots the device into forced-recovery (USB recovery) mode, the mode [`wendy os install`](../os/install.md) flashes it in. It does what holding the Force Recovery button while pressing Reset does, without touching the board, and what `reboot forced-recovery` does when run on the device.

```text
A reboot into forced-recovery mode stops the apps running on my-jetson.local and takes it off the network.
It stays in recovery mode until it is flashed with 'wendy os install' or power-cycled.
Reboot my-jetson.local into forced-recovery mode now? (y/n)
```

A device in recovery mode has no network, so the command waits, for up to two minutes, only until the device stops answering:

```text
✓ my-jetson.local has gone down to boot into forced-recovery mode. Flash it with 'wendy os install', or power-cycle it to leave recovery mode.
```

Whether the device reached recovery mode shows on the USB side: connect its flashing port to the computer and run `wendy os install`. Other boards have no such mode, and their agent refuses the request instead of rebooting normally.

## Options

| Flag | Meaning |
|---|---|
| `--yes`, `-y` | Reboot without asking. Required when no terminal is attached, including with `--json`. |
| `--no-wait` | Return as soon as the device has accepted the reboot. |
| `--recovery` | Reboot an NVIDIA Jetson into forced-recovery mode. The device stays there until it is flashed or power-cycled. |
| `--json` | Print one JSON object: `{"status": "rebooted", "device": "...", "mode": "normal", "uptimeSeconds": 21}`, or `{"status": "requested", ...}` with `--no-wait`. With `--recovery`, `mode` is `"forced-recovery"` and `status` is `"offline"` once the device has gone down. |

## When to use it

- A device is reachable but misbehaving, and you are not next to it to power-cycle it.
- A device that was unenrolled cannot enroll again because its reset cleanup keeps failing. Leftover network rules and routes from the previous boot are gone after a reboot. See [`wendy device enroll`](./enroll.md).
- A Jetson that runs WendyOS needs to be reflashed, and its Force Recovery button is hard to reach. Use `--recovery`.

An unenrolled device accepts a reboot on its setup port, like the rest of setup. An enrolled device accepts it only from a signed-in user of its organization. `wendy cloud device reboot` sends the same request through Wendy Cloud.

## When it refuses

- **`the agent on <device> does not support reboot`**: the device runs an agent from before this command, or the macOS agent. Update it with [`wendy device update`](./update.md).
- **`an agent or OS update is being installed`**: a reboot now would interrupt the update. Run the command again when the update has finished.
- **`<device> went down for the reboot but has not come back`**: the device stopped answering and did not return within five minutes. Check its power and network, then look for it with `wendy discover`.
- **`the CLI did not see <device> restart`**: the device accepted the request but kept answering from the boot it was already in, so the host did not reboot. With `--recovery` the message is **`<device> kept answering from the boot it was already in`**.
- **`forced-recovery mode exists only on NVIDIA Jetson boards`**: `--recovery` was used on a Raspberry Pi, an x86 device or a simulator.
- **`<device> rebooted back into its OS instead of into forced-recovery mode`**: the device went down and came back on the network, so its board did not act on the request. Use the Force Recovery button instead.
