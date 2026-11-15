# `wendy device reboot`

Reboots the target device and waits until it answers again from a new boot.

## Usage

```sh
wendy device reboot [--yes] [--no-wait] [--json]
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

## Options

| Flag | Meaning |
|---|---|
| `--yes`, `-y` | Reboot without asking. Required when no terminal is attached, including with `--json`. |
| `--no-wait` | Return as soon as the device has accepted the reboot. |
| `--json` | Print one JSON object: `{"status": "rebooted", "device": "...", "uptimeSeconds": 21}`, or `{"status": "requested", "device": "..."}` with `--no-wait`. |

## When to use it

- A device is reachable but misbehaving, and you are not next to it to power-cycle it.
- A device that was unenrolled cannot enroll again because its reset cleanup keeps failing. Leftover network rules and routes from the previous boot are gone after a reboot. See [`wendy device enroll`](./enroll.md).

An unenrolled device accepts a reboot on its setup port, like the rest of setup. An enrolled device accepts it only from a signed-in user of its organization. `wendy cloud device reboot` sends the same request through Wendy Cloud.

## When it refuses

- **`the agent on <device> does not support reboot`**: the device runs an agent from before this command, or the macOS agent. Update it with [`wendy device update`](./update.md).
- **`an agent or OS update is being installed`**: a reboot now would interrupt the update. Run the command again when the update has finished.
- **`<device> went down for the reboot but has not come back`**: the device stopped answering and did not return within five minutes. Check its power and network, then look for it with `wendy discover`.
- **`the CLI did not see <device> restart`**: the device accepted the request but kept answering from the boot it was already in, so the host did not reboot.
