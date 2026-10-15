# `wendy device pair`

Discover and manage paired devices in the `Bluetooth | SensorLink | IP Cameras | USB Arms`
tabs. Use `Tab` or `Shift+Tab` to switch tabs. Opening a tab refreshes its devices.

```sh
wendy device pair
```

| Key | Action |
|-----|--------|
| `↑` / `↓` | Select a device |
| `Tab` / `Shift+Tab` | Switch tabs |
| `Enter` | Pair a device, or edit an IP camera's login |
| `f` | Forget the selected device |
| `r` | Refresh discovery |
| `q` / `Esc` | Quit |

Bluetooth scans on the connected WendyOS device. Unnamed peripherals are hidden
by default; `h` shows or hides them. `Enter` connects and pairs, and `d`
disconnects the selected peripheral.

SensorLink discovers sources on the CLI's local network. Devices must belong to
the same organization as the target device, and you must be logged into that
organization. An mTLS Wendy Lite source with a valid advertised PKI principal can
pair without a numeric asset ID when the target Agent supports principal pairing.
Update the target Agent if it reports that principal pairing is unsupported.
Sources without either a numeric asset ID or an authenticated PKI identity appear
under "Found sensors that need enrollment". Enroll those sources before pairing,
then press `r` to rescan. Saved pairings remain available to forget when their
sources are offline.

IP cameras are discovered on the connected device's network. `Enter` opens a
username and password form that saves the camera's login on the device. Passwords
are masked. Inside the form, `Tab` switches fields, `Enter` advances or saves, and
`Esc` returns to the camera list. Saved offline cameras can be forgotten with `f`,
which removes the camera and its stored login.

| Flag | Description |
|------|-------------|
| `--list` | Print current SensorLink pairings |
| `--name` | Set a friendly name for a SensorLink pairing |
| `--sensors` | Limit a SensorLink pairing to these sensor names |

Passing `--name` or `--sensors` opens the SensorLink tab first.

The camera username starts with `admin` as an editable suggestion. Enter the
account configured on your camera and replace any factory-default password
before pairing it.

## USB robot arms (native Mac and Linux Agents)

Linux uses the Go Agent's `robotarm.DiscoverLinux` sysfs inventory. Native
macOS uses the separate Swift Agent implementation (`USBSerialInventory` and
`RobotArmService`) backed by IOKit. Running the Go Agent on macOS does not
provide USB arm discovery; select an updated native Mac Agent instead.

The **USB Arms** tab lists USB-backed serial endpoints attached to the selected
Agent's host, alongside saved arm bindings. A CLI on laptop A targeting laptop B
shows B's devices and saves records on B. It does not scan A's local serial ports.
Start directly on that tab with:

```sh
wendy device pair --device <target-laptop> --usb-arms
```

Select an endpoint to inspect its current path, VID/PID, USB serial number,
manufacturer/product, interface and USB location. `Enter` asks you to identify it
as an SO-101; `y` saves the binding and `n` or `Esc` cancels. `f` forgets a saved
binding, including when it is disconnected. `r` refreshes host metadata.

USB **pairing means saving host-local device configuration**, not Bluetooth
bonding. Currently `so101` is the supported binding model. Shared WCH adapter IDs
(`1a86:55d3`, `1a86:55d4`, `1a86:7523`) are adapter candidates only; they do not
prove that an SO-101 is attached. Other USB serial endpoints appear as **unknown
USB serial devices**. You may explicitly identify one as SO-101, but that is a
user configuration assertion, not hardware verification. Built-in consoles and
Bluetooth serial endpoints are excluded.

- **Candidate / unknown:** discovered USB serial metadata; no arm configured.
- **Configured / unverified:** the user saved an SO-101 binding; the arm has not
  been physically verified. `verified` remains false.
- **Present:** one matching USB endpoint exists. This does not establish motor
  power, serial access, port availability, joint telemetry or control readiness.
- **Disconnected:** a saved binding has no matching current endpoint. Its current
  path is empty and it remains available to forget.
  New binding is rejected; reconnect the device and refresh first.
- **Ambiguous:** several endpoints share the same identity. They are shown with
  their current paths, but `connected` is false and binding is blocked until the
  ambiguity is resolved. The Agent never chooses one automatically.

Serial-based identity includes VID/PID, adapter serial number and USB interface;
node renumbering and USB socket changes retain the binding. Different serial
numbers or interfaces remain distinct. Devices without a serial number use their
USB topology location and interface instead. This identity follows the **USB
port**, including a replacement adapter; it cannot establish physical-device
continuity. Interactive saving explicitly acknowledges this limitation. Scripts
must pass `--allow-location-binding`. Changing the hub/topology may require a new
binding. No machine-specific `/dev` path is hard-coded or trusted as identity.

For scripts, list the target's inventory and bindings as JSON, then use an exact
`id` from that response:

```sh
wendy --json device pair --device <target-laptop> --usb-arms --list
wendy --json device pair --device <target-laptop> --usb-arms --bind '<id>' --arm-name Follower
wendy --json device pair --device <target-laptop> --usb-arms --forget '<id>'
```

The additive USB JSON list is `{ "devices": [...] }`, including an empty array
when nothing is found. Bind returns `{ "device": {...} }`; forget returns
`{ "forgotten": "<id>" }`. Fields use snake_case and include explicit default
values, including `configured`, `verified`, `connected` and `access_status`.
Without `--usb-arms`, existing `--list`, `--name` and `--sensors` retain their
SensorLink meaning. USB operations cannot be combined with SensorLink flags.
Older Agents return an explicit update/unsupported message; there is no fallback
to discovery on the CLI's machine.
`--arm-name` accepts at most 128 UTF-8 bytes. Binding requires a currently
connected, unambiguous endpoint; metadata discovery alone cannot authorize it.

The Linux Agent stores bindings in `/var/lib/wendy/robot-arm-bindings.json`.
The Mac Agent stores them in `robot-arm-bindings.json` inside its existing
Application Support state directory (the running app's bundle identifier, or
process name when unbundled). Records survive Agent restarts. Forgetting deletes
only the saved configuration; it does not disconnect a USB device or change motors.
If the binding file is corrupt, list, bind and forget fail without overwriting
it. Inspect and back up the file on that Agent's host before repairing its JSON
or deliberately resetting saved configuration. Disconnecting the USB device
does not repair a corrupt file.

Discovery, save and forget only read OS metadata or update that JSON file. They
never open a serial port, send torque writes, home/calibrate motors, access a
camera or enable physical control. `access_status` is `unknown`: busy, permission
and unpowered-servo conditions cannot be diagnosed from metadata alone. Use `r`
after reconnecting hardware; a disappearing device cannot be newly bound.

Pairing does **not** install or start an SO-101 hardware adapter and does not
produce live joint telemetry. Observation, calibration verification and control
remain separate, explicitly managed work. A future adapter can resolve a saved
binding's identity through the Agent's current inventory; it must recheck
connection/ambiguity and manage exclusive access before using the serial port.
Calibration import/reference selection is not implemented by this increment.
